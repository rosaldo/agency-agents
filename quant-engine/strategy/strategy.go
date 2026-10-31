// Package strategy is the contract between the engine and a trading strategy.
// The engine asks, the strategy answers: a strategy never sees the exchange,
// the balance, orders, risk limits or the environment. Sizing, order routing,
// protective stops and every limit belong to the engine.
package strategy

import (
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/rosaldo/agency-agents/quant-engine/market"
)

// Strategy answers the engine once per closed candle and symbol.
type Strategy interface {
	// Name is a stable id matching ^[a-z0-9-]+$. It feeds the parameter hash,
	// the journal, the state directory, the environment prefix and client IDs.
	Name() string
	// Configure receives the yml `strategy:` block (a zero Node when absent);
	// it validates and fails early.
	Configure(params yaml.Node) error
	// Warmup is the number of closed candles needed per Decide.
	Warmup() int
	// Inputs lists auxiliary symbols (e.g. a benchmark); nil = none.
	Inputs() []string
	// Decide must be deterministic given Input, so backtest equals live.
	Decide(in Input) Decision
}

type Input struct {
	Symbol   string
	Now      time.Time                  // close time of the last candle
	Candles  []market.Candle            // closed only, oldest first, len <= Warmup()
	Aux      map[string][]market.Candle // candles of Inputs(), same window
	Position *PositionView              // nil = flat (the engine's own quantity only)
	// Deriv is set for a DerivativesUser only: one snapshot per entry of
	// Candles, same index, read at or before that candle's close; a Missing
	// one means none was read for that candle.
	Deriv []market.Deriv
}

type PositionView struct {
	Side     Side
	Qty      float64
	AvgPrice float64
	Stop     float64 // stop resting on the exchange
	Target   float64 // 0 = none
}

type Side string

const (
	Long  Side = "long"
	Short Side = "short"
)

type Action int

const (
	None Action = iota // the zero Decision does nothing (safe default)
	Enter
	Exit
)

type Decision struct {
	Action Action
	Side   Side    // Enter only
	Stop   float64 // Enter: required protective stop price
	Target float64 // Enter: optional exit price, 0 = none
	Reason string  // journal; on None while flat, journaled as a skipped signal (leave empty when there is no signal)
}

// NewsSource is optional. With news_guard: required, a strategy that does not
// implement it (or returns no feeds) gets no entry: the guard fails closed.
type NewsSource interface {
	NewsPolicy() NewsPolicy
}

type NewsPolicy struct {
	Feeds    []string            // RSS/Atom URLs
	Keywords []string            // no engine default list
	Aliases  map[string][]string // base asset -> extra names
	Lookback time.Duration
}

// DerivativesUser is optional: a strategy whose NeedsDerivatives is true gets
// Input.Deriv, and is refused at load on a spot market or on an adapter
// without exchange.DerivativesReader (fail closed).
type DerivativesUser interface {
	NeedsDerivatives() bool
}

// Trailer is optional: a strategy that implements it moves its own stop by
// any rule it likes (moving average, ATR, structure...). The engine calls
// Trail once per step for every open position it holds, after Decide
// returned no Exit, and decides itself whether the answer is applied:
// tighten only, on the right side of the price, clear of the liquidation,
// swapped on the exchange without a moment unprotected. ok=false or stop<=0
// keeps the current stop. A stop moved on candle t guards from t+1 on.
type Trailer interface {
	Trail(in Input) (stop float64, ok bool)
}
