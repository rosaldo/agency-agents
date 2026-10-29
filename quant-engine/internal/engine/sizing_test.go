package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// FR-R1: leverage only sets the margin locked; the size comes from the risk
// budget, so leverage 1 and 3 buy the same quantity.
func TestLeverageDoesNotChangeTheSize(t *testing.T) {
	qty := map[int]float64{}
	for _, lev := range []int{1, 3} {
		e, f, j, _ := scriptedEngine(t, fmt.Sprintf("perp: {leverage: %d}\n", lev), func(strategy.Input) strategy.Decision {
			return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 100}
		})
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		ps, _ := f.Positions(context.Background(), exchange.Demo)
		if len(ps) != 1 {
			t.Fatalf("leverage %d: no entry (%s)", lev, j.rejected())
		}
		qty[lev] = ps[0].Qty
	}
	if qty[1] != qty[3] {
		t.Fatalf("qty with leverage 1 = %v, with 3 = %v; want the same", qty[1], qty[3])
	}
}

// FR-R3: an entry whose margin (notional / leverage) exceeds the free margin
// is not sent; the same entry fits with more leverage.
func TestInsufficientMarginDoesNotEnter(t *testing.T) {
	// stop 104.5 on entry 105: qty = 100 / 0.5 = 200, notional 21 000 > cash 10 000
	enter := func(strategy.Input) strategy.Decision {
		return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 104.5}
	}
	e, f, j, _ := scriptedEngine(t, "", enter)
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Calls("PlaceOrder") != 0 || !strings.Contains(j.rejected(), "margin") {
		t.Fatalf("orders %d, rejections %q; want none sent and a margin rejection", f.Calls("PlaceOrder"), j.rejected())
	}
	e, f, j, _ = scriptedEngine(t, "perp: {leverage: 3}\n", enter) // margin 7 000
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Calls("PlaceOrder") != 1 {
		t.Fatalf("leverage 3: orders %d (%s), want the entry", f.Calls("PlaceOrder"), j.rejected())
	}
}

// FR-R4: holding a long, an Enter on the other side is refused; reversing
// takes an Exit to flat first, in a separate step.
func TestReversalOnlyThroughFlat(t *testing.T) {
	answer := strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 100}
	e, f, j, _ := scriptedEngine(t, "perp: {allow_short: true}\n", func(strategy.Input) strategy.Decision { return answer })
	ctx := context.Background()
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	f.Advance()
	answer = strategy.Decision{Action: strategy.Enter, Side: strategy.Short, Stop: 200}
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	ps, _ := f.Positions(ctx, exchange.Demo)
	if len(ps) != 1 || f.Calls("PlaceOrder") != 1 || !strings.Contains(j.rejected(), "reverse through flat") {
		t.Fatalf("short while long: positions %+v, orders %d, rejections %q", ps, f.Calls("PlaceOrder"), j.rejected())
	}
}

// balanceOverride reports a fixed balance, to tell equity from free cash.
type balanceOverride struct {
	exchange.Exchange
	b *exchange.Balance
}

func (o balanceOverride) Balance(context.Context, exchange.Env) (exchange.Balance, error) {
	return *o.b, nil
}

// FR-R2: drawdown is measured on equity, not on free margin: margin locked
// by a position does not trip the breaker, an equity loss does.
func TestDrawdownIsOnEquity(t *testing.T) {
	e, f, _, _ := scriptedEngine(t, "", func(strategy.Input) strategy.Decision { return strategy.Decision{} })
	b := &exchange.Balance{Cash: 10_000, Equity: 10_000}
	e.Ex = balanceOverride{f, b}
	ctx := context.Background()
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	b.Cash = 1_000 // margin locked, equity unchanged
	if err := e.Step(ctx); err != nil {
		t.Fatalf("free margin drop tripped the breaker: %v", err)
	}
	b.Equity = 9_600 // 4% > daily 3%
	if err := e.Step(ctx); !IsHalt(err) {
		t.Fatalf("equity drawdown: err = %v, want a halt", err)
	}
}
