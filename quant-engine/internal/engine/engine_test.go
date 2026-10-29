package engine

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/news"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
)

const cfgYAML = `
exchange: fake
asset_class: CLASS
interval: 1h
symbols: [AAA]
strategy: {period: 3, stop_pct: 4, target_pct: 5}
risk: {confirmed_by_user: true}
`

type stubNews struct {
	clear bool
	err   error
	delay time.Duration
}

func (s stubNews) Clear(ctx context.Context, symbol string, at time.Time) (bool, string, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return false, "", ctx.Err()
	}
	return s.clear, "stub verdict", s.err
}

// parse configures the example strategy from y.
func parse(t *testing.T, y string) (*config.Config, strategy.Strategy) {
	t.Helper()
	s := example.New()
	c, err := config.Parse([]byte(y), s)
	if err != nil {
		t.Fatal(err)
	}
	return c, s
}

// stepOnSignal runs one step on the entry candle and reports whether an
// entry order was sent.
func stepOnSignal(t *testing.T, class string, news NewsChecker) bool {
	t.Helper()
	c, s := parse(t, strings.Replace(cfgYAML, "CLASS", class, 1))
	cs, err := market.ReadCSVFile("../../testdata/backtest_one_trade.csv")
	if err != nil {
		t.Fatal(err)
	}
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs}})
	for i := 0; i < 3; i++ {
		f.Advance()
	}
	e := New(c, s, f, exchange.Demo, nil, func() time.Time { return cs[3].Time })
	e.News, e.NewsTimeout = news, 50*time.Millisecond
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f.Calls("PlaceOrder") > 0
}

func TestNewsGuard(t *testing.T) {
	cases := []struct {
		name, class string
		news        NewsChecker
		want        bool
	}{
		{"crypto ignores news", "crypto", nil, true},
		{"equity without a checker stays out", "equity", nil, false},
		{"equity with clear news enters", "equity", stubNews{clear: true}, true},
		{"equity with bad news stays out", "equity", stubNews{clear: false}, false},
		{"equity with a failing check stays out", "equity", stubNews{clear: true, err: errors.New("down")}, false},
		{"equity with a slow check stays out", "equity", stubNews{clear: true, delay: time.Second}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stepOnSignal(t, tc.class, tc.news); got != tc.want {
				t.Fatalf("entry sent = %v, want %v", got, tc.want)
			}
		})
	}
}

type withNews struct{ strategy.Strategy }

func (withNews) NewsPolicy() strategy.NewsPolicy {
	return strategy.NewsPolicy{Feeds: []string{"https://example.invalid/rss"}, Keywords: []string{"Halt"}}
}

func TestStrategyFeedsWireTheRSSChecker(t *testing.T) {
	s := withNews{example.New()}
	c, err := config.Parse([]byte(strings.Replace(cfgYAML, "CLASS", "equity", 1)), s)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := New(c, s, nil, exchange.Demo, nil, time.Now).News.(*news.RSS)
	if !ok || len(r.Keywords) != 1 || r.Keywords[0] != "halt" || r.Lookback != 24*time.Hour {
		t.Fatalf("the strategy's news policy did not wire the RSS checker: %+v", r)
	}
}

// With news_guard required, a strategy without a news policy never enters.
func TestNoNewsPolicyFailsClosed(t *testing.T) {
	c, s := parse(t, strings.Replace(cfgYAML, "CLASS", "equity", 1))
	if New(c, s, nil, exchange.Demo, nil, time.Now).News != nil {
		t.Fatal("a news checker without a news policy")
	}
}

// Review B2: Reconcile runs every step, not only at start; the stop goes
// back at the price the strategy gave at entry.
func TestEveryStepProtectsAnUnprotectedPosition(t *testing.T) {
	c, s := parse(t, strings.Replace(cfgYAML, "CLASS", "crypto", 1))
	cs, _ := market.ReadCSVFile("../../testdata/backtest_one_trade.csv")
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs}})
	for i := 0; i < 3; i++ {
		f.Advance()
	}
	e := New(c, s, f, exchange.Demo, nil, func() time.Time { return cs[3].Time })
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ps, _ := f.Positions(ctx, exchange.Demo)
	if len(ps) != 1 || ps[0].StopClientID == "" {
		t.Fatalf("no protected entry: %+v", ps)
	}
	if _, err := f.CancelOrder(ctx, exchange.Demo, ps[0].StopClientID); err != nil { // e.g. the exchange dropped it
		t.Fatal(err)
	}
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	ps, _ = f.Positions(ctx, exchange.Demo)
	if len(ps) != 1 || ps[0].StopClientID == "" || !near(ps[0].StopPrice, 100.8) {
		t.Fatalf("unprotected position left after a step, or a stop off the entry's: %+v", ps)
	}
}

// A position whose stop the engine never knew is flattened, not guessed.
func TestUnknownStopFlattens(t *testing.T) {
	c, s := parse(t, strings.Replace(cfgYAML, "CLASS", "crypto", 1))
	cs, _ := market.ReadCSVFile("../../testdata/backtest_one_trade.csv")
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs}})
	e := New(c, s, f, exchange.Demo, nil, func() time.Time { return cs[0].Time })
	f.SetPosition("AAA", 3, 100) // e.g. a late fill of an order this run does not know
	if err := e.Step(context.Background()); !IsHalt(err) {
		t.Fatalf("step = %v, want a halt", err)
	}
	if ps, _ := f.Positions(context.Background(), exchange.Demo); len(ps) != 0 {
		t.Fatalf("position kept without a known stop: %+v", ps)
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
