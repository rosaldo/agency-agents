package backtest

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// enterThenTighterAdd enters at 01:00 (stop 90) and adds at 02:00 moving the
// stop of the whole position up to 95.
func enterThenTighterAdd(in strategy.Input) strategy.Decision {
	switch {
	case in.Position == nil && in.Now.Hour() == 1:
		return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 90, Fraction: 0.5, Reason: "first"}
	case in.Position != nil && in.Now.Hour() == 2:
		return strategy.Decision{Action: strategy.Add, Side: strategy.Long, Stop: 95, Fraction: 0.5, Reason: "second"}
	}
	return strategy.Decision{}
}

// A trade entered in tranches measures R against the stop in force for the
// whole position (the add's): without costs, a trade closed by it loses 1 R.
func TestTrancheTradeRIsMeasuredAgainstTheAddStop(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for i, b := range [][4]float64{{100, 100, 100, 100}, {101, 101, 101, 101}, {103, 103, 103, 103}, {104, 104, 104, 104},
		{104, 104, 85, 85}, {85, 85, 85, 85}} {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(i) * time.Hour), Open: b[0], High: b[1], Low: b[2], Close: b[3], Volume: 10})
	}
	res, err := Run(context.Background(), &scripted.Strategy{Decider: enterThenTighterAdd}, []byte(parityYAML),
		Input{Candles: cs, Capital: 10_000, QtyStep: 1, MinQty: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Trades) != 1 {
		t.Fatalf("trades %+v; want one", res.Trades)
	}
	tr := res.Trades[0]
	if tr.Stop != 95 || tr.ExitReason != "stop" || tr.ExitPrice != 95 || math.Abs(tr.R+1) > 1e-9 {
		t.Fatalf("trade %+v; want stop 95, closed by it at 95, R = -1", tr)
	}
}
