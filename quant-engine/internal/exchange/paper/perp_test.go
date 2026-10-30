package paper

import (
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/market"
)

// perp is a simulator on swap public data: candle 0 trades at 100, then the
// given candles; leverage 2, isolated.
func perp(t *testing.T, next ...market.Candle) (*fake.Fake, *Sim) {
	t.Helper()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cs := []market.Candle{{Time: t0, Open: 100, High: 100, Low: 100, Close: 100}}
	for i, c := range next {
		c.Time = t0.Add(time.Duration(i+1) * time.Hour)
		cs = append(cs, c)
	}
	data := fake.New(fake.Options{Paper: true, Shorts: true, Candles: map[string][]market.Candle{"AAA": cs}})
	s := New(data, 10_000, func() time.Time { return t0 })
	want := exchange.MarginSettings{Leverage: 2, MarginMode: "isolated"}
	if got, err := s.ConfigureMarket(ctx, exchange.Demo, "AAA", want); err != nil || got != want {
		t.Fatalf("configure = %+v %v", got, err)
	}
	return data, s
}

func order(t *testing.T, s *Sim, id string, side exchange.Side, qty float64) {
	t.Helper()
	if st, err := s.PlaceOrder(ctx, exchange.Demo, exchange.OrderRequest{ClientID: id, Symbol: "AAA", Side: side, Qty: qty, Type: exchange.Market}); err != nil || st.Status != exchange.Filled {
		t.Fatalf("order %s = %+v %v", id, st, err)
	}
}

// T12: a short locks notional/leverage of margin, reports leverage, margin
// mode and an estimated liquidation, and its buy stop fills above the price.
func TestPaperPerpShortWithLeverage(t *testing.T) {
	data, s := perp(t, market.Candle{Open: 102, High: 106, Low: 101, Close: 105})
	order(t, s, "e", exchange.Sell, 10)
	ps, _ := s.Positions(ctx, exchange.Demo)
	if len(ps) != 1 || ps[0].PSide() != exchange.Short || ps[0].Leverage != 2 || ps[0].MarginMode != "isolated" || ps[0].LiquidationPrice != 150 {
		t.Fatalf("position = %+v; want short, leverage 2 isolated, liquidation 150 (1/leverage)", ps)
	}
	if b, _ := s.Balance(ctx, exchange.Demo); b.Cash != 9_500 || b.Equity != 10_000 {
		t.Fatalf("balance = %+v; want 500 of margin locked, equity 10000", b)
	}
	if _, err := s.PlaceStop(ctx, exchange.Demo, "bad", "AAA", exchange.Sell, 10, 104); err == nil {
		t.Fatal("a sell stop accepted on a short")
	}
	if _, err := s.PlaceStop(ctx, exchange.Demo, "s", "AAA", exchange.Buy, 10, 104); err != nil {
		t.Fatal(err)
	}
	data.Advance() // high 106 crosses the 104 stop
	if _, err := s.Candles(ctx, exchange.Demo, "AAA", time.Hour, 5); err != nil {
		t.Fatal(err)
	}
	if ps, _ := s.Positions(ctx, exchange.Demo); len(ps) != 0 {
		t.Fatalf("short stop not filled: %+v", ps)
	}
	if st := s.orders["s"]; st.Status != exchange.Filled || st.FillPrice != 104 {
		t.Fatalf("stop = %+v", st)
	}
	if b, _ := s.Balance(ctx, exchange.Demo); b.Cash != 9_960 {
		t.Fatalf("cash = %v, want 9960 (lost 4 x 10 on the short)", b.Cash)
	}
}

// T12: a long's stop fills before its liquidation; a gap through the
// liquidation liquidates it instead, losing the margin, stop cancelled.
func TestPaperPerpLiquidation(t *testing.T) {
	for _, c := range []struct {
		name   string
		candle market.Candle
		cash   float64
		stop   exchange.Status
	}{
		{"stop first", market.Candle{Open: 90, High: 90, Low: 45, Close: 46}, 9_600, exchange.Filled},
		{"gap past the liquidation", market.Candle{Open: 48, High: 48, Low: 45, Close: 46}, 9_500, exchange.Canceled},
	} {
		t.Run(c.name, func(t *testing.T) {
			data, s := perp(t, c.candle)
			order(t, s, "e", exchange.Buy, 10)
			if ps, _ := s.Positions(ctx, exchange.Demo); len(ps) != 1 || ps[0].LiquidationPrice != 50 {
				t.Fatalf("position = %+v; want liquidation 50", ps)
			}
			if _, err := s.PlaceStop(ctx, exchange.Demo, "s", "AAA", exchange.Sell, 10, 60); err != nil {
				t.Fatal(err)
			}
			data.Advance()
			_, _ = s.Candles(ctx, exchange.Demo, "AAA", time.Hour, 5)
			if ps, _ := s.Positions(ctx, exchange.Demo); len(ps) != 0 {
				t.Fatalf("position left: %+v", ps)
			}
			if b, _ := s.Balance(ctx, exchange.Demo); b.Cash != c.cash || s.orders["s"].Status != c.stop {
				t.Fatalf("cash %v stop %s; want %v %s", b.Cash, s.orders["s"].Status, c.cash, c.stop)
			}
		})
	}
}

// A liquidation reached on the last price, with no stop at all.
func TestPaperPerpLiquidatesOnTheLastPrice(t *testing.T) {
	data, s := perp(t, market.Candle{Open: 140, High: 155, Low: 140, Close: 155})
	order(t, s, "e", exchange.Sell, 10)
	data.Advance()
	if ps, _ := s.Positions(ctx, exchange.Demo); len(ps) != 0 {
		t.Fatalf("short past its liquidation 150 still open: %+v", ps)
	}
	if b, _ := s.Balance(ctx, exchange.Demo); b.Equity != 9_500 {
		t.Fatalf("equity = %v, want 9500 (margin lost)", b.Equity)
	}
}

func TestPaperPerpReduceOnlyAndFundingNote(t *testing.T) {
	_, s := perp(t)
	if _, err := s.PlaceOrder(ctx, exchange.Demo, exchange.OrderRequest{ClientID: "r", Symbol: "AAA", Side: exchange.Sell, Qty: 1, Type: exchange.Market, ReduceOnly: true}); err == nil {
		t.Fatal("reduce-only opened a short")
	}
	order(t, s, "e", exchange.Buy, 10)
	order(t, s, "x", exchange.Sell, 4) // closes part of the long
	if ps, _ := s.Positions(ctx, exchange.Demo); len(ps) != 1 || ps[0].PSide() != exchange.Long || ps[0].Qty != 6 {
		t.Fatalf("positions = %+v", ps)
	}
	if b, _ := s.Balance(ctx, exchange.Demo); b.Cash != 9_700 || b.Equity != 10_000 {
		t.Fatalf("balance = %+v; want 4/10 of the 500 margin freed", b)
	}
	if !strings.Contains(s.FundingNote(), "funding not modelled") {
		t.Fatalf("funding note = %q", s.FundingNote())
	}
	_, spot := setup(t)
	if spot.FundingNote() != "" {
		t.Fatal("spot declares funding")
	}
}
