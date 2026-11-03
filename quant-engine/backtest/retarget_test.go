package backtest

import (
	"context"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

type retargetTwoPct struct{ *scripted.Strategy }

func (retargetTwoPct) Retarget(in strategy.Input) float64 { return in.Position.AvgPrice * 1.02 }

// The trade records the target in force after an Add: the Retargeter's, 2%
// above the aggregate average price, which then exits it as "target".
func TestBacktestRecordsTheRetargetedTarget(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for k, x := range []float64{100, 100, 105, 103, 102, 106} {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(k) * time.Hour), Open: x, High: x, Low: x, Close: x, Volume: 10})
	}
	answers := []strategy.Decision{{}, {}, {Action: strategy.Enter, Side: strategy.Long, Stop: 100, Target: 999, Fraction: 0.5},
		{Action: strategy.Add, Side: strategy.Long, Target: 999, Fraction: 0.5}}
	s := retargetTwoPct{&scripted.Strategy{Decider: func(strategy.Input) strategy.Decision {
		if len(answers) == 0 {
			return strategy.Decision{}
		}
		d := answers[0]
		answers = answers[1:]
		return d
	}}}
	yml := "exchange: fake\nasset_class: crypto\ninterval: 1h\nsymbols: [AAA]\nrisk: {confirmed_by_user: true}\n"
	res, err := Run(context.Background(), s, []byte(yml), Input{Candles: cs, Capital: 10_000, QtyStep: 0.001, MinQty: 0.001})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Trades) != 1 {
		t.Fatalf("trades %+v, want one", res.Trades)
	}
	tr := res.Trades[0]
	if tr.Target != tr.EntryPrice*1.02 || tr.ExitReason != "target" {
		t.Fatalf("trade target %v exit %q, want %v (2%% above the average %v) and a target exit", tr.Target, tr.ExitReason, tr.EntryPrice*1.02, tr.EntryPrice)
	}
}
