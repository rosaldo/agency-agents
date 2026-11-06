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

// gapSetup is a long on AAA entered at 105 with its stop at 100, on candles
// whose next open (99) gaps through the stop; the strategy enters whenever
// flat, 5 below the last close.
func gapSetup(t *testing.T) (*config.Config, *scripted.Strategy, []market.Candle, func() *fake.Fake) {
	t.Helper()
	s := &scripted.Strategy{Decider: func(in strategy.Input) strategy.Decision {
		if in.Position != nil {
			return strategy.Decision{Reason: "hold"}
		}
		return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: in.Candles[len(in.Candles)-1].Close - 5, Reason: "test"}
	}}
	c, err := config.Parse([]byte(scriptedYAML), s)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for i, o := range [][4]float64{{100, 100, 100, 100}, {100, 100, 100, 100}, {100, 100, 100, 100}, {105, 105, 105, 105},
		{99, 100, 98, 99}, {99, 99, 99, 99}, {99, 99, 99, 99}} {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(i) * time.Hour), Open: o[0], High: o[1], Low: o[2], Close: o[3], Volume: 10})
	}
	newFake := func() *fake.Fake {
		f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs}})
		for i := 0; i < 3; i++ {
			f.Advance()
		}
		return f
	}
	return c, s, cs, newFake
}

func outsideEvents(j *events) []journal.Event {
	var out []journal.Event
	for _, ev := range *j {
		if ev.Kind == journal.ClosedOutside {
			out = append(out, ev)
		}
	}
	return out
}

// enterLong steps e once at cs[3] and returns the long it opened, as the
// loop's account read hands it to Remember.
func enterLong(t *testing.T, e *Engine, f *fake.Fake) exchange.Position {
	t.Helper()
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	ps, _ := f.Positions(context.Background(), exchange.Demo)
	if len(ps) != 1 || ps[0].StopClientID == "" || ps[0].StopPrice != 100 {
		t.Fatalf("positions %+v, want one long with its stop at 100", ps)
	}
	return ps[0]
}

// After a restart the saved positions are what the engine last saw: a stop
// that filled while it was down is priced from that fill when the exchange
// still has the order, and left unpriced when it does not.
func TestRestartPricesAStopFilledWhileDownOnlyWithTheBookKept(t *testing.T) {
	c, s, cs, newFake := gapSetup(t)
	f := newFake()
	p := enterLong(t, New(c, s, f, exchange.Demo, &events{}, func() time.Time { return cs[3].Time }), f)
	f.Advance() // the stop fills at the gap's open while the engine is down

	j := &events{}
	e := New(c, s, f, exchange.Demo, j, func() time.Time { return cs[4].Time })
	e.Restore(nil, []exchange.Position{p}, nil, nil)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if evs := outsideEvents(j); len(evs) != 1 || evs[0].Price != 99 || evs[0].Qty != p.Qty || evs[0].Side != "sell" {
		t.Fatalf("book kept: closed_outside %+v, want one sell of %v at 99", evs, p.Qty)
	}

	lost := newFake() // the venue's book no longer has the stop: the same position, no order behind it
	lost.Advance()
	j = &events{}
	e = New(c, s, lost, exchange.Demo, j, func() time.Time { return cs[4].Time })
	e.Restore(nil, []exchange.Position{p}, nil, nil)
	_ = e.Step(context.Background())
	if evs := outsideEvents(j); len(evs) != 1 || evs[0].Price != 0 || evs[0].Qty != 0 {
		t.Fatalf("book lost: closed_outside %+v, want one without a price", evs)
	}
}

// A short's stop is a buy, and filling above the entry is a loss.
func TestShortClosedByItsStopIsAPricedBuyAtALoss(t *testing.T) {
	ctx := context.Background()
	e, f, j, now := swapEngine(t, func(in strategy.Input) strategy.Decision {
		if in.Position != nil {
			return strategy.Decision{Reason: "hold"}
		}
		if len(in.Candles) > 4 {
			return strategy.Decision{Reason: "done"}
		}
		return strategy.Decision{Action: strategy.Enter, Side: strategy.Short, Stop: 110, Reason: "test"}
	})
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	ps, _ := f.Positions(ctx, exchange.Demo)
	e.Remember(ps)
	f.Advance() // high 111 crosses the stop at 110
	*now = now.Add(time.Hour)
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	evs := outsideEvents(j)
	if len(evs) != 1 || evs[0].Side != "buy" || evs[0].Qty != 20 || evs[0].Price != 110 ||
		!strings.Contains(evs[0].Reason, "P&L -100.00 against the average 105") { // 20 x (110 - 105), a short's loss
		t.Fatalf("closed_outside %+v, want a buy of 20 at 110 with P&L -100.00", evs)
	}
}

// cancelAs reports the stop as the venue would after a partial fill and a
// cancel: some quantity executed, the order no longer filled.
type cancelAs struct {
	*fake.Fake
	status exchange.Status
}

func (c cancelAs) CancelOrder(ctx context.Context, env exchange.Env, id string) (exchange.OrderState, error) {
	st, err := c.Fake.CancelOrder(ctx, env, id)
	st.Status = c.status
	return st, err
}

// Only a stop that is Filled prices the close: one that executed some and
// was cancelled says nothing of how the rest went.
func TestStopNotFilledLeavesTheCloseUnpriced(t *testing.T) {
	c, s, cs, newFake := gapSetup(t)
	f := newFake()
	j := &events{}
	now := cs[3].Time
	e := New(c, s, f, exchange.Demo, j, func() time.Time { return now })
	e.Remember([]exchange.Position{enterLong(t, e, f)})
	f.Advance()
	now = cs[4].Time
	e.Ex = cancelAs{f, exchange.Canceled}
	_ = e.Step(context.Background())
	if evs := outsideEvents(j); len(evs) != 1 || evs[0].Price != 0 {
		t.Fatalf("closed_outside %+v, want one without a price", evs)
	}
}

// What the engine saw of a position goes with it: a later position closed
// outside before any account read is not priced from the old one's stop.
func TestAGonePositionsStopDoesNotPriceTheNext(t *testing.T) {
	ctx := context.Background()
	c, s, cs, newFake := gapSetup(t)
	f := newFake()
	j := &events{}
	now := cs[3].Time
	e := New(c, s, f, exchange.Demo, j, func() time.Time { return now })
	e.Remember([]exchange.Position{enterLong(t, e, f)})
	f.Advance() // the first stop fills at 99
	now = cs[4].Time
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	ps, _ := f.Positions(ctx, exchange.Demo)
	if len(ps) != 1 {
		t.Fatalf("no second position after the stop-out: %+v (%s)", ps, j.rejected())
	}
	// no account read: the second position is closed by hand before the next step
	if _, err := f.PlaceOrder(ctx, exchange.Demo, exchange.OrderRequest{ClientID: "manual", Symbol: "AAA",
		Side: exchange.Sell, Qty: ps[0].Qty, Type: exchange.Market, ReduceOnly: true}); err != nil {
		t.Fatal(err)
	}
	f.Advance()
	now = cs[5].Time
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	evs := outsideEvents(j)
	if len(evs) != 2 || evs[0].Price != 99 || evs[1].Price != 0 || evs[1].Qty != 0 {
		t.Fatalf("closed_outside %+v, want the first priced at 99 and the second without a price", evs)
	}
}
