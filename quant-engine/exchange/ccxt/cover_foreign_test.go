package ccxt

import (
	"errors"
	"testing"
	"time"

	lib "github.com/rosaldo/ccxt/v5"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exec"
)

// refuseStopsWhenArmed refuses every stop (an order with a trigger price) once armed.
type refuseStopsWhenArmed struct {
	*swapStub
	armed bool
}

func (c *refuseStopsWhenArmed) CreateOrder(symbol, typ, side string, amount float64, opts ...lib.CreateOrderOptions) (lib.Order, error) {
	var o lib.CreateOrderOptionsStruct
	for _, fn := range opts {
		fn(&o)
	}
	if c.armed && o.Params != nil {
		if _, ok := (*o.Params)["triggerPrice"]; ok {
			return lib.Order{}, &lib.Error{Type: lib.InvalidOrderErrType, Message: "no stops"}
		}
	}
	return c.swapStub.CreateOrder(symbol, typ, side, amount, opts...)
}

// With the user's foreign quantity on the account, a swap that cannot place
// a stop flattens only the engine's added part, reduce-only, never the foreign.
func TestCoverWithForeignQtyFlattensOnlyTheOwnDeficit(t *testing.T) {
	c := &refuseStopsWhenArmed{swapStub: newSwapStub()}
	a := swapAdapter(t, c)
	e := executor(a)
	if err := e.Enter(ctx, perp, now, exec.Plan{Side: exchange.Long, Entry: 100, Stop: 90, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	c.positions = account("long", 2+4) // 4 foreign
	p := onePosition(t, a)
	c.armed = true
	c.positions = account("long", 2+3+4)
	_, err := e.Add(ctx, perp, now.Add(time.Hour), p, exec.Plan{Side: exchange.Long, Entry: 100, Stop: 95, Qty: 3})
	if !errors.Is(err, exec.ErrHalt) {
		t.Fatalf("err %v, want halt", err)
	}
	var flat []lib.Order
	for _, o := range c.orders {
		if o.TriggerPrice == nil && deref(o.Side) == "sell" {
			flat = append(flat, o)
		}
	}
	if len(flat) != 1 || val(flat[0].Amount) != 3 || flat[0].ReduceOnly != nil && !*flat[0].ReduceOnly {
		t.Fatalf("market sells %+v; want one reduce-only sell of the added 3", flat)
	}
	if rs := restingStops(c.stub); len(rs) != 1 || val(rs[0].Amount) != 2 {
		t.Fatalf("resting stops %+v; want the old stop for 2", rs)
	}
}
