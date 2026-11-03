package loop

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// retargetTwoPct sets the target 2% above the average price as filled.
type retargetTwoPct struct{ strategy.Strategy }

func (retargetTwoPct) Retarget(in strategy.Input) float64 { return in.Position.AvgPrice * 1.02 }

// retargetFake is a fake on closes 100 100 100 105 103 102 106 106.
func retargetFake() (*fake.Fake, []market.Candle) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for k, x := range []float64{100, 100, 100, 105, 103, 102, 106, 106} {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(k) * time.Hour), Open: x, High: x, Low: x, Close: x, Volume: 10})
	}
	return fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 0.001, MinQty: 0.001, Candles: map[string][]market.Candle{"AAA": cs}}), cs
}

func withTarget(d strategy.Decision, target float64) strategy.Decision { d.Target = target; return d }

// The target a Retargeter set after an Add is saved, shown by status, and
// exits the position after a restart (106 reaches 2% above the average of
// 105 and 103; the Decision's 999 never would).
func TestRetargetedTargetSurvivesARestart(t *testing.T) {
	f, cs := retargetFake()
	dir := filepath.Join(t.TempDir(), "state")
	s := retargetTwoPct{scriptOf(nil, wait, wait, wait, withTarget(tranche(strategy.Enter, 100), 999), withTarget(tranche(strategy.Add, 0), 999))}
	c, err := config.Parse([]byte(trancheYAML), s)
	if err != nil {
		t.Fatal(err)
	}
	var midRun State
	tick := &FakeTicker{Ex: f, Candles: cs, OnStep: func(i int) {
		if i == 4 { // after the Add's step, as a kill -9 now would leave it
			midRun, _ = LoadState(dir, "paper")
			_ = RequestStop(dir, false)
		}
	}}
	Run(context.Background(), c, s, f, exchange.Demo, tick, dir)
	p := ownPosition(t, f)
	want := p.AvgPrice * 1.02
	if midRun.Targets["AAA"] != want || len(midRun.RetargetDue) != 0 {
		t.Fatalf("state after the Add's step: target %v owed %v, want %v and the debt cleared", midRun.Targets["AAA"], midRun.RetargetDue, want)
	}
	st, _ := LoadState(dir, "paper")
	if st.Targets["AAA"] != want {
		t.Fatalf("saved target %v, want %v (2%% above the filled average %v)", st.Targets["AAA"], want, p.AvgPrice)
	}
	status, err := ReadStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(status); !strings.Contains(string(b), `"targets":{"AAA":`) || status.Targets["AAA"] != want {
		t.Fatalf("status %s, want the target %v", b, want)
	}

	s2 := scriptOf(nil) // no Retargeter: the restored target alone exits
	c2, _ := config.Parse([]byte(trancheYAML), s2)
	f.Advance()
	Run(context.Background(), c2, s2, f, exchange.Demo, tickerFrom(f, cs, tick.i.Load()+1), dir)
	if ps, _ := f.Positions(context.Background(), exchange.Demo); len(ps) != 0 {
		t.Fatalf("position %+v not closed at the restored target %v", ps, want)
	}
}

// failPositionsOnce fails the first Positions read after the first order.
type failPositionsOnce struct {
	*fake.Fake
	done bool
}

func (x *failPositionsOnce) Positions(ctx context.Context, env exchange.Env) ([]exchange.Position, error) {
	if !x.done && x.Fake.Calls("PlaceOrder") > 0 {
		x.done = true
		return nil, errors.New("positions unavailable")
	}
	return x.Fake.Positions(ctx, env)
}

// A retarget owed because the position was unreadable after the fill is
// saved, survives a restart, and is made by the restarted process.
func TestOwedRetargetSurvivesARestart(t *testing.T) {
	f, cs := retargetFake()
	ex := &failPositionsOnce{Fake: f}
	dir := filepath.Join(t.TempDir(), "state")
	s := retargetTwoPct{scriptOf(nil, wait, wait, wait, withTarget(tranche(strategy.Enter, 100), 999))}
	c, _ := config.Parse([]byte(trancheYAML), s)
	var midRun State
	tick := &FakeTicker{Ex: f, Candles: cs, OnStep: func(i int) {
		if i == 3 { // after the entry's step: what a kill -9 now would leave
			midRun, _ = LoadState(dir, "paper")
			_ = RequestStop(dir, false)
		}
	}}
	Run(context.Background(), c, s, ex, exchange.Demo, tick, dir)
	st, _ := LoadState(dir, "paper")
	for when, st := range map[string]State{"mid-run": midRun, "after the stop": st} {
		if !slices.Equal(st.RetargetDue, []string{"AAA"}) || st.Targets["AAA"] != 999 {
			t.Fatalf("%s: saved owed %v target %v, want AAA owed and the Decision's 999", when, st.RetargetDue, st.Targets["AAA"])
		}
	}

	s2 := retargetTwoPct{scriptOf(nil)}
	c2, _ := config.Parse([]byte(trancheYAML), s2)
	f.Advance()
	tick2 := tickerFrom(f, cs, tick.i.Load()+1)
	tick2.OnStep = func(int) { _ = RequestStop(dir, false) }
	Run(context.Background(), c2, s2, ex, exchange.Demo, tick2, dir)
	st, _ = LoadState(dir, "paper")
	if want := ownPosition(t, f).AvgPrice * 1.02; math.Abs(st.Targets["AAA"]-want) > 1e-12 || len(st.RetargetDue) != 0 {
		t.Fatalf("after the restart: target %v owed %v, want %v and nothing owed", st.Targets["AAA"], st.RetargetDue, want)
	}
}

// A kill -9 right after an Add's fill, before the end-of-step save: the
// retarget was owed in the save before the order, so the restarted process
// makes it on the position as filled.
func TestKillAfterAnAddFillStillRetargets(t *testing.T) {
	f, cs := retargetFake()
	ex := &crashAfterFill{Fake: f, armedAt: -1}
	dir := filepath.Join(t.TempDir(), "state")
	s := retargetTwoPct{scriptOf(func(d strategy.Decision) {
		if d.Action == strategy.Add {
			ex.armedAt = f.Calls("PlaceOrder")
		}
	}, wait, wait, wait, withTarget(tranche(strategy.Enter, 100), 999), withTarget(tranche(strategy.Add, 0), 999))}
	c, _ := config.Parse([]byte(trancheYAML), s)
	tick := &FakeTicker{Ex: f, Candles: cs}
	if !runUntilKilled(t, c, s, ex, tick, dir) {
		t.Fatal("setup: the loop was never killed after the add")
	}
	ex.armedAt = -1
	st, _ := LoadState(dir, "paper")
	if !slices.Equal(st.RetargetDue, []string{"AAA"}) {
		t.Fatalf("state at the kill owes %v, want AAA", st.RetargetDue)
	}
	f.Advance()
	s2 := retargetTwoPct{scriptOf(nil)}
	c2, _ := config.Parse([]byte(trancheYAML), s2)
	tick2 := tickerFrom(f, cs, tick.i.Load()+1)
	tick2.OnStep = func(int) { _ = RequestStop(dir, false) }
	Run(context.Background(), c2, s2, ex, exchange.Demo, tick2, dir)
	st, _ = LoadState(dir, "paper")
	if want := ownPosition(t, f).AvgPrice * 1.02; math.Abs(st.Targets["AAA"]-want) > 1e-12 || len(st.RetargetDue) != 0 {
		t.Fatalf("after the restart: target %v owed %v, want %v and nothing owed", st.Targets["AAA"], st.RetargetDue, want)
	}
}
