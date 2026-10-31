package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/paper"
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

// derivSeries is one snapshot per candle, read as live reads it: at the step
// after the candle closed (close + 5s), so snapshot i lands inside candle
// i+1 and OpenInterest i+1 tells them apart.
func derivSeries(cs []market.Candle, interval time.Duration) []market.Deriv {
	ds := make([]market.Deriv, len(cs))
	for i, c := range cs {
		ds[i] = market.Deriv{Time: c.Time.Add(interval + 5*time.Second), OpenInterest: float64(i + 1), FundingRate: 0.0001, MarkPrice: c.Close}
	}
	return ds
}

func derivEngine(t *testing.T, s strategy.Strategy, yml string, series bool) (*Engine, *fake.Fake, []market.Candle) {
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
	return New(c, s, f, exchange.Demo, &events{}, func() time.Time { return cs[0].Time }), f, cs
}

// Input.Deriv has one entry per candle; the closed candle's is the snapshot
// read while it formed, never the one read after its close, and a candle with
// none is marked missing.
func TestInputDerivIsAlignedToTheClosedCandle(t *testing.T) {
	var got [][]market.Deriv
	var lens []int
	s := derivUser{&scripted.Strategy{Decider: func(in strategy.Input) strategy.Decision {
		got = append(got, in.Deriv)
		lens = append(lens, len(in.Candles))
		return strategy.Decision{}
	}}, true}
	e, f, cs := derivEngine(t, s, swapYAML, true)
	for i := range cs {
		if i > 0 && !f.Advance() {
			t.Fatal("candles ran out")
		}
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	for step, ds := range got {
		if len(ds) != lens[step] {
			t.Fatalf("step %d: %d snapshots for %d candles", step, len(ds), lens[step])
		}
		for k, d := range ds {
			candle := step - len(ds) + 1 + k // index of the candle this entry belongs to
			want := float64(candle)          // snapshot candle-1, OpenInterest candle; candle 0 has none
			if d.OpenInterest != want || d.Missing() != (candle == 0) {
				t.Fatalf("step %d, candle %d: open interest %v (missing %v), want %v", step, candle, d.OpenInterest, d.Missing(), want)
			}
			if !d.Missing() && d.Time.After(cs[candle].Time.Add(time.Hour)) {
				t.Fatalf("step %d, candle %d: snapshot read after the close", step, candle)
			}
		}
	}
}

// A strategy without the interface (or one that does not need the data)
// gets no Input.Deriv, and the engine reads no snapshot unless the recorder
// asks for it.
func TestStrategiesWithoutDerivativesAreUntouched(t *testing.T) {
	for name, s := range map[string]strategy.Strategy{
		"no interface": &scripted.Strategy{},
		"not needed":   derivUser{&scripted.Strategy{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			var seen []strategy.Input
			decide := func(in strategy.Input) strategy.Decision { seen = append(seen, in); return strategy.Decision{} }
			switch v := s.(type) {
			case *scripted.Strategy:
				v.Decider = decide
			case derivUser:
				v.Decider = decide
			}
			e, f, _ := derivEngine(t, s, swapYAML, true)
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
		})
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
