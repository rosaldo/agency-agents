package loop

import (
	"context"
	"encoding/json"
	"fmt"
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

// status shows risk.max_open_positions in limits and the slots left, and
// leaves its JSON as it was when the limit is off.
func TestStatusShowsTheOpenPositionsLimitAndFreeSlots(t *testing.T) {
	positions := []exchange.Position{{Symbol: "AAA", Qty: 1}, {Symbol: "BBB", Qty: 2}, {Symbol: "CCC"}}
	for _, tc := range []struct {
		limit int
		want  string // in the status JSON
		not   string
	}{
		{3, `"max_open_positions":3`, ""},
		{3, `"free_slots":1`, ""},
		{2, `"free_slots":0`, ""},
		{1, `"free_slots":0`, ""}, // more open than the limit (lowered since): no slot, never below 0
		{0, `"min_demo_days":0}`, "free_slots"},
		{0, `"limits"`, "max_open_positions"},
	} {
		dir := t.TempDir()
		if err := SaveState(dir, State{Mode: "paper", Level: "paper", Limits: Limits{MaxOpenPositions: tc.limit}, Positions: positions}); err != nil {
			t.Fatal(err)
		}
		s, err := ReadStatus(dir)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(s)
		if !strings.Contains(string(b), tc.want) || tc.not != "" && strings.Contains(string(b), tc.not) {
			t.Fatalf("limit %d: status %s, want %s and no %q", tc.limit, b, tc.want, tc.not)
		}
	}
}

// A symbol taken out of symbols: with its position still open keeps its
// slot: the run after the change, on the same account and state, opens
// nothing while the limit is full, and status agrees with the engine.
func TestARemovedSymbolsOpenPositionKeepsItsSlot(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for i := 0; i < 12; i++ {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(i) * time.Hour), Open: 100, High: 100, Low: 100, Close: 100, Volume: 10})
	}
	s := &scripted.Strategy{Decider: func(in strategy.Input) strategy.Decision {
		if in.Position != nil {
			return strategy.Decision{Reason: "hold"}
		}
		return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 50, Reason: "go"}
	}}
	cfg := func(syms string) *config.Config {
		c, err := config.Parse([]byte(fmt.Sprintf("exchange: fake\nasset_class: crypto\nnews_guard: none\ninterval: 1h\nsymbols: [%s]\nrisk: {confirmed_by_user: true, max_open_positions: 1}\n", syms)), s)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	f := fake.New(fake.Options{Paper: true, Cash: 100_000, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs, "BBB": cs, "CCC": cs}})
	dir := filepath.Join(t.TempDir(), ".scripted")
	tick := &FakeTicker{Ex: f, Candles: cs}
	stopAt := func(n int) {
		tick.OnStep = func(i int) {
			if i == n {
				_ = RequestStop(dir, false)
			}
		}
	}
	stopAt(2)
	Run(context.Background(), cfg("AAA"), s, f, exchange.Demo, tick, dir)
	stopAt(5)
	Run(context.Background(), cfg("BBB, CCC"), s, f, exchange.Demo, tick, dir)
	ps, _ := f.Positions(context.Background(), exchange.Demo)
	var open []string
	for _, p := range ps {
		if p.Qty > 0 {
			open = append(open, p.Symbol)
		}
	}
	if len(open) != 1 || open[0] != "AAA" {
		t.Fatalf("open on the account %v with max_open_positions 1, want AAA alone", open)
	}
	evs, _, _ := journal.Read(filepath.Join(dir, "journal.jsonl"))
	noSlot := 0
	for _, e := range evs {
		if strings.Contains(e.Reason, "no slot: 1 of 1 positions open") {
			noSlot++
		}
	}
	if noSlot == 0 {
		t.Fatal("the second run journaled no entry without a slot")
	}
	st, err := ReadStatus(dir)
	if err != nil || st.FreeSlots == nil || *st.FreeSlots != 0 {
		t.Fatalf("status free_slots %v (%v), want 0", st.FreeSlots, err)
	}
}
