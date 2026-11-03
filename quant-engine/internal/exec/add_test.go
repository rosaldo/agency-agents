package exec

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
)

// addPlan is a tranche of 5 at 100 with a stop tightened from 90 to 92.
var addPlan = Plan{Entry: 100, Stop: 92, Qty: 5}

// After an add fills, the position's stop is swapped for one of the whole
// quantity at the add's stop: the new one placed first, the old one
// cancelled after, so no instant leaves the added quantity without a stop.
func TestAddSwapsTheStopForOneOfTheWholeQuantity(t *testing.T) {
	f, s, e, p := entered(t)
	stale, err := e.Add(ctx, "AAA", t1, p, addPlan)
	if err != nil || stale != "" {
		t.Fatalf("stale %q, err %v", stale, err)
	}
	newID := e.ID("AAA", t1, "add-stop")
	q := onlyPosition(t, f)
	if q.Qty != 15 || q.StopQty != 15 || q.StopPrice != 92 || q.StopClientID != newID {
		t.Fatalf("position %+v; want 15 under one stop for 15 at 92", q)
	}
	if got := strings.Join(s.calls, ","); got != "place "+newID+",cancel "+p.StopClientID {
		t.Fatalf("calls %s; want the new stop placed before the old one is cancelled", got)
	}
	if st, _ := f.Order(p.StopClientID); st.Status != exchange.Canceled {
		t.Fatalf("old stop %+v, want cancelled", st)
	}
	assertProtected(t, f)
}

// The venue refuses the swap's stop once: Reconcile covers the quantity the
// old stop misses at the add's stop; the add reports the failure, no halt.
func TestAddWhoseSwapFailsOnceIsCoveredByReconcile(t *testing.T) {
	f, _, e, p := entered(t)
	f.FailNext("PlaceStop", errors.Join(exchange.ErrRefused, errors.New("busy")), false)
	_, err := e.Add(ctx, "AAA", t1, p, addPlan)
	if err == nil || errors.Is(err, ErrHalt) {
		t.Fatalf("err %v; want the failed swap reported, without a halt", err)
	}
	if q := onlyPosition(t, f); q.Qty != 15 || q.StopQty != 15 || q.StopPrice != 92 {
		t.Fatalf("position %+v; want 15 covered by a stop for 15 at 92", q)
	}
	assertProtected(t, f)
}

// refusingStops refuses every stop: none can be placed.
type refusingStops struct{ *spy }

func (*refusingStops) PlaceStop(context.Context, exchange.Env, string, string, exchange.Side, float64, float64) (exchange.OrderState, error) {
	return exchange.OrderState{Status: exchange.Rejected}, errors.Join(exchange.ErrRefused, errors.New("no stops today"))
}

// No stop for the whole quantity can be placed: the added part, the one the
// old stop does not cover, is flattened and the engine halts; the old stop
// keeps guarding the quantity from before the add.
func TestAddWhoseStopCannotBePlacedFlattensTheTrancheAndHalts(t *testing.T) {
	f, s, e, p := entered(t)
	e.Ex = &refusingStops{s}
	_, err := e.Add(ctx, "AAA", t1, p, addPlan)
	if !errors.Is(err, ErrHalt) {
		t.Fatalf("err %v, want ErrHalt", err)
	}
	if q := onlyPosition(t, f); q.Qty != 10 || q.StopClientID != p.StopClientID || q.StopQty != 10 {
		t.Fatalf("position %+v; want the 10 from before the add, under its old stop", q)
	}
}

// The old stop fires during the swap: it closed only the quantity before the
// add, so the added part stays under a stop for what is left (new first),
// instead of losing the new stop with ErrStopFilled.
func TestAddOldStopFilledDuringTheSwapLeavesTheRestCovered(t *testing.T) {
	f, s, e, p := entered(t)
	s.onCancel = func(id string) (exchange.OrderState, error, bool) {
		if id != p.StopClientID {
			return exchange.OrderState{}, nil, false
		}
		if _, err := f.PlaceOrder(ctx, exchange.Demo, exchange.OrderRequest{ClientID: "old-fired", Symbol: "AAA", Side: exchange.Sell,
			Qty: 10, Type: exchange.Market, ReduceOnly: true}); err != nil {
			t.Fatal(err)
		}
		return exchange.OrderState{ClientID: id, Status: exchange.Filled, Filled: 10}, nil, true
	}
	if _, err := e.Add(ctx, "AAA", t1, p, addPlan); err != nil {
		t.Fatal(err)
	}
	if q := onlyPosition(t, f); q.Qty != 5 || q.StopQty != 5 || q.StopPrice != 92 || q.StopClientID != e.ID("AAA", t1, "add-stop-rest") {
		t.Fatalf("position %+v; want the added 5 under a stop for 5 at 92", q)
	}
	assertProtected(t, f)
}
