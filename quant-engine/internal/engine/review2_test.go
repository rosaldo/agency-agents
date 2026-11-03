package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/market"
)

// bigLot is a market whose own minimum no plan can reach.
type bigLot struct{ *fake.Fake }

func (bigLot) Lot(string) (step, min float64) { return 1, 1e9 }

// The engine sizes with the market's own lot.
func TestEntryUsesTheMarketLot(t *testing.T) {
	c, s := parse(t, strings.Replace(cfgYAML, "CLASS", "crypto", 1))
	cs, _ := market.ReadCSVFile("../../testdata/backtest_one_trade.csv")
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs}})
	for i := 0; i < 3; i++ {
		f.Advance()
	}
	if err := New(c, s, bigLot{f}, exchange.Demo, nil, func() time.Time { return cs[3].Time }).Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Calls("PlaceOrder") != 0 {
		t.Fatal("entry sized with the adapter-wide lot, not the market's")
	}
}

type cancelRefused struct {
	*fake.Fake
	on *bool
	n  *int
}

func (c cancelRefused) CancelOrder(ctx context.Context, env exchange.Env, id string) (exchange.OrderState, error) {
	if *c.on {
		*c.n++
		return exchange.OrderState{}, errors.New("cancel refused")
	}
	return c.Fake.CancelOrder(ctx, env, id)
}

// A target exit that fails keeps the target.
func TestTargetKeptWhenTheExitFails(t *testing.T) {
	c, s := parse(t, strings.Replace(cfgYAML, "CLASS", "crypto", 1))
	cs, _ := market.ReadCSVFile("../../testdata/backtest_one_trade.csv")
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs}})
	on, n, i := false, 0, 0
	e := New(c, s, cancelRefused{f, &on, &n}, exchange.Demo, nil, func() time.Time { return cs[i].Time })
	for ; i < len(cs); i++ {
		_, on = e.Target("AAA")
		_ = e.Step(context.Background())
		if n > 0 {
			if _, ok := e.Target("AAA"); !ok {
				t.Fatal("target dropped after a failed exit")
			}
			return
		}
		f.Advance()
	}
	t.Fatal("no target exit reached")
}
