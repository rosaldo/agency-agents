package engine

import (
	"context"
	"fmt"
	"math"
	"slices"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// Exact exit target (strategy.Retargeter): once an Enter's or an Add's fill
// is confirmed, the engine reads the position back from the exchange and asks
// the strategy for the target on its real average price and quantity. The
// target is the engine's own check on each closed candle (TargetHit); no
// order rests on the exchange for it, so it applies the same way in
// backtest, paper and live, and is saved in the state with the other
// targets. A position that cannot be read after the fill is journaled and
// the call is owed (persisted): it is made at the symbol's next step, on the
// position read then, before the decision.

// view is what the strategy is told about p, the engine's own position.
func (e *Engine) view(p exchange.Position) *strategy.PositionView {
	side := strategy.Long
	if p.PSide() == exchange.Short {
		side = strategy.Short
	}
	t := e.trades[p.Symbol] // the persisted trade: the same in live, paper and backtest, and after a restart
	return &strategy.PositionView{Side: side, Qty: p.Qty, AvgPrice: p.AvgPrice, Stop: p.StopPrice, Target: e.targets[p.Symbol],
		BudgetUsed: t.Used, Tranches: t.Tranches}
}

// retarget asks a Retargeter for sym's target after a confirmed fill on the
// candle of in.
func (e *Engine) retarget(ctx context.Context, sym string, in strategy.Input) {
	if _, ok := e.Strategy.(strategy.Retargeter); !ok {
		return
	}
	ps, err := e.Ex.Positions(ctx, e.Env)
	if err != nil {
		e.retargetDue[sym] = true
		e.note(journal.Retarget, sym, fmt.Sprintf("position unreadable after the fill (%v); target %.8g kept, retarget at the next step", err, e.targets[sym]))
		return
	}
	for _, p := range ps {
		if p.Symbol == sym && p.Qty > 0 {
			e.applyRetarget(p, in)
			return
		}
	}
	// gone already (its stop, a liquidation): the next step forgets it
}

// applyRetarget sets p's target to the Retargeter's answer on p as read.
func (e *Engine) applyRetarget(p exchange.Position, in strategy.Input) {
	sym := p.Symbol
	delete(e.retargetDue, sym)
	in.Position = e.view(p)
	cur := e.targets[sym]
	next := e.Strategy.(strategy.Retargeter).Retarget(in)
	switch {
	case next == 0:
		return
	case next < 0 || math.IsNaN(next) || math.IsInf(next, 0):
		e.note(journal.Retarget, sym, fmt.Sprintf("retarget refused: %v is not a price; target %.8g kept", next, cur))
		return
	}
	e.targets[sym] = next
	e.note(journal.Retarget, sym, fmt.Sprintf("target %.8g -> %.8g on avg price %.8g qty %.8g", cur, next, p.AvgPrice, p.Qty))
}

// RestoreRetarget reloads the symbols whose retarget a previous run owed;
// nil (a state from before them) means none.
func (e *Engine) RestoreRetarget(due []string) {
	for _, sym := range due {
		e.retargetDue[sym] = true
	}
}

// RetargetDue lists, sorted, the symbols whose retarget is owed.
func (e *Engine) RetargetDue() []string {
	var out []string
	for sym := range e.retargetDue {
		out = append(out, sym)
	}
	slices.Sort(out)
	return out
}
