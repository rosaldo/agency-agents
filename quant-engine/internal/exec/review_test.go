package exec

import (
	"context"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
)

// cancelAfter cancels ctx right after a successful CancelOrder: the stop
// command arrives between the stop cancel and the market sell of an exit.
type cancelAfter struct {
	*fake.Fake
	cancel func()
}

func (c cancelAfter) CancelOrder(ctx context.Context, env exchange.Env, id string) (exchange.OrderState, error) {
	st, err := c.Fake.CancelOrder(ctx, env, id)
	c.cancel()
	return st, err
}

// An exit interrupted by stop never leaves the position naked.
func TestExitInterruptedByStopStaysProtected(t *testing.T) {
	f, e := setup(t, false)
	if err := e.Enter(ctx, "AAA", t0, plan); err != nil {
		t.Fatal(err)
	}
	p := onlyPosition(t, f)
	cctx, cancel := context.WithCancel(context.Background())
	e2 := &Executor{Ex: cancelAfter{f, cancel}, Env: exchange.Demo, Now: func() time.Time { return t0 }, Risk: e.Risk}
	_ = e2.Exit(cctx, p, t0, "target")
	ps, _ := f.Positions(ctx, exchange.Demo)
	for _, q := range ps {
		if q.StopClientID == "" {
			t.Fatalf("exit interrupted by stop left the position without a stop: %+v", q)
		}
	}
}

// A position that appears unprotected is protected by Reconcile;
// the engine calls it every step (see the engine test).
var _ = fake.New
