package paper

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
)

// charged is perp() with costs c on AAA; the delay does not wait, it runs
// onDelay (e.g. the market moving meanwhile).
func charged(t *testing.T, c config.SimCosts, onDelay func(time.Duration), next ...market.Candle) *Sim {
	t.Helper()
	_, s := perp(t, next...)
	s.prof = Profile{Name: "test", Costs: map[string]config.SimCosts{"AAA": c}}
	s.sleep = func(_ context.Context, d time.Duration) error {
		if onDelay != nil {
			onDelay(d)
		}
		return nil
	}
	return s
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func fill(t *testing.T, s *Sim, id string, side exchange.Side, qty float64) float64 {
	t.Helper()
	st, err := s.PlaceOrder(ctx, exchange.Demo, exchange.OrderRequest{ClientID: id, Symbol: "AAA", Side: side, Qty: qty, Type: exchange.Market})
	if err != nil || st.Status != exchange.Filled {
		t.Fatalf("order %s = %+v %v", id, st, err)
	}
	return st.FillPrice
}

// Latency: the fill takes the price after the delay, not the price
// at the decision.
func TestPaperSimLatencyFillsAtTheLaterPrice(t *testing.T) {
	var waited time.Duration
	var s *Sim
	data, _ := perp(t, market.Candle{Open: 110, High: 110, Low: 110, Close: 110})
	s = New(data, 10_000, time.Now, Profile{Name: "real", Costs: map[string]config.SimCosts{"AAA": {Latency: 550 * time.Millisecond}}})
	s.sleep = func(_ context.Context, d time.Duration) error { waited = d; data.Advance(); return nil }
	if px := fill(t, s, "e", exchange.Buy, 1); px != 110 || waited != 550*time.Millisecond {
		t.Fatalf("fill %v after %v; want 110 (the price after the delay) after 550ms", px, waited)
	}
}

// Fee: the taker fee leaves cash on the entry and on the exit.
func TestPaperSimFeeIsDebited(t *testing.T) {
	s := charged(t, config.SimCosts{CommissionPct: 0.1}, nil)
	fill(t, s, "e", exchange.Buy, 10) // 1000 notional, 500 margin at leverage 2, fee 1
	if b, _ := s.Balance(ctx, exchange.Demo); !near(b.Cash, 9_499) || !near(b.Equity, 9_999) {
		t.Fatalf("after entry %+v; want cash 9499, equity 9999", b)
	}
	fill(t, s, "x", exchange.Sell, 10)
	if b, _ := s.Balance(ctx, exchange.Demo); !near(b.Cash, 9_998) {
		t.Fatalf("after exit %+v; want cash 9998 (two fees of 1)", b)
	}

	// a stop pays the fee too: 10 at 95 = 950 notional, fee 0.95
	s = charged(t, config.SimCosts{CommissionPct: 0.1}, nil, market.Candle{Open: 100, High: 100, Low: 90, Close: 92})
	fill(t, s, "e", exchange.Buy, 10)
	if _, err := s.PlaceStop(ctx, exchange.Demo, "s", "AAA", exchange.Sell, 10, 95); err != nil {
		t.Fatal(err)
	}
	s.pos["AAA"].since = time.Time{}
	s.data.(interface{ Advance() bool }).Advance()
	if _, err := s.Candles(ctx, exchange.Demo, "AAA", time.Hour, 5); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Balance(ctx, exchange.Demo); len(s.pos) != 0 || !near(b.Cash, 9_948.05) {
		t.Fatalf("after the stop %+v; want cash 9948.05 (9499 + 500 margin - 50 loss - 0.95 fee)", b)
	}
}

// Slippage: against the trader on the entry, the exit and a stop,
// whether a candle or the last price triggers it.
func TestPaperSimSlippageHitsEntryExitAndStops(t *testing.T) {
	c := config.SimCosts{SlippagePct: 1}
	s := charged(t, c, nil)
	if in, out := fill(t, s, "e", exchange.Buy, 1), fill(t, s, "x", exchange.Sell, 1); !near(in, 101) || !near(out, 99) {
		t.Fatalf("entry %v exit %v; want 101 and 99", in, out)
	}
	if in, out := fill(t, s, "se", exchange.Sell, 1), fill(t, s, "sx", exchange.Buy, 1); !near(in, 99) || !near(out, 101) {
		t.Fatalf("short entry %v exit %v; want 99 and 101", in, out)
	}

	// a candle crosses the stop: it fills at the stop, slipped
	s = charged(t, c, nil, market.Candle{Open: 100, High: 100, Low: 90, Close: 92})
	data := s.data.(interface{ Advance() bool })
	fill(t, s, "e", exchange.Buy, 1)
	if _, err := s.PlaceStop(ctx, exchange.Demo, "s", "AAA", exchange.Sell, 1, 95); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Time{} }
	s.pos["AAA"].since = time.Time{}
	data.Advance()
	if _, err := s.Candles(ctx, exchange.Demo, "AAA", time.Hour, 5); err != nil {
		t.Fatal(err)
	}
	if st := s.orders["s"]; st.Status != exchange.Filled || !near(st.FillPrice, 95*0.99) {
		t.Fatalf("candle stop = %+v; want filled at 94.05", st)
	}

	// the last price crosses the stop: it fills at the price after the delay, slipped
	moved := false
	sim := charged(t, config.SimCosts{SlippagePct: 1, Latency: time.Second}, nil, market.Candle{Open: 94, High: 94, Low: 94, Close: 94}, market.Candle{Open: 93, High: 93, Low: 93, Close: 93})
	fill(t, sim, "e", exchange.Buy, 1)
	if _, err := sim.PlaceStop(ctx, exchange.Demo, "s", "AAA", exchange.Sell, 1, 95); err != nil {
		t.Fatal(err)
	}
	sim.data.(interface{ Advance() bool }).Advance() // last 94: below the stop
	sim.sleep = func(context.Context, time.Duration) error {
		moved = sim.data.(interface{ Advance() bool }).Advance()
		return nil
	}
	if ps, err := sim.Positions(ctx, exchange.Demo); err != nil || len(ps) != 0 {
		t.Fatalf("positions %+v %v; want the stop filled", ps, err)
	}
	if st := sim.orders["s"]; !moved || !near(st.FillPrice, 93*0.99) {
		t.Fatalf("last-price stop = %+v (delay ran: %v); want filled at 92.07, the later price slipped", st, moved)
	}
}

// The same scenario under each profile, with the yml's numbers,
// ends optimistic >= real >= pessimistic.
func TestPaperSimProfilesOrderTheResult(t *testing.T) {
	var equity []float64
	for _, p := range config.SimProfiles {
		y := "exchange: hyperliquid\nmarket: swap\nasset_class: crypto\nsymbols: [AAA]\ninterval: 1h\n" +
			"strategy: {period: 20, stop_pct: 4, target_pct: 5}\nrisk: {confirmed_by_user: true}\n" +
			"perp: {leverage: 2}\nsim: {profile: " + p + ", liquidity_class: mid_alt}\n"
		cfg, err := config.Parse([]byte(y), example.New())
		if err != nil {
			t.Fatal(err)
		}
		s := charged(t, cfg.SimCosts()["AAA"], nil,
			market.Candle{Open: 104, High: 104, Low: 104, Close: 104},
			market.Candle{Open: 96, High: 96, Low: 90, Close: 92})
		data := s.data.(interface{ Advance() bool })
		fill(t, s, "e1", exchange.Buy, 10)
		data.Advance()
		fill(t, s, "x1", exchange.Sell, 10) // a winning exit at 104
		fill(t, s, "e2", exchange.Buy, 10)
		if _, err := s.PlaceStop(ctx, exchange.Demo, "s2", "AAA", exchange.Sell, 10, 100); err != nil {
			t.Fatal(err)
		}
		s.pos["AAA"].since = time.Time{}
		data.Advance()
		if _, err := s.Candles(ctx, exchange.Demo, "AAA", time.Hour, 5); err != nil { // the stop fills
			t.Fatal(err)
		}
		b, err := s.Balance(ctx, exchange.Demo)
		if err != nil || len(s.pos) != 0 {
			t.Fatalf("%s: %+v %v, positions %v", p, b, err, s.pos)
		}
		equity = append(equity, b.Equity)
	}
	if !(equity[0] > equity[1] && equity[1] > equity[2]) {
		t.Fatalf("equity optimistic, real, pessimistic = %v; want strictly decreasing", equity)
	}
}
