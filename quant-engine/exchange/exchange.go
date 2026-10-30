// Package exchange is the broker-agnostic contract the engine trades through.
// Adapters live in their own packages under exchange/ and are chosen by the
// `exchange:` key of the yml; no vendor is named outside them.
package exchange

import (
	"context"
	"errors"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/market"
)

// Env is explicit on every call: demo for paper, Real (mainnet) only for mainnet.
type Env string

const (
	Demo Env = "demo"
	Real Env = "mainnet"
)

type Side string

const (
	Buy  Side = "buy"
	Sell Side = "sell"
)

type OrderType string

const (
	Market OrderType = "market"
	Limit  OrderType = "limit"
)

type Status string

const (
	Open     Status = "open"
	Filled   Status = "filled"
	Canceled Status = "canceled"
	Rejected Status = "rejected"
	Unknown  Status = "unknown" // sent, fate not known yet: never taken as resting nor as refused
)

// PositionSide is the side of a position; spot holdings are always Long.
type PositionSide string

const (
	Long  PositionSide = "long"
	Short PositionSide = "short"
)

// Opening is the order side that opens a position on side.
func (s PositionSide) Opening() Side {
	if s == Short {
		return Sell
	}
	return Buy
}

// Closing is the order side of a stop or exit of a position on side.
func (s PositionSide) Closing() Side {
	if s == Short {
		return Buy
	}
	return Sell
}

// OrderRequest is an entry or exit. AttachedStop > 0 asks the exchange to
// place the protective stop together with the entry (Capabilities.AttachedStop).
type OrderRequest struct {
	ClientID     string
	Symbol       string
	Side         Side
	Qty          float64
	Type         OrderType
	Limit        float64
	AttachedStop float64
	ReduceOnly   bool // exits and flatten: may only shrink a position (FR-P2)
}

type OrderState struct {
	ClientID   string
	ExchangeID string // the exchange's order id, once known
	Symbol     string
	Side       Side
	Qty        float64
	Filled     float64 // executed quantity
	FillPrice  float64
	StopPrice  float64
	Status     Status
}

// Position is the engine's OWN position in a symbol and the stop resting on
// the exchange for it (StopClientID empty = unprotected). Side "" is Long.
type Position struct {
	Symbol           string
	Side             PositionSide
	Qty              float64 // own quantity
	AvgPrice         float64
	StopClientID     string
	StopPrice        float64
	StopQty          float64 // quantity the stop still closes; above Qty = oversized, the executor resizes it
	AccountQty       float64 // what the exchange reports on Side (netting); 0 = not reported
	Leverage         float64 // applied; 0 = not reported (spot)
	MarginMode       string  // "isolated" | "cross" | "" (spot or not reported)
	LiquidationPrice float64 // 0 = not reported
	Blocked          string  // non-empty: no entries on this symbol, with the reason
}

// PSide is the position's side, Long when unset.
func (p Position) PSide() PositionSide {
	if p.Side == Short {
		return Short
	}
	return Long
}

type Quote struct{ Bid, Ask, Last float64 }

// Balance: Cash is free cash (spot) or free margin (perp) in the quote or
// settle currency; Equity is the account value with unrealized P&L.
type Balance struct{ Cash, Equity float64 }

// MarginSettings are a perp market's leverage and margin mode.
type MarginSettings struct {
	Leverage   int
	MarginMode string // "isolated" | "cross"; "" on spot
}

type Capabilities struct {
	AttachedStop       bool    // entry and stop in one order (bracket/OCO)
	Demo               bool    // has a demo/testnet environment
	QtyStep            float64 // quantity increment
	MinQty             float64 // minimum order quantity
	Swap               bool    // bound to linear swap markets: shorts allowed, exits reduce-only
	ReportsLiquidation bool    // a position must carry LiquidationPrice; missing = violation
	Netting            bool    // one net position per symbol on the account (FR-P8 netting rule applies)
}

type Exchange interface {
	Candles(ctx context.Context, env Env, symbol string, interval time.Duration, n int) ([]market.Candle, error)
	Quote(ctx context.Context, env Env, symbol string) (Quote, error)
	PlaceOrder(ctx context.Context, env Env, req OrderRequest) (OrderState, error)
	PlaceStop(ctx context.Context, env Env, clientID, symbol string, side Side, qty, stopPrice float64) (OrderState, error)
	CancelOrder(ctx context.Context, env Env, clientID string) (OrderState, error)
	Positions(ctx context.Context, env Env) ([]Position, error)
	Balance(ctx context.Context, env Env) (Balance, error)
	Capabilities() Capabilities
	// ConfigureMarket applies margin mode and leverage on symbol and returns
	// what the exchange reports as applied; spot returns {1, ""}.
	ConfigureMarket(ctx context.Context, env Env, symbol string, want MarginSettings) (MarginSettings, error)
}

var (
	ErrRealInPaper = errors.New("exchange: mainnet environment refused in paper mode")
	// ErrRefused wraps an answer by which the venue did NOT take the order.
	ErrRefused = errors.New("exchange: refused")
	// ErrNotOwn: the order is not in the engine's registry; it is never
	// cancelled nor changed (FR-P8).
	ErrNotOwn = errors.New("exchange: order not opened by this engine")
)

// FundingReporter is implemented by adapters whose venue reports the funding
// paid or received on a symbol (FR-P5): the engine journals it.
type FundingReporter interface {
	Funding(ctx context.Context, env Env, symbol string, since time.Time) ([]FundingPayment, error)
}

// FundingPayment is one funding settlement; Amount is as the venue reports
// it, in the settle currency.
type FundingPayment struct {
	Time   time.Time
	Symbol string
	Amount float64
}

// IDFormatter is implemented by adapters whose venue needs client order IDs
// in a given shape; the engine hands it a 16-byte digest.
type IDFormatter interface {
	FormatClientID(digest [16]byte) string
}

// LiquidationEstimator is implemented by adapters that know the venue's
// liquidation formula: the distance from entry to liquidation, as a fraction
// of entry, of a fresh position at leverage (FR-P4, pre-entry). ok=false
// means no formula for symbol; the engine then uses the conservative
// fallback 1/leverage minus the round-trip cost.
type LiquidationEstimator interface {
	LiquidationDistance(symbol string, side PositionSide, entry, leverage float64) (frac float64, ok bool)
}

// CheckEnv is the guard every adapter runs first: a paper engine never
// reaches the real environment.
func CheckEnv(paper bool, env Env) error {
	switch {
	case env == Real && paper:
		return ErrRealInPaper
	case env != Demo && env != Real:
		return errors.New("exchange: unknown environment " + string(env))
	}
	return nil
}
