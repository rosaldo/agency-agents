package ccxt

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	lib "github.com/rosaldo/ccxt/v5"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exec"
)

// swapStub lists the market types FR-P1 must tell apart.
type swapStub struct{ *stub }

func (c *swapStub) LoadMarkets(...any) (map[string]lib.MarketInterface, error) {
	m := func(sym, typ string, swap, linear, inverse bool, contract float64) lib.MarketInterface {
		return lib.MarketInterface{Symbol: s(sym), Type: s(typ), Spot: b(typ == "spot"), Swap: b(swap), Future: b(typ == "future"),
			Option: b(typ == "option"), Linear: b(linear), Inverse: b(inverse), ContractSize: f(contract), Settle: s("USDC"),
			BaseCurrency: s("AAA"), QuoteCurrency: s("USDC"), Precision: lib.Precision{Amount: f(0.001)}}
	}
	return map[string]lib.MarketInterface{
		"AAA/USDC:USDC":        m("AAA/USDC:USDC", "swap", true, true, false, 1),
		"AAA/USD:AAA":          m("AAA/USD:AAA", "swap", true, false, true, 1),
		"AAA/USDC:USDC-261231": m("AAA/USDC:USDC-261231", "future", false, true, false, 1),
		"AAA/USDC:USDC-C":      m("AAA/USDC:USDC-C", "option", false, true, false, 1),
		"AAA/USDC":             m("AAA/USDC", "spot", false, false, false, 0),
		"MRG/USDC":             m("MRG/USDC", "margin", false, false, false, 0),
		"BIG/USDC:USDC":        m("BIG/USDC:USDC", "swap", true, true, false, 0.01),
	}, nil
}

func newSwapStub() *swapStub {
	c := &swapStub{newStub(now)}
	c.balances = lib.Balances{Free: map[string]*float64{"USDC": f(1000)}, Total: map[string]*float64{"USDC": f(1000)}}
	return c
}

func swapAdapter(t *testing.T, c Client) *Adapter {
	t.Helper()
	a, err := New(c, Options{Symbols: []string{"AAA/USDC:USDC"}, Market: "swap", Env: exchange.Real,
		Book: filepath.Join(t.TempDir(), "book.json"), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// FR-P1: spot and linear swap are traded; future, option, inverse swap,
// margin and a market of the other type than the yml's are refused at start.
func TestOnlySpotAndLinearSwapAreTraded(t *testing.T) {
	c := &swapStub{newStub(now)}
	for sym, market := range map[string]string{"AAA/USDC:USDC": "swap", "AAA/USDC": "spot"} {
		if _, err := New(c, Options{Symbols: []string{sym}, Market: market, Env: exchange.Real}); err != nil {
			t.Errorf("%s (%s) refused: %v", sym, market, err)
		}
	}
	for sym, want := range map[string]string{
		"AAA/USD:AAA": "inverse", "AAA/USDC:USDC-261231": "future", "AAA/USDC:USDC-C": "option",
		"MRG/USDC": "margin", "BIG/USDC:USDC": "contract size", "AAA/USDC": "spot market",
	} {
		_, err := New(c, Options{Symbols: []string{sym}, Market: "swap", Env: exchange.Real})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want it refused naming %q", sym, err, want)
		}
	}
	if _, err := New(c, Options{Symbols: []string{"AAA/USDC:USDC"}, Market: "spot", Env: exchange.Real}); err == nil {
		t.Error("swap market accepted on market: spot")
	}
}

// FR-P2: a short opens with a sell, its stop is a reduce-only BUY trigger and
// the exit a reduce-only buy; every market order carries a reference price.
func TestSwapShortStopAndExitAreReduceOnly(t *testing.T) {
	c := newSwapStub()
	a := swapAdapter(t, c)
	e := executor(a)
	if err := e.Enter(ctx, "AAA/USDC:USDC", now, exec.Plan{Side: exchange.Short, Entry: 100, Stop: 110, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	ps, err := a.Positions(ctx, exchange.Real)
	if err != nil || len(ps) != 1 || ps[0].Side != exchange.Short || ps[0].Qty != 2 || ps[0].StopPrice != 110 {
		t.Fatalf("positions %+v %v; want a short of 2 with its stop at 110", ps, err)
	}
	if err := e.Exit(ctx, ps[0], now.Add(time.Hour), "target"); err != nil {
		t.Fatal(err)
	}
	want := []struct {
		side   string
		reduce bool
		stop   bool
	}{{"sell", false, false}, {"buy", true, true}, {"buy", true, false}}
	if len(c.params) != len(want) {
		t.Fatalf("orders sent %d, want %d", len(c.params), len(want))
	}
	for i, w := range want {
		p := c.params[i]
		_, stop := p["triggerPrice"]
		if (p["reduceOnly"] == true) != w.reduce || stop != w.stop || c.prices[i] <= 0 {
			t.Errorf("order %d: params %v price %v, want reduceOnly %v stop %v and a price", i, p, c.prices[i], w.reduce, w.stop)
		}
	}
	sides := []string{}
	for _, o := range c.orders {
		sides = append(sides, deref(o.Side))
	}
	slices.Sort(sides)
	if strings.Join(sides, ",") != "buy,buy,sell" {
		t.Errorf("sides %v", sides)
	}
	if ps, _ := a.Positions(ctx, exchange.Real); len(ps) != 0 {
		t.Errorf("short not closed: %+v", ps)
	}
}

// An entry against the engine's open position and a reduce-only order with
// nothing to reduce never leave (inversion through flat, FR-R4).
func TestNoInversionAndNoNakedReduceOnly(t *testing.T) {
	c := newSwapStub()
	a := swapAdapter(t, c)
	if _, err := a.PlaceOrder(ctx, exchange.Real, exchange.OrderRequest{ClientID: "r", Symbol: "AAA/USDC:USDC", Side: exchange.Buy, Qty: 1, Type: exchange.Market, ReduceOnly: true}); err == nil {
		t.Error("reduce-only with no position sent")
	}
	if _, err := a.PlaceOrder(ctx, exchange.Real, exchange.OrderRequest{ClientID: "e", Symbol: "AAA/USDC:USDC", Side: exchange.Buy, Qty: 1, Type: exchange.Market}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.PlaceOrder(ctx, exchange.Real, exchange.OrderRequest{ClientID: "x", Symbol: "AAA/USDC:USDC", Side: exchange.Sell, Qty: 2, Type: exchange.Market}); err == nil {
		t.Error("sell entry against an open long sent")
	}
	if len(c.params) != 1 {
		t.Fatalf("orders sent %d, want only the entry", len(c.params))
	}
}

// NFR-0: no order the adapter sends carries a builder, broker or referral
// field — only the client order ID, reduceOnly and the stop trigger.
func TestOrdersCarryNoBuilderOrReferralField(t *testing.T) {
	c := newSwapStub()
	a := swapAdapter(t, c)
	e := executor(a)
	_ = e.Enter(ctx, "AAA/USDC:USDC", now, exec.Plan{Side: exchange.Long, Entry: 100, Stop: 90, Qty: 1})
	ps, _ := a.Positions(ctx, exchange.Real)
	if len(ps) != 1 {
		t.Fatalf("no position to exit: %+v", ps)
	}
	_ = e.Exit(ctx, ps[0], now.Add(time.Hour), "exit")
	allowed := map[string]bool{"clientOrderId": true, "reduceOnly": true, "triggerPrice": true}
	if len(c.params) < 3 {
		t.Fatalf("orders sent %d", len(c.params))
	}
	for _, p := range c.params {
		for k := range p {
			if !allowed[k] {
				t.Errorf("order param %q sent", k)
			}
		}
	}
}

// FR-P3: margin mode and leverage are both sent; the position read back is
// the answer, and a position with other settings blocks entries.
func TestConfigureMarketReadsBackAndBlocksADivergence(t *testing.T) {
	c := newSwapStub()
	a := swapAdapter(t, c)
	want := exchange.MarginSettings{Leverage: 2, MarginMode: "isolated"}
	got, err := a.ConfigureMarket(ctx, exchange.Real, "AAA/USDC:USDC", want)
	if err != nil || got != want || strings.Join(c.settings, ",") != "margin isolated,leverage 2" {
		t.Fatalf("configure: %+v %v, calls %v", got, err, c.settings)
	}
	if _, err := a.PlaceOrder(ctx, exchange.Real, exchange.OrderRequest{ClientID: "e", Symbol: "AAA/USDC:USDC", Side: exchange.Buy, Qty: 1, Type: exchange.Market}); err != nil {
		t.Fatal(err)
	}
	c.positions = []lib.Position{{Symbol: s("AAA/USDC:USDC"), Side: s("long"), Contracts: f(1), Leverage: f(5), MarginMode: s("cross"), LiquidationPrice: f(80)}}
	ps, err := a.Positions(ctx, exchange.Real)
	if err != nil || len(ps) != 1 || ps[0].Blocked == "" || ps[0].LiquidationPrice != 80 || ps[0].Leverage != 5 {
		t.Fatalf("divergent position not blocked: %+v %v", ps, err)
	}
	c.positions[0].Leverage, c.positions[0].MarginMode = f(2), s("isolated")
	if ps, _ := a.Positions(ctx, exchange.Real); ps[0].Blocked != "" || ps[0].AccountQty != 1 {
		t.Fatalf("matching position blocked: %+v", ps)
	}
	if got, _ := a.ConfigureMarket(ctx, exchange.Real, "AAA/USDC:USDC", exchange.MarginSettings{Leverage: 3, MarginMode: "isolated"}); got.Leverage != 2 {
		t.Fatalf("read back %+v, want the position's leverage 2", got)
	}
}

// Swap balance is the exchange's free margin and account value in the
// settle currency, not cash plus holdings.
func TestSwapBalanceIsMarginAndAccountValue(t *testing.T) {
	c := &swapStub{newStub(now)}
	c.balances = lib.Balances{Free: map[string]*float64{"USDC": f(700)}, Total: map[string]*float64{"USDC": f(1020)}}
	b, err := swapAdapter(t, c).Balance(ctx, exchange.Real)
	if err != nil || b.Cash != 700 || b.Equity != 1020 {
		t.Fatalf("balance %+v %v, want cash 700 equity 1020", b, err)
	}
	c.balances = lib.Balances{}
	if _, err := swapAdapter(t, c).Balance(ctx, exchange.Real); err == nil {
		t.Fatal("balance without the settle currency accepted")
	}
}

// The venue shapes client order IDs (Hyperliquid: 0x + 128-bit hex, V9).
func TestVenueFormatsClientIDs(t *testing.T) {
	a, err := New(&swapStub{newStub(now)}, Options{Symbols: []string{"AAA/USDC:USDC"}, Market: "swap", Env: exchange.Real,
		Venue: Venue{ClientID: func(d [16]byte) string { return "0xid" }}})
	if err != nil {
		t.Fatal(err)
	}
	if id := (&exec.Executor{Ex: a}).ID("AAA/USDC:USDC", now, "entry"); id != "0xid" {
		t.Fatalf("client ID %q", id)
	}
	if !errors.Is(errors.Join(exchange.ErrRefused), exchange.ErrRefused) {
		t.Fatal("unreachable")
	}
}
