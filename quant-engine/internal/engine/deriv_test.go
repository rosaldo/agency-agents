package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/paper"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// derivUser is the scripted strategy declaring it needs derivatives data.
type derivUser struct {
	*scripted.Strategy
	needs bool
}

func (d derivUser) NeedsDerivatives() bool { return d.needs }

// noPort hides every optional port of the exchange it wraps.
type noPort struct{ exchange.Exchange }

const swapYAML = scriptedYAML + "market: swap\nperp: {allow_short: true}\n"

// derivSeries is one snapshot per candle, read as live reads it: in the step
// that decides on the candle, 5s after its close; OpenInterest i+1 tells
// them apart.
func derivSeries(cs []market.Candle, interval time.Duration) []market.Deriv {
	ds := make([]market.Deriv, len(cs))
	for i, c := range cs {
		ds[i] = market.Deriv{Time: c.Time.Add(interval + 5*time.Second), OpenInterest: float64(i + 1), FundingRate: 0.0001, MarkPrice: c.Close}
	}
	return ds
}

func derivEngine(t *testing.T, s strategy.Strategy, yml string, series bool) (*Engine, *fake.Fake, []market.Candle, *events) {
	t.Helper()
	c, err := config.Parse([]byte(yml), s)
	if err != nil {
		t.Fatal(err)
	}
	cs, _ := market.ReadCSVFile("../../testdata/backtest_one_trade.csv")
	o := fake.Options{Paper: true, Cash: 10_000, QtyStep: 1, MinQty: 1, Shorts: true, Candles: map[string][]market.Candle{"AAA": cs}}
	if series {
		o.Deriv = map[string][]market.Deriv{"AAA": derivSeries(cs, c.Interval())}
	}
	f := fake.New(o)
	j := &events{}
	return New(c, s, f, exchange.Demo, j, func() time.Time { return cs[0].Time }), f, cs, j
}

// runAll steps the engine once per candle.
func runAll(t *testing.T, e *Engine, f *fake.Fake, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if i > 0 && !f.Advance() {
			t.Fatal("candles ran out")
		}
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

// Input.Deriv has one entry per candle: the closed candle's is the snapshot
// read in this very step, after its close and before the decision; the
// older candles keep the ones read in their own steps.
func TestInputDerivIsTheSnapshotReadInTheCandlesStep(t *testing.T) {
	var got [][]market.Deriv
	var lens []int
	s := derivUser{&scripted.Strategy{Decider: func(in strategy.Input) strategy.Decision {
		got = append(got, in.Deriv)
		lens = append(lens, len(in.Candles))
		return strategy.Decision{}
	}}, true}
	e, f, cs, _ := derivEngine(t, s, swapYAML, true)
	runAll(t, e, f, len(cs))
	for step, ds := range got {
		if len(ds) != lens[step] {
			t.Fatalf("step %d: %d snapshots for %d candles", step, len(ds), lens[step])
		}
		for k, d := range ds {
			candle := step - len(ds) + 1 + k // index of the candle this entry belongs to
			if d.OpenInterest != float64(candle+1) || d.Missing() || !d.Candle.Equal(cs[candle].Time) {
				t.Fatalf("step %d, candle %d: %+v, want snapshot %d bound to it", step, candle, d, candle)
			}
		}
	}
}

// A snapshot read outside its candle's step (here: before the close, inside
// the candle) is not bound: the candle is marked missing and the gap is
// journaled once per streak, not once per step.
func TestSnapshotOutsideTheStepIsMissingAndNoted(t *testing.T) {
	var last []market.Deriv
	s := derivUser{&scripted.Strategy{Decider: func(in strategy.Input) strategy.Decision { last = in.Deriv; return strategy.Decision{} }}, true}
	e, f, cs, j := derivEngine(t, s, swapYAML, false)
	stale := make([]market.Deriv, len(cs))
	for i, c := range cs {
		stale[i] = market.Deriv{Time: c.Time.Add(30 * time.Minute), OpenInterest: 1}
	}
	f.SetDeriv("AAA", stale)
	runAll(t, e, f, 3)
	if !last[len(last)-1].Missing() {
		t.Fatalf("a snapshot read before the close was bound: %+v", last)
	}
	if n := countDerivNotes(*j, "not read"); n != 1 {
		t.Fatalf("%d gap notes over 3 failing steps, want 1: %+v", n, *j)
	}
}

// Read failures are journaled once per failure streak, and the recovery too.
func TestDerivFailuresAreNotedOncePerStreak(t *testing.T) {
	s := derivUser{&scripted.Strategy{Decider: func(strategy.Input) strategy.Decision { return strategy.Decision{} }}, true}
	e, f, _, j := derivEngine(t, s, swapYAML, true)
	for i, fail := range []bool{true, true, false, true} {
		if i > 0 && !f.Advance() {
			t.Fatal("candles ran out")
		}
		if fail {
			f.FailNext("Snapshot", errors.New("429 too many requests"), false)
		}
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n, back := countDerivNotes(*j, "429"), countDerivNotes(*j, "read again"); n != 2 || back != 1 {
		t.Fatalf("failure notes %d (want 2), recovery notes %d (want 1): %+v", n, back, *j)
	}
}

func countDerivNotes(j events, part string) int {
	n := 0
	for _, ev := range j {
		if ev.Kind == journal.Derivatives && strings.Contains(ev.Reason, part) {
			n++
		}
	}
	return n
}

// A strategy without the interface (or one that does not need the data)
// gets no Input.Deriv; the engine reads no snapshot unless the recorder asks
// for it, and then only after the step's trading actions, so a slow or
// failing endpoint never delays them. A DerivativesUser reads before Decide.
func TestStrategiesWithoutDerivativesAreUntouched(t *testing.T) {
	for name, s := range map[string]strategy.Strategy{
		"no interface": &scripted.Strategy{},
		"not needed":   derivUser{&scripted.Strategy{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			var seen []strategy.Input
			var f *fake.Fake
			var readsAtDecide []int
			decide := func(in strategy.Input) strategy.Decision {
				seen = append(seen, in)
				readsAtDecide = append(readsAtDecide, f.Calls("Snapshot"))
				return strategy.Decision{}
			}
			switch v := s.(type) {
			case *scripted.Strategy:
				v.Decider = decide
			case derivUser:
				v.Decider = decide
			}
			var e *Engine
			e, f, _, _ = derivEngine(t, s, swapYAML, true)
			if err := e.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if f.Calls("Snapshot") != 0 || seen[0].Deriv != nil {
				t.Fatalf("snapshots read %d, Input.Deriv %v; want none", f.Calls("Snapshot"), seen[0].Deriv)
			}
			var rec []market.Deriv
			e.OnDeriv = func(sym string, d market.Deriv) { rec = append(rec, d) }
			f.Advance()
			if err := e.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(rec) != 1 || seen[1].Deriv != nil {
				t.Fatalf("recorded %v, Input.Deriv %v; want one recorded and none handed over", rec, seen[1].Deriv)
			}
			if readsAtDecide[1] != 0 {
				t.Fatalf("the recorder read before the decision (%d reads at Decide)", readsAtDecide[1])
			}
		})
	}
	var f *fake.Fake
	reads := -1
	user := derivUser{&scripted.Strategy{Decider: func(strategy.Input) strategy.Decision {
		reads = f.Calls("Snapshot")
		return strategy.Decision{}
	}}, true}
	var e *Engine
	e, f, _, _ = derivEngine(t, user, swapYAML, true)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatalf("a DerivativesUser decided after %d reads, want the step's snapshot read first", reads)
	}
}

// A strategy that needs derivatives data is refused at load on a spot
// market or on an adapter without the port, the paper simulator's data
// adapter included (fail closed).
func TestDerivativesUserIsRefusedWithoutThePort(t *testing.T) {
	user := derivUser{&scripted.Strategy{}, true}
	fk := func() *fake.Fake { return fake.New(fake.Options{Paper: true}) }
	cases := []struct {
		name string
		s    strategy.Strategy
		yml  string
		ex   exchange.Exchange
		want string // "" = accepted
	}{
		{"user, swap, port", user, swapYAML, fk(), ""},
		{"user, paper over a port", user, swapYAML, paper.New(fk(), 1000, time.Now, paper.Profile{}), ""},
		{"user, spot", user, scriptedYAML, fk(), "swap"},
		{"user, no port", user, swapYAML, noPort{fk()}, "does not read"},
		{"user, paper over no port", user, swapYAML, paper.New(noPort{fk()}, 1000, time.Now, paper.Profile{}), "does not read"},
		{"not needed, no port", derivUser{&scripted.Strategy{}, false}, swapYAML, noPort{fk()}, ""},
		{"no interface, no port", &scripted.Strategy{}, swapYAML, noPort{fk()}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := config.Parse([]byte(tc.yml), tc.s)
			if err != nil {
				t.Fatal(err)
			}
			err = CheckDerivatives(c, tc.s, tc.ex)
			if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("CheckDerivatives = %v, want %q", err, tc.want)
			}
		})
	}
}
