package ccxt

// Re-review 3 probes (R1a, R2), ported from the frozen engine's round 3 to
// this adapter: they failed there and must pass here.

import (
	"errors"
	"fmt"
	"testing"

	lib "github.com/rosaldo/ccxt/v5"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exec"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
)

// R1a: DDoSProtection, RateLimitExceeded and InvalidNonce are refusals
// (CCXT's routerIsOutcomeUnknownError treats them as known outcomes); the
// rest of the OperationFailed family and ExchangeError stay unknown.
func TestProbeR3RefusedClassification(t *testing.T) {
	nowRefused := map[lib.ErrorType]bool{lib.DDoSProtectionErrType: true, lib.RateLimitExceededErrType: true, lib.InvalidNonceErrType: true}
	unknown := map[lib.ErrorType]bool{}
	for typ := range lib.ErrorParents {
		for p := typ; p != ""; p = lib.ErrorParents[p] {
			if p == lib.OperationFailedErrType && !nowRefused[typ] {
				unknown[typ] = true
			}
		}
	}
	unknown[lib.ExchangeErrorErrType] = true
	unknown[lib.DuplicateOrderIdErrType] = true
	for typ := range lib.ErrorParents {
		got := refused(fmt.Errorf("wrapped: %w", lib.NewError(string(typ), "m")))
		if unknown[typ] && got {
			t.Errorf("%s (outcome unknown) classified as refused", typ)
		}
		if nowRefused[typ] && !got {
			t.Errorf("%s (known outcome) not classified as refused", typ)
		}
	}
	for _, e := range []error{errors.New("InsufficientFunds: x"), lib.NewError("Exception", "x"), lib.NewError("BaseError", "x")} {
		if refused(e) {
			t.Errorf("untyped %v refused", e)
		}
	}
}

// R1a end to end: a 429 on the entry is a clean rejection, wrapped as
// exchange.ErrRefused — no halt, nothing left unknown in the book.
func TestRateLimitedEntryIsARefusal(t *testing.T) {
	c := &buyRefused{stub: newStub(now), err: lib.RateLimitExceeded("429")}
	a := adapter(t, c)
	err := executor(a).Enter(ctx, "AAA/USDT", now, exec.Plan{Entry: 100, Stop: 90, Qty: 1})
	if err == nil || errors.Is(err, exec.ErrHalt) || !errors.Is(err, exchange.ErrRefused) {
		t.Fatalf("429 entry: err = %v, want a refusal without halt", err)
	}
	if _, err := a.Positions(ctx, exchange.Real); err != nil {
		t.Fatalf("book wedged after a refused entry: %v", err)
	}
}

type stopLandedTimeout struct{ *stub }

func (c *stopLandedTimeout) CreateOrder(symbol, typ, side string, amount float64, opts ...lib.CreateOrderOptions) (lib.Order, error) {
	o, err := c.stub.CreateOrder(symbol, typ, side, amount, opts...) // it reached the exchange
	if isStop(opts) {
		return lib.Order{}, lib.RequestTimeout("no answer")
	}
	return o, err
}

type recorder struct{ evs []journal.Event }

func (r *recorder) Append(ev journal.Event) error { r.evs = append(r.evs, ev); return nil }

// R2: a stop whose answer was lost is cancelled by its client order ID before
// the position is flattened, and the cancel is journaled: no stop is left
// resting untracked on the exchange.
func TestProbeR3StopTimeoutLeavesAnOrphanStop(t *testing.T) {
	c := &stopLandedTimeout{stub: newStub(now)}
	a := adapter(t, c)
	e := executor(a)
	j := &recorder{}
	e.Journal = j
	err := e.Enter(ctx, "AAA/USDT", now, exec.Plan{Entry: 100, Stop: 90, Target: 105, Qty: 1})
	ps, _ := a.Positions(ctx, exchange.Real)
	if !errors.Is(err, exec.ErrHalt) || len(ps) != 0 || marketSells(c.stub) != 1 {
		t.Fatalf("Enter halt=%v positions=%d sells=%d; want halt, flat, one flatten", errors.Is(err, exec.ErrHalt), len(ps), marketSells(c.stub))
	}
	if n := stops(c.stub); n != 0 {
		t.Fatalf("resting stops on exchange=%d, want 0", n)
	}
	found := false
	for _, ev := range j.evs {
		if ev.Kind == string(exchange.Canceled) && ev.Reason == "cancel the stop of unknown fate before flattening" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the stop's cancel is not in the journal: %+v", j.evs)
	}
}
