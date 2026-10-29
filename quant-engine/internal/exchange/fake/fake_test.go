package fake

import (
	"context"
	"errors"
	"testing"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/market"
)

var ctx = context.Background()

func newFake(t *testing.T, o Options) *Fake {
	t.Helper()
	cs, err := market.ReadCSVFile("../../../testdata/stop_hit.csv")
	if err != nil {
		t.Fatal(err)
	}
	o.Candles = map[string][]market.Candle{"AAA": cs}
	if o.Cash == 0 {
		o.Cash = 10_000
	}
	return New(o)
}

func TestOrderFillPositionStopAndBalance(t *testing.T) {
	f := newFake(t, Options{Paper: true, QtyStep: 1, MinQty: 1})
	var _ exchange.Exchange = f

	st, err := f.PlaceOrder(ctx, exchange.Demo, exchange.OrderRequest{ClientID: "e1", Symbol: "AAA", Side: exchange.Buy, Qty: 10, Type: exchange.Market})
	if err != nil || st.Status != exchange.Filled || st.FillPrice != 100 {
		t.Fatalf("entry = %+v, %v", st, err)
	}
	if _, err := f.PlaceStop(ctx, exchange.Demo, "s1", "AAA", exchange.Sell, 10, 96); err != nil {
		t.Fatal(err)
	}
	ps, _ := f.Positions(ctx, exchange.Demo)
	if len(ps) != 1 || ps[0].Qty != 10 || ps[0].StopClientID != "s1" || ps[0].StopPrice != 96 {
		t.Fatalf("positions = %+v", ps)
	}
	if b, _ := f.Balance(ctx, exchange.Demo); b.Cash != 9_000 || b.Equity != 10_000 {
		t.Fatalf("balance after entry = %+v", b)
	}

	f.Advance() // low 94 crosses the 96 stop
	if ps, _ = f.Positions(ctx, exchange.Demo); len(ps) != 0 {
		t.Fatalf("stop did not close the position: %+v", ps)
	}
	if b, _ := f.Balance(ctx, exchange.Demo); b.Cash != 9_960 || b.Equity != 9_960 {
		t.Fatalf("balance after stop = %+v, want 9960", b)
	}
	if f.Unprotected() != 0 {
		t.Fatalf("a candle passed with an unprotected position")
	}
}

func TestAttachedStopIsPlacedWithTheEntry(t *testing.T) {
	f := newFake(t, Options{Paper: true, AttachedStop: true})
	st, err := f.PlaceOrder(ctx, exchange.Demo, exchange.OrderRequest{ClientID: "e1", Symbol: "AAA", Side: exchange.Buy, Qty: 1, Type: exchange.Market, AttachedStop: 96})
	if err != nil || st.Status != exchange.Filled {
		t.Fatal(st, err)
	}
	if ps, _ := f.Positions(ctx, exchange.Demo); len(ps) != 1 || ps[0].StopPrice != 96 {
		t.Fatalf("attached stop missing: %+v", ps)
	}
	f2 := newFake(t, Options{Paper: true})
	if _, err := f2.PlaceOrder(ctx, exchange.Demo, exchange.OrderRequest{ClientID: "e1", Symbol: "AAA", Side: exchange.Buy, Qty: 1, Type: exchange.Market, AttachedStop: 96}); err == nil {
		t.Fatal("an adapter without attached stops must refuse one")
	}
}

func TestRealRefusedInPaper(t *testing.T) {
	f := newFake(t, Options{Paper: true})
	if _, err := f.Balance(ctx, exchange.Real); !errors.Is(err, exchange.ErrRealInPaper) {
		t.Fatalf("real call in paper: %v", err)
	}
	if _, err := f.PlaceOrder(ctx, exchange.Real, exchange.OrderRequest{ClientID: "e", Symbol: "AAA", Side: exchange.Buy, Qty: 1, Type: exchange.Market}); !errors.Is(err, exchange.ErrRealInPaper) {
		t.Fatalf("real order in paper: %v", err)
	}
	if f.Calls("PlaceOrder") != 1 {
		t.Fatalf("calls are counted even when refused")
	}
}

func TestSameClientIDDoesNotDuplicate(t *testing.T) {
	f := newFake(t, Options{Paper: true})
	req := exchange.OrderRequest{ClientID: "e1", Symbol: "AAA", Side: exchange.Buy, Qty: 2, Type: exchange.Market}
	f.FailNext("PlaceOrder", errors.New("timeout"), true) // accepted, response lost
	if _, err := f.PlaceOrder(ctx, exchange.Demo, req); err == nil {
		t.Fatal("injected failure not returned")
	}
	if _, err := f.PlaceOrder(ctx, exchange.Demo, req); err != nil {
		t.Fatal(err)
	}
	if ps, _ := f.Positions(ctx, exchange.Demo); len(ps) != 1 || ps[0].Qty != 2 {
		t.Fatalf("resend duplicated the order: %+v", ps)
	}
}

func TestInjectedFailureBeforeApply(t *testing.T) {
	f := newFake(t, Options{Paper: true})
	f.FailNext("Positions", errors.New("down"), false)
	if _, err := f.Positions(ctx, exchange.Demo); err == nil {
		t.Fatal("injected failure not returned")
	}
	if _, err := f.Positions(ctx, exchange.Demo); err != nil {
		t.Fatal("failure must be one-shot")
	}
}

func TestUnprotectedPositionIsCountedWhenACandlePasses(t *testing.T) {
	f := newFake(t, Options{Paper: true})
	f.SetPosition("AAA", 1, 100)
	f.Advance()
	if f.Unprotected() != 1 {
		t.Fatalf("unprotected = %d, want 1", f.Unprotected())
	}
}
