// Package engine runs one decision step per closed candle — exits, then the
// strategy's decision, sizing, risk and order — the same code for backtest and
// mainnet.
package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/paper"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exec"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/news"
	"github.com/rosaldo/agency-agents/quant-engine/internal/news/typesafe"
	"github.com/rosaldo/agency-agents/quant-engine/internal/ownbook"
	"github.com/rosaldo/agency-agents/quant-engine/internal/risk"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

type Engine struct {
	Cfg      *config.Config
	Strategy strategy.Strategy
	Ex       exchange.Exchange
	Env      exchange.Env
	Exec     *exec.Executor
	Risk     *risk.Breaker
	// News is the check used when news_guard is required: before an entry on
	// an equity, after it on a crypto (the loop's worker, which needs a
	// *news.Guard). nil means no checker is configured, so such entries are
	// refused.
	News        NewsChecker
	NewsTimeout time.Duration // 0 = 10s
	// PastNews says the candles are of the past (backtest): headlines are not
	// replayable, so a crypto entry is taken and never exited on news, and an
	// equity entry is never taken.
	PastNews bool
	// Simulated says the fills are simulated (paper simulator, backtest):
	// sizing then counts the active sim profile as it is; with real fills
	// (mainnet, testnet) it counts the profile floored at the real one (FR-R1).
	Simulated bool
	// OnStale, when set, persists the stale stops at once: a crash right
	// after a move must not lose the one left behind.
	OnStale func()
	// OnTrade, when set, persists the trade budgets and fractions used before
	// each tranche's order: a kill -9 between the fill and the end-of-step save
	// must not renew the budget. An error refuses the order.
	OnTrade func() error
	// OnDeriv, when set, gets each new derivatives snapshot of a swap symbol
	// (the loop's recorder); snapshots are then read for every strategy.
	OnDeriv func(symbol string, d market.Deriv)
	// DerivReplay says the snapshots were seeded from a recording (backtest):
	// none is read, since a read's deadline is wall-clock time.
	DerivReplay bool
	// ClosedCandle, when set (feed: websocket), is the open time of the candle
	// the stream said ended, which this step decides on: a symbol whose REST
	// candle is another one takes no decision (journal.CandleNotFinal). Zero
	// = no expectation. Buffer, when set, keeps the windows between steps.
	ClosedCandle func() time.Time
	Buffer       *CandleBuffer

	deriv      map[string][]market.Deriv // snapshots read, oldest first; the last one always kept
	derivDown  map[string]bool           // symbol -> snapshot reads failing (one journal note per streak)
	closed     map[string]time.Time      // symbol -> open time of the candle this step decided on
	targets    map[string]float64
	stops      map[string]float64    // stop given at entry or moved by the trail, re-placed if the exchange drops it
	staleStops map[string]string     // symbol -> old stop a trail could not cancel (FR-P9)
	trades     map[string]risk.Trade // symbol -> budget fixed at the first tranche and fraction used (graduated entry)
	blocked    map[string]string     // last journaled block reason per symbol
	newsUntil  map[string]time.Time  // symbol -> end of the block a news catastrophe set
	funding    map[string]time.Time
}

// New wires the executor and the breaker from the config; cfg must have been
// parsed for s. j may be nil.
func New(cfg *config.Config, s strategy.Strategy, ex exchange.Exchange, env exchange.Env, j exec.Recorder, now func() time.Time) *Engine {
	br := &risk.Breaker{DailyPct: cfg.Risk.MaxDailyDrawdownPct, WeeklyPct: cfg.Risk.MaxWeeklyDrawdownPct, Loc: cfg.Location()}
	e := &Engine{Cfg: cfg, Strategy: s, Ex: ex, Env: env, Risk: br, targets: map[string]float64{}, stops: map[string]float64{}, trades: map[string]risk.Trade{},
		staleStops: map[string]string{}, blocked: map[string]string{}, funding: map[string]time.Time{}, deriv: map[string][]market.Deriv{}, derivDown: map[string]bool{}, closed: map[string]time.Time{}, newsUntil: map[string]time.Time{}, Exec: &exec.Executor{Name: cfg.StrategyName(), Ex: ex, Env: env, Now: now, Risk: br, Journal: j}}
	_, e.Simulated = ex.(*paper.Sim) // by type: no real adapter can be taken for the simulator and lose the floor
	if n := cfg.NewsPolicy(); n != nil && len(n.Feeds) > 0 {
		var kw []string
		for _, k := range n.Keywords {
			kw = append(kw, strings.ToLower(k))
		}
		lookback := n.Lookback
		if lookback <= 0 {
			lookback = 24 * time.Hour
		}
		g := &news.Guard{RSS: &news.RSS{Feeds: n.Feeds, Keywords: kw, Aliases: n.Aliases,
			Lookback: lookback, Client: &http.Client{Timeout: 10 * time.Second}}}
		if c := n.Classifier; c != nil {
			v := config.NewsKeyVar(cfg.StrategyName())
			g.Classifier = &typesafe.Client{URL: typesafe.Endpoint, Key: config.Secret(os.Getenv(v)), KeyVar: v,
				HTTP: &http.Client{Timeout: 10 * time.Second}}
			g.Question = news.Question{Type: c.Type, Instructions: c.Question, Criteria: c.Criteria, Model: c.Model}
			g.Threshold = c.Threshold
		}
		e.News = g
	}
	return e
}

// NewsChecker says whether recent news leaves an entry on symbol clear. It is
// a port: no news source is built in.
type NewsChecker interface {
	Clear(ctx context.Context, symbol string, at time.Time) (clear bool, reason string, err error)
}

// newsBlocks applies news_guard fail-closed: no checker, an error, a timeout
// or a not-clear verdict all keep the engine out. A crypto is checked after
// its entry instead (the loop's worker), and only needs the checker.
func (e *Engine) newsBlocks(ctx context.Context, symbol string, at time.Time) string {
	crypto := e.Cfg.AssetClass == "crypto"
	switch {
	case e.Cfg.NewsGuard != "required", crypto && e.PastNews:
		return ""
	case e.News == nil:
		return "news_guard is required and no news checker is configured"
	case crypto:
		return ""
	}
	timeout := e.NewsTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ok, reason, err := e.News.Clear(cctx, symbol, at)
	switch {
	case err != nil:
		return "news check failed: " + err.Error()
	case !ok:
		return "news check not clear: " + reason
	}
	return ""
}

// IsHalt reports an error that must end the run.
func IsHalt(err error) bool { return errors.Is(err, exec.ErrHalt) || errors.Is(err, risk.ErrHalted) }

// Target is the exit target of an open position, if this run opened it.
func (e *Engine) Target(symbol string) (float64, bool) {
	t, ok := e.targets[symbol]
	return t, ok
}

// Targets returns a copy of the open positions' targets.
func (e *Engine) Targets() map[string]float64 {
	out := make(map[string]float64, len(e.targets))
	for k, v := range e.targets {
		out[k] = v
	}
	return out
}

// Restore reloads the targets, the positions' stops, the stale stops and
// the trade budgets saved by a previous run; nil trades (a state from before
// graduated entries) means no tranche in progress. A trade's stop tighter
// than the saved position's is the one kept: a kill after an add's fill
// left the position saved as before it, and the tranche was sized against
// the add's stop.
func (e *Engine) Restore(targets map[string]float64, positions []exchange.Position, stale map[string]string, trades map[string]risk.Trade) {
	for k, v := range targets {
		e.targets[k] = v
	}
	for k, v := range stale {
		e.staleStops[k] = v
	}
	short := map[string]bool{}
	for _, p := range positions {
		if p.StopPrice > 0 {
			e.stops[p.Symbol] = p.StopPrice
		}
		short[p.Symbol] = p.PSide() == exchange.Short
	}
	for k, t := range trades {
		e.trades[k] = t
		if cur, ok := e.stops[k]; t.Stop > 0 && (!ok || !short[k] && t.Stop > cur || short[k] && t.Stop < cur) {
			e.stops[k] = t.Stop
		}
	}
}

// Trades returns a copy of the open trades' budgets and used fractions.
func (e *Engine) Trades() map[string]risk.Trade {
	out := make(map[string]risk.Trade, len(e.trades))
	for k, v := range e.trades {
		out[k] = v
	}
	return out
}

// forget drops what the engine keeps about a position that is gone.
func (e *Engine) forget(sym string) {
	delete(e.targets, sym)
	delete(e.stops, sym)
	delete(e.trades, sym)
}

// ConfigureMarkets applies perp.margin_mode and perp.leverage to every
// traded swap market before any order and checks what the exchange reports
// as applied: any divergence refuses to trade (FR-P3). Spot has nothing to set.
func (e *Engine) ConfigureMarkets(ctx context.Context) error {
	if e.Cfg.Market != "swap" {
		return nil
	}
	want := exchange.MarginSettings{Leverage: e.Cfg.Perp.Leverage, MarginMode: e.Cfg.Perp.MarginMode}
	for _, sym := range e.Cfg.Symbols {
		got, err := e.Ex.ConfigureMarket(ctx, e.Env, sym, want)
		if err != nil {
			return fmt.Errorf("configure %s: %w", sym, err)
		}
		if got != want {
			return fmt.Errorf("configure %s: the exchange applied leverage %d %s, the yml asks %d %s",
				sym, got.Leverage, got.MarginMode, want.Leverage, want.MarginMode)
		}
	}
	return nil
}

// Flatten closes every position at market, cancelling its stop first.
func (e *Engine) Flatten(ctx context.Context) error {
	ps, err := e.Ex.Positions(ctx, e.Env)
	if err != nil {
		return fmt.Errorf("flatten: %w", err)
	}
	var errs []error
	for _, p := range ps {
		if p.Qty <= 0 { // a blocked symbol with nothing of the engine's
			continue
		}
		if err := e.Exec.Exit(ctx, p, e.Exec.Now(), "flatten"); err != nil {
			errs = append(errs, err)
			continue
		}
		e.forget(p.Symbol)
	}
	return errors.Join(errs...)
}

// Reconcile protects positions left without a stop, before any decision: the
// stop goes back at the price the strategy gave at entry. A position whose
// stop the engine does not know gets stop 0, which the executor flattens.
// The stale stops of earlier moves are retried first, here, so the guard
// after a halt and the start retry them too.
func (e *Engine) Reconcile(ctx context.Context) error {
	e.retryStale(ctx)
	return e.Exec.Reconcile(ctx, func(p exchange.Position) float64 { return e.stops[p.Symbol] })
}

// Step evaluates the last closed candle of every symbol.
func (e *Engine) Step(ctx context.Context) error {
	bal, err := e.Ex.Balance(ctx, e.Env)
	if err != nil {
		return fmt.Errorf("step: balance: %w", err)
	}
	if err := e.Risk.Allow(e.Exec.Now(), bal.Equity); err != nil {
		return err
	}
	// every step, not only at start: a late fill or a stop the exchange
	// dropped gets protected before anything else
	if err := e.Reconcile(ctx); err != nil {
		return err
	}
	ps, err := e.Ex.Positions(ctx, e.Env)
	if err != nil {
		return fmt.Errorf("step: positions: %w", err)
	}
	held := map[string]exchange.Position{}
	for _, p := range ps {
		if err := e.guardLiquidation(ctx, p); err != nil { // FR-P4 every step: liquidation moves with margin and funding
			return err
		}
		held[p.Symbol] = p
	}
	e.journalBlocks(held)
	e.journalFunding(ctx, held)
	var errs []error
	free := bal.Cash // free margin left for this step's entries
	for _, sym := range e.Cfg.Symbols {
		err := e.stepSymbol(ctx, sym, held, bal.Equity, &free)
		if IsHalt(err) {
			return err
		}
		errs = append(errs, err)
	}
	if !NeedsDerivatives(e.Strategy) { // the recorder reads after every symbol's trading actions: a slow endpoint delays none
		for _, sym := range e.Cfg.Symbols {
			if c, ok := e.closed[sym]; ok {
				e.readDeriv(ctx, sym, c)
			}
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) stepSymbol(ctx context.Context, sym string, held map[string]exchange.Position, equity float64, free *float64) error {
	delete(e.closed, sym)
	n := max(e.Strategy.Warmup(), 1)
	var want time.Time
	if e.ClosedCandle != nil {
		want = e.ClosedCandle()
	}
	cs, err := e.candles(ctx, sym, n, want)
	if err != nil || len(cs) == 0 {
		return fmt.Errorf("step: candles %s: %v", sym, err)
	}
	last := cs[len(cs)-1]
	if e.notFinal(sym, sym, last, want) {
		return nil
	}
	e.closed[sym] = last.Time
	if NeedsDerivatives(e.Strategy) {
		e.readDeriv(ctx, sym, last.Time) // after the close, before the decision: the snapshot of this candle
	}
	e.dropDeriv(sym, cs[0].Time)
	p := held[sym]
	holding := p.Qty > 0 // a blocked symbol may come back with nothing held
	if !holding {
		delete(e.trades, sym) // no trade in progress while flat
	}
	if _, known := e.stops[sym]; known && !holding {
		e.note(journal.ClosedOutside, sym, "the engine's position is gone without an exit of this step "+
			"(its stop, a liquidation or a close outside the engine); target cleared")
		e.forget(sym)
	}
	side := strategy.Long
	if p.PSide() == exchange.Short {
		side = strategy.Short
	}
	if until, ok := e.newsUntil[sym]; ok && !e.Exec.Now().Before(until) {
		delete(e.newsUntil, sym)
	}
	if _, news := e.newsUntil[sym]; holding && news { // an emergency exit that failed: again, before anything else
		return e.exit(ctx, p, last.Time, "emergency exit (news catastrophe), retried")
	}
	if holding && TargetHit(side, last.Close, e.targets[sym]) {
		return e.exit(ctx, p, last.Time, "target") // the exchange stop guards it if this fails
	}
	in := strategy.Input{Symbol: sym, Now: last.Time, Candles: cs}
	if holding {
		in.Position = &strategy.PositionView{Side: side, Qty: p.Qty, AvgPrice: p.AvgPrice, Stop: p.StopPrice, Target: e.targets[sym]}
	}
	if NeedsDerivatives(e.Strategy) {
		in.Deriv = market.AlignDeriv(cs, e.deriv[sym], e.Cfg.Interval())
	}
	for _, aux := range e.Strategy.Inputs() {
		if in.Aux == nil {
			in.Aux = map[string][]market.Candle{}
		}
		if in.Aux[aux], err = e.candles(ctx, aux, n, want); err != nil {
			return fmt.Errorf("step: candles %s: %w", aux, err)
		}
		if a := in.Aux[aux]; len(a) > 0 && e.notFinal(sym, aux, a[len(a)-1], want) {
			return nil
		}
	}
	d := e.Strategy.Decide(in)
	switch {
	case d.Action == strategy.Exit && holding:
		return e.exit(ctx, p, last.Time, "exit: "+d.Reason)
	case d.Action == strategy.Add:
		return e.add(ctx, sym, p, d, last, equity, free)
	case d.Action == strategy.None && holding:
		return e.trail(ctx, p, in, last.Close, last.Time)
	case d.Action == strategy.None && d.Reason != "":
		e.skip(sym, d.Reason) // a signal the strategy dropped itself; no Reason = no signal, nothing journaled
		return nil
	case d.Action != strategy.Enter:
		return nil
	case holding:
		e.skip(sym, "a position is already open (one per symbol; reverse through flat)")
		return nil
	}
	if err := e.validateEntry(d, last.Close); err != nil {
		e.skip(sym, err.Error())
		return nil
	}
	if d.Side == strategy.Short && !e.Ex.Capabilities().Swap {
		e.skip(sym, "short entries need a swap market; this adapter trades spot")
		return nil
	}
	if p.Blocked != "" {
		e.skip(sym, "symbol blocked: "+p.Blocked)
		return nil
	}
	if until, ok := e.newsUntil[sym]; ok {
		e.skip(sym, "symbol blocked by a news catastrophe until "+until.UTC().Format(time.RFC3339))
		return nil
	}
	if id := e.staleStops[sym]; id != "" {
		e.skip(sym, "symbol blocked: old stop "+id+" not cancelled yet")
		return nil
	}
	if err := e.entryClearsLiquidation(sym, exchange.PositionSide(d.Side), last.Close, d.Stop); err != nil {
		e.skip(sym, err.Error())
		return nil
	}
	if why := e.newsBlocks(ctx, sym, last.Time); why != "" {
		e.skip(sym, why)
		return nil
	}
	trade, qty, why := risk.Trade{}.Tranche(d.Fraction, e.sizeInput(sym, equity, last.Close, d.Stop, d.Side == strategy.Short))
	if qty == 0 {
		e.skip(sym, why)
		return nil
	}
	margin := qty * last.Close / float64(e.Cfg.Perp.Leverage)
	if margin > *free {
		e.skip(sym, fmt.Sprintf("margin %.2f for qty %.8g at leverage %d is above the free margin %.2f", margin, qty, e.Cfg.Perp.Leverage, *free))
		return nil
	}
	*free -= margin // also on a failed entry: it may have filled
	trade.Stop = d.Stop
	e.trades[sym] = trade // before the order: a fill of unknown fate keeps the budget used
	if why := e.persistTrades(); why != "" {
		e.skip(sym, why)
		return nil
	}
	if err := e.Exec.Enter(ctx, sym, last.Time, exec.Plan{Side: exchange.PositionSide(d.Side), Entry: last.Close, Stop: d.Stop, Target: d.Target, Qty: qty}); err != nil {
		return err
	}
	e.stops[sym] = d.Stop
	if d.Target > 0 {
		e.targets[sym] = d.Target
	}
	return e.guardFilled(ctx, sym)
}

// persistTrades saves the trades through OnTrade; a failure is the reason
// the order is not sent (the fraction stays counted: conservative).
func (e *Engine) persistTrades() string {
	if e.OnTrade == nil {
		return ""
	}
	if err := e.OnTrade(); err != nil {
		return "trade budget not persisted, order not sent: " + err.Error()
	}
	return ""
}

// sizeInput is what sizes a tranche of sym entered at entry with stop.
func (e *Engine) sizeInput(sym string, equity, entry, stop float64, short bool) risk.SizeInput {
	caps := e.Ex.Capabilities()
	step, minQty := caps.QtyStep, caps.MinQty
	if l, ok := e.Ex.(interface {
		Lot(string) (float64, float64)
	}); ok {
		step, minQty = l.Lot(sym) // per market when the adapter knows it
	}
	c := e.riskCosts(sym)
	return risk.SizeInput{Equity: equity, RiskPct: e.Cfg.Risk.MaxRiskPerTradePct, Entry: entry, Stop: stop, Short: short,
		SlippagePct: c.SlippagePct, CommissionPct: c.CommissionPct, QtyStep: step, MinQty: minQty}
}

// add is a further tranche of the position p (graduated entry):
// sized from the budget fixed at the trade's first tranche, never the
// current equity, against the position's stop (or a tighter one the Add
// brings); free margin, the leverage ceiling and the stop clear of the
// liquidation are checked on the AGGREGATE position, the drawdown breaker
// by the executor. It is decided on the same closed candle as an Enter.
// Once it fills, the executor swaps the position's stop for one covering the
// whole quantity, and the stop is checked against the exchange's liquidation
// at the new average price.
func (e *Engine) add(ctx context.Context, sym string, p exchange.Position, d strategy.Decision, last market.Candle, equity float64, free *float64) error {
	side, short := strategy.Long, p.PSide() == exchange.Short
	if short {
		side = strategy.Short
	}
	trade, known := e.trades[sym]
	cur := p.StopPrice
	if cur <= 0 {
		cur = e.stops[sym]
	}
	stop := cur
	if d.Stop > 0 {
		stop = d.Stop
	}
	var why string
	switch {
	case p.Qty <= 0:
		why = "add refused: no own open position"
	case d.Side != side:
		why = fmt.Sprintf("add refused: side %q on a %s position", d.Side, side)
	case !known:
		why = "add refused: no trade budget recorded for this position (entered before graduated entries); a new budget is never fixed on an open position"
	case cur <= 0:
		why = "add refused: the position's stop is unknown"
	case !short && stop < cur, short && stop > cur:
		why = fmt.Sprintf("add refused: stop %.8g is looser than the position's %.8g", stop, cur)
	case p.Blocked != "":
		why = "symbol blocked: " + p.Blocked
	case e.staleStops[sym] != "":
		why = "symbol blocked: old stop " + e.staleStops[sym] + " not cancelled yet"
	case !e.newsUntil[sym].IsZero():
		why = "symbol blocked by a news catastrophe until " + e.newsUntil[sym].UTC().Format(time.RFC3339)
	}
	if why == "" {
		why = e.newsBlocks(ctx, sym, last.Time)
	}
	if why != "" {
		e.skip(sym, why)
		return nil
	}
	next, qty, why := trade.Tranche(d.Fraction, e.sizeInput(sym, equity, last.Close, stop, short))
	if qty == 0 {
		e.skip(sym, "add refused: "+why)
		return nil
	}
	why = ""
	lev := float64(e.Cfg.Perp.Leverage)
	total := p.Qty + qty
	agg := p
	agg.Qty, agg.AvgPrice, agg.StopPrice = total, (p.AvgPrice*p.Qty+last.Close*qty)/total, stop
	if p.Leverage > 0 {
		lev = p.Leverage
	}
	agg.LiquidationPrice = risk.LiquidationPrice(short, agg.AvgPrice, e.liquidationDistance(sym, p.PSide(), agg.AvgPrice, lev))
	margin := qty * last.Close / float64(e.Cfg.Perp.Leverage)
	switch {
	case margin > *free:
		why = fmt.Sprintf("margin %.2f for qty %.8g at leverage %d is above the free margin %.2f", margin, qty, e.Cfg.Perp.Leverage, *free)
	case total*last.Close > equity*float64(e.Cfg.Perp.Leverage):
		why = fmt.Sprintf("the position's notional %.2f after the add is above equity %.2f x leverage %d", total*last.Close, equity, e.Cfg.Perp.Leverage)
	default:
		if err := e.liquidationViolation(agg); err != nil {
			why = "stop vs liquidation of the position after the add: " + err.Error()
		}
	}
	if why != "" {
		e.skip(sym, "add refused: "+why)
		return nil
	}
	*free -= margin
	next.Stop = stop
	e.trades[sym] = next // before the order: a fill of unknown fate keeps the fraction used
	if why := e.persistTrades(); why != "" {
		e.skip(sym, "add refused: "+why)
		return nil
	}
	stale, err := e.Exec.Add(ctx, sym, last.Time, p, exec.Plan{Side: p.PSide(), Entry: last.Close, Stop: stop, Target: d.Target, Qty: qty})
	e.keepStale(sym, stale)
	switch {
	case errors.Is(err, exec.ErrStopFilled):
		e.forget(sym)
		return nil
	case err != nil:
		e.keepRestingStop(ctx, sym)
		return err // Reconcile covers the whole quantity next step, or flattened and halted already
	}
	e.stops[sym] = stop
	if d.Target > 0 {
		e.targets[sym] = d.Target
	}
	if stale != "" && e.Cfg.Market == "spot" { // the exit sells the whole quantity, the add's included
		ps, err := e.Ex.Positions(ctx, e.Env)
		if err != nil {
			return fmt.Errorf("after add %s: positions: %w", sym, err)
		}
		for _, q := range ps {
			if q.Symbol == sym {
				return e.exitIfStaleOnSpot(ctx, q, stale, stop, last.Time, "add-stop", "add: old stop not cancelled on spot")
			}
		}
	}
	return e.guardFilled(ctx, sym)
}

// candles reads sym's last n closed candles, through the buffer when there
// is one and the step knows which candle it expects.
func (e *Engine) candles(ctx context.Context, sym string, n int, want time.Time) ([]market.Candle, error) {
	if e.Buffer != nil && !want.IsZero() {
		return e.Buffer.read(ctx, e.Ex, e.Env, sym, e.Cfg.Interval(), n, want)
	}
	return e.Ex.Candles(ctx, e.Env, sym, e.Cfg.Interval(), n)
}

// notFinal journals and reports a REST candle of src that is not the one
// the stream said ended (want): sym then takes no decision this step. Its
// stop still rests on the exchange, and Reconcile ran already.
func (e *Engine) notFinal(sym, src string, last market.Candle, want time.Time) bool {
	if want.IsZero() || last.Time.Equal(want) {
		return false
	}
	e.note(journal.CandleNotFinal, sym, fmt.Sprintf("the last closed candle of %s over REST opens at %s, the stream ended the one at %s; no decision this step",
		src, last.Time.UTC().Format(time.RFC3339), want.UTC().Format(time.RFC3339)))
	return true
}

// keepRestingStop takes sym's stop price from the exchange after a failed
// add: the swap's own Reconcile may already have covered the whole quantity
// at the add's stop, and the engine must not fall back to the older one. An
// unreadable position leaves the engine's stop as it was.
func (e *Engine) keepRestingStop(ctx context.Context, sym string) {
	ps, err := e.Ex.Positions(ctx, e.Env)
	if err != nil {
		return
	}
	for _, p := range ps {
		if p.Symbol == sym && p.StopClientID != "" && p.StopPrice > 0 {
			e.stops[sym] = p.StopPrice
		}
	}
}

// EmergencyExit is a news catastrophe on sym: no entry on it until until,
// and its position, if the engine holds one, closed at market now (stop
// cancelled first, reduce-only: Exec.Exit). A failed exit is retried at the
// next step, before any decision; the stop on the exchange guards meanwhile.
func (e *Engine) EmergencyExit(ctx context.Context, sym string, until time.Time, reason string) error {
	if until.After(e.newsUntil[sym]) {
		e.newsUntil[sym] = until
	}
	e.note(journal.EmergencyExit, sym, reason+"; no entry until "+e.newsUntil[sym].UTC().Format(time.RFC3339))
	ps, err := e.Ex.Positions(ctx, e.Env)
	if err != nil {
		return fmt.Errorf("emergency exit: positions: %w", err)
	}
	for _, p := range ps {
		if p.Symbol == sym && p.Qty > 0 {
			return e.exit(ctx, p, e.Exec.Now(), "emergency exit: "+reason)
		}
	}
	return nil
}

// Holding lists the symbols the engine holds a position of its own on.
func (e *Engine) Holding() []string {
	var out []string
	for _, sym := range e.Cfg.Symbols {
		if _, ok := e.stops[sym]; ok {
			out = append(out, sym)
		}
	}
	return out
}

// exit closes p at market; on failure its target and stop are kept for the
// next step.
func (e *Engine) exit(ctx context.Context, p exchange.Position, candle time.Time, reason string) error {
	if err := e.Exec.Exit(ctx, p, candle, reason); err != nil {
		return err
	}
	e.forget(p.Symbol)
	return nil
}

// validateEntry enforces the engine's rules on an Enter: a protective
// stop on the losing side of the entry, and a side the config allows.
func (e *Engine) validateEntry(d strategy.Decision, entry float64) error {
	switch {
	case d.Side != strategy.Long && d.Side != strategy.Short:
		return fmt.Errorf("unknown side %q", d.Side)
	case d.Side == strategy.Short && !e.Cfg.Perp.AllowShort:
		return errors.New("short refused: perp.allow_short is false")
	case d.Stop <= 0:
		return errors.New("no stop: an entry needs a protective stop")
	case d.Side == strategy.Long && d.Stop >= entry, d.Side == strategy.Short && d.Stop <= entry:
		return fmt.Errorf("stop %.8g on the wrong side of the %s entry %.8g", d.Stop, d.Side, entry)
	}
	return nil
}

// TargetHit reports a closed candle reaching the exit target: a long's from
// below, a short's from above. Target 0 means none.
func TargetHit(side strategy.Side, close, target float64) bool {
	if target <= 0 {
		return false
	}
	if side == strategy.Short {
		return close <= target
	}
	return close >= target
}

// journalBlocks journals each change of a symbol's block (FR-P8, R1b): the
// account holding foreign quantity, an own order of unknown fate, a position
// on the other side; and its lifting.
func (e *Engine) journalBlocks(held map[string]exchange.Position) {
	for _, sym := range e.Cfg.Symbols {
		why := held[sym].Blocked
		switch was := e.blocked[sym]; {
		case why == was:
			continue
		case why == "":
			e.note(journal.Unblocked, sym, "was: "+was)
		case strings.HasPrefix(why, ownbook.ForeignQty):
			e.note(journal.ForeignQty, sym, why)
		default:
			e.note(journal.Blocked, sym, why)
		}
		e.blocked[sym] = why
	}
}

// journalFunding journals the funding the venue reports on the held swap
// positions since the last look (FR-P5); none before the engine first saw
// the position. A failed read is tried again next step.
func (e *Engine) journalFunding(ctx context.Context, held map[string]exchange.Position) {
	fr, ok := e.Ex.(exchange.FundingReporter)
	if !ok || e.Cfg.Market != "swap" {
		return
	}
	now := e.Exec.Now()
	for sym, p := range held {
		since, seen := e.funding[sym]
		if p.Qty <= 0 || !seen {
			if p.Qty > 0 {
				e.funding[sym] = now
			}
			continue
		}
		pays, err := fr.Funding(ctx, e.Env, sym, since)
		if err != nil {
			continue
		}
		for _, f := range pays {
			if f.Time.After(since) {
				_ = e.appendEvent(journal.Event{Time: f.Time, Kind: journal.Funding, Symbol: sym, Amount: f.Amount, Reason: "funding as the venue reports it"})
				e.funding[sym] = f.Time
			}
		}
	}
	for sym := range e.funding {
		if held[sym].Qty <= 0 {
			delete(e.funding, sym)
		}
	}
}

func (e *Engine) note(kind, sym, reason string) {
	_ = e.appendEvent(journal.Event{Time: e.Exec.Now(), Kind: kind, Symbol: sym, Reason: reason})
}

func (e *Engine) appendEvent(ev journal.Event) error {
	if e.Exec.Journal == nil {
		return nil
	}
	return e.Exec.Journal.Append(ev)
}

// skip journals a signal that did not become an order.
func (e *Engine) skip(sym, why string) {
	if e.Exec.Journal != nil {
		_ = e.Exec.Journal.Append(journal.Event{Time: e.Exec.Now(), Kind: journal.Rejected, Symbol: sym, Reason: "signal skipped: " + why})
	}
}
