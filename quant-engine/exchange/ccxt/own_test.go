package ccxt

// FR-P8 / FR-P6: the adapter manages only the engine's own orders and
// quantity. Netting mode = swap (one position per symbol on the account);
// non-netting = spot and a hedged account. No test relies on a dedicated
// account: the user's quantity sits next to the engine's.

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	lib "github.com/rosaldo/ccxt/v5"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exec"
	"github.com/rosaldo/agency-agents/quant-engine/internal/ownbook"
)

const perp = "AAA/USDC:USDC"

// enteredSwap is a long of 2 on perp with its stop at 90.
func enteredSwap(t *testing.T) (*swapStub, *Adapter, *exec.Executor) {
	t.Helper()
	c := newSwapStub()
	a := swapAdapter(t, c)
	e := executor(a)
	if err := e.Enter(ctx, perp, now, exec.Plan{Side: exchange.Long, Entry: 100, Stop: 90, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	return c, a, e
}

func account(side string, contracts float64) []lib.Position {
	return []lib.Position{{Symbol: s(perp), Side: s(side), Contracts: f(contracts)}}
}

func onePosition(t *testing.T, a *Adapter) exchange.Position {
	t.Helper()
	ps, err := a.Positions(ctx, exchange.Real)
	if err != nil || len(ps) != 1 {
		t.Fatalf("positions %+v %v, want one", ps, err)
	}
	return ps[0]
}

// later moves the adapter's clock past cutConfirm: the next reading can
// confirm a drop of own_qty the previous one only held (B1).
func later(a *Adapter) {
	t := a.opt.Now().Add(cutConfirm)
	a.opt.Now = func() time.Time { return t }
}

func restingStops(c *stub) []lib.Order {
	var out []lib.Order
	for _, o := range c.orders {
		if o.TriggerPrice != nil && deref(o.Status) == "open" {
			out = append(out, o)
		}
	}
	return out
}

// Netting, the user added to the engine's long: the engine keeps managing its
// own 2 only (stop and exit sized to it, even from a stale position), journals
// foreign_qty_detected through Blocked, and takes no new entry.
func TestNettingForeignQtyKeepsTheOwnOnly(t *testing.T) {
	c, a, e := enteredSwap(t)
	c.positions = account("long", 5)
	p := onePosition(t, a)
	if p.Qty != 2 || p.AccountQty != 5 || p.StopQty != 2 || !strings.HasPrefix(p.Blocked, ownbook.ForeignQty) {
		t.Fatalf("position %+v; want own 2, account 5, stop for 2, blocked foreign_qty_detected", p)
	}
	if !a.Capabilities().Netting {
		t.Fatal("a swap adapter must declare netting")
	}
	stale := p
	stale.Qty = 5 // a position read before: never sells the user's 3
	if err := e.Exit(ctx, stale, now.Add(time.Hour), "target"); err != nil {
		t.Fatal(err)
	}
	if got := c.sent[len(c.sent)-1]; got != 2 {
		t.Fatalf("exit sent %g, want the own 2", got)
	}
}

// Netting, the user reduced: own_qty only goes down, to the account's, and
// the stop is resized to it (new first, the larger one cancelled after).
func TestNettingAccountBelowTheOwnShrinksItAndResizesTheStop(t *testing.T) {
	c, a, e := enteredSwap(t)
	c.positions = account("long", 1.5)
	if p := onePosition(t, a); p.Qty != 2 || p.StopQty != 2 || !strings.HasPrefix(p.Blocked, "unconfirmed") {
		t.Fatalf("first reading %+v; want own 2 kept with its stop, entries blocked", p)
	}
	later(a)
	p := onePosition(t, a)
	if p.Qty != 1.5 || p.StopQty != 2 || p.Blocked != "" {
		t.Fatalf("position %+v; want own 1.5 with the stop still for 2, not blocked", p)
	}
	if err := e.Reconcile(ctx, func(exchange.Position) float64 { return 90 }); err != nil {
		t.Fatal(err)
	}
	rs := restingStops(c.stub)
	if len(rs) != 1 || val(rs[0].Amount) != 1.5 || val(rs[0].TriggerPrice) != 90 {
		t.Fatalf("resting stops %+v; want one for 1.5 at 90", rs)
	}
	if p := onePosition(t, a); p.StopQty != 1.5 || p.StopClientID != deref(rs[0].ClientOrderId) {
		t.Fatalf("position %+v; want the resized stop", p)
	}
}

// Netting, the account is flat (liquidated or closed outside): the engine
// forgets its position and cancels its own stop; nothing is blocked.
func TestNettingFlatAccountForgetsThePositionAndItsStop(t *testing.T) {
	c, a, _ := enteredSwap(t)
	c.positions = []lib.Position{}
	if p := onePosition(t, a); p.Qty != 2 || len(restingStops(c.stub)) != 1 {
		t.Fatalf("first reading %+v; want own 2 kept with its stop", p)
	}
	later(a)
	ps, err := a.Positions(ctx, exchange.Real)
	if err != nil || len(ps) != 0 {
		t.Fatalf("positions %+v %v; want none", ps, err)
	}
	if n := len(restingStops(c.stub)); n != 0 {
		t.Fatalf("%d own stop(s) left resting without a position", n)
	}
}

// Netting, the account is on the other side (the user reversed): own_qty
// drops to 0, the engine's own stop is cancelled, the user's short is never
// touched and the symbol is refused.
func TestNettingOppositeSideIsLeftUntouchedAndRefused(t *testing.T) {
	c, a, _ := enteredSwap(t)
	c.positions = account("short", 3)
	sent := len(c.sent)
	if p := onePosition(t, a); p.Qty != 2 || !strings.Contains(p.Blocked, "not the engine's") || len(restingStops(c.stub)) != 1 {
		t.Fatalf("first reading %+v; want own 2 kept with its stop and the symbol refused", p)
	}
	later(a)
	p := onePosition(t, a)
	if p.Qty != 0 || !strings.Contains(p.Blocked, "not the engine's") {
		t.Fatalf("position %+v; want nothing own and the symbol refused", p)
	}
	if len(c.sent) != sent || len(restingStops(c.stub)) != 0 {
		t.Fatalf("orders sent %d->%d, resting stops %d; want none sent and the own stop cancelled", sent, len(c.sent), len(restingStops(c.stub)))
	}
	if _, err := executor(a).Ex.PlaceOrder(ctx, exchange.Real, exchange.OrderRequest{ClientID: "x", Symbol: perp, Side: exchange.Buy,
		Qty: 1, Type: exchange.Market, ReduceOnly: true}); err == nil {
		t.Fatal("a reduce-only order against the user's short left")
	}
}

// A reading that does not show the engine's fresh fill yet
// (empty, or short of it) cuts nothing: own and its stop stay, entries wait.
// Readings close together never confirm; a later one that agrees with own
// clears the wait.
func TestLateAccountReadingKeepsTheOwnAndItsStop(t *testing.T) {
	for _, lag := range [][]lib.Position{{}, account("long", 1)} {
		c, a, _ := enteredSwap(t)
		c.positions = lag
		for range 2 {
			p := onePosition(t, a)
			rs := restingStops(c.stub)
			if p.Qty != 2 || p.StopQty != 2 || !strings.HasPrefix(p.Blocked, "unconfirmed") || len(rs) != 1 || val(rs[0].Amount) != 2 {
				t.Fatalf("lag %v: position %+v, stops %+v; want own 2 under its stop for 2, entries blocked", lag, p, rs)
			}
		}
		later(a)
		c.positions = account("long", 2)
		p := onePosition(t, a)
		if p.Qty != 2 || p.StopQty != 2 || p.Blocked != "" || len(restingStops(c.stub)) != 1 {
			t.Fatalf("lag %v: position %+v; want own 2 under its stop, unblocked", lag, p)
		}
		later(a) // the wait was cleared: a new lag starts it again
		c.positions = lag
		if p := onePosition(t, a); p.Qty != 2 {
			t.Fatalf("lag %v: a fresh lag cut own at once: %+v", lag, p)
		}
	}
}

// A later reading with other numbers (own, kept) than the pending
// one confirms nothing: it starts the wait again, own and its stop stay.
func TestChangedReadingRestartsTheWait(t *testing.T) {
	c, a, _ := enteredSwap(t)
	c.positions = account("long", 1)
	onePosition(t, a)
	later(a)
	c.positions = []lib.Position{}
	p := onePosition(t, a)
	rs := restingStops(c.stub)
	if p.Qty != 2 || p.StopQty != 2 || !strings.HasPrefix(p.Blocked, "unconfirmed") || len(rs) != 1 || val(rs[0].Amount) != 2 {
		t.Fatalf("position %+v, stops %+v; want own 2 under its stop for 2, entries blocked", p, rs)
	}
}

// An unreadable account fails closed: every symbol blocked, own
// and its stop untouched.
func TestUnreadableAccountBlocksAndKeepsTheStop(t *testing.T) {
	c, a, _ := enteredSwap(t)
	c.posErr = lib.NetworkError("down")
	p := onePosition(t, a)
	if p.Qty != 2 || p.StopQty != 2 || !strings.Contains(p.Blocked, "unreadable") || len(restingStops(c.stub)) != 1 {
		t.Fatalf("position %+v; want own 2 under its stop and the symbol blocked", p)
	}
}

// A leverage other than the yml's blocks only a symbol the engine
// holds; the user's position on a hedged account does not.
func TestLeverageMismatchBlocksOnlyAnOwnHolding(t *testing.T) {
	c := newSwapStub()
	a := swapAdapter(t, c)
	a.want = exchange.MarginSettings{Leverage: 3, MarginMode: "isolated"}
	c.positions = []lib.Position{{Symbol: s(perp), Side: s("long"), Contracts: f(5), Hedged: b(true), Leverage: f(10), MarginMode: s("cross")}}
	if ps, err := a.Positions(ctx, exchange.Real); err != nil || len(ps) != 0 {
		t.Fatalf("positions %+v %v; the user's leverage must not block a symbol the engine does not hold", ps, err)
	}
	c2, a2, _ := enteredSwap(t)
	a2.want = a.want
	c2.positions = []lib.Position{{Symbol: s(perp), Side: s("long"), Contracts: f(2), Leverage: f(10), MarginMode: s("cross")}}
	if p := onePosition(t, a2); !strings.Contains(p.Blocked, "leverage 10") {
		t.Fatalf("position %+v; want the own holding blocked on leverage", p)
	}
}

// Netting, nothing own and the account holds a position: entries blocked.
func TestNettingForeignWhileFlatBlocksTheSymbol(t *testing.T) {
	c := newSwapStub()
	a := swapAdapter(t, c)
	c.positions = account("long", 1)
	if p := onePosition(t, a); p.Qty != 0 || !strings.HasPrefix(p.Blocked, ownbook.ForeignQty) {
		t.Fatalf("position %+v; want a blocked symbol with nothing own", p)
	}
}

// Non-netting (a hedged account): only the engine's own fills count; the
// other side and the same side's extra are not its business.
func TestHedgedAccountIsNotNetted(t *testing.T) {
	c, a, _ := enteredSwap(t)
	c.positions = []lib.Position{{Symbol: s(perp), Side: s("long"), Contracts: f(5), Hedged: b(true)},
		{Symbol: s(perp), Side: s("short"), Contracts: f(3), Hedged: b(true)}}
	if p := onePosition(t, a); p.Qty != 2 || p.Blocked != "" || p.AccountQty != 5 {
		t.Fatalf("position %+v; want own 2, unblocked", p)
	}
}

// Non-netting (spot): with the user's coins in the account and a stale
// position, exit and stop never sell more than the engine bought.
func TestSpotNeverSellsTheAccountsOtherCoins(t *testing.T) {
	c := newStub(now)
	c.balances.Free["AAA"], c.balances.Total["AAA"] = f(6), f(6) // 5 of them the user's
	a := adapter(t, c)
	e := executor(a)
	if err := e.Enter(ctx, "AAA/USDT", now, exec.Plan{Entry: 100, Stop: 90, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.PlaceStop(ctx, exchange.Real, "big-stop", "AAA/USDT", exchange.Sell, 6, 89); err != nil {
		t.Fatal(err)
	}
	if got := c.sent[len(c.sent)-1]; got != 1 {
		t.Fatalf("stop sent %g, want the own 1", got)
	}
	p := onePosition(t, a)
	p.Qty = 6
	if err := e.Exit(ctx, p, now.Add(time.Hour), "target"); err != nil {
		t.Fatal(err)
	}
	if got := c.sent[len(c.sent)-1]; got != 1 {
		t.Fatalf("exit sent %g, want the own 1", got)
	}
}

// own_qty and the client and exchange IDs live in the book file: a new
// adapter (restart, kill -9) reads them back.
func TestOwnQuantitySurvivesARestart(t *testing.T) {
	c, a, _ := enteredSwap(t)
	b, err := New(c, a.opt)
	if err != nil {
		t.Fatal(err)
	}
	p := onePosition(t, b)
	if p.Qty != 2 || p.StopPrice != 90 || b.b.Orders[p.StopClientID].ID == "" {
		t.Fatalf("after restart %+v; want own 2 with its stop and the stop's exchange ID", p)
	}
}

// bookCheck asserts, inside every CreateOrder, that the client ID is already
// in the book file: a crash during the send leaves a trace to resolve.
type bookCheck struct {
	*stub
	t    *testing.T
	book string
}

func (c *bookCheck) CreateOrder(symbol, typ, side string, amount float64, opts ...lib.CreateOrderOptions) (lib.Order, error) {
	var o lib.CreateOrderOptionsStruct
	for _, fn := range opts {
		fn(&o)
	}
	id := (*o.Params)["clientOrderId"].(string)
	raw, err := os.ReadFile(c.book)
	var bk book
	if err != nil || json.Unmarshal(raw, &bk) != nil || bk.Orders[id].State.ClientID != id {
		c.t.Errorf("order %s sent before it was in the book (%v)", id, err)
	}
	return c.stub.CreateOrder(symbol, typ, side, amount, opts...)
}

func TestClientIDIsPersistedBeforeTheOrderLeaves(t *testing.T) {
	c := &bookCheck{stub: newStub(now), t: t}
	a := adapter(t, c)
	c.book = a.opt.Book
	if err := executor(a).Enter(ctx, "AAA/USDT", now, exec.Plan{Entry: 100, Stop: 90, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	if len(c.sent) != 2 {
		t.Fatalf("orders %d, want entry and stop", len(c.sent))
	}
}

// countCancels counts the cancels reaching the exchange.
type countCancels struct {
	*stub
	n int
}

func (c *countCancels) CancelOrder(id string, opts ...lib.CancelOrderOptions) (lib.Order, error) {
	c.n++
	return c.stub.CancelOrder(id, opts...)
}

// An order the engine did not open is never cancelled: ErrNotOwn, and
// nothing reaches the exchange.
func TestCancelOutsideTheRegistryIsRefused(t *testing.T) {
	c := &countCancels{stub: newStub(now)}
	a := adapter(t, c)
	c.orders["user-1"] = lib.Order{Id: s("user-1"), Symbol: s("AAA/USDT"), Status: s("open"), ClientOrderId: s("user-1")}
	if _, err := a.CancelOrder(ctx, exchange.Real, "user-1"); !errors.Is(err, exchange.ErrNotOwn) || c.n != 0 {
		t.Fatalf("err %v, cancels sent %d; want ErrNotOwn and none", err, c.n)
	}
}

// lostStop answers a new stop with a timeout (landed or not) and can fail
// its cancel; lookups by client order ID read the stub's orders.
type lostStop struct {
	*stub
	land, failCancel bool
	lost             string // client ID of the lost stop
}

func (c *lostStop) CreateOrder(symbol, typ, side string, amount float64, opts ...lib.CreateOrderOptions) (lib.Order, error) {
	if !isStop(opts) || c.lost != "" || len(c.sent) < 2 { // entry and first stop go through
		return c.stub.CreateOrder(symbol, typ, side, amount, opts...)
	}
	var o lib.CreateOrderOptionsStruct
	for _, fn := range opts {
		fn(&o)
	}
	c.lost = (*o.Params)["clientOrderId"].(string)
	if c.land {
		_, _ = c.stub.CreateOrder(symbol, typ, side, amount, opts...)
	}
	return lib.Order{}, lib.RequestTimeout("no answer")
}

func (c *lostStop) lookup(status string) []lib.Order {
	var out []lib.Order
	for _, o := range c.orders {
		if deref(o.Status) == status {
			out = append(out, o)
		}
	}
	return out
}
func (c *lostStop) FetchOpenOrders(...lib.FetchOpenOrdersOptions) ([]lib.Order, error) {
	return c.lookup("open"), nil
}
func (c *lostStop) FetchClosedOrders(...lib.FetchClosedOrdersOptions) ([]lib.Order, error) {
	return c.lookup("closed"), nil
}
func (c *lostStop) CancelOrder(id string, opts ...lib.CancelOrderOptions) (lib.Order, error) {
	if c.failCancel {
		return lib.Order{}, lib.NetworkError("connection reset")
	}
	o, err := c.stub.CancelOrder(id, opts...)
	if err != nil {
		return o, lib.OrderNotFound("unknown order")
	}
	return o, nil
}

// A trail stop of unknown fate whose cancel fails is resolved by
// a lookup by client order ID before it can become stale: found resting, it
// is stale (retried); not on the exchange, it is not, and no block follows.
func TestUnknownStopIsLookedUpBeforeItTurnsStale(t *testing.T) {
	for _, landed := range []bool{true, false} {
		c := &lostStop{stub: newStub(now), land: landed, failCancel: landed}
		a := adapter(t, c)
		e := executor(a)
		if err := e.Enter(ctx, "AAA/USDT", now, exec.Plan{Entry: 100, Stop: 90, Qty: 1}); err != nil {
			t.Fatal(err)
		}
		p := onePosition(t, a)
		stale, err := e.MoveStop(ctx, p, 95, now.Add(time.Hour))
		if err == nil {
			t.Fatalf("landed=%v: the lost stop must surface", landed)
		}
		want := ""
		if landed {
			want = c.lost
		}
		if stale != want {
			t.Fatalf("landed=%v: stale %q, want %q", landed, stale, want)
		}
		c.failCancel = false
		q := onePosition(t, a)
		if q.Blocked != "" || q.StopClientID != p.StopClientID {
			t.Fatalf("landed=%v: position %+v; want the old stop, not blocked", landed, q)
		}
		if n := len(restingStops(c.stub)); n != 1 { // a landed one is swept: it protects nothing
			t.Fatalf("landed=%v: %d resting stops, want the old one only", landed, n)
		}
	}
}

// FR-P5: the venue's funding history, per symbol; spot has none.
func TestFundingIsReadFromTheVenue(t *testing.T) {
	c := newSwapStub()
	c.funding = []lib.FundingHistory{{Symbol: s(perp), Timestamp: func() *int64 { v := now.UnixMilli(); return &v }(), Amount: f(-0.4)},
		{Symbol: s("BBB/USDC:USDC"), Timestamp: func() *int64 { v := now.UnixMilli(); return &v }(), Amount: f(9)}}
	fs, err := swapAdapter(t, c).Funding(ctx, exchange.Real, perp, now.Add(-time.Hour))
	if err != nil || len(fs) != 1 || fs[0].Amount != -0.4 || !fs[0].Time.Equal(now) {
		t.Fatalf("funding %+v %v", fs, err)
	}
	if fs, err := adapter(t, newStub(now)).Funding(ctx, exchange.Real, "AAA/USDT", now); err != nil || fs != nil {
		t.Fatalf("spot funding %+v %v, want none", fs, err)
	}
}

// A stop of unknown fate the lookup did not find is taken as
// absent only when the cancel answers "not found"; a cancel that fails
// otherwise (network) leaves it unknown and stale, retried.
func TestUnknownStopIsAbsentOnlyOnNotFound(t *testing.T) {
	c := &lostStop{stub: newStub(now)}
	a := adapter(t, c)
	e := executor(a)
	if err := e.Enter(ctx, "AAA/USDT", now, exec.Plan{Entry: 100, Stop: 90, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	p := onePosition(t, a)
	c.failCancel = true
	stale, err := e.MoveStop(ctx, p, 95, now.Add(time.Hour))
	if err == nil || stale != c.lost {
		t.Fatalf("stale %q err %v; want the lost stop %q stale", stale, err, c.lost)
	}
	if r := a.b.Orders[c.lost]; r.Absent || r.State.Status == exchange.Canceled {
		t.Fatalf("lost stop %+v taken as absent after a network error", r.State)
	}
}
