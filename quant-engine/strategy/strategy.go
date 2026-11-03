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
	// BudgetUsed is the fraction of the trade's risk budget its tranches
	// took so far (the sum of their Fractions), in (0, 1]; Tranches is how
	// many there were. Both count a tranche once it is sent, even if its
	// order then fails; 0 for a position entered without a recorded budget.
	BudgetUsed float64
	Tranches   int
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
	// Add is a further tranche of the open position (graduated entry): same
	// side as the position, which the engine holds itself. Refused flat, on
	// the other side, with a stop looser than the position's, or when the
	// fractions of the trade would sum above 1.
	Add
)

type Decision struct {
	Action Action
	Side   Side    // Enter and Add
	Stop   float64 // Enter: required protective stop price; Add: 0 keeps the position's, else as tight or tighter
	Target float64 // Enter: optional exit price, 0 = none; Add: replaces the target, 0 keeps it
	// Fraction is the part of the trade's risk budget this Enter or Add
	// takes, in (0, 1]; 0 means 1, the whole budget at once. The budget is
	// fixed at the first tranche (equity x max_risk_per_trade_pct) and the
	// fractions of one trade sum to at most 1.
	Fraction float64
	Reason   string // journal; on None while flat, journaled as a skipped signal (leave empty when there is no signal)
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
	// Classifier judges each headline the keywords matched; nil means every
	// match is a catastrophe (keywords alone).
	Classifier *NewsClassifier `json:",omitempty"`
}

// NewsClassifier is the question a classifier (TypeSafe's Jev) answers about
// one headline. Type is "noul": a yes/no question whose yes means a
// catastrophe for the asset; a yes at or above Threshold is one.
type NewsClassifier struct {
	Model     string            // e.g. "jev-latest" or a pinned version
	Type      string            // "noul"
	Question  string            // the instructions
	Criteria  map[string]string // optional: "true" and "false", what a yes and a no mean
	Threshold float64           // in (0, 1]
}

// DerivativesUser is optional: a strategy whose NeedsDerivatives is true gets
// Input.Deriv, and is refused at load on a spot market or on an adapter
// without exchange.DerivativesReader (fail closed).
type DerivativesUser interface {
	NeedsDerivatives() bool
}

// OpenInterestUser is optional, for a DerivativesUser: one whose
// NeedsOpenInterest is true decides on open interest, so a backtest refuses
// snapshots without it for any candle of the period (a funding-only history
// has none) instead of replaying a filter that can never pass.
type OpenInterestUser interface {
	NeedsOpenInterest() bool
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

// Retargeter is optional: a strategy that implements it sets its exit target
// on the position as it actually filled, which only the engine knows (it
// sizes every tranche). The engine calls Retarget once an Enter's or an
// Add's fill is confirmed, with the Input of that candle and in.Position read
// back from the exchange: real Qty and AvgPrice, the stop in force, the
// target in force (the Decision's, or the one before), BudgetUsed and
// Tranches. A positive answer replaces the target; 0 keeps it; a negative or
// non-finite one is refused and journaled. The target is the engine's check
// on each closed candle, the same in backtest, paper and live: one set on
// candle t is checked from t+1 on.
type Retargeter interface {
	Retarget(in Input) float64
}
