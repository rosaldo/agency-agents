package ccxt

// Re-review 3 probes (R1a, R2), ported from the frozen engine's round 3 to
// this adapter: they failed there and must pass here.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// unknownBuy lists AAA and BBB; a market buy of AAA fails with err (fate
// unknown, or a refusal) and its lookups find nothing.
type unknownBuy struct {
	*stub
	err error
}

func (c *unknownBuy) LoadMarkets(...any) (map[string]lib.MarketInterface, error) {
	m := func(base string) lib.MarketInterface {
		return lib.MarketInterface{Symbol: s(base + "/USDT"), Type: s("spot"), Spot: b(true), BaseCurrency: s(base), QuoteCurrency: s("USDT"),
			Precision: lib.Precision{Amount: f(0.001)}, Limits: lib.Limits{Amount: lib.MinMax{Min: f(0.01)}}}
	}
	return map[string]lib.MarketInterface{"AAA/USDT": m("AAA"), "BBB/USDT": m("BBB")}, nil
}
func (c *unknownBuy) CreateOrder(symbol, typ, side string, amount float64, opts ...lib.CreateOrderOptions) (lib.Order, error) {
	if symbol == "AAA/USDT" && side == "buy" && !isStop(opts) {
		return lib.Order{}, c.err
	}
	return c.stub.CreateOrder(symbol, typ, side, amount, opts...)
}
func (c *unknownBuy) FetchOpenOrders(...lib.FetchOpenOrdersOptions) ([]lib.Order, error) {
	return nil, nil
}
func (c *unknownBuy) FetchClosedOrders(...lib.FetchClosedOrdersOptions) ([]lib.Order, error) {
	return nil, nil
}

// R1(b), ported from round 3: a buy of unknown outcome (429, connection
// refused) never wedges Positions, not even after a restart: its symbol comes
// back blocked (no entry) while the other symbol reconciles and flattens, and
// a new start works without editing the book.
func TestProbeR3UnknownBuyStillWedgesTheBook(t *testing.T) {
	for i, e := range []error{lib.RateLimitExceeded("429"), lib.NetworkError("Network error: dial tcp: connection refused")} {
		unknown := i == 1 // 429 is a refusal (R1a): nothing to block
		c := &unknownBuy{stub: newStub(now), err: e}
		o := Options{Symbols: []string{"AAA/USDT", "BBB/USDT"}, Env: exchange.Real, Book: filepath.Join(t.TempDir(), "book.json"), Now: func() time.Time { return now }}
		a, err := New(c, o)
		if err != nil {
			t.Fatal(err)
		}
		x := executor(a)
		if err := x.Enter(ctx, "BBB/USDT", now, exec.Plan{Entry: 100, Stop: 90, Qty: 1}); err != nil {
			t.Fatal(err)
		}
		_ = x.Enter(ctx, "AAA/USDT", now, exec.Plan{Entry: 100, Stop: 90, Target: 105, Qty: 1})
		r, err := New(c, o) // restart
		if err != nil {
			t.Fatal(err)
		}
		var ps []exchange.Position
		for i := 0; i < 3; i++ {
			if ps, err = r.Positions(ctx, exchange.Real); err != nil {
				t.Fatalf("%v: Positions after restart: %v (wedged)", e, err)
			}
		}
		if len(ps) != 1+btoi(unknown) {
			t.Fatalf("%v: positions %+v", e, ps)
		}
		for _, p := range ps {
			switch p.Symbol {
			case "AAA/USDT":
				if p.Qty != 0 || !strings.Contains(p.Blocked, "unknown fate") {
					t.Fatalf("%v: AAA %+v; want blocked with nothing held", e, p)
				}
			case "BBB/USDT":
				if p.Qty != 1 || p.StopClientID == "" || p.Blocked != "" {
					t.Fatalf("%v: BBB %+v; want held and protected", e, p)
				}
				if err := executor(r).Exit(ctx, p, now.Add(time.Hour), "flatten"); err != nil {
					t.Fatalf("%v: flatten BBB: %v", e, err)
				}
			}
		}
		if _, err := New(c, o); err != nil {
			t.Fatalf("%v: a new start: %v", e, err)
		}
		if ps, err := r.Positions(ctx, exchange.Real); err != nil || len(ps) != btoi(unknown) {
			t.Fatalf("%v: after flatten %+v %v", e, ps, err)
		}
	}
}

func btoi(v bool) int {
	if v {
		return 1
	}
	return 0
}

// slowCancel holds the first cancel until release, as a venue that answers
// late: the adapter's call keeps running (and its lock) after ctx gives up.
type slowCancel struct {
	*stub
	once              sync.Once
	inCancel, release chan struct{}
}

func (c *slowCancel) CancelOrder(id string, o ...lib.CancelOrderOptions) (lib.Order, error) {
	first := false
	c.once.Do(func() { first = true; close(c.inCancel) })
	if first {
		<-c.release
	}
	return c.stub.CancelOrder(id, o...)
}

// N2, ported from round 2: a stop request lands while an exit cancels the
// stop, and the cancel takes longer than the protective timeout. The exit's
// reconcile runs in a fresh protective context, so when the late cancel
// lands the stop is put back.
func TestProbeN2StopDuringStopCancelLeavesItNaked(t *testing.T) {
	old := exec.ProtectiveTimeout
	exec.ProtectiveTimeout = 200 * time.Millisecond
	defer func() { exec.ProtectiveTimeout = old }()
	c := &slowCancel{stub: newStub(now), inCancel: make(chan struct{}), release: make(chan struct{})}
	a := adapter(t, c)
	e := executor(a)
	if err := e.Enter(ctx, "AAA/USDT", now, exec.Plan{Entry: 100, Stop: 90, Target: 105, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	ps, _ := a.Positions(ctx, exchange.Real)
	cctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Exit(cctx, ps[0], now, "target") }()
	<-c.inCancel
	cancel()                                                          // the stop request
	time.AfterFunc(300*time.Millisecond, func() { close(c.release) }) // beyond the exit's protective timeout
	<-done
	time.Sleep(50 * time.Millisecond)
	ps, _ = a.Positions(context.Background(), exchange.Real)
	if len(ps) == 1 && (ps[0].StopClientID == "" || stops(c.stub) == 0) {
		t.Fatalf("position left without a stop: %+v, resting stops %d", ps, stops(c.stub))
	}
}
