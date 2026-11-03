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
	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
)

// laggedQuote trades 1 above the candles: the demo's fills (testnet, or
// the simulator's quote) differ from the candles the signal reads.
type laggedQuote struct{ *fake.Fake }

func (x laggedQuote) Quote(ctx context.Context, env exchange.Env, symbol string) (exchange.Quote, error) {
	q, err := x.Fake.Quote(ctx, env, symbol)
	q.Bid, q.Ask, q.Last = q.Bid+1, q.Ask+1, q.Last+1
	return q, err
}

// Paper without a sandbox runs the simulator on a swap: a short at
// leverage 2 with its stop and estimated liquidation; the journal keeps the
// signal's price and the fill's; status says funding is not modelled, and
// status and journal show the simulator's profile.
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
	sim := paper.New(laggedQuote{data}, 10_000, tick.Now, paper.Profile{Name: "real", Costs: map[string]config.SimCosts{"AAA": {}}})
	dir := filepath.Join(t.TempDir(), ".scripted")
	Run(context.Background(), c, s, sim, exchange.Demo, tick, dir)

	evs, _, err := journal.Read(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var signal, fill float64
	start := "no start event"
	for _, e := range evs {
		switch {
		case e.Kind == Start:
			start = e.Reason
		case e.Kind == journal.Proposed:
			signal = e.Price
		case e.Kind == journal.Filled && e.Side == string(exchange.Sell):
			fill = e.Price
		}
	}
	if !strings.HasPrefix(start, "sim profile real") || !strings.Contains(start, "; sizing costs: sim profile real; AAA:") {
		t.Fatalf("journal start %q; want the simulator's profile and the unfloored sizing costs", start)
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
	if !strings.HasPrefix(st.PaperSim, "sim profile real") || !strings.HasPrefix(st.Sizing, "sizing costs: sim profile real; AAA:") {
		t.Fatalf("status sim = %q, sizing %q; want the active profile, unfloored", st.PaperSim, st.Sizing)
	}
	if len(st.Positions) != 1 {
		t.Fatalf("positions = %+v", st.Positions)
	}
	p := st.Positions[0]
	if p.PSide() != exchange.Short || p.Leverage != 2 || p.MarginMode != "isolated" || p.LiquidationPrice != 151.5 || p.StopPrice != 104 {
		t.Fatalf("position = %+v; want short, leverage 2 isolated, liquidation 151.5, stop 104", p)
	}
}

// S2: in every mode status and the journal's start event show the profile
// and the costs sizing counts; with real fills (here live) they are the
// active profile floored at the real one.
func TestStatusAndJournalShowTheSizingCosts(t *testing.T) {
	s := example.New()
	yml := strings.Replace(cfgYAML, "exchange: fake", "exchange: hyperliquid", 1) +
		"mode: mainnet\nvalidation_file: v.yml\nsim: {profile: optimistic, liquidity_class: major, profiles: {optimistic: {commission_pct: 0}}}\n"
	c, err := config.Parse([]byte(yml), s)
	if err != nil {
		t.Fatal(err)
	}
	cs, _ := market.ReadCSVFile("../../testdata/loop_crash.csv")
	f := fake.New(fake.Options{Cash: 10_000, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs}})
	dir := filepath.Join(t.TempDir(), ".example-sma-cross")
	Run(context.Background(), c, s, f, exchange.Real, &FakeTicker{Ex: f, Candles: cs}, dir)

	const want = "sizing costs: sim profile optimistic, floored at real (real fills); AAA: slippage 0.01%, taker fee 0.045%"
	st, err := ReadStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Sizing != want {
		t.Errorf("status sizing %q, want %q", st.Sizing, want)
	}
	evs, _, err := journal.Read(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) == 0 || evs[0].Kind != Start || !strings.Contains(evs[0].Reason, want) {
		t.Errorf("journal start %+v; want it carrying %q", evs, want)
	}
}

// crashQuote trades at 50, below any long's stop, from the quote numbered
// crashFrom on (1-based, counted once armed).
type crashQuote struct {
	*fake.Fake
	armed     *bool
	n         *int
	crashFrom int
}

func (x crashQuote) Quote(ctx context.Context, env exchange.Env, symbol string) (exchange.Quote, error) {
	q, err := x.Fake.Quote(ctx, env, symbol)
	if *x.armed {
		if *x.n++; *x.n >= x.crashFrom {
			q.Bid, q.Ask, q.Last = 50, 50, 50
		}
	}
	return q, err
}

// A stop the simulator sees hit by the final read at shutdown fills after
// the pessimistic delay (1.5 s), and state.json keeps that fill: the read
// waits for the delay instead of timing out and leaving the old position.
func TestShutdownSnapshotWaitsForTheProfileDelay(t *testing.T) {
	s := &scripted.Strategy{Decider: func(in strategy.Input) strategy.Decision {
		if in.Position == nil && len(in.Candles) == 2 {
			return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 96, Reason: "test"}
		}
		return strategy.Decision{}
	}}
	yml := strings.Replace(cfgYAML, "strategy: {period: 3, stop_pct: 4, target_pct: 5}\n", "", 1) +
		"sim: {profile: pessimistic, profiles: {pessimistic: {latency_ms: 1500}}}\n"
	c, err := config.Parse([]byte(yml), s)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for i := range 4 {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(i) * time.Hour), Open: 100, High: 100, Low: 100, Close: 100, Volume: 10})
	}
	data := fake.New(fake.Options{Paper: true, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs}})
	armed, n := false, 0
	tick := &FakeTicker{Ex: data, Candles: cs, OnStep: func(i int) { armed = i == len(cs)-1 }}
	sim := paper.New(crashQuote{data, &armed, &n, 2}, 10_000, tick.Now, paper.Profile{Name: c.Sim.Profile, Costs: c.SimCosts()})
	dir := filepath.Join(t.TempDir(), ".scripted")
	Run(context.Background(), c, s, sim, exchange.Demo, tick, dir)
	st, err := ReadStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Positions) != 0 {
		t.Fatalf("state positions %+v after the stop hit at shutdown; want none\n%s", st.Positions, kinds(t, dir))
	}
}
