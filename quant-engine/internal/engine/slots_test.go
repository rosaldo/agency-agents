package engine

import (
	"context"
	"fmt"
	"slices"
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

// slotRun is a fake account of syms on flat candles at 100 (the k-th symbol
// gaps to 40 from candle gapAt on, when gapAt > 0, through a stop at 50), with
// risk.max_open_positions limit; decide answers per symbol and step.
type slotRun struct {
	t     *testing.T
	e     *Engine
	f     *fake.Fake
	j     *events
	cs    []market.Candle
	step  int
	c     *config.Config
	s     *scripted.Strategy
	limit int
}

func newSlotRun(t *testing.T, limit int, syms []string, gap map[string]int, decide func(step int, in strategy.Input) strategy.Decision) *slotRun {
	t.Helper()
	r := &slotRun{t: t, j: &events{}, limit: limit}
	r.s = &scripted.Strategy{Decider: func(in strategy.Input) strategy.Decision { return decide(r.step, in) }}
	yml := fmt.Sprintf("exchange: fake\nasset_class: crypto\ninterval: 1h\nsymbols: [%s]\nrisk: {confirmed_by_user: true, max_open_positions: %d}\n",
		strings.Join(syms, ", "), limit)
	c, err := config.Parse([]byte(yml), r.s)
	if err != nil {
		t.Fatal(err)
	}
	r.c = c
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	candles := map[string][]market.Candle{}
	for _, sym := range syms {
		var cs []market.Candle
		for i := 0; i < 10; i++ {
			px := 100.0
			if g := gap[sym]; g > 0 && i >= g {
				px = 40
			}
			cs = append(cs, market.Candle{Time: t0.Add(time.Duration(i) * time.Hour), Open: px, High: px, Low: px, Close: px, Volume: 10})
		}
		candles[sym], r.cs = cs, cs
	}
	r.f = fake.New(fake.Options{Paper: true, Cash: 100_000, QtyStep: 1, MinQty: 1, Candles: candles})
	r.f.Advance()
	r.e = New(c, r.s, r.f, exchange.Demo, r.j, r.now)
	return r
}

func (r *slotRun) now() time.Time { return r.cs[r.step+1].Time }

// next steps the engine once on the next candle.
func (r *slotRun) next() {
	r.t.Helper()
	if r.step > 0 {
		r.f.Advance()
	}
	if err := r.e.Step(context.Background()); err != nil {
		r.t.Fatalf("step %d: %v", r.step, err)
	}
	r.step++
	ps, _ := r.f.Positions(context.Background(), exchange.Demo)
	r.e.Remember(ps)
}

// open lists the symbols holding a position.
func (r *slotRun) open() []string {
	ps, _ := r.f.Positions(context.Background(), exchange.Demo)
	var out []string
	for _, p := range ps {
		if p.Qty > 0 {
			out = append(out, p.Symbol)
		}
	}
	slices.Sort(out)
	return out
}

func (r *slotRun) noSlot() []string {
	var out []string
	for _, ev := range *r.j {
		if ev.Kind == journal.Rejected && strings.HasPrefix(ev.Reason, "signal skipped: no slot: ") {
			out = append(out, ev.Symbol+" "+strings.TrimPrefix(ev.Reason, "signal skipped: "))
		}
	}
	return out
}

func enterLongAt(in strategy.Input, priority float64) strategy.Decision {
	return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: in.Candles[len(in.Candles)-1].Close - 50, Priority: priority, Reason: "test"}
}

var ten = []string{"S0", "S1", "S2", "S3", "S4", "S5", "S6", "S7", "S8", "S9"}

// Ten entries on one candle and five slots: the five of highest priority
// open, the other five are journaled as without a slot.
func TestTenSignalsFiveSlotsTakeTheStrongest(t *testing.T) {
	prio := map[string]float64{"S0": 1, "S1": 9, "S2": 3, "S3": 7, "S4": 2, "S5": 10, "S6": 4, "S7": 8, "S8": 0.5, "S9": 6}
	r := newSlotRun(t, 5, ten, nil, func(_ int, in strategy.Input) strategy.Decision {
		if in.Position != nil {
			return strategy.Decision{Reason: "hold"}
		}
		return enterLongAt(in, prio[in.Symbol])
	})
	r.next()
	if got, want := r.open(), []string{"S1", "S3", "S5", "S7", "S9"}; !slices.Equal(got, want) {
		t.Fatalf("open %v, want the five strongest %v", got, want)
	}
	if got := r.noSlot(); len(got) != 5 || got[0] != "S6 no slot: 5 of 5 positions open" {
		t.Fatalf("no-slot events %q, want 5, the strongest left out (S6) first", got)
	}
}

// Equal priorities keep the order of symbols:.
func TestTiedSignalsKeepTheSymbolsOrder(t *testing.T) {
	prio := map[string]float64{"S7": 5, "S2": 5, "S9": 5}
	r := newSlotRun(t, 3, ten, nil, func(_ int, in strategy.Input) strategy.Decision {
		if in.Position != nil {
			return strategy.Decision{Reason: "hold"}
		}
		return enterLongAt(in, prio[in.Symbol]) // S7, S2, S9 tie on top; the rest tie at 0
	})
	r.next()
	if got, want := r.open(), []string{"S2", "S7", "S9"}; !slices.Equal(got, want) {
		t.Fatalf("open %v, want %v", got, want)
	}
	r2 := newSlotRun(t, 2, ten, nil, func(_ int, in strategy.Input) strategy.Decision { return enterLongAt(in, 0) })
	r2.next()
	if got, want := r2.open(), []string{"S0", "S1"}; !slices.Equal(got, want) {
		t.Fatalf("all tied: open %v, want the first two of symbols: %v", got, want)
	}
}

// An entry refused before its order (here a stop above the entry) takes no
// slot: the next signal gets it.
func TestARefusedEntryUsesNoSlot(t *testing.T) {
	r := newSlotRun(t, 1, []string{"AAA", "BBB"}, nil, func(_ int, in strategy.Input) strategy.Decision {
		d := enterLongAt(in, 1)
		if in.Symbol == "AAA" {
			d.Priority, d.Stop = 2, 200
		}
		return d
	})
	r.next()
	if got := r.open(); !slices.Equal(got, []string{"BBB"}) {
		t.Fatalf("open %v, want BBB after AAA was refused", got)
	}
}

// A further tranche of an open position takes no slot: it goes out with the
// slots full, while a new entry on that candle does not.
func TestAnAddPassesWithTheSlotsFull(t *testing.T) {
	r := newSlotRun(t, 1, []string{"AAA", "BBB"}, nil, func(step int, in strategy.Input) strategy.Decision {
		switch {
		case in.Symbol == "AAA" && in.Position == nil:
			d := enterLongAt(in, 0)
			d.Fraction = 0.5
			return d
		case in.Symbol == "AAA":
			return strategy.Decision{Action: strategy.Add, Side: strategy.Long, Fraction: 0.5, Reason: "add"}
		case step > 0:
			return enterLongAt(in, 9)
		}
		return strategy.Decision{Reason: "wait"}
	})
	r.next()
	ps, _ := r.f.Positions(context.Background(), exchange.Demo)
	first := ps[0].Qty
	r.next()
	ps, _ = r.f.Positions(context.Background(), exchange.Demo)
	if got := r.open(); !slices.Equal(got, []string{"AAA"}) || ps[0].Qty <= first {
		t.Fatalf("open %v qty %v (was %v), want AAA alone and grown by the add", got, ps[0].Qty, first)
	}
	if got := r.noSlot(); !slices.Equal(got, []string{"BBB no slot: 1 of 1 positions open"}) {
		t.Fatalf("no-slot events %q", got)
	}
}

// A position closed by the strategy's exit frees its slot on the next
// candle, not on the candle of the exit; one closed by its exchange stop
// frees it on the step that sees it gone.
func TestAClosedPositionFreesItsSlotOnTheNextCandle(t *testing.T) {
	r := newSlotRun(t, 1, []string{"AAA", "BBB"}, nil, func(step int, in strategy.Input) strategy.Decision {
		switch {
		case in.Symbol == "AAA" && step == 0:
			return enterLongAt(in, 0)
		case in.Symbol == "AAA" && in.Position != nil:
			return strategy.Decision{Action: strategy.Exit, Reason: "rule"}
		case in.Symbol == "BBB" && step > 0 && in.Position == nil:
			return enterLongAt(in, 0)
		}
		return strategy.Decision{Reason: "wait"}
	})
	r.next() // AAA opens
	r.next() // AAA exits; BBB has no slot yet
	if got := r.open(); len(got) != 0 {
		t.Fatalf("open %v after the exit, want none", got)
	}
	if got := r.noSlot(); !slices.Equal(got, []string{"BBB no slot: 1 of 1 positions open"}) {
		t.Fatalf("no-slot events %q", got)
	}
	r.next()
	if got := r.open(); !slices.Equal(got, []string{"BBB"}) {
		t.Fatalf("open %v, want BBB in the freed slot", got)
	}

	// the exchange stop: AAA gaps through it on candle 3 (step 2)
	g := newSlotRun(t, 1, []string{"AAA", "BBB"}, map[string]int{"AAA": 3}, func(step int, in strategy.Input) strategy.Decision {
		switch {
		case in.Symbol == "AAA" && step == 0:
			return enterLongAt(in, 0)
		case in.Symbol == "BBB" && in.Position == nil && step > 0:
			return enterLongAt(in, 0)
		}
		return strategy.Decision{Reason: "wait"}
	})
	g.next() // AAA opens
	g.next() // BBB: no slot
	g.next() // AAA's stop filled on this candle's open: BBB enters
	if got := g.open(); !slices.Equal(got, []string{"BBB"}) {
		t.Fatalf("open %v, want BBB once AAA's stop closed it", got)
	}
}

// After a restart the positions the exchange still holds fill the slots.
func TestRestartCountsTheRestoredPositions(t *testing.T) {
	r := newSlotRun(t, 1, []string{"AAA", "BBB"}, nil, func(step int, in strategy.Input) strategy.Decision {
		if in.Position == nil && (in.Symbol == "AAA" || step > 0) {
			return enterLongAt(in, 0)
		}
		return strategy.Decision{Reason: "hold"}
	})
	r.next()
	ps, _ := r.f.Positions(context.Background(), exchange.Demo)
	r.e = New(r.c, r.s, r.f, exchange.Demo, r.j, r.now) // the restart: a new engine on the same account
	r.e.Restore(r.e.Targets(), ps, nil, nil)
	r.next()
	if got := r.open(); !slices.Equal(got, []string{"AAA"}) {
		t.Fatalf("open %v after the restart, want AAA alone", got)
	}
	if got := r.noSlot(); !slices.Equal(got, []string{"BBB no slot: 1 of 1 positions open"}) {
		t.Fatalf("no-slot events %q", got)
	}
}

// Without the limit every entry goes out, whatever the priorities.
func TestNoLimitEntersEverySignal(t *testing.T) {
	r := newSlotRun(t, 0, ten, nil, func(_ int, in strategy.Input) strategy.Decision {
		if in.Position != nil {
			return strategy.Decision{Reason: "hold"}
		}
		return enterLongAt(in, float64(len(in.Symbol)))
	})
	r.next()
	if got := r.open(); len(got) != 10 || len(r.noSlot()) != 0 {
		t.Fatalf("open %v, no-slot %q; want all ten and none refused", got, r.noSlot())
	}
}
