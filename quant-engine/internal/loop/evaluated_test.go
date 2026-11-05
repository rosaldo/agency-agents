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
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// A live run journals one evaluated event per decided candle, with the
// strategy's action and reason as given (empty when it gave none) and the
// candle's close time; the entry's evaluation carries the values that
// triggered it.
func TestLoopJournalsEveryEvaluation(t *testing.T) {
	s := &scripted.Strategy{Decider: func(in strategy.Input) strategy.Decision {
		switch {
		case in.Position != nil:
			return strategy.Decision{}
		case in.Now.Hour() == 2:
			return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 90, Reason: "drop 3.1% >= 3%"}
		case in.Now.Hour() == 1:
			return strategy.Decision{Reason: "drop 0.0% < 3%"}
		}
		return strategy.Decision{}
	}}
	yml := strings.Replace(cfgYAML, "strategy: {period: 3, stop_pct: 4, target_pct: 5}\n", "", 1)
	c, err := config.Parse([]byte(yml), s)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for i := range 4 {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(i) * time.Hour), Open: 100, High: 100, Low: 100, Close: 100, Volume: 10})
	}
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs}})
	dir := filepath.Join(t.TempDir(), ".scripted")
	Run(context.Background(), c, s, f, exchange.Demo, &FakeTicker{Ex: f, Candles: cs}, dir)

	evs, _, err := journal.Read(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range evs {
		if e.Kind == journal.Evaluated {
			got = append(got, e.CandleClose.Format("15:04")+" "+e.Action+" "+e.Side+" "+e.Reason)
		}
	}
	want := []string{"01:00 none  ", "02:00 none  drop 0.0% < 3%", "03:00 enter long drop 3.1% >= 3%", "04:00 none  "}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("evaluated events:\n%q\nwant\n%q\njournal: %s", got, want, kinds(t, dir))
	}
}
