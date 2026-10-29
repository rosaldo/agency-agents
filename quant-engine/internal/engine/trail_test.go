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
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// trailing is the scripted strategy plus a trail rule.
type trailing struct {
	*scripted.Strategy
	rule  func(strategy.Input) (float64, bool)
	calls int
}

func (s *trailing) Trail(in strategy.Input) (float64, bool) { s.calls++; return s.rule(in) }

// answers is a rule that answers each call with the next value (NaN = ok false).
func answers(xs ...float64) func(strategy.Input) (float64, bool) {
	return func(strategy.Input) (float64, bool) {
		if len(xs) == 0 {
			return 0, false
		}
		x := xs[0]
		xs = xs[1:]
		return x, !math.IsNaN(x)
	}
}

// trailEngine enters side with stop on its first step at the first close and
// then trails by rule; the bars are flat (high = low = close) unless lows are given.
func trailEngine(t *testing.T, extra string, o fake.Options, side strategy.Side, stop float64, closes []float64,
	rule func(strategy.Input) (float64, bool)) (*Engine, *fake.Fake, *events, *trailing) {
	t.Helper()
	entered := false
	s := &trailing{Strategy: &scripted.Strategy{Decider: func(in strategy.Input) strategy.Decision {
		if in.Position != nil || entered {
			return strategy.Decision{}
		}
		entered = true
		return strategy.Decision{Action: strategy.Enter, Side: side, Stop: stop, Reason: "test"}
	}}, rule: rule}
	c, err := config.Parse([]byte(scriptedYAML+extra), s)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for i, x := range closes {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(i) * time.Hour), Open: x, High: x, Low: x, Close: x, Volume: 10})
	}
	o.Paper, o.Cash, o.QtyStep, o.MinQty = true, 10_000, 1, 1
	o.Candles = map[string][]market.Candle{"AAA": cs}
	f := fake.New(o)
	j := &events{}
	e := New(c, s, f, exchange.Demo, j, func() time.Time { return cs[0].Time })
	e.Exec.Retries = 1 // an injected failure is not hidden by a resend
	return e, f, j, s
}

func position(t *testing.T, f *fake.Fake) exchange.Position {
	t.Helper()
	ps, _ := f.Positions(context.Background(), exchange.Demo)
	if len(ps) != 1 {
		t.Fatalf("positions = %+v, want one", ps)
	}
	return ps[0]
}

// stepAndCheck runs the entry step, then one step per answer, checking the stop.
func stepAndCheck(t *testing.T, e *Engine, f *fake.Fake, want ...float64) {
	t.Helper()
	ctx := context.Background()
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	for i, w := range want {
		f.Advance()
		if err := e.Step(ctx); err != nil {
			t.Fatal(err)
		}
		if got := position(t, f).StopPrice; got != w {
			t.Fatalf("step %d: stop %g, want %g", i+1, got, w)
		}
	}
}

// FR-P9 (a): only tighter answers move the stop; looser, equal or no answer
// keep it, silently. Long: up only; short: down only.
func TestTrailOnlyTightens(t *testing.T) {
	flat := []float64{100, 100, 100, 100, 100, 100}
	e, f, j, _ := trailEngine(t, "", fake.Options{}, strategy.Long, 90, flat, answers(95, 93, 95, math.NaN(), 97))
	stepAndCheck(t, e, f, 95, 95, 95, 95, 97)
	if n := f.Calls("PlaceStop"); n != 3 { // entry stop + two moves
		t.Fatalf("PlaceStop calls = %d, want 3", n)
	}
	if r := j.rejected(); r != "" {
		t.Fatalf("a looser answer is not a refusal: %q", r)
	}
	e, f, _, _ = trailEngine(t, "market: swap\nperp: {allow_short: true, leverage: 2}\n", fake.Options{Shorts: true},
		strategy.Short, 110, flat, answers(105, 107, 105, 0, 103))
	stepAndCheck(t, e, f, 105, 105, 105, 105, 103)
}

// FR-P9 (b): an answer at or beyond the close is refused and journaled.
func TestTrailOnTheWrongSideIsRefused(t *testing.T) {
	flat := []float64{100, 100, 100, 100}
	e, f, j, _ := trailEngine(t, "", fake.Options{}, strategy.Long, 90, flat, answers(101, 100, 95))
	stepAndCheck(t, e, f, 90, 90, 95)
	if r := j.rejected(); strings.Count(r, "wrong side") != 2 {
		t.Fatalf("refusals = %q, want two wrong-side ones", r)
	}
	e, f, j, _ = trailEngine(t, "market: swap\nperp: {allow_short: true, leverage: 2}\n", fake.Options{Shorts: true},
		strategy.Short, 110, flat, answers(99, 105))
	stepAndCheck(t, e, f, 110, 105)
	if r := j.rejected(); !strings.Contains(r, "wrong side") {
		t.Fatalf("refusals = %q", r)
	}
}

// FR-P9 (c): a move whose stop does not clear the liquidation (FR-P4) is
// refused and journaled, nothing sent. Through Step a tighter stop of a
// position that passed guardLiquidation always clears it; trail checks again
// with the T10 rule, here on a position that does not.
func TestTrailThatBreaksTheLiquidationBufferIsRefused(t *testing.T) {
	e, f, j, s := trailEngine(t, "market: swap\nperp: {allow_short: true, leverage: 2}\n",
		fake.Options{Shorts: true, ReportsLiquidation: true}, strategy.Long, 90, []float64{100, 100}, answers(84))
	p := exchange.Position{Symbol: "AAA", Side: exchange.Long, Qty: 10, AvgPrice: 100, StopClientID: "old", StopPrice: 80, LiquidationPrice: 85}
	if err := e.trail(context.Background(), p, strategy.Input{Symbol: "AAA"}, 100, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if s.calls != 1 || f.Calls("PlaceStop") != 0 {
		t.Fatalf("trail calls %d, PlaceStop calls %d; want asked once and nothing sent", s.calls, f.Calls("PlaceStop"))
	}
	if r := j.rejected(); !strings.Contains(r, "trail refused") || !strings.Contains(r, "liquidation") {
		t.Fatalf("refusal not journaled with the liquidation: %q", r)
	}
}

// FR-P4 with FR-P9: a long's stop trailed above its entry is past any loss,
// so before the liquidation; the every-step check must not flatten it.
func TestStopTrailedPastTheEntryClearsTheLiquidation(t *testing.T) {
	e, f, _, _ := trailEngine(t, "market: swap\nperp: {allow_short: true, leverage: 2}\n",
		fake.Options{Shorts: true, ReportsLiquidation: true}, strategy.Long, 90, []float64{100, 110, 110, 110}, answers(105))
	f.SetLiquidation("AAA", 60)
	stepAndCheck(t, e, f, 105, 105, 105)
}

// FR-P9 (d) on swap: the old stop's cancel fails: the move holds, the symbol
// waits (no new move, no entry) until the retry at the next step clears it.
func TestStaleStopBlocksTheSymbolUntilCancelled(t *testing.T) {
	e, f, _, s := trailEngine(t, "market: swap\nperp: {allow_short: true, leverage: 2}\n", fake.Options{Shorts: true},
		strategy.Long, 90, []float64{100, 100, 100, 100}, answers(95, 97))
	ctx := context.Background()
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	old := position(t, f).StopClientID
	f.Advance()
	f.FailNext("CancelOrder", errors.New("timeout"), false)
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.StaleStops()["AAA"]; got != old || position(t, f).StopPrice != 95 {
		t.Fatalf("stale %q, position %+v; want the old stop stale and the move kept", got, position(t, f))
	}
	f.Advance()
	f.FailNext("CancelOrder", errors.New("timeout"), false) // the retry fails too
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if s.calls != 1 || len(e.StaleStops()) != 1 {
		t.Fatalf("trail calls %d, stale %v; want no move while the old stop is pending", s.calls, e.StaleStops())
	}
	f.Advance()
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.StaleStops()) != 0 || position(t, f).StopPrice != 97 {
		t.Fatalf("stale %v, position %+v; want the stale cleared and the trail resumed", e.StaleStops(), position(t, f))
	}
}

// FR-P9 (d) on spot (§9.14): with an old stop not cancelled two stops could
// sell beyond the own quantity: the position is closed at once and the
// symbol takes no entry until the old stop is gone.
func TestStaleStopOnSpotClosesAndBlocks(t *testing.T) {
	e, f, j, _ := trailEngine(t, "", fake.Options{}, strategy.Long, 90, []float64{100, 100, 100, 100}, answers(95))
	entries := 0
	e.Strategy.(*trailing).Decider = func(in strategy.Input) strategy.Decision {
		if in.Position != nil {
			return strategy.Decision{}
		}
		entries++
		return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 90, Reason: "test"}
	}
	ctx := context.Background()
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	f.Advance()
	f.FailNext("CancelOrder", errors.New("timeout"), false)
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if ps, _ := f.Positions(ctx, exchange.Demo); len(ps) != 0 || len(e.StaleStops()) != 1 {
		t.Fatalf("positions %+v, stale %v; want flat and the old stop pending", ps, e.StaleStops())
	}
	f.Advance()
	f.FailNext("CancelOrder", errors.New("timeout"), false)
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if ps, _ := f.Positions(ctx, exchange.Demo); len(ps) != 0 || !strings.Contains(j.rejected(), "not cancelled yet") {
		t.Fatalf("positions %+v, refusals %q; want the entry blocked", ps, j.rejected())
	}
	f.Advance()
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if ps, _ := f.Positions(ctx, exchange.Demo); len(ps) != 1 || len(e.StaleStops()) != 0 {
		t.Fatalf("positions %+v, stale %v; want the symbol free again", ps, e.StaleStops())
	}
}

// FR-P9 (d): the old stop fills during the swap: the engine forgets the
// position's target and stop.
func TestOldStopFilledDuringTheMoveForgetsThePosition(t *testing.T) {
	e, f, _, _ := trailEngine(t, "", fake.Options{}, strategy.Long, 90, []float64{100, 100, 100}, answers(95))
	ctx := context.Background()
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	f.Advance()
	p := position(t, f)
	e.Ex = &firesOnCancel{Fake: f, id: p.StopClientID, qty: p.Qty}
	e.Exec.Ex = e.Ex
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if ps, _ := f.Positions(ctx, exchange.Demo); len(ps) != 0 || e.stops["AAA"] != 0 {
		t.Fatalf("positions %+v, stop %v; want none", ps, e.stops)
	}
}

// firesOnCancel fills the position when the old stop's cancel arrives.
type firesOnCancel struct {
	*fake.Fake
	id  string
	qty float64
}

func (x *firesOnCancel) CancelOrder(ctx context.Context, env exchange.Env, id string) (exchange.OrderState, error) {
	if id != x.id {
		return x.Fake.CancelOrder(ctx, env, id)
	}
	_, err := x.Fake.PlaceOrder(ctx, env, exchange.OrderRequest{ClientID: "old-fill", Symbol: "AAA", Side: exchange.Sell, Qty: x.qty, Type: exchange.Market, ReduceOnly: true})
	return exchange.OrderState{ClientID: id, Status: exchange.Filled}, err
}

// validateTrail as a table: sanity, tighten-only and side (FR-P9 a, b).
func TestValidateTrail(t *testing.T) {
	for _, tc := range []struct {
		side           exchange.PositionSide
		cur, next      float64
		ok             bool
		apply, refused bool
	}{
		{exchange.Long, 90, 95, true, true, false},
		{exchange.Long, 90, 95, false, false, false},
		{exchange.Long, 90, math.Inf(1), true, false, false},
		{exchange.Long, 90, -1, true, false, false},
		{exchange.Long, 90, 89, true, false, false},
		{exchange.Long, 90, 90, true, false, false},
		{exchange.Long, 90, 100, true, false, true},
		{exchange.Short, 110, 105, true, true, false},
		{exchange.Short, 110, 111, true, false, false},
		{exchange.Short, 110, 100, true, false, true},
	} {
		apply, err := validateTrail(tc.side, tc.cur, tc.next, tc.ok, 100)
		if apply != tc.apply || (err != nil) != tc.refused {
			t.Errorf("%s %g -> %g ok=%v: apply %v err %v", tc.side, tc.cur, tc.next, tc.ok, apply, err)
		}
	}
}
