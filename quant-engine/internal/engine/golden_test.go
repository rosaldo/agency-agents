package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"math"
	"os"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
)

var update = flag.Bool("update", false, "rewrite the golden journals")

// waves is a deterministic price path with many crosses, trails, stops and
// targets.
func waves(n int) []market.Candle {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cs := make([]market.Candle, n)
	for i := range cs {
		x := float64(i)
		c := 100 + 8*math.Sin(x/7) + 3*math.Sin(x/2.3)
		o := 100 + 8*math.Sin((x-1)/7) + 3*math.Sin((x-1)/2.3)
		cs[i] = market.Candle{Time: t0.Add(time.Duration(i) * time.Hour), Open: o, High: max(o, c) + 0.6, Low: min(o, c) - 0.6, Close: c, Volume: 10}
	}
	return cs
}

// Regression: the example strategy, which never sets Decision.Fraction,
// journals byte for byte what the engine journaled before graduated
// entries existed (golden written at that commit).
func TestExampleJournalIsUnchanged(t *testing.T) {
	for name, yml := range map[string]string{
		"spot": "exchange: fake\nasset_class: crypto\ninterval: 1h\nsymbols: [AAA]\nrisk: {confirmed_by_user: true}\n" +
			"sim: {profile: pessimistic, profiles: {pessimistic: {slippage_pct: 0.05, commission_pct: 0.04}}}\n" +
			"strategy: {period: 5, stop_pct: 2, target_pct: 3, trail_period: 4}\n",
		"swap": "exchange: fake\nasset_class: crypto\ninterval: 1h\nsymbols: [AAA]\nrisk: {confirmed_by_user: true}\n" +
			"market: swap\nperp: {leverage: 2}\n" +
			"strategy: {period: 6, stop_pct: 1.5, target_pct: 0, trail_period: 3}\n",
	} {
		t.Run(name, func(t *testing.T) {
			s := example.New()
			c, err := config.Parse([]byte(yml), s)
			if err != nil {
				t.Fatal(err)
			}
			cs := waves(400)
			f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 0.001, MinQty: 0.001, Candles: map[string][]market.Candle{"AAA": cs},
				SlippagePct: c.SimCosts()["AAA"].SlippagePct, CommissionPct: c.SimCosts()["AAA"].CommissionPct, Shorts: c.Market == "swap"})
			var now time.Time
			j := &events{}
			e := New(c, s, f, exchange.Demo, j, func() time.Time { return now })
			e.Simulated = true
			for i, candle := range cs {
				now = candle.Time
				if i > 0 {
					f.Advance()
				}
				if err := e.Step(context.Background()); err != nil {
					t.Fatalf("step %d: %v", i, err)
				}
			}
			var got bytes.Buffer
			enc := json.NewEncoder(&got)
			for _, ev := range *j {
				if err := enc.Encode(ev); err != nil {
					t.Fatal(err)
				}
			}
			path := "../../testdata/example_journal_" + name + ".golden.jsonl"
			if *update {
				if err := os.WriteFile(path, got.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want) {
				t.Fatalf("journal differs from %s (%d bytes, want %d)", path, got.Len(), len(want))
			}
		})
	}
}
