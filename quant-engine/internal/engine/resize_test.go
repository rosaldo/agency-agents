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
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/paper"
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

const perpYAML = "market: swap\nperp: {allow_short: true, leverage: 2}\n"

// enterThenAdd steps the entry (at 105, stop 100), then the add (at 103).
func enterThenAdd(t *testing.T, extra string, addStop float64) (*Engine, *fake.Fake) {
	t.Helper()
	e, f, _ := trancheEngine(t, extra, []float64{105, 103, 102}, enter(0.5, 100), add(0.5, addStop))
	steps(t, e, f, 1)
	f.Advance()
	return e, f
}

// After an add fills, the stop on the exchange covers the whole quantity at
// the add's (tighter) stop, and the trade keeps that stop for a restart.
func TestAddLeavesOneStopForTheWholeQuantity(t *testing.T) {
	e, f := enterThenAdd(t, "", 101)
	old := position(t, f).StopClientID
	steps(t, e, f, 1)
	p := position(t, f)
	if math.Abs(p.Qty-35) > 1e-9 || p.StopQty != p.Qty || p.StopPrice != 101 { // 10 + 50 / (103 - 101)
		t.Fatalf("position %+v; want 35 under one stop for all of it at 101", p)
	}
	if st, _ := f.Order(old); st.Status != exchange.Canceled {
		t.Fatalf("the entry's stop %+v, want cancelled", st)
	}
	if got := e.Trades()["AAA"].Stop; got != 101 {
		t.Fatalf("trade stop %v, want 101", got)
	}
}

// The old stop's cancel fails after the swap: it is kept as stale (and
// persisted at once), the symbol blocked, as for a trail's.
func TestAddWhoseOldStopIsNotCancelledKeepsItStale(t *testing.T) {
	e, f := enterThenAdd(t, perpYAML, 0)
	old := position(t, f).StopClientID
	saved := 0
	e.OnStale = func() { saved++ }
	e.Exec.Retries = 1
	f.FailNext("CancelOrder", errors.New("timeout"), false)
	steps(t, e, f, 1)
	if got := e.StaleStops()["AAA"]; got != old || saved != 1 {
		t.Fatalf("stale stops %v (saved %d times); want the entry's stop %s, saved once", e.StaleStops(), saved, old)
	}
	if p := position(t, f); p.StopQty != p.Qty {
		t.Fatalf("position %+v; want the new stop for the whole quantity", p)
	}
}

// On spot the old stop left stale and the new one could both sell: the
// position is closed at once.
func TestAddWhoseOldStopIsNotCancelledOnSpotExits(t *testing.T) {
	e, f := enterThenAdd(t, "", 0)
	e.Exec.Retries = 1
	f.FailNext("CancelOrder", errors.New("timeout"), false)
	steps(t, e, f, 1)
	if ps, _ := f.Positions(context.Background(), exchange.Demo); len(ps) != 0 {
		t.Fatalf("positions %+v; want the position closed", ps)
	}
}

// After the add the stop is checked against the liquidation the exchange
// reports, at the new average price: entered at 100 with stop 95 against a
// liquidation at 89 it clears (buffer 0.5); with the add at 105 the average
// is 101.67 and it does not, so the position is flattened and the engine halts.
func TestAddStopVsLiquidationIsCheckedAtTheAveragePrice(t *testing.T) {
	e, f, _ := trancheEngine(t, perpYAML, []float64{100, 105}, enter(0.5, 95), add(0.5, 0))
	f.SetLiquidation("AAA", 89)
	steps(t, e, f, 1)
	f.Advance()
	err := e.Step(context.Background())
	if !IsHalt(err) || !strings.Contains(journalOf(e).halted(), "stop vs liquidation") {
		t.Fatalf("err %v, halt %q; want a halt over the stop vs the liquidation", err, journalOf(e).halted())
	}
	if ps, _ := f.Positions(context.Background(), exchange.Demo); len(ps) != 0 {
		t.Fatalf("positions %+v; want the position flattened", ps)
	}
}

// The paper simulator gets the same swap from the executor: after the add
// its simulated stop closes the whole quantity and the entry's is cancelled.
func TestPaperAddResizesTheSimulatedStop(t *testing.T) {
	answers := []strategy.Decision{enter(0.5, 100), add(0.5, 0)}
	s := &scripted.Strategy{Decider: func(strategy.Input) strategy.Decision {
		if len(answers) == 0 {
			return strategy.Decision{}
		}
		d := answers[0]
		answers = answers[1:]
		return d
	}}
	c, err := config.Parse([]byte(scriptedYAML), s)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for k, x := range []float64{100, 100, 100, 105, 103, 102} {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(k) * time.Hour), Open: x, High: x, Low: x, Close: x, Volume: 10})
	}
	data := fake.New(fake.Options{Paper: true, QtyStep: 0.001, MinQty: 0.001, Candles: map[string][]market.Candle{"AAA": cs}})
	for k := 0; k < 3; k++ {
		data.Advance()
	}
	now := cs[3].Time
	sim := paper.New(data, 10_000, func() time.Time { return now }, paper.Profile{})
	e := New(c, s, sim, exchange.Demo, &events{}, func() time.Time { return now })
	ctx := context.Background()
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	data.Advance()
	now = cs[4].Time
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	ps, _ := sim.Positions(ctx, exchange.Demo)
	if len(ps) != 1 || math.Abs(ps[0].Qty-(10+16.666)) > 1e-9 || ps[0].StopQty != ps[0].Qty {
		t.Fatalf("positions %+v; want 26.666 under a simulated stop for all of it", ps)
	}
	if st, _ := sim.CancelOrder(ctx, exchange.Demo, e.Exec.ID("AAA", cs[3].Time, "stop")); st.Status != exchange.Canceled {
		t.Fatalf("the entry's simulated stop %+v, want cancelled", st)
	}
}
