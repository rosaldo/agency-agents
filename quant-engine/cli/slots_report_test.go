package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/engine"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// An entry left out for want of a slot shows in the report as a signal the
// engine refused, with its reason; its evaluation keeps the strategy's
// reason, so SEEN lists no evaluation without one.
func TestReportShowsAnEntryWithoutASlot(t *testing.T) {
	s := &scripted.Strategy{Decider: func(in strategy.Input) strategy.Decision {
		if in.Position != nil {
			return strategy.Decision{Reason: "holding"}
		}
		return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 90, Priority: map[string]float64{"AAA": 1, "BBB": 2}[in.Symbol], Reason: "signal"}
	}}
	c, err := config.Parse([]byte("exchange: fake\nasset_class: crypto\ninterval: 1h\nsymbols: [AAA, BBB]\nrisk: {confirmed_by_user: true, max_open_positions: 1}\n"), s)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for i := 0; i < 4; i++ {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(i) * time.Hour), Open: 100, High: 100, Low: 100, Close: 100, Volume: 10})
	}
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs, "BBB": cs}})
	dir := t.TempDir()
	j, err := journal.Open(dir, "paper", "paper", "feedc0de1234")
	if err != nil {
		t.Fatal(err)
	}
	var now time.Time
	e := engine.New(c, s, f, exchange.Demo, j, func() time.Time { return now })
	e.Evaluations = true
	for i, candle := range cs {
		now = candle.Time.Add(time.Hour + time.Second)
		if i > 0 {
			f.Advance()
		}
		if err := e.Step(context.Background()); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	j.Close()
	b, err := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	out := reportOn(t, string(b), nil, "--from", "2026-01-01")
	if !strings.Contains(out, "refused by the engine: no slot: 1 of 1 positions open") || strings.Contains(out, "without a reason") {
		t.Fatalf("report:\n%s", out)
	}
	if _, analyzed, _ := strings.Cut(out, "ANALYZED"); !strings.Contains(analyzed, "  AAA\n") || !strings.Contains(analyzed, "enter long") {
		t.Fatalf("ANALYZED lacks AAA's refused entry:\n%s", out)
	}
}
