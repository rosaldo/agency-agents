package ccxt

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	lib "github.com/rosaldo/ccxt/v5"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exec"
	"github.com/rosaldo/agency-agents/quant-engine/internal/risk"
)

func f(v float64) *float64 { return &v }
func s(v string) *string   { return &v }

// stub is an in-memory CCXT client: no network.
type stub struct {
	now        time.Time
	ohlcv      []lib.OHLCV
	orders     map[string]lib.Order // exchange id -> order
	nextStatus string               // status the next market order reports first
	params     []map[string]any
	delay      time.Duration
	balances   lib.Balances
}

func newStub(now time.Time) *stub {
	return &stub{now: now, orders: map[string]lib.Order{}, balances: lib.Balances{
		Free: map[string]*float64{"USDT": f(1000)}, Total: map[string]*float64{"USDT": f(1000)}}}
}

func (c *stub) LoadMarkets(...any) (map[string]lib.MarketInterface, error) {
	return map[string]lib.MarketInterface{"AAA/USDT": {Symbol: s("AAA/USDT"), BaseCurrency: s("AAA"), QuoteCurrency: s("USDT"),
		Precision: lib.Precision{Amount: f(0.001)}, Limits: lib.Limits{Amount: lib.MinMax{Min: f(0.01)}}}}, nil
}
func (c *stub) FetchOHLCV(string, ...lib.FetchOHLCVOptions) ([]lib.OHLCV, error) { return c.ohlcv, nil }
func (c *stub) FetchTicker(string, ...lib.FetchTickerOptions) (lib.Ticker, error) {
	time.Sleep(c.delay)
	return lib.Ticker{Bid: f(99), Ask: f(101), Last: f(100)}, nil
}
func (c *stub) FetchBalance(...any) (lib.Balances, error) { return c.balances, nil }
func (c *stub) FetchOpenOrders(...lib.FetchOpenOrdersOptions) ([]lib.Order, error) {
	return nil, errors.New("not supported")
}
func (c *stub) FetchClosedOrders(...lib.FetchClosedOrdersOptions) ([]lib.Order, error) {
	return nil, errors.New("not supported")
}

func (c *stub) add(symbol, typ, side string, amount float64, trigger *float64, params map[string]any) lib.Order {
	c.params = append(c.params, params)
	id := s("x" + string(rune('a'+len(c.orders))))
	status := "closed"
	if trigger != nil {
		status = "open"
	} else if c.nextStatus != "" {
		status, c.nextStatus = c.nextStatus, ""
	}
	o := lib.Order{Id: id, Symbol: s(symbol), Type: s(typ), Side: s(side), Amount: f(amount), Status: s(status), TriggerPrice: trigger}
	if status == "closed" {
		o.Average = f(100)
	}
	c.orders[*id] = o
	return o
}

func (c *stub) CreateOrder(symbol, typ, side string, amount float64, opts ...lib.CreateOrderOptions) (lib.Order, error) {
	var o lib.CreateOrderOptionsStruct
	for _, fn := range opts {
		fn(&o)
	}
	var p map[string]any
	if o.Params != nil {
		p = *o.Params
	}
	return c.add(symbol, typ, side, amount, nil, p), nil
}
func (c *stub) CreateStopMarketOrder(symbol, side string, amount, trigger float64, opts ...lib.CreateStopMarketOrderOptions) (lib.Order, error) {
	return c.add(symbol, "market", side, amount, &trigger, nil), nil
}
func (c *stub) CancelOrder(id string, _ ...lib.CancelOrderOptions) (lib.Order, error) {
	o, ok := c.orders[id]
	if !ok {
		return o, errors.New("unknown order")
	}
	o.Status = s("canceled")
	c.orders[id] = o
	return o, nil
}
func (c *stub) FetchOrder(id string, _ ...lib.FetchOrderOptions) (lib.Order, error) {
	o, ok := c.orders[id]
	if !ok {
		return o, errors.New("unknown order")
	}
	if *o.Status == "open" && o.TriggerPrice == nil { // a pending market order fills on the next look
		o.Status, o.Average = s("closed"), f(100)
		c.orders[id] = o
	}
	return o, nil
}

var (
	ctx = context.Background()
	now = time.Date(2026, 1, 1, 12, 30, 0, 0, time.UTC)
)

func adapter(t *testing.T, c Client) *Adapter {
	t.Helper()
	a, err := New(c, Options{Symbols: []string{"AAA/USDT"}, Env: exchange.Real, Paper: false, Book: filepath.Join(t.TempDir(), "book.json"), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestCandlesDropTheOpenCandle(t *testing.T) {
	c := newStub(now)
	for h := 9; h <= 12; h++ { // 12:00 is still open at 12:30
		ts := time.Date(2026, 1, 1, h, 0, 0, 0, time.UTC).UnixMilli()
		c.ohlcv = append(c.ohlcv, lib.OHLCV{Timestamp: ts, Open: 1, High: 2, Low: 0.5, Close: float64(h), Volume: 10})
	}
	cs, err := adapter(t, c).Candles(ctx, exchange.Real, "AAA/USDT", time.Hour, 10)
	if err != nil || len(cs) != 3 || cs[2].Close != 11 {
		t.Fatalf("candles = %+v, %v; want 9..11 closed only", cs, err)
	}
}

func TestTimeframe(t *testing.T) {
	for d, want := range map[time.Duration]string{time.Minute: "1m", 15 * time.Minute: "15m", 4 * time.Hour: "4h", 24 * time.Hour: "1d"} {
		if got, err := timeframe(d); err != nil || got != want {
			t.Errorf("%v -> %q %v, want %q", d, got, err, want)
		}
	}
	if _, err := timeframe(90 * time.Second); err == nil {
		t.Error("90s accepted")
	}
}

func TestOrderStopAndTriggeredStop(t *testing.T) {
	c := newStub(now)
	c.nextStatus = "open" // the market buy is reported pending first
	a := adapter(t, c)
	st, err := a.PlaceOrder(ctx, exchange.Real, exchange.OrderRequest{ClientID: "id-1", Symbol: "AAA/USDT", Side: exchange.Buy, Qty: 2, Type: exchange.Market})
	if err != nil || st.Status != exchange.Filled || st.FillPrice != 100 {
		t.Fatalf("entry = %+v, %v", st, err)
	}
	if c.params[0]["clientOrderId"] != "id-1" {
		t.Fatalf("client order ID not sent: %v", c.params[0])
	}
	again, _ := a.PlaceOrder(ctx, exchange.Real, exchange.OrderRequest{ClientID: "id-1", Symbol: "AAA/USDT", Side: exchange.Buy, Qty: 2, Type: exchange.Market})
	if len(c.params) != 1 || again.Status != exchange.Filled {
		t.Fatal("a known client ID was sent twice")
	}
	if _, err := a.PlaceStop(ctx, exchange.Real, "id-1s", "AAA/USDT", exchange.Sell, 2, 95); err != nil {
		t.Fatal(err)
	}
	ps, _ := a.Positions(ctx, exchange.Real)
	if len(ps) != 1 || ps[0].Qty != 2 || ps[0].StopClientID != "id-1s" || ps[0].StopPrice != 95 {
		t.Fatalf("positions = %+v", ps)
	}
	// a fresh adapter on the same book still knows the position (restart)
	b, _ := New(c, Options{Symbols: []string{"AAA/USDT"}, Env: exchange.Real, Book: a.opt.Book, Now: a.opt.Now})
	if ps, _ = b.Positions(ctx, exchange.Real); len(ps) != 1 {
		t.Fatalf("book lost on restart: %+v", ps)
	}
	for id, o := range c.orders { // the exchange fires the stop
		if o.TriggerPrice != nil {
			o.Status, o.Average = s("closed"), f(95)
			c.orders[id] = o
		}
	}
	if ps, _ = b.Positions(ctx, exchange.Real); len(ps) != 0 {
		t.Fatalf("a filled stop must close the position: %+v", ps)
	}
}

func TestCancelAndBalanceAndCapabilities(t *testing.T) {
	c := newStub(now)
	a := adapter(t, c)
	_, _ = a.PlaceOrder(ctx, exchange.Real, exchange.OrderRequest{ClientID: "e", Symbol: "AAA/USDT", Side: exchange.Buy, Qty: 1, Type: exchange.Market})
	_, _ = a.PlaceStop(ctx, exchange.Real, "st", "AAA/USDT", exchange.Sell, 1, 90)
	if st, err := a.CancelOrder(ctx, exchange.Real, "st"); err != nil || st.Status != exchange.Canceled {
		t.Fatalf("cancel = %+v %v", st, err)
	}
	if ps, _ := a.Positions(ctx, exchange.Real); len(ps) != 1 || ps[0].StopClientID != "" {
		t.Fatalf("cancelled stop still attached: %+v", ps)
	}
	if b, err := a.Balance(ctx, exchange.Real); err != nil || b.Cash != 1000 || b.Equity != 1100 {
		t.Fatalf("balance = %+v %v; want cash 1000, equity 1000 + 1 x 100", b, err)
	}
	if cp := a.Capabilities(); cp.AttachedStop || cp.QtyStep != 0.001 || cp.MinQty != 0.01 {
		t.Fatalf("capabilities = %+v", cp)
	}
}

func TestEnvironmentIsEnforced(t *testing.T) {
	a, err := New(newStub(now), Options{Symbols: []string{"AAA/USDT"}, Env: exchange.Demo, Paper: true, Book: filepath.Join(t.TempDir(), "b.json"), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Balance(ctx, exchange.Real); !errors.Is(err, exchange.ErrRealInPaper) {
		t.Fatalf("real in paper: %v", err)
	}
	if _, err := New(newStub(now), Options{Env: exchange.Demo, Paper: false}); err == nil {
		t.Fatal("live bound to the demo environment accepted")
	}
}

func TestCancelledContextReturnsAtOnce(t *testing.T) {
	c := newStub(now)
	c.delay = 5 * time.Second
	a := adapter(t, c)
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	if _, err := a.Quote(cctx, exchange.Real, "AAA/USDT"); err == nil || time.Since(t0) > time.Second {
		t.Fatalf("slow exchange held the call: %v after %v", err, time.Since(t0))
	}
}

func TestUnknownMarketRefused(t *testing.T) {
	if _, err := New(newStub(now), Options{Symbols: []string{"ZZZ/USDT"}, Env: exchange.Real}); err == nil {
		t.Fatal("unknown market accepted")
	}
}

// pendingStub keeps market orders open through the adapter's polling,
// refuses to cancel them ("already filled") and reports them filled after.
type pendingStub struct {
	*stub
	fillAfter int
	looks     int
}

func (c *pendingStub) FetchOrder(id string, _ ...lib.FetchOrderOptions) (lib.Order, error) {
	o := c.orders[id]
	if o.TriggerPrice == nil && deref(o.Status) == "open" {
		if c.looks++; c.looks > c.fillAfter {
			o.Status, o.Average, o.Filled = s("closed"), f(100), o.Amount
			c.orders[id] = o
		}
	}
	return o, nil
}

func (c *pendingStub) CancelOrder(id string, _ ...lib.CancelOrderOptions) (lib.Order, error) {
	if o := c.orders[id]; o.TriggerPrice == nil {
		return o, errors.New("order already filled")
	}
	return c.stub.CancelOrder(id)
}

func stops(c *stub) int {
	n := 0
	for _, o := range c.orders {
		if o.TriggerPrice != nil && deref(o.Status) == "open" {
			n++
		}
	}
	return n
}

// Review B2: an unconfirmed market buy never returns success, and a fill that
// lands later is protected by the next reconcile.
func TestUnconfirmedEntryHaltsAndLateFillGetsAStop(t *testing.T) {
	c := &pendingStub{stub: newStub(now), fillAfter: 100}
	c.nextStatus = "open"
	a := adapter(t, c)
	e := &exec.Executor{Ex: a, Env: exchange.Real, Now: func() time.Time { return now },
		Risk: &risk.Breaker{DailyPct: 100, WeeklyPct: 100, Loc: time.UTC}}
	err := e.Enter(ctx, "AAA/USDT", now, exec.Plan{Entry: 100, Stop: 90, Target: 105, Qty: 1})
	if !errors.Is(err, exec.ErrHalt) {
		t.Fatalf("unconfirmed entry: err = %v, want ErrHalt", err)
	}
	c.fillAfter = 0 // the exchange reports the fill now
	if err := e.Reconcile(ctx, func(exchange.Position) float64 { return 90 }); err != nil {
		t.Fatal(err)
	}
	ps, _ := a.Positions(ctx, exchange.Real)
	if len(ps) != 1 || ps[0].StopClientID == "" || stops(c.stub) != 1 {
		t.Fatalf("late fill not protected: positions %+v, resting stops %d", ps, stops(c.stub))
	}
}

// lostStub executes the order but loses the response, and can find orders by
// client order ID among recent orders.
type lostStub struct {
	*stub
	lose bool
}

func (c *lostStub) CreateOrder(symbol, typ, side string, amount float64, opts ...lib.CreateOrderOptions) (lib.Order, error) {
	o, _ := c.stub.CreateOrder(symbol, typ, side, amount, opts...)
	cid := c.params[len(c.params)-1]["clientOrderId"].(string)
	o.ClientOrderId = &cid
	o.Filled = o.Amount
	c.orders[*o.Id] = o
	if c.lose {
		c.lose = false
		return lib.Order{}, errors.New("timeout")
	}
	return o, nil
}

func (c *lostStub) FetchOpenOrders(...lib.FetchOpenOrdersOptions) ([]lib.Order, error) {
	return nil, nil
}
func (c *lostStub) FetchClosedOrders(...lib.FetchClosedOrdersOptions) ([]lib.Order, error) {
	var out []lib.Order
	for _, o := range c.orders {
		out = append(out, o)
	}
	return out, nil
}

// Review M4: the intent is in the book before sending; a retry resolves the
// order by its client order ID instead of sending it again.
func TestLostResponseIsResolvedNotResent(t *testing.T) {
	c := &lostStub{stub: newStub(now), lose: true}
	a := adapter(t, c)
	req := exchange.OrderRequest{ClientID: "id-9", Symbol: "AAA/USDT", Side: exchange.Buy, Qty: 2, Type: exchange.Market}
	if _, err := a.PlaceOrder(ctx, exchange.Real, req); err == nil {
		t.Fatal("lost response must surface")
	}
	b, _ := New(c, Options{Symbols: []string{"AAA/USDT"}, Env: exchange.Real, Book: a.opt.Book, Now: a.opt.Now}) // even after a restart
	st, err := b.PlaceOrder(ctx, exchange.Real, req)
	if err != nil || st.Status != exchange.Filled || len(c.params) != 1 {
		t.Fatalf("retry: %+v %v, orders sent %d (want 1)", st, err, len(c.params))
	}
}

// Review M5: the book holds the executed quantity net of a fee charged in the
// base asset, and that is what the stop sells.
func TestPositionIsTheNetFilledQuantity(t *testing.T) {
	c := &feeStub{stub: newStub(now)}
	a := adapter(t, c)
	st, err := a.PlaceOrder(ctx, exchange.Real, exchange.OrderRequest{ClientID: "e", Symbol: "AAA/USDT", Side: exchange.Buy, Qty: 2, Type: exchange.Market})
	if err != nil || st.Qty != 1.998 {
		t.Fatalf("fill = %+v %v, want qty 1.998 (2 minus 0.002 AAA fee)", st, err)
	}
	if ps, _ := a.Positions(ctx, exchange.Real); len(ps) != 1 || ps[0].Qty != 1.998 {
		t.Fatalf("positions = %+v", ps)
	}
}

type feeStub struct{ *stub }

func (c *feeStub) CreateOrder(symbol, typ, side string, amount float64, opts ...lib.CreateOrderOptions) (lib.Order, error) {
	o, _ := c.stub.CreateOrder(symbol, typ, side, amount, opts...)
	o.Filled, o.Fee = f(amount), lib.Fee{Cost: f(0.002), Currency: s("AAA")}
	c.orders[*o.Id] = o
	return o, nil
}

// Review minor 1: lot sizes are per market, not the largest of all.
func TestLotIsPerSymbol(t *testing.T) {
	c := &twoMarkets{stub: newStub(now)}
	a, err := New(c, Options{Symbols: []string{"AAA/USDT", "BBB/USDT"}, Env: exchange.Real, Book: filepath.Join(t.TempDir(), "b.json")})
	if err != nil {
		t.Fatal(err)
	}
	if step, min := a.Lot("AAA/USDT"); step != 0.001 || min != 0.01 {
		t.Fatalf("AAA lot = %v/%v", step, min)
	}
	if step, min := a.Lot("BBB/USDT"); step != 1 || min != 10 {
		t.Fatalf("BBB lot = %v/%v", step, min)
	}
}

type twoMarkets struct{ *stub }

func (c *twoMarkets) LoadMarkets(...any) (map[string]lib.MarketInterface, error) {
	ms, _ := c.stub.LoadMarkets()
	ms["BBB/USDT"] = lib.MarketInterface{Symbol: s("BBB/USDT"), BaseCurrency: s("BBB"), QuoteCurrency: s("USDT"),
		Precision: lib.Precision{Amount: f(1)}, Limits: lib.Limits{Amount: lib.MinMax{Min: f(10)}}}
	return ms, nil
}
