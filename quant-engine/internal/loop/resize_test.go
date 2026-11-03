package loop

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// scriptOf answers each Decide with the next decision, then none; onEach
// sees each one before it is returned.
func scriptOf(onEach func(strategy.Decision), answers ...strategy.Decision) *scripted.Strategy {
	return &scripted.Strategy{Decider: func(strategy.Input) strategy.Decision {
		if len(answers) == 0 {
			return strategy.Decision{}
		}
		d := answers[0]
		answers = answers[1:]
		if onEach != nil {
			onEach(d)
		}
		return d
	}}
}

// crashBeforeSwap is the exchange of a process killed (kill -9) after an
// order's fill and before the stop swap that follows it.
type crashBeforeSwap struct {
	*fake.Fake
	armedAt int // orders placed when armed; -1 = not armed
}

func (c *crashBeforeSwap) PlaceStop(ctx context.Context, env exchange.Env, id, sym string, side exchange.Side, qty, px float64) (exchange.OrderState, error) {
	if c.armedAt >= 0 && c.Fake.Calls("PlaceOrder") > c.armedAt {
		panic(killed{})
	}
	return c.Fake.PlaceStop(ctx, env, id, sym, side, qty, px)
}

// A kill -9 between an add's fill and the swap of its stop: the start that
// follows covers the whole quantity at the add's tighter stop, with the
// budget as it was (the next add refused), so the loss at the stop stays
// within the trade budget (1% of 10 000).
func TestKillBetweenAnAddFillAndTheStopSwap(t *testing.T) {
	f, cs := trancheFake()
	ex := &crashBeforeSwap{Fake: f, armedAt: -1}
	dir := filepath.Join(t.TempDir(), "state")
	s := scriptOf(func(d strategy.Decision) {
		if d.Action == strategy.Add {
			ex.armedAt = f.Calls("PlaceOrder")
		}
	}, wait, wait, wait, tranche(strategy.Enter, 100), tranche(strategy.Add, 101)) // at 105, then at 103
	c, err := config.Parse([]byte(trancheYAML), s)
	if err != nil {
		t.Fatal(err)
	}
	tick := &FakeTicker{Ex: f, Candles: cs}
	if !runUntilKilled(t, c, s, ex, tick, dir) {
		t.Fatal("setup: the loop was never killed before the swap")
	}
	if p := ownPosition(t, f); math.Abs(p.Qty-35) > 1e-9 || p.StopQty != 10 || p.StopPrice != 100 {
		t.Fatalf("setup: position at the kill %+v; want 35 under the entry's stop for 10 at 100", p)
	}

	ex.armedAt = -1
	f.Advance() // the restart comes a candle later
	s2 := scriptOf(nil, tranche(strategy.Add, 0))
	c2, _ := config.Parse([]byte(trancheYAML), s2)
	Run(context.Background(), c2, s2, ex, exchange.Demo, tickerFrom(f, cs, tick.i.Load()+1), dir)
	p := ownPosition(t, f)
	if p.StopQty != p.Qty || p.StopPrice != 101 {
		t.Fatalf("position after the start %+v; want one stop for all of it at the add's 101", p)
	}
	if loss := p.Qty * (p.AvgPrice - p.StopPrice); loss > 100+1e-6 {
		t.Fatalf("loss at the stop %.4f is above the trade budget 100", loss)
	}
	st, _ := LoadState(dir, "paper")
	b, _ := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if math.Abs(st.Trades["AAA"].Used-1) > 1e-9 || !strings.Contains(string(b), "above the whole trade budget") {
		t.Fatalf("trades %+v; want the whole budget used and the next add refused; journal:\n%s", st.Trades, b)
	}
}

// A kill -9 right after the first tranche's fill: the state holds the
// fraction used though not the position, so after the start an add of the
// whole budget is refused and the loss at the stop stays within it.
func TestKillAfterTheFirstTrancheFillKeepsItsFraction(t *testing.T) {
	f, cs := trancheFake()
	ex := &crashAfterFill{Fake: f, armedAt: -1}
	dir := filepath.Join(t.TempDir(), "state")
	s := scriptOf(func(d strategy.Decision) {
		if d.Action == strategy.Enter {
			ex.armedAt = f.Calls("PlaceOrder")
		}
	}, wait, wait, wait, tranche(strategy.Enter, 100))
	c, _ := config.Parse([]byte(trancheYAML), s)
	tick := &FakeTicker{Ex: f, Candles: cs}
	if !runUntilKilled(t, c, s, ex, tick, dir) {
		t.Fatal("setup: the loop was never killed after the entry")
	}
	if st, _ := LoadState(dir, "paper"); st.Trades["AAA"].Used != 0.5 || len(st.Positions) != 0 {
		t.Fatalf("setup: state at the kill holds trades %+v positions %+v; want 0.5 used, no position", st.Trades, st.Positions)
	}
	ex.armedAt = -1
	f.Advance()
	s2 := scriptOf(nil, strategy.Decision{Action: strategy.Add, Side: strategy.Long, Fraction: 1})
	c2, _ := config.Parse([]byte(trancheYAML), s2)
	Run(context.Background(), c2, s2, ex, exchange.Demo, tickerFrom(f, cs, tick.i.Load()+1), dir)
	p := ownPosition(t, f)
	b, _ := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if loss := p.Qty * (p.AvgPrice - 100); loss > 100+1e-6 || !strings.Contains(string(b), "add refused") {
		t.Fatalf("loss at the stop %.4f (qty %v avg %v), want <= 100 with the add refused; journal:\n%s", loss, p.Qty, p.AvgPrice, b)
	}
}

// The state file cannot be written when a tranche is about to go out: the
// loop hands the error to the engine, which refuses the order.
func TestTrancheIsNotSentWhenTheStateCannotBeWritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through directory permissions")
	}
	f, cs := trancheFake()
	dir := filepath.Join(t.TempDir(), "state")
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	s := scriptOf(func(d strategy.Decision) {
		if d.Action == strategy.Enter {
			_ = os.Chmod(dir, 0o500) // no new file, so no state written; the journal is already open
		}
	}, wait, wait, wait, tranche(strategy.Enter, 100))
	c, _ := config.Parse([]byte(trancheYAML), s)
	Run(context.Background(), c, s, f, exchange.Demo, &FakeTicker{Ex: f, Candles: cs}, dir)
	_ = os.Chmod(dir, 0o700)
	b, _ := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if f.Calls("PlaceOrder") != 0 || !strings.Contains(string(b), "trade budget not persisted, order not sent") {
		t.Fatalf("orders %d; want none, refused as not persisted; journal:\n%s", f.Calls("PlaceOrder"), b)
	}
}
