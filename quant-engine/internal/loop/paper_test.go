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
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/paper"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// laggedQuote trades 1 above the candles: the demo's fills (testnet, or
// the simulator's quote) differ from the candles the signal reads (O4).
type laggedQuote struct{ *fake.Fake }

func (x laggedQuote) Quote(ctx context.Context, env exchange.Env, symbol string) (exchange.Quote, error) {
	q, err := x.Fake.Quote(ctx, env, symbol)
	q.Bid, q.Ask, q.Last = q.Bid+1, q.Ask+1, q.Last+1
	return q, err
}

// T12: paper without a sandbox runs the simulator on a swap: a short at
// leverage 2 with its stop and estimated liquidation; the journal keeps the
// signal's price and the fill's; status says funding is not modelled.
func TestPaperPerpRunOnTheSimulator(t *testing.T) {
	s := &scripted.Strategy{Decider: func(in strategy.Input) strategy.Decision {
		if in.Position == nil && len(in.Candles) == 2 {
			return strategy.Decision{Action: strategy.Enter, Side: strategy.Short, Stop: 104, Reason: "test"}
		}
		return strategy.Decision{}
	}}
	yml := strings.Replace(cfgYAML, "strategy: {period: 3, stop_pct: 4, target_pct: 5}\n", "", 1) +
		"market: swap\nperp: {leverage: 2, allow_short: true}\n"
	c, err := config.Parse([]byte(yml), s)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for i := range 4 {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(i) * time.Hour), Open: 100, High: 100, Low: 100, Close: 100, Volume: 10})
	}
	data := fake.New(fake.Options{Paper: true, Shorts: true, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs}})
	tick := &FakeTicker{Ex: data, Candles: cs}
	sim := paper.New(laggedQuote{data}, 10_000, tick.Now)
	dir := filepath.Join(t.TempDir(), ".scripted")
	Run(context.Background(), c, s, sim, exchange.Demo, tick, dir)

	evs, _, err := journal.Read(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var signal, fill float64
	for _, e := range evs {
		switch {
		case e.Kind == journal.Proposed:
			signal = e.Price
		case e.Kind == journal.Filled && e.Side == string(exchange.Sell):
			fill = e.Price
		}
	}
	if signal != 100 || fill != 101 {
		t.Fatalf("journal: signal %v fill %v, want 100 and 101\n%s", signal, fill, kinds(t, dir))
	}
	st, err := ReadStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.Funding, "funding not modelled") {
		t.Fatalf("status funding = %q", st.Funding)
	}
	if len(st.Positions) != 1 {
		t.Fatalf("positions = %+v", st.Positions)
	}
	p := st.Positions[0]
	if p.PSide() != exchange.Short || p.Leverage != 2 || p.MarginMode != "isolated" || p.LiquidationPrice != 151.5 || p.StopPrice != 104 {
		t.Fatalf("position = %+v; want short, leverage 2 isolated, liquidation 151.5, stop 104", p)
	}
}
