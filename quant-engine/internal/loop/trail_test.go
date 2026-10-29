package loop

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// trailer enters once at the second candle, stop 90, and trails 3 below the close.
type trailer struct{ *scripted.Strategy }

func (trailer) Trail(in strategy.Input) (float64, bool) {
	return in.Candles[len(in.Candles)-1].Close - 3, true
}

// failCancels fails every CancelOrder while fail is set.
type failCancels struct {
	*fake.Fake
	fail bool
}

func (x *failCancels) CancelOrder(ctx context.Context, env exchange.Env, id string) (exchange.OrderState, error) {
	if x.fail {
		return exchange.OrderState{Status: exchange.Unknown}, errors.New("timeout")
	}
	return x.Fake.CancelOrder(ctx, env, id)
}

// FR-P9 (f): a moved stop and an old stop not yet cancelled survive a
// restart: the stop the exchange dropped comes back at the trailed price,
// never the entry's, and the stale cancel is retried.
func TestTrailedStopAndStaleStopSurviveARestart(t *testing.T) {
	s := trailer{&scripted.Strategy{Decider: func(in strategy.Input) strategy.Decision {
		if in.Position == nil && len(in.Candles) == 2 {
			return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 90, Reason: "test"}
		}
		return strategy.Decision{}
	}}}
	c, err := config.Parse([]byte(strings.Replace(cfgYAML, "strategy: {period: 3, stop_pct: 4, target_pct: 5}\n", "", 1)), s)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for i, x := range []float64{100, 100, 105, 105, 105, 105} {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(i) * time.Hour), Open: x, High: x, Low: x, Close: x, Volume: 10})
	}
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs}})
	ex := &failCancels{Fake: f}
	tick := &FakeTicker{Ex: f, Candles: cs}
	dir := filepath.Join(t.TempDir(), ".scripted")
	tick.OnStep = func(i int) {
		switch i {
		case 1: // entered at 100; the move at 105 will not get the old stop cancelled
			ex.fail = true
		case 2:
			ex.fail = false
			_ = RequestStop(dir, false)
		}
	}
	Run(context.Background(), c, s, ex, exchange.Demo, tick, dir)
	st, _ := LoadState(dir, "paper")
	if len(st.Positions) != 1 || st.Positions[0].StopPrice != 102 || st.StaleStops["AAA"] == "" {
		t.Fatalf("state after stop = %+v; want the stop at 102 and the old one stale", st)
	}
	stale := st.StaleStops["AAA"]
	if _, err := f.CancelOrder(context.Background(), exchange.Demo, st.Positions[0].StopClientID); err != nil { // the venue drops it
		t.Fatal(err)
	}
	tick.OnStep = func(i int) {
		if i == 3 {
			_ = RequestStop(dir, false)
		}
	}
	Run(context.Background(), c, s, ex, exchange.Demo, tick, dir)
	st, _ = LoadState(dir, "paper")
	if len(st.Positions) != 1 || st.Positions[0].StopPrice != 102 || len(st.StaleStops) != 0 {
		t.Fatalf("state after restart = %+v; want the stop back at 102 and the stale one cleared", st)
	}
	evs, _, _ := journal.Read(filepath.Join(dir, "journal.jsonl"))
	retried := false
	for _, e := range evs {
		retried = retried || e.ClientID == stale && strings.Contains(e.Reason, "retry")
	}
	if !retried {
		t.Fatal("the stale cancel was not retried after the restart")
	}
}
