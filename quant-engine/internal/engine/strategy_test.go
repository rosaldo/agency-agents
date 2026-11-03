package engine

import (
	"context"
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

type events []journal.Event

func (e *events) Append(ev journal.Event) error { *e = append(*e, ev); return nil }

func (e events) rejected() string {
	var rs []string
	for _, ev := range e {
		if ev.Kind == journal.Rejected {
			rs = append(rs, ev.Reason)
		}
	}
	return strings.Join(rs, "; ")
}

const scriptedYAML = `
exchange: fake
asset_class: crypto
interval: 1h
symbols: [AAA]
risk: {confirmed_by_user: true}
`

// scriptedEngine runs the fake strategy on backtest_one_trade.csv, at the
// candle closing at 105.
func scriptedEngine(t *testing.T, extra string, decide func(strategy.Input) strategy.Decision) (*Engine, *fake.Fake, *events, *scripted.Strategy) {
	t.Helper()
	s := &scripted.Strategy{Decider: decide}
	c, err := config.Parse([]byte(scriptedYAML+extra), s)
	if err != nil {
		t.Fatal(err)
	}
	cs, _ := market.ReadCSVFile("../../testdata/backtest_one_trade.csv")
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs}})
	for i := 0; i < 3; i++ {
		f.Advance()
	}
	j := &events{}
	return New(c, s, f, exchange.Demo, j, func() time.Time { return cs[3].Time }), f, j, s
}

// An entry without a stop, with the stop on the wrong side or on a side
// the config forbids is refused and journaled; nothing is sent.
func TestBadEntriesAreRefusedAndJournaled(t *testing.T) {
	cases := []struct {
		name, extra string
		d           strategy.Decision
		want        string
	}{
		{"no stop", "", strategy.Decision{Action: strategy.Enter, Side: strategy.Long}, "no stop"},
		{"long stop above entry", "", strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 106}, "wrong side"},
		{"long stop at entry", "", strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 105}, "wrong side"},
		{"short stop below entry", "market: swap\nperp: {allow_short: true}\n", strategy.Decision{Action: strategy.Enter, Side: strategy.Short, Stop: 104}, "wrong side"},
		{"short not allowed", "market: swap\nperp: {allow_short: false}\n", strategy.Decision{Action: strategy.Enter, Side: strategy.Short, Stop: 110}, "allow_short"},
		{"short on a spot adapter", "market: swap\nperp: {allow_short: true}\n", strategy.Decision{Action: strategy.Enter, Side: strategy.Short, Stop: 110}, "short"},
		{"unknown side", "", strategy.Decision{Action: strategy.Enter, Side: "up", Stop: 100}, "side"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, f, j, _ := scriptedEngine(t, tc.extra, func(strategy.Input) strategy.Decision { return tc.d })
			if err := e.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if f.Calls("PlaceOrder") != 0 {
				t.Fatal("a refused entry reached the exchange")
			}
			if r := j.rejected(); !strings.Contains(r, tc.want) {
				t.Fatalf("journal rejections %q, want one naming %q", r, tc.want)
			}
		})
	}
}

// With a position open, the strategy is asked again; Exit leaves at
// market (stop cancelled first) and Enter is refused (one position per symbol).
func TestStrategyExitLeavesAtMarket(t *testing.T) {
	enter := strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 100}
	answer := enter
	e, f, j, s := scriptedEngine(t, "", func(strategy.Input) strategy.Decision { return answer })
	ctx := context.Background()
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if ps, _ := f.Positions(ctx, exchange.Demo); len(ps) != 1 {
		t.Fatalf("setup: no entry: %+v", ps)
	}
	f.Advance()
	if err := e.Step(ctx); err != nil { // Enter again while holding
		t.Fatal(err)
	}
	if ps, _ := f.Positions(ctx, exchange.Demo); len(ps) != 1 || f.Calls("PlaceOrder") != 1 || !strings.Contains(j.rejected(), "position") {
		t.Fatalf("Enter while holding: positions %+v, orders %d, rejections %q", ps, f.Calls("PlaceOrder"), j.rejected())
	}
	answer = strategy.Decision{Action: strategy.Exit, Reason: "done"}
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if ps, _ := f.Positions(ctx, exchange.Demo); len(ps) != 0 || f.Unprotected() != 0 {
		t.Fatalf("Exit left a position or a resting stop: %+v", ps)
	}
	last := s.Log[len(s.Log)-1]
	if !strings.Contains(last, "Side:long") || !strings.Contains(last, "Stop:100") {
		t.Fatalf("the strategy did not see its open position: %s", last)
	}
	var exit bool
	for _, ev := range *j {
		exit = exit || (ev.Kind == journal.Filled && ev.Side == string(exchange.Sell) && strings.Contains(ev.Reason, "exit"))
	}
	if !exit {
		t.Fatalf("no market exit in the journal: %+v", *j)
	}
}

// The target exit is side-aware: long when the close reaches it from
// below, short when it reaches it from above.
func TestTargetHitIsSideAware(t *testing.T) {
	cases := []struct {
		side          strategy.Side
		close, target float64
		want          bool
	}{
		{strategy.Long, 110, 110, true},
		{strategy.Long, 111, 110, true},
		{strategy.Long, 109, 110, false},
		{strategy.Short, 90, 90, true},
		{strategy.Short, 89, 90, true},
		{strategy.Short, 91, 90, false},
		{strategy.Long, 1e9, 0, false}, // no target
	}
	for _, tc := range cases {
		if got := TargetHit(tc.side, tc.close, tc.target); got != tc.want {
			t.Errorf("TargetHit(%s, %v, %v) = %v, want %v", tc.side, tc.close, tc.target, got, tc.want)
		}
	}
}

// Flat, a None that carries a Reason is a signal the strategy dropped
// itself; it is journaled with that reason. A None without a Reason (no
// signal at all) writes nothing, so the journal is not flooded every candle.
func TestNoneWithAReasonIsJournaled(t *testing.T) {
	cases := []struct {
		name, reason, want string
	}{
		{"with reason", "stop -5 is not between 0 and the entry 90", "signal skipped: stop -5 is not between 0 and the entry 90"},
		{"without reason", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, f, j, _ := scriptedEngine(t, "", func(strategy.Input) strategy.Decision {
				return strategy.Decision{Reason: tc.reason}
			})
			if err := e.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if f.Calls("PlaceOrder") != 0 {
				t.Fatal("a None reached the exchange")
			}
			if len(*j) != 0 && tc.want == "" || j.rejected() != tc.want {
				t.Fatalf("journal %+v, want rejections %q", *j, tc.want)
			}
		})
	}
}

// Holding, a None with a Reason is not a dropped signal: nothing is
// journaled and the trail still runs (the frozen engine never skipped while holding).
func TestNoneWithAReasonWhileHoldingStillTrails(t *testing.T) {
	e, f, j, s := trailEngine(t, "", fake.Options{}, strategy.Long, 90, []float64{100, 100}, answers(95))
	ctx := context.Background()
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	s.Decider = func(strategy.Input) strategy.Decision {
		return strategy.Decision{Reason: "stop -5 is not between 0 and the entry 90"}
	}
	f.Advance()
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if r := j.rejected(); r != "" {
		t.Fatalf("a None with a Reason while holding was journaled: %q", r)
	}
	if s.calls != 1 || position(t, f).StopPrice != 95 {
		t.Fatalf("trail calls %d, stop %g; want 1 and 95", s.calls, position(t, f).StopPrice)
	}
}
