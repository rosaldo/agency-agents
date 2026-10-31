package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exec"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// FR-P9, generic trailing stop: the rule is the strategy's (strategy.Trailer);
// the engine only applies an answer that tightens, sits on the losing side of
// the close and clears the liquidation, and swaps it on the exchange through
// exec.MoveStop. A stop whose cancel failed during a swap is kept in
// staleStops (persisted), retried every step, and blocks the symbol: no new
// move, no entry; on spot, where two stops could sell beyond the own
// quantity, the position is closed at once.

// errTrailRefused is a move the engine refuses and journals.
var errTrailRefused = errors.New("trail refused")

// validateTrail says whether next replaces cur on a side position at close.
// false with a nil error keeps cur silently (no answer, or not tighter: the
// common case of a rule that eases off); an error is a refusal to journal.
func validateTrail(side exchange.PositionSide, cur, next float64, ok bool, close float64) (bool, error) {
	short := side == exchange.Short
	switch {
	case !ok || next <= 0 || math.IsNaN(next) || math.IsInf(next, 0) || cur <= 0:
		return false, nil
	case !short && next <= cur, short && next >= cur:
		return false, nil
	case !short && next >= close, short && next <= close:
		return false, fmt.Errorf("%w: stop %.8g on the wrong side of the %s close %.8g", errTrailRefused, next, side, close)
	}
	return true, nil
}

// trail asks the strategy for p's new stop and moves it when the engine's
// rules allow.
func (e *Engine) trail(ctx context.Context, p exchange.Position, in strategy.Input, close float64, candle time.Time) error {
	t, ok := e.Strategy.(strategy.Trailer)
	if !ok || p.Blocked != "" || e.staleStops[p.Symbol] != "" {
		return nil
	}
	next, ok := t.Trail(in)
	apply, err := validateTrail(p.PSide(), p.StopPrice, next, ok, close)
	if err == nil && apply {
		q := p
		q.StopPrice = next
		if verr := e.liquidationViolation(q); verr != nil {
			err = fmt.Errorf("%w: stop %.8g vs liquidation: %v", errTrailRefused, next, verr)
		}
	}
	if err != nil {
		e.skip(p.Symbol, err.Error())
		return nil
	}
	if !apply {
		return nil
	}
	stale, err := e.Exec.MoveStop(ctx, p, next, candle)
	if stale != "" {
		e.staleStops[p.Symbol] = stale
		if e.OnStale != nil {
			e.OnStale()
		}
	}
	switch {
	case errors.Is(err, exec.ErrStopFilled):
		e.forget(p.Symbol)
		return nil
	case err != nil:
		return err // the old stop stands; the next step tries again
	}
	e.stops[p.Symbol] = next
	if stale != "" && e.Cfg.Market == "spot" {
		// without reduce-only, the old stop and the new one could both sell
		q := p
		q.StopClientID, q.StopPrice = e.Exec.ID(p.Symbol, candle, "trail"), next
		return e.exit(ctx, q, candle, "trail: old stop not cancelled on spot")
	}
	return nil
}

// retryStale cancels the stops a move left behind; one that is gone frees
// its symbol.
func (e *Engine) retryStale(ctx context.Context) {
	for sym, id := range e.staleStops {
		if e.Exec.CancelStale(ctx, sym, id) {
			delete(e.staleStops, sym)
		}
	}
}

// StaleStops returns a copy of the stops a move could not cancel, by symbol.
func (e *Engine) StaleStops() map[string]string {
	out := make(map[string]string, len(e.staleStops))
	for k, v := range e.staleStops {
		out[k] = v
	}
	return out
}
