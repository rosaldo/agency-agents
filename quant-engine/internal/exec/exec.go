// Package exec sends orders so that no position ever lives without a
// protective stop resting on the exchange.
package exec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
)

// ErrHalt means a stop could not be placed: the position was flattened and
// the engine must stop trading.
var ErrHalt = errors.New("exec: halt")

type Executor struct {
	Name    string // Strategy.Name(): part of every client ID, so strategies sharing an account never collide
	Ex      exchange.Exchange
	Env     exchange.Env
	Now     func() time.Time
	Retries int // attempts per call with the same client ID; 0 = 3
	Risk    Gate
	Journal Recorder // nil = no journal
}

type Recorder interface {
	Append(journal.Event) error
}

// record never blocks a protective action: a journal write error is dropped
// here and surfaces when the journal itself is checked.
func (e *Executor) record(ev journal.Event) {
	if e.Journal != nil {
		ev.Time = e.Now()
		_ = e.Journal.Append(ev)
	}
}

// send journals an order as sent, then its terminal state or rejection.
func (e *Executor) send(ctx context.Context, ev journal.Event, call func() (exchange.OrderState, error)) (exchange.OrderState, error) {
	ev.Kind = journal.Sent
	e.record(ev)
	st, err := e.retry(ctx, call)
	ev.Kind, ev.Price = string(st.Status), st.FillPrice
	if err != nil {
		ev.Kind, ev.Reason = journal.Rejected, err.Error()
	}
	e.record(ev)
	return st, err
}

// Gate is the risk check every entry must pass (risk.Breaker).
type Gate interface {
	Allow(now time.Time, equity float64) error
}

// Plan is an entry on Side ("" = long) with its protective stop and
// optional target (0 = none). Qty comes from risk.Size.
type Plan struct {
	Side                     exchange.PositionSide
	Entry, Stop, Target, Qty float64
}

// ClientID derives an idempotent 16-byte order digest from strategy name,
// symbol, candle and purpose, so a resend after a lost response can never
// open a second order.
func ClientID(name, symbol string, candle time.Time, purpose string) [16]byte {
	h := sha256.Sum256(fmt.Appendf(nil, "%s|%s|%d|%s", name, symbol, candle.Unix(), purpose))
	return [16]byte(h[:16])
}

// ID is the client order ID for symbol, candle and purpose, in the shape the
// adapter's venue accepts (32 hex characters by default).
func (e *Executor) ID(symbol string, candle time.Time, purpose string) string {
	d := ClientID(e.Name, symbol, candle, purpose)
	if f, ok := e.Ex.(exchange.IDFormatter); ok {
		return f.FormatClientID(d)
	}
	return hex.EncodeToString(d[:])
}

func (e *Executor) retry(ctx context.Context, call func() (exchange.OrderState, error)) (st exchange.OrderState, err error) {
	n := e.Retries
	if n <= 0 {
		n = 3
	}
	for i := 0; i < n && ctx.Err() == nil; i++ {
		if st, err = call(); err == nil {
			return st, nil
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	return st, err
}

// Enter opens plan.Qty of symbol at market on plan.Side with the stop on the exchange:
// attached to the entry when the adapter supports it, otherwise sent as soon
// as the entry fills. A stop that cannot be placed flattens the position and
// returns ErrHalt.
func (e *Executor) Enter(ctx context.Context, symbol string, candle time.Time, plan Plan) error {
	if e.Risk == nil {
		return errors.New("exec: no risk gate; refusing to enter")
	}
	bal, err := e.Ex.Balance(ctx, e.Env)
	if err != nil {
		return fmt.Errorf("entry: balance: %w", err)
	}
	if err := e.Risk.Allow(e.Now(), bal.Equity); err != nil {
		e.record(journal.Event{Kind: journal.Limit, Symbol: symbol, Reason: err.Error()})
		return err
	}
	side := plan.Side
	if side == "" {
		side = exchange.Long
	}
	req := exchange.OrderRequest{ClientID: e.ID(symbol, candle, "entry"), Symbol: symbol,
		Side: side.Opening(), Qty: plan.Qty, Type: exchange.Market}
	if e.Ex.Capabilities().AttachedStop {
		req.AttachedStop = plan.Stop
	}
	ev := journal.Event{ClientID: req.ClientID, Symbol: symbol, Side: string(req.Side), Qty: plan.Qty}
	e.record(journal.Event{Kind: journal.Proposed, ClientID: req.ClientID, Symbol: symbol, Side: string(req.Side), Qty: plan.Qty,
		Price: plan.Entry, Reason: fmt.Sprintf("%s stop %.8g target %.8g", side, plan.Stop, plan.Target)})
	st, err := e.send(ctx, ev, func() (exchange.OrderState, error) { return e.Ex.PlaceOrder(ctx, e.Env, req) })
	if err != nil || st.Status != exchange.Filled {
		return e.unconfirmed(ctx, req, st, err, plan.Stop)
	}
	if req.AttachedStop > 0 {
		return nil
	}
	return e.protect(ctx, symbol, side, st.Qty, plan.Stop, e.ID(symbol, candle, "stop"), e.ID(symbol, candle, "flatten"))
}

// protectiveTimeout bounds actions that must finish even after a stop
// request cancelled the loop's context.
const protectiveTimeout = 30 * time.Second

// protective detaches ctx from cancellation: once a position may exist, the
// actions that protect it are not abandoned halfway.
func protective(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), protectiveTimeout)
}

// unconfirmed handles an order whose fill is not confirmed (an error, or a
// state still open): it asks the exchange to cancel it, protects whatever did
// fill, and halts, unless the exchange cleanly rejected it.
func (e *Executor) unconfirmed(ctx context.Context, req exchange.OrderRequest, st exchange.OrderState, err error, stop float64) error {
	pctx, cancel := protective(ctx)
	defer cancel()
	if st.Status != exchange.Rejected {
		_, _ = e.Ex.CancelOrder(pctx, e.Env, req.ClientID)
	}
	rerr := e.Reconcile(pctx, func(exchange.Position) float64 { return stop })
	if st.Status == exchange.Rejected && err != nil {
		return errors.Join(fmt.Errorf("order %s rejected: %w", req.ClientID, err), rerr)
	}
	herr := fmt.Errorf("%w: order %s not confirmed filled (status %q, %v)", ErrHalt, req.ClientID, st.Status, err)
	e.record(journal.Event{Kind: journal.Halt, Symbol: req.Symbol, Reason: herr.Error()})
	return errors.Join(herr, rerr)
}

// protect places the stop or, failing that, flattens and halts.
func (e *Executor) protect(ctx context.Context, symbol string, side exchange.PositionSide, qty, stop float64, stopID, flatID string) error {
	ctx, cancel := protective(ctx)
	defer cancel()
	if stop <= 0 { // no known stop price: flatten rather than guess one
		return e.flattenAndHalt(ctx, symbol, side, qty, flatID, errors.New("no stop price known"))
	}
	ev := journal.Event{ClientID: stopID, Symbol: symbol, Side: string(side.Closing()), Qty: qty, Price: stop}
	st, err := e.send(ctx, ev, func() (exchange.OrderState, error) {
		return e.Ex.PlaceStop(ctx, e.Env, stopID, symbol, side.Closing(), qty, stop)
	})
	if err == nil && st.Status != exchange.Open {
		err = fmt.Errorf("stop not resting (status %q)", st.Status)
	}
	if err == nil {
		return nil
	}
	if !errors.Is(err, exchange.ErrRefused) { // R2: it may rest on the exchange; never leave it behind
		cev := journal.Event{ClientID: stopID, Symbol: symbol, Side: string(side.Closing()), Qty: qty,
			Reason: "cancel the stop of unknown fate before flattening"}
		_, _ = e.send(ctx, cev, func() (exchange.OrderState, error) { return e.Ex.CancelOrder(ctx, e.Env, stopID) })
	}
	return e.flattenAndHalt(ctx, symbol, side, qty, flatID, err)
}

// flattenAndHalt closes an unprotectable position at market and halts.
func (e *Executor) flattenAndHalt(ctx context.Context, symbol string, side exchange.PositionSide, qty float64, flatID string, err error) error {
	flat := exchange.OrderRequest{ClientID: flatID, Symbol: symbol, Side: side.Closing(), Qty: qty, Type: exchange.Market, ReduceOnly: true}
	ev := journal.Event{ClientID: flatID, Symbol: symbol, Side: string(side.Closing()), Qty: qty}
	_, ferr := e.send(ctx, ev, func() (exchange.OrderState, error) { return e.Ex.PlaceOrder(ctx, e.Env, flat) })
	herr := fmt.Errorf("%w: stop for %s failed (%v); position flattened", ErrHalt, symbol, err)
	if ferr != nil {
		herr = fmt.Errorf("%w: stop for %s failed (%v) and flattening failed: %v", ErrHalt, symbol, err, ferr)
	}
	e.record(journal.Event{Kind: journal.Halt, Symbol: symbol, Reason: herr.Error()})
	return herr
}

// Reconcile runs before any decision: every position without a stop on the
// exchange gets one at stopFor(position), or is flattened when that stop is
// unknown (0) or does not fit on the losing side of the market.
func (e *Executor) Reconcile(ctx context.Context, stopFor func(exchange.Position) float64) error {
	ps, err := e.Ex.Positions(ctx, e.Env)
	if err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	now := e.Now()
	var errs []error
	for _, p := range ps {
		if p.Qty <= 0 || p.StopClientID != "" {
			continue
		}
		errs = append(errs, e.protect(ctx, p.Symbol, p.PSide(), p.Qty, stopFor(p),
			e.ID(p.Symbol, now, "reconcile-stop"), e.ID(p.Symbol, now, "reconcile-flatten")))
	}
	return errors.Join(errs...)
}

// Exit closes a position at market: its stop is cancelled first so a
// resting stop cannot outlive the position; if the sell then fails, the
// stop is put back (or the position flattened and the engine halted).
func (e *Executor) Exit(ctx context.Context, p exchange.Position, candle time.Time, reason string) error {
	if err := ctx.Err(); err != nil {
		return err // not started: the stop still rests on the exchange
	}
	// from the stop cancel on, nothing is abandoned on cancellation: a cancel
	// cut short may still reach the exchange and leave the position naked
	pctx, cancel := protective(ctx)
	defer cancel()
	if p.StopClientID != "" {
		ev := journal.Event{ClientID: p.StopClientID, Symbol: p.Symbol, Side: string(p.PSide().Closing()), Qty: p.Qty, Reason: reason}
		if _, err := e.send(pctx, ev, func() (exchange.OrderState, error) { return e.Ex.CancelOrder(pctx, e.Env, p.StopClientID) }); err != nil {
			return errors.Join(fmt.Errorf("exit %s: cancel stop: %w", p.Symbol, err),
				e.Reconcile(pctx, func(exchange.Position) float64 { return p.StopPrice }))
		}
	}
	req := exchange.OrderRequest{ClientID: e.ID(p.Symbol, candle, "exit"), Symbol: p.Symbol, Side: p.PSide().Closing(), Qty: p.Qty,
		Type: exchange.Market, ReduceOnly: true}
	ev := journal.Event{ClientID: req.ClientID, Symbol: p.Symbol, Side: string(req.Side), Qty: p.Qty, Reason: reason}
	st, err := e.send(pctx, ev, func() (exchange.OrderState, error) { return e.Ex.PlaceOrder(pctx, e.Env, req) })
	if err == nil && st.Status == exchange.Filled {
		return nil
	}
	if st.Status != exchange.Rejected {
		_, _ = e.Ex.CancelOrder(pctx, e.Env, req.ClientID)
	}
	perr := e.Reconcile(pctx, func(exchange.Position) float64 { return p.StopPrice })
	return errors.Join(fmt.Errorf("exit %s not confirmed (status %q): %v", p.Symbol, st.Status, err), perr)
}
