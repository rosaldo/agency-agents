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
	return stepWith(t, c, s, func(e *Engine) { e.News, e.NewsTimeout = news, 50*time.Millisecond })
}

// stepWith builds the engine with New, lets set adjust it, runs one step on
// the entry candle and reports whether an entry order was sent.
func stepWith(t *testing.T, c *config.Config, s strategy.Strategy, set func(*Engine)) bool {
	t.Helper()
	cs, err := market.ReadCSVFile("../../testdata/backtest_one_trade.csv")
	if err != nil {
		t.Fatal(err)
	}
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs}})
	for i := 0; i < 3; i++ {
		f.Advance()
	}
	e := New(c, s, f, exchange.Demo, nil, func() time.Time { return cs[3].Time })
	set(e)
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

type keywordsOnly struct{ strategy.Strategy }

func (keywordsOnly) NewsPolicy() strategy.NewsPolicy {
	return strategy.NewsPolicy{Keywords: []string{"halt"}}
}

// The engine has no news content of its own: with news_guard required, a
// strategy without a news policy, or with a policy that lists no feed, never
// enters (fail closed), on an equity and on a crypto that opts in.
func TestNoNewsPolicyFailsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		yml string
		s   strategy.Strategy
	}{
		"equity, no policy":            {strings.Replace(cfgYAML, "CLASS", "equity", 1), example.New()},
		"crypto required, no policy":   {strings.Replace(cfgYAML, "CLASS", "crypto\nnews_guard: required", 1), example.New()},
		"equity, policy without feeds": {strings.Replace(cfgYAML, "CLASS", "equity", 1), keywordsOnly{example.New()}},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := config.Parse([]byte(tc.yml), tc.s)
			if err != nil {
				t.Fatal(err)
			}
			if stepWith(t, c, tc.s, func(*Engine) {}) {
				t.Fatal("entered with news_guard required and no news content from the strategy")
			}
		})
	}
	// control: the same entry candle enters when the guard is off
	c, s := parse(t, strings.Replace(cfgYAML, "CLASS", "crypto", 1))
	if !stepWith(t, c, s, func(*Engine) {}) {
		t.Fatal("control: no entry on the signal candle with news_guard none")
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
