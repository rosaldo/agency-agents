package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/paper"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// NeedsDerivatives reports a strategy that declares it needs derivatives data.
func NeedsDerivatives(s strategy.Strategy) bool {
	u, ok := s.(strategy.DerivativesUser)
	return ok && u.NeedsDerivatives()
}

// derivReader is the adapter's derivatives port; the paper simulator trades
// on its data adapter's public data, so it is that adapter's.
func derivReader(ex exchange.Exchange) (exchange.DerivativesReader, bool) {
	if sim, ok := ex.(*paper.Sim); ok {
		ex = sim.Data()
	}
	dr, ok := ex.(exchange.DerivativesReader)
	return dr, ok
}

// CheckDerivatives refuses, at load, a strategy that needs derivatives data
// on a spot market or on an adapter that does not read it (fail closed).
func CheckDerivatives(cfg *config.Config, s strategy.Strategy, ex exchange.Exchange) error {
	switch {
	case !NeedsDerivatives(s):
		return nil
	case cfg.Market != "swap":
		return errors.New("the strategy needs derivatives data (open interest, funding), which only swap markets have; the yml trades market: " + cfg.Market)
	}
	if _, ok := derivReader(ex); !ok {
		return errors.New("the strategy needs derivatives data (open interest, funding) and this exchange adapter does not read it")
	}
	return nil
}

// SeedDeriv loads the snapshots recorded before a restart, oldest first.
func (e *Engine) SeedDeriv(h map[string][]market.Deriv) {
	for sym, ds := range h {
		e.deriv[sym] = ds
	}
}

// readDeriv reads sym's snapshot on a swap market, when the strategy needs
// it or the recorder runs, and binds it to the closed candle the step decides
// on (opened at candle). A failed read, or one outside the candle's step
// (market.Deriv.InStep), leaves the candle without one: Input marks it
// missing, and the gap is journaled once per failure streak. A snapshot not
// newer than the last one is dropped, so a restart never hands the recorder
// a row twice.
func (e *Engine) readDeriv(ctx context.Context, sym string, candle time.Time) {
	if e.Cfg.Market != "swap" || (!NeedsDerivatives(e.Strategy) && e.OnDeriv == nil) {
		return
	}
	dr, ok := derivReader(e.Ex)
	if !ok {
		return
	}
	// a hung endpoint never holds the step past the candle's window, after which no read could count
	dctx, cancel := context.WithDeadline(ctx, market.StepEnd(candle, e.Cfg.Interval()))
	defer cancel()
	d, err := dr.Snapshot(dctx, e.Env, sym)
	d.Candle = candle
	if err == nil && !d.InStep(candle, e.Cfg.Interval()) {
		err = fmt.Errorf("read at %s, outside the step of the candle opened at %s", d.Time.UTC().Format(time.RFC3339Nano), candle.UTC().Format(time.RFC3339))
	}
	if err != nil {
		if !e.derivDown[sym] {
			e.note(journal.Derivatives, sym, "snapshot not read: "+err.Error()+"; candles go without one until a read succeeds (noted once per streak)")
		}
		e.derivDown[sym] = true
		return
	}
	if e.derivDown[sym] {
		e.note(journal.Derivatives, sym, "snapshot read again")
		delete(e.derivDown, sym)
	}
	h := e.deriv[sym]
	if len(h) > 0 && !d.Time.After(h[len(h)-1].Time) {
		return
	}
	e.deriv[sym] = append(h, d)
	if e.OnDeriv != nil {
		e.OnDeriv(sym, d)
	}
}

// dropDeriv forgets sym's snapshots bound to candles before from, the oldest
// candle in the window, keeping the last one for readDeriv's check.
func (e *Engine) dropDeriv(sym string, from time.Time) {
	h := e.deriv[sym]
	i := 0
	for i < len(h)-1 && h[i].Candle.Before(from) {
		i++
	}
	e.deriv[sym] = h[i:]
}
