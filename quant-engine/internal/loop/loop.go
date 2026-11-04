// Package loop runs the engine once per closed candle until it is stopped or
// halted, keeping state.json and the journal in the working directory.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/engine"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/news"
	"github.com/rosaldo/agency-agents/quant-engine/internal/risk"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// Journal event kinds the loop adds.
const (
	Start     = "start"
	Heartbeat = "heartbeat" // lets a quiet demo period count toward the mainnet gate
	Stop      = "stop"
	Error     = "error"
)

const (
	beatEvery  = time.Second     // state.json heartbeat (wall clock)
	staleAfter = 5 * time.Second // a heartbeat older than this means the loop is dead
	journalGap = time.Hour       // minimum spacing of journal heartbeats (engine clock)
)

// Ticker paces the loop: Now is the engine clock, Next waits for the next
// closed candle.
type Ticker interface {
	Now() time.Time
	Next(ctx context.Context) error
}

// RealTicker wakes Grace after each interval boundary.
type RealTicker struct {
	Interval, Grace time.Duration
}

func (RealTicker) Now() time.Time { return time.Now() }

func (r RealTicker) Next(ctx context.Context) error {
	wait := time.Until(time.Now().Truncate(r.Interval).Add(r.Interval + r.Grace))
	select {
	case <-time.After(wait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// FakeTicker replays the fake exchange's candles, one per Next, at Delay.
// Now may be read while Next waits (a news verdict handled meanwhile).
type FakeTicker struct {
	Ex      *fake.Fake
	Candles []market.Candle
	Delay   time.Duration
	OnStep  func(i int) // test hook, called after the step on candle i
	i       atomic.Int64
}

func (f *FakeTicker) Now() time.Time { return f.Candles[f.i.Load()].Time }

func (f *FakeTicker) Next(ctx context.Context) error {
	if f.OnStep != nil {
		f.OnStep(int(f.i.Load()))
	}
	select {
	case <-time.After(f.Delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	if !f.Ex.Advance() {
		return io.EOF
	}
	f.i.Add(1)
	return nil
}

type Limits struct {
	MaxRiskPerTradePct   float64 `json:"max_risk_per_trade_pct"`
	MaxDailyDrawdownPct  float64 `json:"max_daily_drawdown_pct"`
	MaxWeeklyDrawdownPct float64 `json:"max_weekly_drawdown_pct"`
	MinDemoDays          int     `json:"min_demo_days"`
}

// State is <state dir>/state-<level>.json: paper, testnet and mainnet are
// different accounts, so each level keeps its own (the book is per level too).
type State struct {
	Running    bool                  `json:"running"`
	Pid        int                   `json:"pid"`
	Mode       string                `json:"mode"`
	Level      string                `json:"level"` // paper | testnet | mainnet; a state of another level is refused
	ParamsHash string                `json:"params_hash"`
	Heartbeat  time.Time             `json:"heartbeat"`
	Equity     float64               `json:"equity"`
	Breaker    risk.Snapshot         `json:"breaker"` // drawdown bases and halt, restored on start
	Limits     Limits                `json:"limits"`
	Positions  []exchange.Position   `json:"positions"`
	Targets    map[string]float64    `json:"targets"`
	StaleStops map[string]string     `json:"stale_stops,omitempty"` // symbol -> old stop a trail could not cancel yet
	Trades     map[string]risk.Trade `json:"trades,omitempty"`      // symbol -> trade budget, fraction used and tranches (graduated entry); absent = none in progress
	NewsUntil  map[string]time.Time  `json:"news_until,omitempty"`  // symbol -> end of the block a news catastrophe set; absent = none
	// RetargetDue: symbols whose strategy.Retargeter call is owed (the position
	// could not be read after a fill); absent = none
	RetargetDue []string   `json:"retarget_due,omitempty"`
	Funding     string     `json:"funding,omitempty"`    // what the adapter leaves out of P&L
	PaperSim    string     `json:"sim,omitempty"`        // the simulator's profile and costs
	Sizing      string     `json:"sizing,omitempty"`     // the profile and costs sizing counts
	Feed        string     `json:"feed,omitempty"`       // rest | websocket
	FeedState   *FeedState `json:"feed_state,omitempty"` // websocket only
	LastEvent   string     `json:"last_event"`
	StopReason  string     `json:"stop_reason"`
}

var levels = []string{"paper", "testnet", "mainnet"}

func statePath(dir, level string) string { return filepath.Join(dir, "state-"+level+".json") }

// derivPath is the derivatives recorder's file of level in dir.
func derivPath(dir, level string) string { return filepath.Join(dir, "deriv-"+level+".csv") }

// BookPath is the own-order book of level in dir.
func BookPath(dir, level string) string { return filepath.Join(dir, "book-"+level+".json") }

// oldFile is a state file from before the levels were split and the
// real-money level was named mainnet: the levels it may hold and what the
// owner does with it.
type oldFile struct {
	name   string
	levels []string // nil: its "mode" says (live = mainnet, paper = paper or testnet)
	fix    string
}

var oldFiles = []oldFile{
	{"state-live.json", []string{"mainnet"}, "it is the mainnet state under its old name: set its \"mode\" and \"breaker\".\"mode\" to \"mainnet\", add \"level\": \"mainnet\" and rename it to state-mainnet.json"},
	{"book-real.json", []string{"mainnet"}, "it is the mainnet book under its old name: rename it to book-mainnet.json"},
	{"book-demo.json", []string{"paper", "testnet"}, "it is the book paper and testnet shared: rename it to book-testnet.json if the testnet ran in this directory, else to book-paper.json"},
	{"state.json", nil, "it is a state from before the split per level: add \"level\" (paper, testnet or mainnet, the one that wrote it), set \"breaker\".\"mode\" to that level and rename it to state-<level>.json"},
}

// holds reports whether f may hold level's state; an unreadable state.json
// may hold any (fail closed).
func (f oldFile) holds(dir, level string) bool {
	if f.levels != nil {
		return slices.Contains(f.levels, level)
	}
	st, err := readState(filepath.Join(dir, f.name))
	switch {
	case err == nil && st.Mode == "live":
		return level == "mainnet"
	case err == nil && st.Mode == "paper":
		return level != "mainnet"
	}
	return true
}

// keepStops ends every refusal over the state files: nothing was traded, and
// it says how to close positions now without the engine's state.
const keepStops = "positions on the exchange keep their exchange stops, if they have any; to close them now, fix the file and run stop --flatten, or close them on the exchange"

// CheckRenamed refuses a state dir that still holds state, of any level,
// under a name from before the split per level (or a state-<level>.json
// without its level): starting without it would drop the drawdown bases and
// the own-order book in silence. Fail closed; the owner renames the file,
// nothing is migrated here.
func CheckRenamed(dir string) error { return checkLevels(dir, levels...) }

// CheckLevel is CheckRenamed for one level: a file of another level never
// blocks it (stop --flatten of mainnet must not wait on a paper file).
func CheckLevel(dir, level string) error { return checkLevels(dir, level) }

func checkLevels(dir string, want ...string) error {
	if err := renamed(dir, want); err != nil {
		return fmt.Errorf("%w; %s", err, keepStops)
	}
	return nil
}

func renamed(dir string, want []string) error {
	for _, f := range oldFiles {
		path := filepath.Join(dir, f.name)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if slices.ContainsFunc(want, func(l string) bool { return f.holds(dir, l) }) {
			return fmt.Errorf("%s is from before the split per level; %s, then run again", path, f.fix)
		}
	}
	for _, l := range levels {
		st, err := readState(statePath(dir, l))
		if !slices.Contains(want, l) { // another level's file: only a misplaced state of a wanted level counts
			if err == nil && st.Level != l && slices.Contains(want, st.Level) {
				return fmt.Errorf("%s holds level %q; move it to %s", statePath(dir, l), st.Level, filepath.Base(statePath(dir, st.Level)))
			}
			continue
		}
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return fmt.Errorf("%s: %w", statePath(dir, l), err)
		case st.Level == "":
			return fmt.Errorf("%s has no level (written before paper and testnet were split); add \"level\": \"%s\" if %s wrote it (else move it to the level that did and set \"breaker\".\"mode\" to that level), then run again",
				statePath(dir, l), l, l)
		case st.Level != l:
			return fmt.Errorf("%s holds level %q; move it to %s", statePath(dir, l), st.Level, filepath.Base(statePath(dir, st.Level)))
		case st.Breaker.Mode != "" && st.Breaker.Mode != l: // else the halt and drawdown bases would start fresh
			return fmt.Errorf("%s: \"breaker\".\"mode\" is %q, not its level; set it to %q, then run again", statePath(dir, l), st.Breaker.Mode, l)
		}
	}
	return nil
}
func stopPath(dir string) string { return filepath.Join(dir, "stop") }

func readState(path string) (State, error) {
	var st State
	b, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

// LoadState reads the state of level.
func LoadState(dir, level string) (State, error) {
	return readState(statePath(dir, level))
}

// SaveState writes the state of st.Level atomically, so a reader never sees
// half a file.
func SaveState(dir string, st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := statePath(dir, st.Level) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(dir, st.Level))
}

// lastState is the state of the running loop, else of the last one to run.
func lastState(dir string) (State, error) {
	var best State
	found := false
	for _, l := range levels {
		st, err := LoadState(dir, l)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return st, err
		}
		if !found || st.Alive() || (!best.Alive() && st.Heartbeat.After(best.Heartbeat)) {
			best, found = st, true
		}
	}
	if !found {
		return best, os.ErrNotExist
	}
	return best, nil
}

// Alive reports a running loop with a fresh heartbeat.
func (st State) Alive() bool { return st.Running && time.Since(st.Heartbeat) < staleAfter }

const flattenMark = "flatten"

// RequestStop asks the loop in dir to stop and returns at once. Positions
// stay protected by their exchange stops, or, with flatten, the loop closes
// them at market on its way out.
func RequestStop(dir string, flatten bool) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var mark []byte
	if flatten {
		mark = []byte(flattenMark)
	}
	return os.WriteFile(stopPath(dir), mark, 0o600)
}

func flattenRequested(dir string) bool {
	b, err := os.ReadFile(stopPath(dir))
	return err == nil && string(b) == flattenMark
}

// ClearStop drops a stale stop request; start calls it before launching.
func ClearStop(dir string) error {
	if err := os.Remove(stopPath(dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func stopRequested(dir string) bool {
	_, err := os.Stat(stopPath(dir))
	return err == nil
}

// Run is the loop. It returns why it ended, which is also in state.json and
// the journal.
func Run(parent context.Context, cfg *config.Config, s strategy.Strategy, ex exchange.Exchange, env exchange.Env, tick Ticker, dir string) string {
	return RunReady(parent, cfg, s, ex, env, tick, dir, func() {})
}

// RunReady is Run that calls ready once, when every precondition passed and
// just before the first step. If it returns without having called ready, the
// loop never ran and the returned reason says why.
func RunReady(parent context.Context, cfg *config.Config, s strategy.Strategy, ex exchange.Exchange, env exchange.Env, tick Ticker, dir string, ready func()) string {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "state dir: " + err.Error()
	}
	level := cfg.Level()
	if err := CheckRenamed(dir); err != nil {
		return "refused: " + err.Error()
	}
	if err := engine.CheckDerivatives(cfg, s, ex); err != nil {
		return "refused: " + err.Error()
	}
	feed, ft, err := checkFeed(cfg, ex, tick)
	if err != nil {
		return "refused: " + err.Error()
	}
	var derivSeen map[string][]market.Deriv // swap only: what the recorder already holds
	var derivOff string                     // why the recorder stays off this run
	if cfg.Market == "swap" {
		var err error
		if derivSeen, err = market.ReadDerivCSVFile(derivPath(dir, level)); err != nil {
			fix := filepath.Base(derivPath(dir, level)) + " is unreadable (" + err.Error() + "); fix or move it, then run again"
			if engine.NeedsDerivatives(s) { // its decisions read this history: fail closed
				return "refused: " + fix
			}
			derivOff = "recorder off: " + fix + " to record again; the file is left as it is"
		}
	}
	prev, err := LoadState(dir, level)
	if err == nil && prev.Level != level {
		err = fmt.Errorf("it holds no %s run", level) // e.g. emptied by hand
	}
	if err != nil && !(errors.Is(err, os.ErrNotExist) && !hasHistory(dir, level)) {
		// fail closed: the halt and drawdown bases live in the state file
		return "refused: " + filepath.Base(statePath(dir, level)) + " is missing or unreadable while the journal has " +
			level + " history (" + err.Error() + "); restore it, or start over in a new working directory; " + keepStops
	}
	j, err := journal.Open(dir, cfg.Mode, cfg.Level(), cfg.ParamsHash())
	if err != nil {
		return err.Error()
	}
	defer j.Close()

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go func() { // a stop request cancels in-flight exchange calls too
		for ctx.Err() == nil && !stopRequested(dir) {
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
			}
		}
		cancel()
	}()

	en := engine.New(cfg, s, ex, env, j, tick.Now)
	en.Restore(prev.Targets, prev.Positions, prev.StaleStops, prev.Trades)
	en.RestoreNews(prev.NewsUntil)
	en.RestoreRetarget(prev.RetargetDue)
	var nw *newsWorker // crypto with news_guard required: checked after the entry
	if g, ok := en.News.(*news.Guard); ok && cfg.AssetClass == "crypto" && cfg.NewsGuard == "required" {
		nw = startNewsWorker(ctx, g)
	}
	r := cfg.Risk
	st := State{Running: true, Pid: os.Getpid(), Mode: cfg.Mode, Level: level, ParamsHash: cfg.ParamsHash(), Heartbeat: time.Now(),
		Equity: prev.Equity, Positions: prev.Positions, Targets: en.Targets(), StaleStops: en.StaleStops(), Trades: en.Trades(), NewsUntil: en.NewsBlocks(), RetargetDue: en.RetargetDue(), Breaker: prev.Breaker,
		Limits: Limits{r.MaxRiskPerTradePct, r.MaxDailyDrawdownPct, r.MaxWeeklyDrawdownPct, cfg.Gate.MinDemoDays}}
	if n, ok := ex.(interface{ FundingNote() string }); ok { // the paper simulator
		st.Funding = n.FundingNote()
	}
	if n, ok := ex.(interface{ SimNote() string }); ok {
		st.PaperSim = n.SimNote()
	}
	st.Sizing = en.SizingNote()
	st.Feed = cfg.Feed
	var mu sync.Mutex
	saveErr := func(update func(*State)) error {
		mu.Lock()
		defer mu.Unlock()
		update(&st)
		if fs, ok := ft.state(); ok {
			st.FeedState = &fs
		}
		st.Heartbeat = time.Now()
		return SaveState(dir, st)
	}
	save := func(update func(*State)) { _ = saveErr(update) }
	record := func(kind, reason string) {
		_ = j.Append(journal.Event{Time: tick.Now(), Kind: kind, Reason: reason})
		save(func(s *State) { s.LastEvent = kind + " " + reason })
	}
	en.OnStale = func() { save(func(s *State) { s.StaleStops = en.StaleStops() }) }
	en.OnNewsBlock = func() { // a failed write is journaled; it never holds the emergency exit back
		if err := saveErr(func(s *State) { s.NewsUntil = en.NewsBlocks() }); err != nil {
			_ = j.Append(journal.Event{Time: tick.Now(), Kind: Error, Reason: "news block not saved: " + err.Error()})
		}
	}
	onNews := func(r newsResult) {
		note := func(ev journal.Event) {
			ev.Time = tick.Now()
			_ = j.Append(ev)
			save(func(s *State) { s.LastEvent = ev.Kind + " " + ev.Reason })
		}
		handleNews(ctx, en.EmergencyExit, nw.guard.Threshold, r, note, record)
		saveAccount(ctx, en, ex, env, save)
	}
	en.OnTrade = func() error { // before each tranche's order, unlike save: a failed write refuses it
		mu.Lock()
		defer mu.Unlock()
		next := st // a refused tranche never reaches st, so no later save writes it
		next.Trades, next.RetargetDue, next.Heartbeat = en.Trades(), en.RetargetDue(), time.Now()
		if err := SaveState(dir, next); err != nil {
			return err
		}
		st = next
		return nil
	}
	if derivOff != "" {
		record(journal.Derivatives, derivOff)
	}
	if derivSeen != nil { // every level records: the venue keeps no open interest history, backtests read this file
		en.SeedDeriv(derivSeen)
		en.OnDeriv = func(sym string, d market.Deriv) {
			if err := appendDeriv(derivPath(dir, level), sym, d); err != nil {
				record(Error, "derivatives recorder: "+err.Error())
			}
		}
	}
	if ft != nil {
		symbols := slices.Clone(cfg.Symbols)
		for _, aux := range s.Inputs() {
			if !slices.Contains(symbols, aux) {
				symbols = append(symbols, aux)
			}
		}
		en.Buffer = &engine.CandleBuffer{} // empty: the first step reads the whole windows
		en.ClosedCandle = ft.Closed
		if err := ft.start(ctx, feed, env, symbols, record, en.Buffer.Reset); err != nil {
			return "refused: feed: websocket: " + err.Error()
		}
	}
	beatDone := make(chan struct{})
	go func() {
		defer close(beatDone)
		for {
			select {
			case <-time.After(beatEvery):
				save(func(*State) {})
			case <-ctx.Done():
				return
			}
		}
	}()

	record(Start, strings.TrimPrefix(st.PaperSim+"; "+st.Sizing, "; "))
	// a restart never resets a drawdown base or lifts a halt of this account;
	// until the breaker is restored, the saved one is kept as it was
	restored := false
	var reason string
	if bal, err := ex.Balance(ctx, env); err != nil {
		reason = "refused: balance unavailable: " + err.Error()
		if ctx.Err() != nil {
			reason = endedReason(dir)
		}
	} else {
		if note := en.Risk.RestoreFor(prev.Breaker, level, bal.Equity); note != "" {
			record(journal.Limit, note)
		}
		restored = true
		reason = run(ctx, en, ex, env, tick, dir, record, save, nw, onNews, ready)
	}
	cancel()
	<-beatDone

	if flattenRequested(dir) {
		fctx, fdone := context.WithTimeout(context.Background(), 30*time.Second)
		if err := en.Flatten(fctx); err != nil {
			reason += "; flatten failed, positions keep their exchange stops: " + err.Error()
		} else {
			reason += "; positions flattened"
		}
		fdone()
	} else if restored {
		// a stop the exchange dropped late (a cancel cut short by the stop
		// request may still land) is put back before the loop goes
		rctx, rdone := context.WithTimeout(context.Background(), 30*time.Second)
		if err := en.Reconcile(rctx); err != nil {
			reason += "; final reconcile failed: " + err.Error()
		}
		rdone()
	}
	_ = os.Remove(stopPath(dir)) // honoured; a stop sent before the loop started is honoured too
	fin, done := context.WithTimeout(context.Background(), finalSnapshotTimeout(cfg))
	defer done()
	ps, perr := ex.Positions(fin, env)
	_ = j.Append(journal.Event{Time: tick.Now(), Kind: Stop, Reason: reason})
	save(func(s *State) {
		s.Running, s.StopReason, s.LastEvent, s.Targets, s.StaleStops, s.Trades = false, reason, Stop+" "+reason, en.Targets(), en.StaleStops(), en.Trades()
		s.NewsUntil, s.RetargetDue = en.NewsBlocks(), en.RetargetDue()
		if restored {
			s.Breaker = snapshot(en)
		}
		if perr == nil {
			s.Positions = ps
		}
	})
	return reason
}

func run(ctx context.Context, en *engine.Engine, ex exchange.Exchange, env exchange.Env, tick Ticker, dir string,
	record func(kind, reason string), save func(func(*State)), nw *newsWorker, onNews func(newsResult), ready func()) string {
	ended := func() string { return endedReason(dir) }
	if err := en.ConfigureMarkets(ctx); err != nil { // before any order, reconcile included
		if ctx.Err() != nil {
			return ended()
		}
		return "refused: " + err.Error()
	}
	if err := en.Reconcile(ctx); err != nil {
		if ctx.Err() != nil {
			return ended()
		}
		return "reconcile failed: " + err.Error()
	}
	ready()
	var lastBeat time.Time
	for {
		if ctx.Err() != nil || stopRequested(dir) {
			return ended()
		}
		if now := tick.Now(); now.Sub(lastBeat) >= journalGap {
			record(Heartbeat, "")
			lastBeat = now
		}
		err := en.Step(ctx)
		switch {
		case errors.Is(err, risk.ErrHalted):
			record(journal.Limit, err.Error())
			return err.Error()
		case engine.IsHalt(err):
			en.Risk.Trip(err.Error()) // persisted: a restart today stays halted
			save(func(s *State) { s.Breaker = snapshot(en) })
			return guard(ctx, en, ex, env, tick, dir, record, save, err.Error())
		case err != nil && ctx.Err() == nil:
			record(Error, err.Error())
		}
		saveAccount(ctx, en, ex, env, save)
		nw.check(en.Holding(), tick.Now())
		if err := next(ctx, tick, nw.results(), onNews); errors.Is(err, io.EOF) {
			return "candles exhausted"
		}
	}
}

// saveAccount puts the account as the exchange reports it in state.json.
func saveAccount(ctx context.Context, en *engine.Engine, ex exchange.Exchange, env exchange.Env, save func(func(*State))) {
	if bal, err := ex.Balance(ctx, env); err == nil {
		ps, _ := ex.Positions(ctx, env)
		save(func(s *State) {
			s.Equity, s.Positions, s.Targets, s.StaleStops, s.Trades, s.Breaker = bal.Equity, ps, en.Targets(), en.StaleStops(), en.Trades(), snapshot(en)
			s.NewsUntil, s.RetargetDue = en.NewsBlocks(), en.RetargetDue()
		})
	}
}

// guard runs after an execution halt until the loop is stopped: it buys
// nothing, but every candle a fill that landed late (an unconfirmed entry)
// or a stop the exchange dropped still gets its stop. The halt is the last
// event and the account is saved every turn, so status shows both.
func guard(ctx context.Context, en *engine.Engine, ex exchange.Exchange, env exchange.Env, tick Ticker, dir string,
	record func(kind, reason string), save func(func(*State)), reason string) string {
	record(journal.Halt, reason)
	for {
		saveAccount(ctx, en, ex, env, save)
		if ctx.Err() != nil || stopRequested(dir) || tick.Next(ctx) != nil {
			return reason
		}
		if err := en.Reconcile(ctx); err != nil && ctx.Err() == nil {
			record(Error, err.Error())
		}
	}
}

// Status is the one-line answer of the status command.
type Status struct {
	Alive         bool                   `json:"alive"`
	Mode          string                 `json:"mode,omitempty"`
	Level         string                 `json:"level,omitempty"`
	Pid           int                    `json:"pid,omitempty"`
	Equity        float64                `json:"equity"`
	DayPnL        float64                `json:"day_pnl"`
	WeekPnL       float64                `json:"week_pnl"`
	Limits        Limits                 `json:"limits"`
	DailyRoomPct  float64                `json:"daily_room_pct"`
	WeeklyRoomPct float64                `json:"weekly_room_pct"`
	Positions     []exchange.Position    `json:"positions"`
	Targets       map[string]float64     `json:"targets,omitempty"` // symbol -> exit target of the engine's own position
	Trades        map[string]TradeStatus `json:"trades,omitempty"`  // symbol -> the trade in progress (graduated entry)
	Halt          string                 `json:"halt,omitempty"`    // the breaker's halt: no entry until it lifts
	Funding       string                 `json:"funding,omitempty"`
	PaperSim      string                 `json:"sim,omitempty"`
	Sizing        string                 `json:"sizing,omitempty"`
	Feed          string                 `json:"feed,omitempty"`
	FeedState     *FeedState             `json:"feed_state,omitempty"`
	LastEvent     string                 `json:"last_event,omitempty"`
	StopReason    string                 `json:"stop_reason,omitempty"`
}

// TradeStatus is a trade in progress as status shows it: the risk budget
// fixed at its first tranche, the fraction of it used, the tranches so far
// and the position's stop.
type TradeStatus struct {
	Budget     float64 `json:"budget"`
	BudgetUsed float64 `json:"budget_used"`
	Tranches   int     `json:"tranches"`
	Stop       float64 `json:"stop,omitempty"`
}

func ReadStatus(dir string) (Status, error) {
	if err := CheckRenamed(dir); err != nil {
		return Status{}, fmt.Errorf("status: %w", err)
	}
	refused := startRefusal(dir)
	st, err := lastState(dir)
	if errors.Is(err, os.ErrNotExist) {
		if refused != "" {
			return Status{StopReason: refused}, nil
		}
		return Status{StopReason: "never started"}, nil
	}
	if err != nil {
		return Status{}, fmt.Errorf("status: %w", err)
	}
	s := Status{Alive: st.Alive(), Mode: st.Mode, Level: st.Level, Pid: st.Pid, Equity: st.Equity, Limits: st.Limits,
		Positions: st.Positions, Targets: st.Targets, Halt: st.Breaker.Halt, Funding: st.Funding, PaperSim: st.PaperSim, Sizing: st.Sizing, Feed: st.Feed, FeedState: st.FeedState, LastEvent: st.LastEvent, StopReason: st.StopReason}
	for sym, t := range st.Trades {
		if s.Trades == nil {
			s.Trades = map[string]TradeStatus{}
		}
		s.Trades[sym] = TradeStatus{Budget: t.Budget, BudgetUsed: t.Used, Tranches: t.Tranches, Stop: t.Stop}
	}
	if b := st.Breaker; b.DayOpen > 0 {
		s.DayPnL = st.Equity - b.DayOpen
		s.DailyRoomPct = st.Limits.MaxDailyDrawdownPct + s.DayPnL/b.DayOpen*100
	}
	if b := st.Breaker; b.WeekOpen > 0 {
		s.WeekPnL = st.Equity - b.WeekOpen
		s.WeeklyRoomPct = st.Limits.MaxWeeklyDrawdownPct + s.WeekPnL/b.WeekOpen*100
	}
	if st.Running && !s.Alive {
		s.StopReason = "loop died without stopping (stale heartbeat); positions keep their exchange stops"
	}
	if refused != "" && !s.Alive { // the last start, newer than what the state says of the last run
		s.StopReason = refused
	}
	return s, nil
}

func refusalPath(dir string) string { return filepath.Join(dir, "start-refused") }

// SaveRefusal keeps why the last start did not start the loop, for status:
// a refused start may leave no state file, or one of an older run.
func SaveRefusal(dir, reason string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(refusalPath(dir), []byte(reason), 0o600)
}

// ClearRefusal forgets it: the loop came up.
func ClearRefusal(dir string) error {
	if err := os.Remove(refusalPath(dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func startRefusal(dir string) string {
	b, err := os.ReadFile(refusalPath(dir))
	if err != nil || len(b) == 0 {
		return ""
	}
	return "start refused: " + strings.TrimPrefix(string(b), "refused: ")
}

// Locked reports whether a loop holds the state directory's lock.
func Locked(dir string) bool {
	unlock, err := Lock(dir)
	if err != nil {
		return true
	}
	unlock()
	return false
}

func snapshot(en *engine.Engine) risk.Snapshot {
	s := en.Risk.Snapshot()
	s.Mode = en.Cfg.Level()
	return s
}

// hasHistory reports a journal with an event of mode; an unreadable journal
// counts as history (fail closed).
func hasHistory(dir, level string) bool {
	evs, _, err := journal.Read(filepath.Join(dir, "journal.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	return err != nil || slices.ContainsFunc(evs, func(e journal.Event) bool { return eventLevel(e) == level })
}

// eventLevel is the level of e; an event from before the split per level
// counts for paper (mode paper, which testnet shared) or mainnet (mode live).
func eventLevel(e journal.Event) string {
	if e.Level != "" {
		return e.Level
	}
	return map[string]string{"paper": "paper", "live": "mainnet"}[e.Mode]
}

func endedReason(dir string) string {
	switch {
	case flattenRequested(dir):
		return "stopped by stop --flatten"
	case stopRequested(dir):
		return "stopped by stop command"
	}
	return "interrupted"
}

// finalSnapshotTimeout bounds the last Positions read at shutdown: long
// enough for the simulator to fill a stop hit at that instant after the
// profile's delay, plus a margin for the read itself.
func finalSnapshotTimeout(cfg *config.Config) time.Duration {
	d := 500 * time.Millisecond
	for _, c := range cfg.SimCosts() {
		d = max(d, c.Latency+500*time.Millisecond)
	}
	return d
}

// appendDeriv appends one snapshot to the recorder's file, with the header
// first when the file is new, in one write.
func appendDeriv(path, sym string, d market.Deriv) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	row := market.DerivCSVRow(sym, d)
	if st, err := f.Stat(); err == nil && st.Size() == 0 {
		row = market.DerivCSVHeader + "\n" + row
	}
	_, err = f.WriteString(row)
	return errors.Join(err, f.Close())
}
