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

// Env is explicit on every call: demo for paper, real only for live.
type Env string

const (
	Demo Env = "demo"
	Real Env = "real"
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
)

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
}

type OrderState struct {
	ClientID  string
	Symbol    string
	Side      Side
	Qty       float64
	FillPrice float64
	StopPrice float64
	Status    Status
}

// Position is a long holding and the stop resting on the exchange for it
// (StopClientID empty = unprotected).
type Position struct {
	Symbol       string
	Qty          float64
	AvgPrice     float64
	StopClientID string
	StopPrice    float64
}

type Quote struct{ Bid, Ask, Last float64 }

type Balance struct{ Cash, Equity float64 }

type Capabilities struct {
	AttachedStop bool    // entry and stop in one order (bracket/OCO)
	Demo         bool    // has a demo/testnet environment
	QtyStep      float64 // quantity increment
	MinQty       float64 // minimum order quantity
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
}

var ErrRealInPaper = errors.New("exchange: real environment refused in paper mode")

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
