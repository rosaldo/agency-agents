package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/risk"
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// seen records what each Decide call was told of the position.
type seen struct {
	*scripted.Strategy
	views []strategy.PositionView // zero = flat
}

func (s *seen) Decide(in strategy.Input) strategy.Decision {
	var v strategy.PositionView
	if in.Position != nil {
		v = *in.Position
	}
	s.views = append(s.views, v)
	return s.Strategy.Decide(in)
}

func watch(e *Engine) *seen {
	s := &seen{Strategy: e.Strategy.(*scripted.Strategy)}
	e.Strategy = s
	return s
}

type used struct {
	budget   float64
	tranches int
}

func usedOf(vs []strategy.PositionView) []used {
	out := make([]used, len(vs))
	for i, v := range vs {
		out[i] = used{v.BudgetUsed, v.Tranches}
	}
	return out
}

// The strategy is told the fraction of the budget used and the tranches so
// far: zero flat, then after each accepted tranche; a refused Add counts
// nothing.
func TestPositionViewCountsTheTranches(t *testing.T) {
	e, f, _ := trancheEngine(t, "", []float64{105, 103, 102, 102}, enter(0.5, 100), add(0.25, 0), add(0.5, 0), strategy.Decision{})
	s := watch(e)
	steps(t, e, f, 4)
	want := []used{{0, 0}, {0.5, 1}, {0.75, 2}, {0.75, 2}} // the 0.5 on top of 0.75 is refused
	if got := usedOf(s.views); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
		t.Fatalf("views %+v, want %+v; rejections %q", got, want, journalOf(e).rejected())
	}
}

// A tranche is counted once saved, before its order: an Add whose order
// then fails (here the engine halts) keeps its fraction and its tranche
// counted (conservative), as the budget check does; the next run is told so.
func TestATrancheWhoseOrderFailsStaysCounted(t *testing.T) {
	e, f, _ := trancheEngine(t, "", []float64{105, 103, 102}, enter(0.5, 100), add(0.25, 0))
	steps(t, e, f, 1)
	f.Advance()
	e.Exec.Retries = 1
	f.FailNext("PlaceOrder", errors.New("timeout"), false)
	if err := e.Step(context.Background()); !IsHalt(err) {
		t.Fatalf("setup: step after the failed add returned %v, want a halt", err)
	}
	if p := position(t, f); p.Qty != 10 {
		t.Fatalf("setup: qty %v after the failed add, want 10", p.Qty)
	}
	ps, _ := f.Positions(context.Background(), exchange.Demo)
	s := &seen{Strategy: &scripted.Strategy{Decider: func(strategy.Input) strategy.Decision { return strategy.Decision{} }}}
	c, _ := config.Parse([]byte(scriptedYAML), s)
	e2 := New(c, s, f, exchange.Demo, &events{}, e.Exec.Now)
	e2.Restore(e.Targets(), ps, nil, e.Trades())
	f.Advance()
	if err := e2.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := usedOf(s.views); len(got) != 1 || got[0] != (used{0.75, 2}) {
		t.Fatalf("view after the failed add %+v, want 0.75 used in 2 tranches", got)
	}
}

// After a restart the strategy is told what the state file saved, not what
// the dead process held in memory; a trade saved before tranches were
// counted shows at least one.
func TestPositionViewAfterARestartIsThePersistedOne(t *testing.T) {
	for name, saved := range map[string]risk.Trade{
		"saved":  {Budget: 100, Used: 0.75, Tranches: 2, Stop: 100},
		"legacy": {Budget: 100, Used: 0.75, Stop: 100},
	} {
		t.Run(name, func(t *testing.T) {
			e, f, _ := trancheEngine(t, "", []float64{105, 103}, enter(0.5, 100))
			steps(t, e, f, 1)
			ps, _ := f.Positions(context.Background(), exchange.Demo)
			s := &seen{Strategy: &scripted.Strategy{Decider: func(strategy.Input) strategy.Decision { return strategy.Decision{} }}}
			c, _ := config.Parse([]byte(scriptedYAML), s)
			e2 := New(c, s, f, exchange.Demo, &events{}, e.Exec.Now)
			e2.Restore(e.Targets(), ps, nil, map[string]risk.Trade{"AAA": saved})
			f.Advance()
			if err := e2.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := used{0.75, max(saved.Tranches, 1)}
			if got := usedOf(s.views); len(got) != 1 || got[0] != want {
				t.Fatalf("views after the restart %+v, want %+v", got, want)
			}
		})
	}
}
