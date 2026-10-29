package ccxt

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	lib "github.com/rosaldo/ccxt/v5"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exec"
)

const btc = "BTC/USDT:USDT"

// contractStub is a linear swap whose contract is 0.01 BTC (OKX's
// BTC-USDT-SWAP ctVal, the fork's contractSize, okx.go:2935-2939), stepped
// in whole contracts (a synthetic step). Like CCXT, orders and positions
// count contracts: a filled order reports filled = amount sent.
type contractStub struct {
	*stub
	amounts []float64 // amount of every CreateOrder, in order
}

func newContractStub() *contractStub {
	c := &contractStub{stub: newStub(now)}
	c.balances = lib.Balances{Free: map[string]*float64{"USDT": f(1000)}, Total: map[string]*float64{"USDT": f(1000)}}
	return c
}

func (c *contractStub) LoadMarkets(...any) (map[string]lib.MarketInterface, error) {
	return map[string]lib.MarketInterface{btc: {Symbol: s(btc), Type: s("swap"), Swap: b(true), Linear: b(true), Inverse: b(false),
		ContractSize: f(0.01), Settle: s("USDT"), BaseCurrency: s("BTC"), QuoteCurrency: s("USDT"),
		Precision: lib.Precision{Amount: f(1)}, Limits: lib.Limits{Amount: lib.MinMax{Min: f(1)}}}}, nil
}

func (c *contractStub) CreateOrder(symbol, typ, side string, amount float64, opts ...lib.CreateOrderOptions) (lib.Order, error) {
	c.amounts = append(c.amounts, amount)
	o, err := c.stub.CreateOrder(symbol, typ, side, amount, opts...)
	if err == nil && deref(o.Status) == "closed" {
		o.Filled = f(amount)
		c.orders[*o.Id] = o
	}
	return o, err
}

func contractAdapter(t *testing.T, c Client, v Venue) *Adapter {
	t.Helper()
	a, err := New(c, Options{Venue: v, Symbols: []string{btc}, Market: "swap", Env: exchange.Real,
		Book: filepath.Join(t.TempDir(), "book.json"), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("contract size 0.01 refused: %v", err)
	}
	return a
}

func near(x, y float64) bool { return math.Abs(x-y) < 1e-12 }

// A swap whose contract is not one base unit is traded (it was refused): the
// engine thinks in BTC, the wire carries contracts, and every quantity read
// back (lot, fill, exchange position) is BTC again, converted once.
func TestContractSizedSwapTradesInBase(t *testing.T) {
	c := newContractStub()
	a := contractAdapter(t, c, Venue{})
	if step, minQty := a.Lot(btc); !near(step, 0.01) || !near(minQty, 0.01) {
		t.Fatalf("lot %g/%g BTC, want 0.01/0.01 (1 contract of 0.01 BTC)", step, minQty)
	}
	if caps := a.Capabilities(); !near(caps.QtyStep, 0.01) || !near(caps.MinQty, 0.01) {
		t.Fatalf("capabilities step %g min %g, want them in BTC", caps.QtyStep, caps.MinQty)
	}
	e := executor(a)
	if err := e.Enter(ctx, btc, now, exec.Plan{Side: exchange.Long, Entry: 100, Stop: 90, Qty: 0.03}); err != nil {
		t.Fatal(err)
	}
	ps, err := a.Positions(ctx, exchange.Real)
	if err != nil || len(ps) != 1 || !near(ps[0].Qty, 0.03) || !near(ps[0].AccountQty, 0.03) || ps[0].StopPrice != 90 {
		t.Fatalf("positions %+v %v; want 0.03 BTC held and on the exchange (3 contracts), stop at 90", ps, err)
	}
	if err := e.Exit(ctx, ps[0], now.Add(time.Hour), "target"); err != nil {
		t.Fatal(err)
	}
	// entry, stop, exit (reduce-only): 3 contracts each
	if len(c.amounts) != 3 || c.amounts[0] != 3 || c.amounts[1] != 3 || c.amounts[2] != 3 || c.params[2]["reduceOnly"] != true {
		t.Fatalf("amounts %v params %v; want 3 contracts for entry, stop and reduce-only exit", c.amounts, c.params)
	}
	if ps, _ := a.Positions(ctx, exchange.Real); len(ps) != 0 {
		t.Fatalf("position not closed: %+v", ps)
	}
}

// The amount sent is floored to whole contract steps: never above the
// quantity risk sized, and float noise (0.03/0.01 = 2.9999999999999996)
// does not cost a contract. Below one step nothing is sent.
func TestContractRoundingNeverExceedsTheRiskQuantity(t *testing.T) {
	c := newContractStub()
	a := contractAdapter(t, c, Venue{})
	for i, tc := range []struct{ qty, want float64 }{{0.03, 3}, {0.0399, 3}, {0.07, 7}, {0.29, 29}} {
		c.amounts = nil
		id := string(rune('a' + i))
		st, err := a.PlaceOrder(ctx, exchange.Real, exchange.OrderRequest{ClientID: id, Symbol: btc, Side: exchange.Buy, Qty: tc.qty, Type: exchange.Market})
		if err != nil || len(c.amounts) != 1 || c.amounts[0] != tc.want || st.Qty > tc.qty || !near(st.Qty, tc.want*0.01) {
			t.Errorf("qty %g: sent %v, state %+v %v; want %g contracts, never above %g BTC", tc.qty, c.amounts, st, err, tc.want, tc.qty)
		}
	}
	for q := 0.01; q < 1; q += 0.0037 {
		if n := a.toContracts(btc, q); n*0.01 > q+1e-12 {
			t.Fatalf("qty %g BTC sent as %g contracts = %g BTC", q, n, n*0.01)
		}
	}
	c.amounts = nil
	if _, err := a.PlaceOrder(ctx, exchange.Real, exchange.OrderRequest{ClientID: "tiny", Symbol: btc, Side: exchange.Buy, Qty: 0.009, Type: exchange.Market}); err == nil || len(c.amounts) != 0 {
		t.Fatalf("0.009 BTC (under one contract) sent: %v %v", c.amounts, err)
	}
}

// A venue whose fork already speaks the base asset (coinbaseinternational's
// gateway route) is not converted again: the same market's lot, amount and
// exchange position stay as the fork gives them.
func TestNativeAmountsAreNotConvertedTwice(t *testing.T) {
	c := newContractStub()
	a := contractAdapter(t, c, Venue{NativeAmounts: true})
	if step, minQty := a.Lot(btc); step != 1 || minQty != 1 {
		t.Fatalf("lot %g/%g, want the fork's 1/1", step, minQty)
	}
	if err := executor(a).Enter(ctx, btc, now, exec.Plan{Side: exchange.Long, Entry: 100, Stop: 90, Qty: 3}); err != nil {
		t.Fatal(err)
	}
	c.positions = []lib.Position{{Symbol: s(btc), Side: s("long"), Contracts: f(3)}}
	ps, err := a.Positions(ctx, exchange.Real)
	if err != nil || len(ps) != 1 || ps[0].Qty != 3 || ps[0].AccountQty != 3 || c.amounts[0] != 3 {
		t.Fatalf("positions %+v %v, amounts %v; want 3 everywhere", ps, err, c.amounts)
	}
}
