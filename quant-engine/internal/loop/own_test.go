package loop

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// trailRun is the trail fixture: entry at 100 (stop 90), then a move each
// candle at close-3; cancels fail while ex.fail is set.
func trailRun(t *testing.T) (*config.Config, trailer, *fake.Fake, *failCancels, *FakeTicker, string) {
	t.Helper()
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
	return c, s, f, &failCancels{Fake: f}, &FakeTicker{Ex: f, Candles: cs}, filepath.Join(t.TempDir(), ".scripted")
}

// snapAfterStale reads state.json on the first exchange call after a cancel
// failed: the instant a kill -9 would freeze.
type snapAfterStale struct {
	*failCancels
	dir    string
	armed  bool
	frozen *State
}

func (x *snapAfterStale) CancelOrder(ctx context.Context, env exchange.Env, id string) (exchange.OrderState, error) {
	st, err := x.failCancels.CancelOrder(ctx, env, id)
	x.armed = x.armed || (err != nil && x.frozen == nil)
	return st, err
}

func (x *snapAfterStale) Balance(ctx context.Context, env exchange.Env) (exchange.Balance, error) {
	if x.armed {
		st, _ := LoadState(x.dir, "paper")
		x.frozen, x.armed = &st, false
	}
	return x.failCancels.Balance(ctx, env)
}

// The stale stop a move leaves is in state.json at once,
// before the step's own save: a kill -9 right after the move keeps it.
func TestStaleStopIsSavedRightAfterTheMove(t *testing.T) {
	c, s, _, fc, tick, dir := trailRun(t)
	ex := &snapAfterStale{failCancels: fc, dir: dir}
	tick.OnStep = func(i int) {
		switch i {
		case 1:
			fc.fail = true
		case 2:
			_ = RequestStop(dir, false)
		}
	}
	Run(context.Background(), c, s, ex, exchange.Demo, tick, dir)
	if ex.frozen == nil || ex.frozen.StaleStops["AAA"] == "" {
		t.Fatalf("state right after the move = %+v; want the stale stop in it", ex.frozen)
	}
}

// A run that ends without flatten reconciles once more on its way out: a
// stop the venue dropped at the last moment is put back.
func TestRunEndsWithAFinalReconcile(t *testing.T) {
	c, s, f, fc, tick, dir := trailRun(t)
	tick.OnStep = func(i int) {
		if i == 2 {
			ps, _ := f.Positions(context.Background(), exchange.Demo)
			_, _ = f.CancelOrder(context.Background(), exchange.Demo, ps[0].StopClientID) // the venue drops it
			_ = RequestStop(dir, false)
		}
	}
	Run(context.Background(), c, s, fc, exchange.Demo, tick, dir)
	ps, _ := f.Positions(context.Background(), exchange.Demo)
	if len(ps) != 1 || ps[0].StopClientID == "" || ps[0].StopPrice != 102 {
		t.Fatalf("positions after the run = %+v; want the stop back at 102", ps)
	}
}
