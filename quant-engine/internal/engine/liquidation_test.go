package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// liqEngine is swapEngine (candle closing at 105, leverage 2, zero costs,
// min_liquidation_buffer 0.5) with the fake's liquidation options; the
// strategy enters once on side with stop.
func liqEngine(t *testing.T, o fake.Options, side strategy.Side, stop float64) (*Engine, *fake.Fake, *events) {
	t.Helper()
	return liqEngineWith(t, "", o, side, stop)
}

// liqEngineWith is liqEngine with extra yml.
func liqEngineWith(t *testing.T, extra string, o fake.Options, side strategy.Side, stop float64) (*Engine, *fake.Fake, *events) {
	t.Helper()
	s := &scripted.Strategy{Decider: func(in strategy.Input) strategy.Decision {
		if in.Position != nil {
			return strategy.Decision{}
		}
		return strategy.Decision{Action: strategy.Enter, Side: side, Stop: stop, Reason: "test"}
	}}
	c, err := config.Parse([]byte(scriptedYAML+"market: swap\nperp: {allow_short: true, leverage: 2}\n"+extra), s)
	if err != nil {
		t.Fatal(err)
	}
	cs, _ := market.ReadCSVFile("../../testdata/backtest_one_trade.csv")
	o.Paper, o.Cash, o.QtyStep, o.MinQty, o.Shorts = true, 10_000, 1, 1, true
	o.Candles = map[string][]market.Candle{"AAA": cs}
	f := fake.New(o)
	for i := 0; i < 3; i++ {
		f.Advance()
	}
	now := cs[3].Time
	j := &events{}
	return New(c, s, f, exchange.Demo, j, func() time.Time { return now }), f, j
}

func (e events) halted() string {
	for _, ev := range e {
		if ev.Kind == journal.Halt {
			return ev.Reason
		}
	}
	return ""
}

// Before the entry: at leverage 2 with no maintenance formula the liquidation
// is 1/2 away; with buffer 0.5 the stop may be at most 25% from the entry.
// Long and short, both sides of the line.
func TestEntryStopMustClearTheLiquidationBuffer(t *testing.T) {
	for _, tc := range []struct {
		side  strategy.Side
		stop  float64
		enter bool
	}{
		{strategy.Long, 80, true},   // 23.8%
		{strategy.Long, 78, false},  // 25.7%
		{strategy.Short, 130, true}, // 23.8%
		{strategy.Short, 132, false},
	} {
		e, f, j := liqEngine(t, fake.Options{}, tc.side, tc.stop)
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		ps, _ := f.Positions(context.Background(), exchange.Demo)
		if got := len(ps) == 1; got != tc.enter {
			t.Errorf("%s stop %g: entered=%v, want %v (%s)", tc.side, tc.stop, got, tc.enter, j.rejected())
		}
		if !tc.enter && !strings.Contains(j.rejected(), "liquidation") {
			t.Errorf("%s stop %g: refusal not journaled with the liquidation: %q", tc.side, tc.stop, j.rejected())
		}
	}
}

// After the fill: the exchange's liquidation price is the truth. Long
// entry 105 stop 100: liquidation 97 leaves 8, half of it (4) is less than
// the stop's 5 ⇒ flatten and halt; 90 leaves 15 ⇒ keeps. Short mirrored:
// stop 110, liquidation 113 halts, 120 keeps.
func TestFilledPositionIsCheckedAgainstTheExchangesLiquidation(t *testing.T) {
	for _, tc := range []struct {
		side      strategy.Side
		stop, liq float64
		halt      bool
	}{
		{strategy.Long, 100, 97, true},
		{strategy.Long, 100, 90, false},
		{strategy.Long, 100, 106, true}, // liquidation on the wrong side
		{strategy.Short, 110, 113, true},
		{strategy.Short, 110, 120, false},
	} {
		e, f, j := liqEngine(t, fake.Options{ReportsLiquidation: true}, tc.side, tc.stop)
		f.SetLiquidation("AAA", tc.liq)
		err := e.Step(context.Background())
		ps, _ := f.Positions(context.Background(), exchange.Demo)
		if tc.halt {
			if !IsHalt(err) || len(ps) != 0 || !strings.Contains(j.halted(), "liquidation") {
				t.Errorf("%s stop %g liq %g: err %v, positions %+v, halt %q; want flattened and halted", tc.side, tc.stop, tc.liq, err, ps, j.halted())
			}
			continue
		}
		if err != nil || len(ps) != 1 {
			t.Errorf("%s stop %g liq %g: err %v, positions %+v; want it kept", tc.side, tc.stop, tc.liq, err, ps)
		}
	}
}

// A venue that reports liquidation prices and leaves one out is a
// violation; a venue that reports none falls back to the estimate.
func TestMissingLiquidationPriceIsAViolationWhereTheVenueReportsIt(t *testing.T) {
	e, f, j := liqEngine(t, fake.Options{ReportsLiquidation: true}, strategy.Long, 100)
	err := e.Step(context.Background())
	if ps, _ := f.Positions(context.Background(), exchange.Demo); !IsHalt(err) || len(ps) != 0 || !strings.Contains(j.halted(), "no liquidation price") {
		t.Fatalf("err %v, positions %+v, halt %q; want flattened and halted", err, ps, j.halted())
	}
	e, f, _ = liqEngine(t, fake.Options{}, strategy.Long, 100)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ps, _ := f.Positions(context.Background(), exchange.Demo); len(ps) != 1 {
		t.Fatalf("positions %+v; want the entry kept on the fallback estimate", ps)
	}
}

// Every step: the liquidation moves (margin, funding); a later step
// that finds it too near flattens and halts.
func TestLiquidationIsRecheckedEveryStep(t *testing.T) {
	e, f, j := liqEngine(t, fake.Options{ReportsLiquidation: true}, strategy.Long, 100)
	f.SetLiquidation("AAA", 80)
	ctx := context.Background()
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	f.Advance() // low 104: the stop at 100 is not touched
	if err := e.Step(ctx); err != nil {
		t.Fatalf("second step with the liquidation still at 80: %v", err)
	}
	f.SetLiquidation("AAA", 98)
	err := e.Step(ctx)
	if ps, _ := f.Positions(ctx, exchange.Demo); !IsHalt(err) || len(ps) != 0 || j.halted() == "" {
		t.Fatalf("err %v, positions %+v; want flattened and halted once the liquidation moved to 98", err, ps)
	}
}

// Without a position the applied leverage cannot be read
// back, so the estimate after the fill uses what the position reports, not
// the yml's. yml 2x clears a 4.8% stop; the exchange's 20x (5% to
// liquidation, 2.5% after the buffer) does not.
func TestFallbackUsesThePositionsAppliedLeverage(t *testing.T) {
	e, f, j := liqEngine(t, fake.Options{Leverage: 20}, strategy.Long, 100)
	err := e.Step(context.Background())
	if ps, _ := f.Positions(context.Background(), exchange.Demo); !IsHalt(err) || len(ps) != 0 || j.halted() == "" {
		t.Fatalf("err %v, positions %+v; want flattened and halted at the applied 20x", err, ps)
	}
}

// Spot has no liquidation: a stop far from the entry is only a sizing matter.
func TestSpotHasNoLiquidationCheck(t *testing.T) {
	e, f, j, _ := scriptedEngine(t, "", func(in strategy.Input) strategy.Decision {
		if in.Position != nil {
			return strategy.Decision{}
		}
		return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 50, Reason: "test"}
	})
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ps, _ := f.Positions(context.Background(), exchange.Demo); len(ps) != 1 {
		t.Fatalf("spot entry with a 52%% stop refused: %s", j.rejected())
	}
}

// With the sim profile's costs: commission 1% and slippage 1% take a
// round trip of 4% off the liquidation distance (0.5 - 0.04) x 0.5 = 23%, so
// the 23.8% stop that clears it at zero cost is refused.
func TestLiquidationBufferCountsTheSimProfileCosts(t *testing.T) {
	e, f, j := liqEngineWith(t, "sim: {profiles: {real: {commission_pct: 1, slippage_pct: 1}}}\n", fake.Options{}, strategy.Long, 80)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ps, _ := f.Positions(context.Background(), exchange.Demo); len(ps) != 0 || !strings.Contains(j.rejected(), "liquidation") {
		t.Fatalf("entered %v (%q); want the stop refused by the costs", ps, j.rejected())
	}
}
