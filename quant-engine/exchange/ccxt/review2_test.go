package ccxt

import (
	"context"
	"errors"
	"testing"
	"time"

	lib "github.com/rosaldo/ccxt/v5"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exec"
	"github.com/rosaldo/agency-agents/quant-engine/internal/risk"
)

func executor(a *Adapter) *exec.Executor {
	return &exec.Executor{Ex: a, Env: exchange.Real, Now: func() time.Time { return now },
		Risk: &risk.Breaker{DailyPct: 100, WeeklyPct: 100, Loc: time.UTC}}
}

func marketSells(c *stub) int {
	n := 0
	for _, o := range c.orders {
		if o.TriggerPrice == nil && deref(o.Side) == "sell" {
			n++
		}
	}
	return n
}

// stopRefused refuses the first `failures` stops (<0 = every stop).
type stopRefused struct {
	*stub
	failures, calls int
}

func (c *stopRefused) CreateOrder(symbol, typ, side string, amount float64, opts ...lib.CreateOrderOptions) (lib.Order, error) {
	if isStop(opts) {
		if c.calls++; c.failures < 0 || c.calls <= c.failures {
			return lib.Order{}, lib.InvalidOrder("stop refused")
		}
	}
	return c.stub.CreateOrder(symbol, typ, side, amount, opts...)
}

// isStop reports a CreateOrder carrying params.triggerPrice.
func isStop(opts []lib.CreateOrderOptions) bool {
	var o lib.CreateOrderOptionsStruct
	for _, fn := range opts {
		fn(&o)
	}
	return o.Params != nil && (*o.Params)["triggerPrice"] != nil
}

// Review 2 N1: a stop the exchange refused is never taken as placed on retry:
// the position is flattened and the engine halts.
func TestRefusedStopFlattensAndHalts(t *testing.T) {
	for _, failures := range []int{1, -1} {
		c := &stopRefused{stub: newStub(now), failures: failures}
		a := adapter(t, c)
		err := executor(a).Enter(ctx, "AAA/USDT", now, exec.Plan{Entry: 100, Stop: 90, Target: 105, Qty: 1})
		ps, _ := a.Positions(ctx, exchange.Real)
		if !errors.Is(err, exec.ErrHalt) || marketSells(c.stub) != 1 || len(ps) != 0 {
			t.Fatalf("refused %d: err=%v sells=%d positions=%+v", failures, err, marketSells(c.stub), ps)
		}
	}
}

// cancelInFlight blocks the cancel until released: the stop command arrives
// while the stop of an exit is being cancelled.
type cancelInFlight struct {
	*stub
	in, release chan struct{}
}

func (c *cancelInFlight) CancelOrder(id string, o ...lib.CancelOrderOptions) (lib.Order, error) {
	close(c.in)
	<-c.release
	return c.stub.CancelOrder(id, o...)
}

// Review 2 N2: a stop arriving during the stop cancel of an exit never leaves
// the position without a stop: the exit is carried through.
func TestStopDuringTheStopCancelStaysProtected(t *testing.T) {
	c := &cancelInFlight{stub: newStub(now), in: make(chan struct{}), release: make(chan struct{})}
	a := adapter(t, c)
	e := executor(a)
	if err := e.Enter(ctx, "AAA/USDT", now, exec.Plan{Entry: 100, Stop: 90, Target: 105, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	ps, _ := a.Positions(ctx, exchange.Real)
	cctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Exit(cctx, ps[0], now, "target") }()
	<-c.in
	cancel()
	close(c.release)
	<-done
	time.Sleep(50 * time.Millisecond) // a call abandoned by do() would finish now
	ps, _ = a.Positions(ctx, exchange.Real)
	if len(ps) != 0 && (ps[0].StopClientID == "" || stops(c.stub) == 0) {
		t.Fatalf("position left without a stop: %+v, resting stops %d", ps, stops(c.stub))
	}
}

// buyRefused answers a market buy with err; it never reaches the exchange.
type buyRefused struct {
	*stub
	err error
}

func (c *buyRefused) CreateOrder(string, string, string, float64, ...lib.CreateOrderOptions) (lib.Order, error) {
	return lib.Order{}, c.err
}
func (c *buyRefused) FetchOpenOrders(...lib.FetchOpenOrdersOptions) ([]lib.Order, error) {
	return nil, nil
}
func (c *buyRefused) FetchClosedOrders(...lib.FetchClosedOrdersOptions) ([]lib.Order, error) {
	return nil, nil
}

// Review 2 N3: a buy the exchange refused outright (insufficient funds) is
// rejected, not unknown: positions, reconcile and flatten keep working, even
// after a restart. A network error stays unknown (fail closed).
func TestRefusedBuyDoesNotWedgeTheBook(t *testing.T) {
	c := &buyRefused{stub: newStub(now), err: lib.InsufficientFunds("account has insufficient balance")}
	a := adapter(t, c)
	err := executor(a).Enter(ctx, "AAA/USDT", now, exec.Plan{Entry: 100, Stop: 90, Target: 105, Qty: 1})
	if err == nil || errors.Is(err, exec.ErrHalt) {
		t.Fatalf("refused buy: err = %v, want a plain rejection", err)
	}
	b, err := New(c, Options{Symbols: []string{"AAA/USDT"}, Env: exchange.Real, Book: a.opt.Book, Now: a.opt.Now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Positions(ctx, exchange.Real); err != nil {
		t.Fatalf("after restart Positions: %v", err)
	}
	c.err = lib.RequestTimeout("no answer")
	st, err := b.PlaceOrder(ctx, exchange.Real, exchange.OrderRequest{ClientID: "t", Symbol: "AAA/USDT", Side: exchange.Buy, Qty: 1, Type: exchange.Market})
	if err == nil || st.Status != exchange.Open {
		t.Fatalf("timeout: %+v %v, want an unknown (open) order", st, err)
	}
}
