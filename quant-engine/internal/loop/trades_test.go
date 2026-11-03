package loop

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/risk"
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// A state file written before graduated entries has no "trades": it loads
// as no tranche in progress, everything else intact; a new one carries the
// budget and the fraction used across a restart.
func TestStateWithoutTradesLoadsAsNoTrancheInProgress(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"running":false,"pid":0,"mode":"paper","level":"paper","params_hash":"abc","heartbeat":"2026-10-30T10:00:00Z",` +
		`"equity":10000,"breaker":{"day":"2026-10-30T00:00:00Z","week":"2026-10-27T00:00:00Z","day_open":10000,"week_open":10000,"last_equity":10000,"mode":"paper"},` +
		`"limits":{"max_risk_per_trade_pct":1,"max_daily_drawdown_pct":3,"max_weekly_drawdown_pct":6,"min_demo_days":0},` +
		`"positions":[{"Symbol":"AAA","Side":"long","Qty":10,"AvgPrice":105,"StopClientID":"s1","StopPrice":100}],"targets":{"AAA":110},"last_event":"","stop_reason":"stopped"}`
	if err := os.WriteFile(statePath(dir, "paper"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(dir, "paper")
	if err != nil {
		t.Fatal(err)
	}
	if st.Trades != nil || st.Targets["AAA"] != 110 || len(st.Positions) != 1 || st.Equity != 10000 {
		t.Fatalf("legacy state %+v, want no trades and the rest intact", st)
	}
	st.Trades = map[string]risk.Trade{"AAA": {Budget: 100, Used: 0.5}}
	if err := SaveState(dir, st); err != nil {
		t.Fatal(err)
	}
	back, err := LoadState(dir, "paper")
	if err != nil || back.Trades["AAA"] != (risk.Trade{Budget: 100, Used: 0.5}) {
		t.Fatalf("round trip %+v (%v), want budget 100 used 0.5", back.Trades, err)
	}
}

// status shows each trade in progress as saved: budget, budget_used,
// tranches and stop.
func TestStatusShowsTheSavedTrades(t *testing.T) {
	dir := t.TempDir()
	st := State{Mode: "paper", Level: "paper", Trades: map[string]risk.Trade{"AAA": {Budget: 100, Used: 0.75, Tranches: 2, Stop: 98}}}
	if err := SaveState(dir, st); err != nil {
		t.Fatal(err)
	}
	s, err := ReadStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	want := `"trades":{"AAA":{"budget":100,"budget_used":0.75,"tranches":2,"stop":98}}`
	if !strings.Contains(string(b), want) {
		t.Fatalf("status %s, want %s", b, want)
	}
}

const trancheYAML = "exchange: fake\nasset_class: crypto\ninterval: 1h\nsymbols: [AAA]\nrisk: {confirmed_by_user: true}\n"

// trancheFake is a fake on closes 100 100 100 105 103 102 102 102; QtyStep
// 0.001, cash 10 000, so the trade budget (1%) is 100.
func trancheFake() (*fake.Fake, []market.Candle) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for k, x := range []float64{100, 100, 100, 105, 103, 102, 102, 102} {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(k) * time.Hour), Open: x, High: x, Low: x, Close: x, Volume: 10})
	}
	return fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 0.001, MinQty: 0.001, Candles: map[string][]market.Candle{"AAA": cs}}), cs
}

var wait = strategy.Decision{} // the warm-up candles 100 100 100

func tranche(a strategy.Action, stop float64) strategy.Decision {
	return strategy.Decision{Action: a, Side: strategy.Long, Stop: stop, Fraction: 0.5}
}

// The loop writes the trade budget to state-<level>.json once the
// first tranche is sent, and drops it once the position is closed.
func TestLoopKeepsTheTradeInTheStateFile(t *testing.T) {
	f, cs := trancheFake()
	dir := filepath.Join(t.TempDir(), "state")
	answers := []strategy.Decision{wait, wait, wait, tranche(strategy.Enter, 100), {Action: strategy.Exit}}
	s := &scripted.Strategy{Decider: func(strategy.Input) strategy.Decision {
		if len(answers) == 0 {
			return strategy.Decision{}
		}
		d := answers[0]
		answers = answers[1:]
		return d
	}}
	c, err := config.Parse([]byte(trancheYAML), s)
	if err != nil {
		t.Fatal(err)
	}
	var held map[string]risk.Trade
	tick := &FakeTicker{Ex: f, Candles: cs, OnStep: func(i int) {
		if i == 3 { // after the entry step
			st, _ := LoadState(dir, "paper")
			held = st.Trades
		}
	}}
	Run(context.Background(), c, s, f, exchange.Demo, tick, dir)
	if held["AAA"] != (risk.Trade{Budget: 100, Used: 0.5, Tranches: 1, Stop: 100}) {
		t.Fatalf("state after the entry holds trades %+v, want AAA budget 100 used 0.5", held)
	}
	if st, _ := LoadState(dir, "paper"); len(st.Trades) != 0 || len(st.Positions) != 0 {
		t.Fatalf("state after the exit holds trades %+v positions %+v, want none", st.Trades, st.Positions)
	}
}

// crashAfterFill is the exchange of a process killed (kill -9) right after
// the fill of an order: from then on, the first Balance — the end-of-step
// save's — never returns.
type crashAfterFill struct {
	*fake.Fake
	armedAt int // orders placed when armed; -1 = not armed
}

func (c *crashAfterFill) Balance(ctx context.Context, env exchange.Env) (exchange.Balance, error) {
	if c.armedAt >= 0 && c.Fake.Calls("PlaceOrder") > c.armedAt {
		panic(killed{})
	}
	return c.Fake.Balance(ctx, env)
}

type killed struct{}

// runUntilKilled runs the loop; a kill ends it with nothing saved after it.
func runUntilKilled(t *testing.T, c *config.Config, s strategy.Strategy, ex exchange.Exchange, tick *FakeTicker, dir string) (dead bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(killed); !ok {
				panic(r)
			}
			dead = true
		}
	}()
	Run(context.Background(), c, s, ex, exchange.Demo, tick, dir)
	return false
}

// A kill -9 between an Add's fill and the end-of-step save must
// not renew the budget: after the restart the next Add is refused, and the
// position's loss at the stop stays within the trade budget (1% of 10 000).
func TestKillAfterAnAddFillKeepsTheFractionUsed(t *testing.T) {
	f, cs := trancheFake()
	ex := &crashAfterFill{Fake: f, armedAt: -1}
	dir := filepath.Join(t.TempDir(), "state")
	script := func(arm bool, answers ...strategy.Decision) *scripted.Strategy {
		return &scripted.Strategy{Decider: func(in strategy.Input) strategy.Decision {
			if len(answers) == 0 {
				return strategy.Decision{}
			}
			d := answers[0]
			answers = answers[1:]
			if arm && d.Action == strategy.Add {
				ex.armedAt = f.Calls("PlaceOrder") // the kill lands after this Add's fill
			}
			return d
		}}
	}
	s := script(true, wait, wait, wait, tranche(strategy.Enter, 100), tranche(strategy.Add, 0)) // at 105, then at 103
	c, err := config.Parse([]byte(trancheYAML), s)
	if err != nil {
		t.Fatal(err)
	}
	tick := &FakeTicker{Ex: f, Candles: cs}
	if !runUntilKilled(t, c, s, ex, tick, dir) {
		b, _ := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
		t.Fatalf("setup: the loop was never killed after the add; journal:\n%s", b)
	}
	if p := ownPosition(t, f); math.Abs(p.Qty-(10+16.666)) > 1e-9 {
		t.Fatalf("setup: qty %v at the kill, want 10 + 16.666", p.Qty)
	}

	ex.armedAt = -1
	f.Advance() // the restart comes a candle later
	s2 := script(false, tranche(strategy.Add, 0))
	c2, _ := config.Parse([]byte(trancheYAML), s2)
	Run(context.Background(), c2, s2, ex, exchange.Demo, tickerFrom(f, cs, tick.i.Load()+1), dir)
	p := ownPosition(t, f)
	if loss := p.Qty * (p.AvgPrice - 100); loss > 100+1e-6 {
		t.Fatalf("loss at the stop %.4f (qty %v avg %v) is above the trade budget 100: the restart renewed the fraction used", loss, p.Qty, p.AvgPrice)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "journal.jsonl")); !strings.Contains(string(b), "above the whole trade budget") {
		t.Fatalf("the restart never refused an add; journal:\n%s", b)
	}
}

func ownPosition(t *testing.T, f *fake.Fake) exchange.Position {
	t.Helper()
	ps, _ := f.Positions(context.Background(), exchange.Demo)
	if len(ps) != 1 {
		t.Fatalf("positions %+v, want one", ps)
	}
	return ps[0]
}

// A kill -9 right after an Add's fill: the restarted process tells the
// strategy the budget used and the tranches the state file saved before
// the order, not a count rebuilt in memory.
func TestRestartTellsTheStrategyThePersistedTranches(t *testing.T) {
	f, cs := trancheFake()
	ex := &crashAfterFill{Fake: f, armedAt: -1}
	dir := filepath.Join(t.TempDir(), "state")
	s := scriptOf(func(d strategy.Decision) {
		if d.Action == strategy.Add {
			ex.armedAt = f.Calls("PlaceOrder")
		}
	}, wait, wait, wait, tranche(strategy.Enter, 100), tranche(strategy.Add, 0))
	c, _ := config.Parse([]byte(trancheYAML), s)
	tick := &FakeTicker{Ex: f, Candles: cs}
	if !runUntilKilled(t, c, s, ex, tick, dir) {
		t.Fatal("setup: the loop was never killed after the add")
	}
	ex.armedAt = -1
	f.Advance()
	var seen []strategy.PositionView
	s2 := &scripted.Strategy{Decider: func(in strategy.Input) strategy.Decision {
		if in.Position != nil {
			seen = append(seen, *in.Position)
		}
		return strategy.Decision{}
	}}
	c2, _ := config.Parse([]byte(trancheYAML), s2)
	Run(context.Background(), c2, s2, ex, exchange.Demo, tickerFrom(f, cs, tick.i.Load()+1), dir)
	if len(seen) == 0 || seen[0].BudgetUsed != 1 || seen[0].Tranches != 2 {
		t.Fatalf("views after the restart %+v, want the budget wholly used in 2 tranches", seen)
	}
}

// failingBalance is an exchange whose balance cannot be read once fail is
// set, so the end-of-step save never refreshes the trades from the engine.
type failingBalance struct {
	*fake.Fake
	fail *atomic.Bool
}

func (b failingBalance) Balance(ctx context.Context, env exchange.Env) (exchange.Balance, error) {
	if b.fail.Load() {
		return exchange.Balance{}, errors.New("balance unavailable")
	}
	return b.Fake.Balance(ctx, env)
}

// A tranche refused because the state could not be written is not counted
// by a later save that succeeds: a restart sees only the tranches whose
// state was written before their order.
func TestATrancheWhoseSaveFailedIsNotCountedByALaterSave(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through directory permissions")
	}
	f, cs := trancheFake()
	dir := filepath.Join(t.TempDir(), "state")
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	var fail atomic.Bool
	s := scriptOf(func(d strategy.Decision) {
		if d.Action == strategy.Add {
			_ = os.Chmod(dir, 0o500) // the Add's state cannot be written
			fail.Store(true)
		}
	}, wait, wait, wait, tranche(strategy.Enter, 100), tranche(strategy.Add, 0))
	c, _ := config.Parse([]byte(trancheYAML), s)
	var restart State
	tick := &FakeTicker{Ex: f, Candles: cs, OnStep: func(i int) {
		switch i {
		case 4: // after the refused Add: the disk is writable again
			_ = os.Chmod(dir, 0o700)
		case 5: // a later save went through: what a restart would load
			restart, _ = LoadState(dir, "paper")
		}
	}}
	Run(context.Background(), c, s, failingBalance{f, &fail}, exchange.Demo, tick, dir)
	b, _ := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if !strings.Contains(string(b), "trade budget not persisted") || !strings.Contains(string(b), "balance unavailable") {
		t.Fatalf("setup: want the Add refused and a later save; journal:\n%s", b)
	}
	if got := restart.Trades["AAA"]; got.Used != 0.5 || got.Tranches != 1 {
		t.Fatalf("state after a later save holds %+v, want only the entry (used 0.5, 1 tranche)", got)
	}
}
