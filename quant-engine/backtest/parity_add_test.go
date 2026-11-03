package backtest

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/loop"
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// enterThenAdd enters half the budget at 01:00 (stop 90) and adds the other
// half at 02:00.
func enterThenAdd(in strategy.Input) strategy.Decision {
	switch {
	case in.Position == nil && in.Now.Hour() == 1:
		return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 90, Fraction: 0.5, Reason: "first"}
	case in.Position != nil && in.Now.Hour() == 2:
		return strategy.Decision{Action: strategy.Add, Side: strategy.Long, Fraction: 0.5, Reason: "second"}
	}
	return strategy.Decision{}
}

// Graduated entry parity: on the same candles, the backtest and the loop
// make the same Decide calls and the same fills, Enter then Add, and the
// stop that fills is the one for the whole position: the backtest's trade
// carries the total quantity, the loop's journal a resting stop for it from the add's own step.
func TestBacktestAndLoopAddAlike(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for i, b := range [][4]float64{{100, 100, 100, 100}, {101, 101, 101, 101}, {103, 103, 103, 103}, {104, 104, 104, 104},
		{104, 104, 85, 85}, {85, 85, 85, 85}} {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(i) * time.Hour), Open: b[0], High: b[1], Low: b[2], Close: b[3], Volume: 10})
	}
	bs := &scripted.Strategy{Decider: enterThenAdd}
	res, err := Run(context.Background(), bs, []byte(parityYAML), Input{Candles: cs, Capital: 10_000, QtyStep: 1, MinQty: 1})
	if err != nil {
		t.Fatal(err)
	}
	// 50 / (101 - 90) = 4, then 50 / (103 - 90) = 3
	if len(res.Trades) != 1 || res.Trades[0].Qty != 7 || res.Trades[0].ExitReason != "stop" || res.Trades[0].ExitPrice != 90 {
		t.Fatalf("backtest trades %+v; want one of 7 (4 + 3) closed by the stop at 90", res.Trades)
	}

	ls := &scripted.Strategy{Decider: enterThenAdd}
	c, err := config.Parse([]byte(parityYAML), ls)
	if err != nil {
		t.Fatal(err)
	}
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs}})
	dir := filepath.Join(t.TempDir(), ".scripted")
	loop.Run(context.Background(), c, ls, f, exchange.Demo, &loop.FakeTicker{Ex: f, Candles: cs}, dir)
	evs, _, err := journal.Read(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var fills []string
	whole := false
	for _, e := range evs {
		switch {
		case e.Kind == journal.Filled:
			fills = append(fills, fmt.Sprintf("%s %v", e.Side, e.Qty))
		case e.Kind == journal.Resting && e.Qty == 7 && e.Time.Equal(cs[2].Time): // swapped in the add's own step
			whole = true
		}
	}
	if !slices.Equal(bs.Log, ls.Log) {
		t.Fatalf("Decide calls differ:\nbacktest %q\nloop     %q", bs.Log, ls.Log)
	}
	if want := []string{"buy 4", "buy 3"}; !slices.Equal(fills, want) || !whole {
		t.Fatalf("loop fills %q (a resting stop for 7: %v); want %q and a stop for the whole 7", fills, whole, want)
	}
}

// The backtest and the loop tell the strategy the same budget used and
// tranche count on the same candles, Enter then Add then Add, a refused
// fourth tranche counting nothing.
func TestBacktestAndLoopCountTheTranchesAlike(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for i, x := range []float64{100, 101, 103, 104, 105, 106, 106} {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(i) * time.Hour), Open: x, High: x, Low: x, Close: x, Volume: 10})
	}
	type used struct {
		budget   float64
		tranches int
	}
	script := func(views *[]used) *scripted.Strategy {
		return &scripted.Strategy{Decider: func(in strategy.Input) strategy.Decision {
			if in.Position == nil {
				*views = append(*views, used{})
			} else {
				*views = append(*views, used{in.Position.BudgetUsed, in.Position.Tranches})
			}
			switch h := in.Now.Hour(); {
			case in.Position == nil && h == 1:
				return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 90, Fraction: 0.5, Reason: "first"}
			case in.Position != nil && h >= 2 && h <= 4:
				return strategy.Decision{Action: strategy.Add, Side: strategy.Long, Fraction: 0.25, Reason: "more"}
			}
			return strategy.Decision{}
		}}
	}
	var bv, lv []used
	if _, err := Run(context.Background(), script(&bv), []byte(parityYAML), Input{Candles: cs, Capital: 10_000, QtyStep: 1, MinQty: 1}); err != nil {
		t.Fatal(err)
	}
	ls := script(&lv)
	c, err := config.Parse([]byte(parityYAML), ls)
	if err != nil {
		t.Fatal(err)
	}
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs}})
	loop.Run(context.Background(), c, ls, f, exchange.Demo, &loop.FakeTicker{Ex: f, Candles: cs}, filepath.Join(t.TempDir(), ".scripted"))
	want := []used{{}, {}, {0.5, 1}, {0.75, 2}, {1, 3}, {1, 3}, {1, 3}} // one Decide per candle; the add at 04:00 is refused
	if !slices.Equal(bv, want) || !slices.Equal(lv, want) {
		t.Fatalf("budget used / tranches seen:\nbacktest %+v\nloop     %+v\nwant     %+v", bv, lv, want)
	}
}
