package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
)

// The venue refuses the add's swap once and Reconcile covers the whole
// quantity at the add's stop: the engine keeps the stop the exchange holds
// (the add's), not the one from before the add.
func TestAddWhoseSwapFailsKeepsTheStopTheExchangeHolds(t *testing.T) {
	e, f, _ := trancheEngine(t, "", []float64{105, 103}, enter(0.5, 95), add(0.5, 98))
	steps(t, e, f, 1)
	f.Advance()
	e.Exec.Retries = 1
	f.FailNext("PlaceStop", errors.Join(exchange.ErrRefused, errors.New("busy")), false)
	if err := e.Step(context.Background()); err == nil || IsHalt(err) {
		t.Fatalf("step err %v; want the failed swap reported, without a halt", err)
	}
	ps, err := f.Positions(context.Background(), exchange.Demo)
	if err != nil || len(ps) != 1 || ps[0].StopPrice != 98 || ps[0].StopQty != ps[0].Qty {
		t.Fatalf("positions %+v (%v); want one covered whole at 98", ps, err)
	}
	if got := e.stops["AAA"]; got != 98 {
		t.Fatalf("engine stop %v; want 98, the one resting on the exchange", got)
	}
}
