package engine

import (
	"context"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// swapEngine is scriptedEngine on a swap fake (shorts allowed), at the
// candle closing at 105; now moves with the returned clock.
func swapEngine(t *testing.T, decide func(strategy.Input) strategy.Decision) (*Engine, *fake.Fake, *events, *time.Time) {
	t.Helper()
	s := &scripted.Strategy{Decider: decide}
	c, err := config.Parse([]byte(scriptedYAML+"market: swap\nperp: {allow_short: true, leverage: 2}\n"), s)
	if err != nil {
		t.Fatal(err)
	}
	cs, _ := market.ReadCSVFile("../../testdata/backtest_one_trade.csv")
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 1, MinQty: 1, Shorts: true, Candles: map[string][]market.Candle{"AAA": cs}})
	for i := 0; i < 3; i++ {
		f.Advance()
	}
	now := cs[3].Time
	j := &events{}
	return New(c, s, f, exchange.Demo, j, func() time.Time { return now }), f, j, &now
}

// FR-P2: a short sells at market, its stop is a BUY above the entry, sized on
// the short's own risk, and the stop closes it when crossed.
func TestShortEntryHasABuyStopAbove(t *testing.T) {
	ctx := context.Background()
	e, f, j, now := swapEngine(t, func(in strategy.Input) strategy.Decision {
		if in.Position != nil {
			return strategy.Decision{}
		}
		return strategy.Decision{Action: strategy.Enter, Side: strategy.Short, Stop: 110, Reason: "test"}
	})
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	ps, _ := f.Positions(ctx, exchange.Demo)
	if len(ps) != 1 || ps[0].Side != exchange.Short || ps[0].Qty != 20 || ps[0].StopPrice != 110 { // 100 / (110-105)
		t.Fatalf("positions %+v (%s), want one short of 20 with its stop at 110", ps, j.rejected())
	}
	stop, _ := f.Order(ps[0].StopClientID)
	if stop.Side != exchange.Buy {
		t.Fatalf("stop of a short is a %s, want buy", stop.Side)
	}
	f.Advance() // high 111 crosses the stop
	*now = now.Add(time.Hour)
	if ps, _ := f.Positions(ctx, exchange.Demo); len(ps) != 0 {
		t.Fatalf("stop not filled: %+v", ps)
	}
	if b, _ := f.Balance(ctx, exchange.Demo); b.Equity != 10_000-20*5 {
		t.Fatalf("equity after the stop %v, want 9900", b.Equity)
	}
}

// A strategy Exit on a short buys back reduce-only, after cancelling the stop.
func TestShortExitBuysBackReduceOnly(t *testing.T) {
	ctx := context.Background()
	e, f, j, _ := swapEngine(t, func(in strategy.Input) strategy.Decision {
		if in.Position != nil {
			if in.Position.Side != strategy.Short {
				t.Errorf("strategy sees a %s position, want short", in.Position.Side)
			}
			return strategy.Decision{Action: strategy.Exit, Reason: "done"}
		}
		return strategy.Decision{Action: strategy.Enter, Side: strategy.Short, Stop: 110, Target: 90}
	})
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Step(ctx); err != nil {
		t.Fatalf("exit: %v (%s)", err, j.rejected())
	}
	if ps, _ := f.Positions(ctx, exchange.Demo); len(ps) != 0 || f.Unprotected() != 0 {
		t.Fatalf("short not closed cleanly: %+v unprotected %d", ps, f.Unprotected())
	}
	if _, ok := e.Target("AAA"); ok {
		t.Fatal("target kept after the exit")
	}
}

// FR-P3: margin mode and leverage go to the exchange before any order, and an
// applied value that differs from the yml refuses to trade.
func TestConfigureMarketsChecksWhatWasApplied(t *testing.T) {
	ctx := context.Background()
	e, f, _, _ := swapEngine(t, func(strategy.Input) strategy.Decision { return strategy.Decision{} })
	if err := e.ConfigureMarkets(ctx); err != nil || f.Calls("ConfigureMarket") != 1 {
		t.Fatalf("configure: %v, calls %d", err, f.Calls("ConfigureMarket"))
	}
	e2, _, _, _ := swapEngine(t, nil)
	e2.Ex = fake.New(fake.Options{Paper: true, Applied: &exchange.MarginSettings{Leverage: 2, MarginMode: "cross"}})
	if err := e2.ConfigureMarkets(ctx); err == nil {
		t.Fatal("cross applied where isolated was asked: accepted")
	}
}
