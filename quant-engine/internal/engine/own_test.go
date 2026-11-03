package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/ownbook"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// accountView adds what an adapter reports beyond the fake: a symbol blocked
// with nothing of the engine's held, and funding.
type accountView struct {
	*fake.Fake
	blocked string
	funding []exchange.FundingPayment
}

func (x *accountView) Positions(ctx context.Context, env exchange.Env) ([]exchange.Position, error) {
	ps, err := x.Fake.Positions(ctx, env)
	if x.blocked != "" && len(ps) == 0 {
		ps = append(ps, exchange.Position{Symbol: "AAA", Blocked: x.blocked})
	}
	return ps, err
}

func (x *accountView) Funding(_ context.Context, _ exchange.Env, sym string, since time.Time) ([]exchange.FundingPayment, error) {
	var out []exchange.FundingPayment
	for _, p := range x.funding {
		if p.Symbol == sym && !p.Time.Before(since) {
			out = append(out, p)
		}
	}
	return out, nil
}

func view(e *Engine, f *fake.Fake) *accountView {
	x := &accountView{Fake: f}
	e.Ex, e.Exec.Ex = x, x
	return x
}

func enterAlways(strategy.Input) strategy.Decision {
	return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 100, Reason: "test"}
}

func count(j *events, kind string) int {
	n := 0
	for _, ev := range *j {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

// A symbol blocked by foreign quantity, with nothing of the engine's,
// takes no entry; the block is journaled once (foreign_qty_detected), its
// lifting too, and only then the entry goes; flatten skips it.
func TestBlockedSymbolTakesNoEntryAndIsJournaledOnce(t *testing.T) {
	e, f, j, _ := scriptedEngine(t, "", enterAlways)
	x := view(e, f)
	x.blocked = ownbook.ForeignQty + ": the account holds 3 long, the engine none"
	for i := 0; i < 2; i++ {
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if f.Calls("PlaceOrder") != 0 || count(j, journal.ForeignQty) != 1 {
		t.Fatalf("orders %d, foreign events %d; want none and one", f.Calls("PlaceOrder"), count(j, journal.ForeignQty))
	}
	if err := e.Flatten(context.Background()); err != nil {
		t.Fatalf("flatten with a blocked, empty symbol: %v", err)
	}
	x.blocked = ""
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Calls("PlaceOrder") != 1 || count(j, journal.Unblocked) != 1 {
		t.Fatalf("orders %d, unblocked events %d; want the entry and one", f.Calls("PlaceOrder"), count(j, journal.Unblocked))
	}
}

// A position gone without the engine's exit (liquidated, closed or
// reversed by the user) is journaled and its target cleared, also when the
// symbol comes back blocked with nothing of the engine's.
func TestPositionClosedOutsideIsJournaledAndItsTargetCleared(t *testing.T) {
	e, f, j, s := scriptedEngine(t, "", func(in strategy.Input) strategy.Decision {
		if in.Position != nil {
			return strategy.Decision{}
		}
		return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 100, Target: 120, Reason: "test"}
	})
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := position(t, f)
	if _, err := f.PlaceOrder(context.Background(), exchange.Demo, exchange.OrderRequest{ClientID: "outside", Symbol: "AAA",
		Side: exchange.Sell, Qty: p.Qty, Type: exchange.Market, ReduceOnly: true}); err != nil {
		t.Fatal(err)
	}
	view(e, f).blocked = "the account holds 1 short against the engine's 2 long: not the engine's, left untouched" // the user reversed
	s.Decider = func(strategy.Input) strategy.Decision { return strategy.Decision{} }
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.Target("AAA"); ok || count(j, journal.ClosedOutside) != 1 {
		t.Fatalf("target kept %v, closed-outside events %d; want cleared and one", ok, count(j, journal.ClosedOutside))
	}
}

// Funding the venue reports on a held swap position goes to the
// journal, once, and only from when the engine saw the position.
func TestFundingIsJournaled(t *testing.T) {
	e, f, j, _ := scriptedEngine(t, "market: swap\nperp: {leverage: 2}\n", func(in strategy.Input) strategy.Decision {
		if in.Position != nil {
			return strategy.Decision{}
		}
		return enterAlways(in)
	})
	x := view(e, f)
	now := e.Exec.Now()
	x.funding = []exchange.FundingPayment{{Time: now.Add(-time.Hour), Symbol: "AAA", Amount: 9}} // before the position
	for i := 0; i < 2; i++ {                                                                     // entry; then the first look starts the window
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	x.funding = append(x.funding, exchange.FundingPayment{Time: now.Add(time.Minute), Symbol: "AAA", Amount: -0.25})
	e.Exec.Now = func() time.Time { return now.Add(time.Hour) }
	for i := 0; i < 2; i++ {
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var got []float64
	for _, ev := range *j {
		if ev.Kind == journal.Funding {
			got = append(got, ev.Amount)
		}
	}
	if len(got) != 1 || got[0] != -0.25 {
		t.Fatalf("funding events %v; want the one payment after the entry, once", got)
	}
}

// The stale stops are retried by Reconcile, which the guard
// after a halt runs every candle.
func TestReconcileRetriesTheStaleStops(t *testing.T) {
	e, f, j, _ := scriptedEngine(t, "", enterAlways)
	f.SetPosition("AAA", 1, 100)
	for _, id := range []string{"old-stop", "new-stop"} { // the fake rests one stop per position
		if _, err := f.PlaceStop(context.Background(), exchange.Demo, id, "AAA", exchange.Sell, 1, 90); err != nil {
			t.Fatal(err)
		}
	}
	e.Restore(nil, nil, map[string]string{"AAA": "old-stop"}, nil)
	if err := e.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	retried := false
	for _, ev := range *j {
		retried = retried || ev.ClientID == "old-stop" && strings.Contains(ev.Reason, "retry")
	}
	if len(e.StaleStops()) != 0 || !retried {
		t.Fatalf("stale %v; want the retry to clear it", e.StaleStops())
	}
}
