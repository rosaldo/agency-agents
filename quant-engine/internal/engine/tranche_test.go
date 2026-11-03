package engine

import (
	"context"
	"errors"
	"math"
	"strings"
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

// trancheEngine steps the scripted strategy on closes 100 100 100 then
// closes[...], starting at the first of closes; QtyStep 0.001, cash 10 000,
// budget 1% = 100. Each element of answers is the decision of one step.
func trancheEngine(t *testing.T, extra string, closes []float64, answers ...strategy.Decision) (*Engine, *fake.Fake, *events) {
	t.Helper()
	i := 0
	s := &scripted.Strategy{Decider: func(strategy.Input) strategy.Decision {
		if i >= len(answers) {
			return strategy.Decision{}
		}
		i++
		return answers[i-1]
	}}
	yml := scriptedYAML + extra
	if strings.Contains(extra, "risk:") {
		yml = strings.Replace(scriptedYAML, "risk: {confirmed_by_user: true}\n", "", 1) + extra
	}
	c, err := config.Parse([]byte(yml), s)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for k, x := range append([]float64{100, 100, 100}, closes...) {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(k) * time.Hour), Open: x, High: x, Low: x, Close: x, Volume: 10})
	}
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 0.001, MinQty: 0.001, Shorts: strings.Contains(extra, "swap"), Candles: map[string][]market.Candle{"AAA": cs}})
	for k := 0; k < 3; k++ {
		f.Advance()
	}
	return New(c, s, f, exchange.Demo, &events{}, func() time.Time { return cs[3].Time }), f, nil
}

func steps(t *testing.T, e *Engine, f *fake.Fake, n int) {
	t.Helper()
	for k := 0; k < n; k++ {
		if k > 0 && !f.Advance() {
			t.Fatal("candles ran out")
		}
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func journalOf(e *Engine) *events { return e.Exec.Journal.(*events) }

func enter(f float64, stop float64) strategy.Decision {
	return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: stop, Fraction: f}
}

func add(f float64, stop float64) strategy.Decision {
	return strategy.Decision{Action: strategy.Add, Side: strategy.Long, Stop: stop, Fraction: f}
}

// Enter takes f x budget, each Add g x the budget fixed at the first
// tranche (not the current equity), against the position's stop; fractions
// above 1 are refused and journaled.
func TestAddSizesFromTheBudgetFixedAtTheFirstTranche(t *testing.T) {
	e, f, _ := trancheEngine(t, "", []float64{105, 103, 102}, enter(0.5, 100), add(0.5, 0), add(0.1, 0))
	steps(t, e, f, 1)
	if p := position(t, f); math.Abs(p.Qty-10) > 1e-9 { // 50 / (105 - 100)
		t.Fatalf("first tranche qty %v, want 10", p.Qty)
	}
	f.Advance()
	steps(t, e, f, 2) // equity is 9 980 at the add: 49.9 / 3 would be 16.633
	p := position(t, f)
	if math.Abs(p.Qty-(10+16.666)) > 1e-9 {
		t.Fatalf("qty after the add %v, want 10 + 16.666 (50 / (103 - 100)); rejections %q", p.Qty, journalOf(e).rejected())
	}
	if got := e.Trades()["AAA"]; got != (risk.Trade{Budget: 100, Used: 1, Tranches: 2, Stop: 100}) {
		t.Fatalf("trade %+v, want budget 100 used 1", got)
	}
	if f.Calls("PlaceOrder") != 2 || !strings.Contains(journalOf(e).rejected(), "above the whole trade budget") {
		t.Fatalf("orders %d, rejections %q; want the third tranche refused", f.Calls("PlaceOrder"), journalOf(e).rejected())
	}
}

// An Add while flat, on the other side, with a looser stop, or on a
// position whose trade budget is unknown is refused, journaled, never sent.
func TestBadAddsAreRefusedAndJournaled(t *testing.T) {
	short := add(0.5, 0)
	short.Side = strategy.Short
	cases := []struct {
		name    string
		answers []strategy.Decision
		want    string
	}{
		{"flat", []strategy.Decision{add(0.5, 99)}, "no own open position"},
		{"other side", []strategy.Decision{enter(0.5, 100), short}, "side"},
		{"looser stop", []strategy.Decision{enter(0.5, 100), add(0.5, 99)}, "looser"},
		{"bad fraction", []strategy.Decision{enter(0.5, 100), add(1.5, 0)}, "fraction"},
		{"bad fraction on enter", []strategy.Decision{enter(-1, 100)}, "fraction"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, f, _ := trancheEngine(t, "", []float64{105, 103}, tc.answers...)
			steps(t, e, f, len(tc.answers))
			sent := 0
			if strings.HasPrefix(tc.name, "other") || strings.HasPrefix(tc.name, "looser") || tc.name == "bad fraction" {
				sent = 1
			}
			for _, ev := range *journalOf(e) {
				if ev.Symbol != "AAA" {
					t.Fatalf("event %+v without the symbol", ev)
				}
			}
			if f.Calls("PlaceOrder") != sent || !strings.Contains(journalOf(e).rejected(), tc.want) {
				t.Fatalf("orders %d, rejections %q; want %d and %q", f.Calls("PlaceOrder"), journalOf(e).rejected(), sent, tc.want)
			}
		})
	}
}

// A position held before graduated entries (a state file without trades)
// has no trade budget: an Add on it is refused, never sized afresh.
func TestAddWithoutARecordedBudgetIsRefused(t *testing.T) {
	e, f, _ := trancheEngine(t, "", []float64{105, 103}, enter(0.5, 100))
	steps(t, e, f, 1)
	ps, _ := f.Positions(context.Background(), exchange.Demo)
	s := &scripted.Strategy{Decider: func(strategy.Input) strategy.Decision { return add(0.5, 0) }}
	c, _ := config.Parse([]byte(scriptedYAML), s)
	e2 := New(c, s, f, exchange.Demo, &events{}, e.Exec.Now)
	e2.Restore(e.Targets(), ps, nil, nil) // the old state: no trades
	f.Advance()
	if err := e2.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Calls("PlaceOrder") != 1 || !strings.Contains(journalOf(e2).rejected(), "no trade budget") {
		t.Fatalf("orders %d, rejections %q; want the add refused", f.Calls("PlaceOrder"), journalOf(e2).rejected())
	}
	e3 := New(c, s, f, exchange.Demo, &events{}, e.Exec.Now) // a restart with the saved trades keeps the budget
	e3.Restore(e.Targets(), ps, nil, e.Trades())
	if got := e3.Trades()["AAA"]; got != (risk.Trade{Budget: 100, Used: 0.5, Tranches: 1, Stop: 100}) {
		t.Fatalf("restored trade %+v, want budget 100 used 0.5", got)
	}
}

// An Add may tighten the stop: it is sized against the tighter one.
func TestAddWithATighterStopSizesAgainstIt(t *testing.T) {
	e, f, _ := trancheEngine(t, "", []float64{105, 103}, enter(0.5, 100), add(0.5, 101))
	steps(t, e, f, 2)
	if p := position(t, f); math.Abs(p.Qty-(10+25)) > 1e-9 || e.stops["AAA"] != 101 { // 50 / (103 - 101)
		t.Fatalf("qty %v stop %v, want 35 and 101 (%s)", p.Qty, e.stops["AAA"], journalOf(e).rejected())
	}
}

// Each tranche: the free margin, against what the position already locks.
func TestAddAboveTheFreeMarginIsRefused(t *testing.T) {
	e, f, _ := trancheEngine(t, "", []float64{105, 103}, enter(0.5, 100), add(0.5, 102.99)) // 50 / 0.01 = 5 000 units
	steps(t, e, f, 2)
	if f.Calls("PlaceOrder") != 1 || !strings.Contains(journalOf(e).rejected(), "margin") {
		t.Fatalf("orders %d, rejections %q; want a margin refusal", f.Calls("PlaceOrder"), journalOf(e).rejected())
	}
	wantFirstTrancheOnly(t, e)
}

// wantFirstTrancheOnly fails unless the trade still counts only the
// first tranche: a refused Add counts neither a tranche nor its fraction.
func wantFirstTrancheOnly(t *testing.T, e *Engine) {
	t.Helper()
	if got := e.Trades()["AAA"]; got.Used != 0.5 || got.Tranches != 1 {
		t.Fatalf("trade %+v after a refused add, want used 0.5 and 1 tranche", got)
	}
}

// richCash reports more free cash than the equity backs (the margin check
// alone would pass): the leverage ceiling on the aggregate still holds.
type richCash struct{ *fake.Fake }

func (r richCash) Balance(ctx context.Context, env exchange.Env) (exchange.Balance, error) {
	b, err := r.Fake.Balance(ctx, env)
	b.Cash = 1e9
	return b, err
}

func TestAddAboveTheLeverageCeilingIsRefused(t *testing.T) {
	e, f, _ := trancheEngine(t, "", []float64{105, 103}, enter(0.5, 100), add(0.5, 102.9)) // 500 units, notional 51 500 > 10 000 x 1
	e.Ex, e.Exec.Ex = richCash{f}, richCash{f}
	steps(t, e, f, 2)
	if f.Calls("PlaceOrder") != 1 || !strings.Contains(journalOf(e).rejected(), "leverage") {
		t.Fatalf("orders %d, rejections %q; want a leverage refusal", f.Calls("PlaceOrder"), journalOf(e).rejected())
	}
	wantFirstTrancheOnly(t, e)
}

// Each tranche: the stop must clear the liquidation of the AGGREGATE
// position (its average price), with the buffer.
func TestAddWhoseAggregateStopMeetsTheLiquidationIsRefused(t *testing.T) {
	// leverage 20: liquidation ~5% away, half of it is buffer; 102.5 is 2.4% under 105
	extra := "market: swap\nperp: {leverage: 20}\nrisk: {confirmed_by_user: true, max_leverage: 20}\n"
	e, f, _ := trancheEngine(t, extra, []float64{105, 111}, enter(0.5, 102.5), add(0.5, 0))
	steps(t, e, f, 2)
	if f.Calls("PlaceOrder") != 1 || !strings.Contains(journalOf(e).rejected(), "liquidation") {
		t.Fatalf("orders %d, rejections %q; want a liquidation refusal", f.Calls("PlaceOrder"), journalOf(e).rejected())
	}
	wantFirstTrancheOnly(t, e)
}

// An exit ends the trade: the next Enter fixes a new budget.
func TestExitEndsTheTrade(t *testing.T) {
	e, f, _ := trancheEngine(t, "", []float64{105, 103}, enter(0.5, 100), strategy.Decision{Action: strategy.Exit})
	steps(t, e, f, 2)
	if _, ok := e.Trades()["AAA"]; ok {
		t.Fatalf("trades %+v after the exit, want none", e.Trades())
	}
}

// The ceiling is on the AGGREGATE notional: the tranche alone
// (90.909 x 103 = 9 363.6) fits 10 000 x 1, the position after it does not.
func TestAddWhoseAggregateNotionalIsAboveTheLeverageCeilingIsRefused(t *testing.T) {
	e, f, _ := trancheEngine(t, "", []float64{105, 103}, enter(0.5, 100), add(0.5, 102.45)) // 50 / 0.55 = 90.909 units
	e.Ex, e.Exec.Ex = richCash{f}, richCash{f}
	steps(t, e, f, 2)
	if f.Calls("PlaceOrder") != 1 || !strings.Contains(journalOf(e).rejected(), "leverage") {
		t.Fatalf("orders %d, rejections %q; want a leverage refusal of the aggregate", f.Calls("PlaceOrder"), journalOf(e).rejected())
	}
	wantFirstTrancheOnly(t, e)
}

// blockedOwn reports the engine's own position as blocked (e.g. foreign qty
// detected on the symbol after the entry).
type blockedOwn struct{ *fake.Fake }

func (b blockedOwn) Positions(ctx context.Context, env exchange.Env) ([]exchange.Position, error) {
	ps, err := b.Fake.Positions(ctx, env)
	for i := range ps {
		ps[i].Blocked = "foreign qty detected"
	}
	return ps, err
}

type notClear struct{}

func (notClear) Clear(context.Context, string, time.Time) (bool, string, error) {
	return false, "exchange hack", nil
}

// An Add on a symbol with a stale stop, a blocked symbol or a
// not-clear news check is refused, journaled, never sent.
func TestAddOnABlockedSymbolIsRefused(t *testing.T) {
	cases := map[string]struct {
		block func(*Engine, *fake.Fake)
		want  string
	}{
		"stale stop": {func(e *Engine, _ *fake.Fake) { e.staleStops["AAA"] = "old-stop" }, "old stop old-stop not cancelled"},
		"blocked":    {func(e *Engine, f *fake.Fake) { e.Ex, e.Exec.Ex = blockedOwn{f}, blockedOwn{f} }, "foreign qty detected"},
		"news": {func(e *Engine, _ *fake.Fake) {
			cfg := *e.Cfg
			cfg.NewsGuard, cfg.AssetClass, e.Cfg, e.News = "required", "equity", &cfg, notClear{} // checked before the entry on an equity
		}, "news check not clear"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e, f, _ := trancheEngine(t, "", []float64{105, 103}, enter(0.5, 100), add(0.5, 0))
			steps(t, e, f, 1)
			tc.block(e, f)
			f.Advance()
			if err := e.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if f.Calls("PlaceOrder") != 1 || !strings.Contains(journalOf(e).rejected(), tc.want) {
				t.Fatalf("orders %d, rejections %q; want the add refused with %q", f.Calls("PlaceOrder"), journalOf(e).rejected(), tc.want)
			}
		})
	}
}

// When the trade budget cannot be persisted before a tranche's
// order, the order is not sent and the reason is journaled; the fraction
// stays counted (conservative).
func TestTrancheIsNotSentWhenItsBudgetCannotBePersisted(t *testing.T) {
	e, f, _ := trancheEngine(t, "", []float64{105, 103}, enter(0.5, 100), add(0.5, 0))
	saved := 0
	e.OnTrade = func() error { saved++; return nil }
	steps(t, e, f, 1)
	e.OnTrade = func() error { return errors.New("disk full") }
	f.Advance()
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if saved != 1 || f.Calls("PlaceOrder") != 1 || !strings.Contains(journalOf(e).rejected(), "not persisted") {
		t.Fatalf("saves %d, orders %d, rejections %q; want the add saved-or-not-sent", saved, f.Calls("PlaceOrder"), journalOf(e).rejected())
	}
	if got := e.Trades()["AAA"]; got.Used != 1 {
		t.Fatalf("trade %+v, want the fraction counted (used 1)", got)
	}
}
