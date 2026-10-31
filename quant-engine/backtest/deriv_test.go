package backtest

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/loop"
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

type derivUser struct{ *scripted.Strategy }

func (derivUser) NeedsDerivatives() bool { return true }

const derivYAML = parityYAML + "market: swap\n"

// oiFalls enters long when open interest fell from the previous candle to
// this one, and leaves when it rose or this candle has no snapshot; seen logs
// every snapshot it was handed.
func oiFalls(seen *[]string) func(strategy.Input) strategy.Decision {
	return func(in strategy.Input) strategy.Decision {
		var line []string
		for _, d := range in.Deriv {
			line = append(line, fmt.Sprintf("%v/%v/%v", d.Missing(), d.HasOpenInterest, d.OpenInterest))
		}
		*seen = append(*seen, in.Now.Format(time.RFC3339)+" "+strings.Join(line, " "))
		n := len(in.Deriv)
		if n < 2 {
			return strategy.Decision{}
		}
		last, prev := in.Deriv[n-1], in.Deriv[n-2]
		switch {
		case in.Position == nil && last.HasOpenInterest && prev.HasOpenInterest && last.OpenInterest < prev.OpenInterest:
			c := in.Candles[len(in.Candles)-1].Close
			return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: c * 0.5, Reason: "oi falls"}
		case in.Position != nil && (!last.HasOpenInterest || !prev.HasOpenInterest || last.OpenInterest > prev.OpenInterest):
			return strategy.Decision{Action: strategy.Exit, Reason: "oi rises"}
		}
		return strategy.Decision{}
	}
}

// Replaying the file the loop recorded hands the strategy the very snapshots
// the loop's steps had, gaps included (a failed read, one read at the close),
// so it decides alike: same Decide calls, same snapshots, same trades.
func TestBacktestReplaysTheRecordingAsTheLoopDecided(t *testing.T) {
	cs := parityCandles()
	ds := make([]market.Deriv, len(cs))
	for i, oi := range []float64{10, 9, 8, 9, 10, 9, 8, 7, 6, 9, 10} {
		ds[i] = market.Deriv{Time: cs[i].Time.Add(time.Hour + 5*time.Second), OpenInterest: oi, HasOpenInterest: true, FundingRate: -0.00001, MarkPrice: cs[i].Close}
	}
	ds[4].Time = time.Time{}                               // the read failed
	ds[6].Time = cs[6].Time.Add(time.Hour)                 // read at the close: stale for the step
	ds[10].OpenInterest, ds[10].HasOpenInterest = 0, false // read, but the venue reported no open interest

	var loopSeen, btSeen []string
	ls := derivUser{&scripted.Strategy{Decider: oiFalls(&loopSeen)}}
	c, err := config.Parse([]byte(derivYAML), ls)
	if err != nil {
		t.Fatal(err)
	}
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 1, MinQty: 1, Shorts: true,
		Candles: map[string][]market.Candle{"AAA": cs}, Deriv: map[string][]market.Deriv{"AAA": ds}})
	dir := filepath.Join(t.TempDir(), ".scripted")
	if r := loop.Run(context.Background(), c, ls, f, exchange.Demo, &loop.FakeTicker{Ex: f, Candles: cs}, dir); r != "candles exhausted" {
		t.Fatalf("loop: %q", r)
	}
	rec, err := market.ReadDerivCSVFile(filepath.Join(dir, "deriv-paper.csv"))
	if err != nil {
		t.Fatal(err)
	}

	bs := derivUser{&scripted.Strategy{Decider: oiFalls(&btSeen)}}
	res, err := Run(context.Background(), bs, []byte(derivYAML), Input{Candles: cs, Capital: 10_000, QtyStep: 1, MinQty: 1, Deriv: rec["AAA"]})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Trades) < 2 || !strings.Contains(strings.Join(loopSeen, "\n"), "true/false/0") || !strings.Contains(strings.Join(loopSeen, "\n"), "false/false/0") {
		t.Fatalf("trades %+v, snapshots %q: the case must trade and hold gaps", res.Trades, loopSeen)
	}
	if !slices.Equal(btSeen, loopSeen) {
		t.Fatalf("snapshots differ:\nbacktest %q\nloop     %q", btSeen, loopSeen)
	}
	if !slices.Equal(bs.Log, ls.Log) {
		t.Fatalf("Decide calls differ:\nbacktest %q\nloop     %q", bs.Log, ls.Log)
	}
	if n := res.Summary.CandlesWithoutDerivatives; n == nil || *n != 2 {
		t.Fatalf("candles without derivatives data = %v, want the 2 recorded gaps counted", n)
	}
}

// Funding history carries no open interest, and says so: a rule on open
// interest never sees one, so it never passes on the funding alone.
func TestFundingOnlyReplayReportsNoOpenInterest(t *testing.T) {
	cs := parityCandles()
	var rows []string
	for h := range len(cs) + 1 {
		rows = append(rows, fmt.Sprintf(`{"coin":"AAA","fundingRate":"-0.0001","premium":"0","time":%d}`, cs[0].Time.Add(time.Duration(h)*time.Hour+21*time.Millisecond).UnixMilli()))
	}
	ds, err := market.ReadFundingHistory(strings.NewReader("["+strings.Join(rows, ",")+"]"), "AAA", cs, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	s := derivUser{&scripted.Strategy{Decider: oiFalls(&seen)}}
	res, err := Run(context.Background(), s, []byte(derivYAML), Input{Candles: cs, Capital: 10_000, QtyStep: 1, MinQty: 1, Deriv: ds})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Trades) != 0 || strings.Contains(strings.Join(seen, " "), "/true/") || len(seen) != len(cs) {
		t.Fatalf("trades %+v, snapshots %q; want no open interest seen and no entry", res.Trades, seen)
	}
}

// A strategy that needs derivatives data is refused unless the data spans
// every candle: it never runs as if its filter passed.
func TestBacktestRefusesADerivativesUserWithoutDataForThePeriod(t *testing.T) {
	cs := parityCandles()
	var ds []market.Deriv
	var late []market.Deriv // every candle but the first
	for i, k := range cs {
		d := market.Deriv{Time: k.Time.Add(time.Hour + 5*time.Second), Candle: k.Time, OpenInterest: 1, HasOpenInterest: true}
		if i < 6 {
			ds = append(ds, d)
		}
		if i > 0 {
			late = append(late, d)
		}
	}
	for name, tc := range map[string]struct {
		yml  string
		ds   []market.Deriv
		want string
	}{
		"none":         {derivYAML, nil, "has none for AAA"},
		"starts late":  {derivYAML, late, "covers 2026-01-01T01:00:00Z to 2026-01-01T10:00:00Z"},
		"part":         {derivYAML, ds, "covers 2026-01-01T00:00:00Z to 2026-01-01T05:00:00Z"},
		"spot":         {parityYAML, ds, "only swap markets"},
		"out of steps": {derivYAML, []market.Deriv{{Time: cs[0].Time, Candle: cs[0].Time}, {Time: cs[10].Time, Candle: cs[10].Time}}, "has none for AAA"},
	} {
		t.Run(name, func(t *testing.T) {
			s := derivUser{&scripted.Strategy{Decider: func(strategy.Input) strategy.Decision { return strategy.Decision{} }}}
			_, err := Run(context.Background(), s, []byte(tc.yml), Input{Candles: cs, Capital: 10_000, QtyStep: 1, MinQty: 1, Deriv: tc.ds})
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(s.Log) != 0 {
				t.Fatalf("err = %v after %d decisions, want a refusal saying %q", err, len(s.Log), tc.want)
			}
		})
	}
}
