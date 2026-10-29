package paper

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/market"
)

var ctx = context.Background()

func setup(t *testing.T) (*fake.Fake, *Sim) {
	t.Helper()
	cs, err := market.ReadCSVFile("../../../testdata/stop_hit.csv") // 100, then low 94, then 96
	if err != nil {
		t.Fatal(err)
	}
	data := fake.New(fake.Options{Paper: true, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs}})
	now := cs[0].Time
	return data, New(data, 10_000, func() time.Time { return now })
}

func TestPaperFillsAndStopsOnPublicData(t *testing.T) {
	data, s := setup(t)
	var _ exchange.Exchange = s
	st, err := s.PlaceOrder(ctx, exchange.Demo, exchange.OrderRequest{ClientID: "e", Symbol: "AAA", Side: exchange.Buy, Qty: 10, Type: exchange.Market})
	if err != nil || st.Status != exchange.Filled || st.FillPrice != 100 {
		t.Fatalf("entry = %+v %v", st, err)
	}
	if again, _ := s.PlaceOrder(ctx, exchange.Demo, exchange.OrderRequest{ClientID: "e", Symbol: "AAA", Side: exchange.Buy, Qty: 10, Type: exchange.Market}); again != st {
		t.Fatal("known client ID filled twice")
	}
	if _, err := s.PlaceStop(ctx, exchange.Demo, "s", "AAA", exchange.Sell, 10, 96); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Balance(ctx, exchange.Demo); b.Cash != 9_000 || b.Equity != 10_000 {
		t.Fatalf("balance = %+v", b)
	}
	data.Advance() // public candle with low 94 crosses the 96 stop
	if _, err := s.Candles(ctx, exchange.Demo, "AAA", time.Hour, 5); err != nil {
		t.Fatal(err)
	}
	if ps, _ := s.Positions(ctx, exchange.Demo); len(ps) != 0 {
		t.Fatalf("stop not filled: %+v", ps)
	}
	if b, _ := s.Balance(ctx, exchange.Demo); b.Cash != 9_960 {
		t.Fatalf("cash after stop = %v, want 9960", b.Cash)
	}
}

func TestQuoteBelowStopFills(t *testing.T) {
	data, s := setup(t)
	_, _ = s.PlaceOrder(ctx, exchange.Demo, exchange.OrderRequest{ClientID: "e", Symbol: "AAA", Side: exchange.Buy, Qty: 1, Type: exchange.Market})
	_, _ = s.PlaceStop(ctx, exchange.Demo, "s", "AAA", exchange.Sell, 1, 97)
	data.Advance()
	data.Advance() // last 96 < 97
	if ps, _ := s.Positions(ctx, exchange.Demo); len(ps) != 0 {
		t.Fatalf("stop below the last price not filled: %+v", ps)
	}
}

func TestPaperRefusesReal(t *testing.T) {
	_, s := setup(t)
	if _, err := s.Balance(ctx, exchange.Real); !errors.Is(err, exchange.ErrRealInPaper) {
		t.Fatalf("real: %v", err)
	}
}

func TestCancelAndExit(t *testing.T) {
	_, s := setup(t)
	_, _ = s.PlaceOrder(ctx, exchange.Demo, exchange.OrderRequest{ClientID: "e", Symbol: "AAA", Side: exchange.Buy, Qty: 1, Type: exchange.Market})
	_, _ = s.PlaceStop(ctx, exchange.Demo, "s", "AAA", exchange.Sell, 1, 90)
	if st, err := s.CancelOrder(ctx, exchange.Demo, "s"); err != nil || st.Status != exchange.Canceled {
		t.Fatalf("cancel: %+v %v", st, err)
	}
	if _, err := s.PlaceOrder(ctx, exchange.Demo, exchange.OrderRequest{ClientID: "x", Symbol: "AAA", Side: exchange.Sell, Qty: 1, Type: exchange.Market}); err != nil {
		t.Fatal(err)
	}
	if ps, _ := s.Positions(ctx, exchange.Demo); len(ps) != 0 {
		t.Fatalf("positions = %+v", ps)
	}
}
