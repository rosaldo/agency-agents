package exec

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
)

// QA-T10b-1: the old stop filled in part while the trail swapped it: the new
// stop, sized for the whole position, is replaced by one for what is left
// (new first), so it never closes more than the own quantity.
func TestMoveStopAfterAPartialFillOfTheOldStopCoversTheRest(t *testing.T) {
	f, s, e, p := entered(t)
	s.onCancel = func(id string) (exchange.OrderState, error, bool) {
		if id != p.StopClientID {
			return exchange.OrderState{}, nil, false
		}
		if _, err := f.PlaceOrder(ctx, exchange.Demo, exchange.OrderRequest{ClientID: "old-part", Symbol: "AAA", Side: exchange.Sell,
			Qty: 4, Type: exchange.Market, ReduceOnly: true}); err != nil {
			t.Fatal(err)
		}
		return exchange.OrderState{ClientID: id, Status: exchange.Canceled, Filled: 4}, nil, true
	}
	stale, err := e.MoveStop(ctx, p, 95, t1)
	if err != nil || stale != "" {
		t.Fatalf("stale %q, err %v", stale, err)
	}
	rest := e.ID("AAA", t1, "trail-rest")
	q := onlyPosition(t, f)
	if q.Qty != 6 || q.StopClientID != rest || q.StopQty != 6 || q.StopPrice != 95 {
		t.Fatalf("position %+v; want 6 left under a stop for 6 at 95", q)
	}
	if st, _ := f.Order(e.ID("AAA", t1, "trail")); st.Status == exchange.Open {
		t.Fatalf("the stop for 10 still rests: %+v", st)
	}
	if got := strings.Join(s.calls[len(s.calls)-2:], ","); got != "place "+rest+",cancel "+e.ID("AAA", t1, "trail") {
		t.Fatalf("calls %v; want the rest placed before the larger one is cancelled", s.calls)
	}
	assertProtected(t, f)
}

// QA-T10b-2 (2b): a new stop of unknown fate whose cancel fails is stale only
// when the answer says it rests; one the venue does not know is not.
func TestUnknownNewStopIsStaleOnlyWhenItRests(t *testing.T) {
	for _, rests := range []bool{true, false} {
		f, s, e, p := entered(t)
		f.FailNext("PlaceStop", errors.New("timeout"), false)
		newID := e.ID("AAA", t1, "trail")
		if rests {
			s.onCancel = func(id string) (exchange.OrderState, error, bool) {
				return exchange.OrderState{ClientID: id, Status: exchange.Open}, errors.New("cancel timeout"), id == newID
			}
		}
		stale, err := e.MoveStop(ctx, p, 95, t1)
		want := ""
		if rests {
			want = newID
		}
		if err == nil || stale != want {
			t.Fatalf("rests=%v: stale %q err %v; want stale %q", rests, stale, err, want)
		}
		assertProtected(t, f)
	}
}

// shrunk reports the position smaller than the fake holds, as an account
// netted down (FR-P8): its stop then closes more than the own quantity.
type shrunk struct {
	*fake.Fake
	qty float64
}

func (x *shrunk) Positions(ctx context.Context, env exchange.Env) ([]exchange.Position, error) {
	ps, err := x.Fake.Positions(ctx, env)
	for i := range ps {
		ps[i].Qty = x.qty
	}
	return ps, err
}

// FR-P8: Reconcile resizes a stop larger than the own quantity, new stop first.
func TestReconcileResizesAStopLargerThanTheOwnQuantity(t *testing.T) {
	f, _, e, p := entered(t)
	e.Ex = &shrunk{Fake: f, qty: 6}
	if err := e.Reconcile(ctx, func(exchange.Position) float64 { return 90 }); err != nil {
		t.Fatal(err)
	}
	q := onlyPosition(t, f)
	if q.StopClientID == p.StopClientID || q.StopQty != 6 || q.StopPrice != 90 {
		t.Fatalf("position %+v; want a stop for 6 at 90 in place of the old one", q)
	}
	if st, _ := f.Order(p.StopClientID); st.Status == exchange.Open {
		t.Fatalf("the larger stop still rests: %+v", st)
	}
}
