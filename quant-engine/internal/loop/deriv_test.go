package loop

import (
	"context"
	"os"
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

const derivYAML = `
exchange: fake
asset_class: crypto
interval: 1h
symbols: [AAA]
market: swap
risk: {confirmed_by_user: true}
`

type derivUser struct{ *scripted.Strategy }

func (derivUser) NeedsDerivatives() bool { return true }

type noPort struct{ exchange.Exchange }

// derivRun runs the loop to the end of the candles on a fresh fake whose
// snapshots are read one per step, at close + 5s.
func derivRun(t *testing.T, s strategy.Strategy, yml, dir string, wrap func(*fake.Fake) exchange.Exchange) string {
	t.Helper()
	c, err := config.Parse([]byte(yml), s)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := market.ReadCSVFile("../../testdata/backtest_one_trade.csv")
	if err != nil {
		t.Fatal(err)
	}
	ds := make([]market.Deriv, len(cs))
	for i, k := range cs {
		ds[i] = market.Deriv{Time: k.Time.Add(time.Hour + 5*time.Second), OpenInterest: float64(1000 + i), FundingRate: -0.00001, MarkPrice: k.Close}
	}
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 1, MinQty: 1, Shorts: true,
		Candles: map[string][]market.Candle{"AAA": cs}, Deriv: map[string][]market.Deriv{"AAA": ds}})
	var ex exchange.Exchange = f
	if wrap != nil {
		ex = wrap(f)
	}
	return Run(context.Background(), c, s, ex, exchange.Demo, &FakeTicker{Ex: f, Candles: cs}, dir)
}

func none(strategy.Input) strategy.Decision { return strategy.Decision{} }

// Every level records the swap snapshots to deriv-<level>.csv in the state
// directory, for any strategy; a restart replaying the same snapshots writes
// no row twice.
func TestRecorderWritesEachSnapshotOnceAcrossRestarts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if r := derivRun(t, &scripted.Strategy{Decider: none}, derivYAML, dir, nil); r != "candles exhausted" {
		t.Fatalf("reason = %q", r)
	}
	path := filepath.Join(dir, "deriv-paper.csv")
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := market.ReadDerivCSVFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if rows := got["AAA"]; len(rows) != 6 || rows[0].OpenInterest != 1000 || rows[5].OpenInterest != 1005 {
		t.Fatalf("recorded %+v, want the 6 snapshots in order", rows)
	}
	if r := derivRun(t, &scripted.Strategy{Decider: none}, derivYAML, dir, nil); r != "candles exhausted" {
		t.Fatalf("restart reason = %q", r)
	}
	if again, _ := os.ReadFile(path); string(again) != string(first) {
		t.Fatalf("restart changed the file:\n%s\nwas:\n%s", again, first)
	}
}

// After a restart the strategy gets the snapshots recorded before it.
func TestRestartSeedsTheSnapshotsRecordedBefore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	derivRun(t, &scripted.Strategy{Decider: none}, derivYAML, dir, nil)
	var last market.Deriv
	s := derivUser{&scripted.Strategy{Decider: func(in strategy.Input) strategy.Decision {
		last = in.Deriv[len(in.Deriv)-1]
		return strategy.Decision{}
	}}}
	derivRun(t, s, derivYAML, dir, nil) // replays the same candles: every snapshot is already recorded
	// snapshot 4, read inside the last candle (5 was read after its close)
	if last.OpenInterest != 1004 {
		t.Fatalf("last candle's snapshot = %+v, want the recorded one (1004)", last)
	}
}

func TestSpotRecordsNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	derivRun(t, &scripted.Strategy{Decider: none}, strings.Replace(derivYAML, "market: swap\n", "", 1), dir, nil)
	if _, err := os.Stat(filepath.Join(dir, "deriv-paper.csv")); !os.IsNotExist(err) {
		t.Fatalf("spot left a derivatives file: %v", err)
	}
}

// A strategy that needs derivatives data on an adapter without the port is
// refused at load: no step runs.
func TestRunRefusesADerivativesUserWithoutThePort(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	steps := 0
	s := derivUser{&scripted.Strategy{Decider: func(strategy.Input) strategy.Decision { steps++; return strategy.Decision{} }}}
	r := derivRun(t, s, derivYAML, dir, func(f *fake.Fake) exchange.Exchange { return noPort{f} })
	if !strings.HasPrefix(r, "refused:") || !strings.Contains(r, "derivatives") || steps != 0 {
		t.Fatalf("reason = %q after %d steps", r, steps)
	}
}
