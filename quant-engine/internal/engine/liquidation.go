package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exec"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/risk"
)

// FR-P4, stop before liquidation, on swap markets only (spot has none):
//   - before an entry, the distance to liquidation comes from the adapter's
//     formula (exchange.LiquidationEstimator) or, without one, from the
//     conservative fallback 1/leverage; the round-trip cost comes off either;
//   - after the fill and on every step, the exchange's own liquidation price
//     is the truth; missing where the venue reports one is a violation, and
//     on a venue that reports none the estimate uses the position's applied
//     leverage, not the yml's;
//   - a violation flattens the position and halts.

// liquidationDistance is the estimated distance from entry to liquidation of
// a fresh position, as a fraction of entry, net of the round-trip cost.
func (e *Engine) liquidationDistance(sym string, side exchange.PositionSide, entry, leverage float64) float64 {
	c := e.Cfg.Costs
	rt := risk.RoundTripCost(c.CommissionPct, c.SpreadPct, c.SlippagePct)
	if est, ok := e.Ex.(exchange.LiquidationEstimator); ok {
		if d, ok := est.LiquidationDistance(sym, side, entry, leverage); ok {
			return d - rt
		}
	}
	return risk.FallbackLiquidationDistance(leverage, rt)
}

// entryClearsLiquidation is the pre-entry check at the yml's leverage.
func (e *Engine) entryClearsLiquidation(sym string, side exchange.PositionSide, entry, stop float64) error {
	if e.Cfg.Market != "swap" {
		return nil
	}
	short := side == exchange.Short
	liq := risk.LiquidationPrice(short, entry, e.liquidationDistance(sym, side, entry, float64(e.Cfg.Perp.Leverage)))
	return risk.StopBeforeLiquidation(short, entry, stop, liq, e.Cfg.Risk.MinLiquidationBuffer)
}

// liquidationViolation checks an open position against the liquidation price
// the exchange reports for it.
func (e *Engine) liquidationViolation(p exchange.Position) error {
	if e.Cfg.Market != "swap" || p.Qty <= 0 {
		return nil
	}
	short := p.PSide() == exchange.Short
	stop := p.StopPrice
	if stop <= 0 {
		stop = e.stops[p.Symbol]
	}
	liq := p.LiquidationPrice
	if liq <= 0 {
		if e.Ex.Capabilities().ReportsLiquidation {
			return errors.New("the exchange reports no liquidation price for the position")
		}
		lev := p.Leverage // applied: FR-P3 can only read it back once a position exists
		if lev <= 0 {
			lev = float64(e.Cfg.Perp.Leverage)
		}
		liq = risk.LiquidationPrice(short, p.AvgPrice, e.liquidationDistance(p.Symbol, p.PSide(), p.AvgPrice, lev))
	}
	return risk.StopBeforeLiquidation(short, p.AvgPrice, stop, liq, e.Cfg.Risk.MinLiquidationBuffer)
}

// guardLiquidation flattens p and halts when its stop does not clear the
// liquidation (the same rule as a stop the exchange refused).
func (e *Engine) guardLiquidation(ctx context.Context, p exchange.Position) error {
	verr := e.liquidationViolation(p)
	if verr == nil {
		return nil
	}
	why := "stop vs liquidation: " + verr.Error()
	herr := fmt.Errorf("%w: %s %s; position flattened", exec.ErrHalt, p.Symbol, why)
	if xerr := e.Exec.Exit(ctx, p, e.Exec.Now(), why); xerr != nil {
		herr = fmt.Errorf("%w: %s %s; flattening failed: %v", exec.ErrHalt, p.Symbol, why, xerr)
	} else {
		delete(e.targets, p.Symbol)
		delete(e.stops, p.Symbol)
	}
	if e.Exec.Journal != nil {
		_ = e.Exec.Journal.Append(journal.Event{Time: e.Exec.Now(), Kind: journal.Halt, Symbol: p.Symbol, Reason: herr.Error()})
	}
	return herr
}

// guardFilled re-reads the positions right after an entry filled and checks
// the new one against the exchange's liquidation price.
func (e *Engine) guardFilled(ctx context.Context, sym string) error {
	if e.Cfg.Market != "swap" {
		return nil
	}
	ps, err := e.Ex.Positions(ctx, e.Env)
	if err != nil {
		return fmt.Errorf("after entry %s: positions: %w", sym, err) // the next step checks again
	}
	for _, p := range ps {
		if p.Symbol == sym {
			return e.guardLiquidation(ctx, p)
		}
	}
	return nil
}
