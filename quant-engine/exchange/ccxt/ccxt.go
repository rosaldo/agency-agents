// Package ccxt is the one generic exchange adapter: any spot crypto exchange
// CCXT supports, chosen by its CCXT id in the yml. No code is specific to an
// exchange. Spot has no positions, so the adapter keeps a book of what the
// engine bought and the stop resting for it, persisted to a file.
package ccxt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	lib "github.com/rosaldo/ccxt/v5"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/market"
)

// Client is the part of a CCXT exchange the adapter uses.
type Client interface {
	LoadMarkets(params ...any) (map[string]lib.MarketInterface, error)
	FetchOHLCV(symbol string, options ...lib.FetchOHLCVOptions) ([]lib.OHLCV, error)
	FetchTicker(symbol string, options ...lib.FetchTickerOptions) (lib.Ticker, error)
	FetchBalance(params ...any) (lib.Balances, error)
	CreateOrder(symbol, typeVar, side string, amount float64, options ...lib.CreateOrderOptions) (lib.Order, error)
	CreateStopMarketOrder(symbol, side string, amount, triggerPrice float64, options ...lib.CreateStopMarketOrderOptions) (lib.Order, error)
	CancelOrder(id string, options ...lib.CancelOrderOptions) (lib.Order, error)
	FetchOrder(id string, options ...lib.FetchOrderOptions) (lib.Order, error)
	FetchOpenOrders(options ...lib.FetchOpenOrdersOptions) ([]lib.Order, error)
	FetchClosedOrders(options ...lib.FetchClosedOrdersOptions) ([]lib.Order, error)
}

// Known reports whether id is a CCXT exchange id.
func Known(id string) bool { return slices.Contains(lib.Exchanges, id) }

// Dial builds the CCXT client for id. With credentials it trades; env Demo
// switches CCXT to the exchange's sandbox (its support is per exchange and
// must be checked in that exchange's documentation).
func Dial(id string, env exchange.Env, cred *config.Credentials) (Client, error) {
	ex := lib.CreateExchange(id, nil)
	if ex == nil {
		return nil, fmt.Errorf("ccxt: unknown exchange %q", id)
	}
	if cred != nil {
		ex.SetApiKey(cred.Key.Reveal())
		ex.SetSecret(cred.Secret.Reveal())
		if cred.Passphrase != "" {
			ex.SetPassword(cred.Passphrase.Reveal())
		}
	}
	if env == exchange.Demo && cred != nil {
		ex.SetSandboxMode(true)
	}
	return ex, nil
}

type Options struct {
	Symbols []string     // the traded markets (capabilities and quote currency)
	Env     exchange.Env // the environment the client is bound to
	Paper   bool         // refuse exchange.Real
	Book    string       // file holding the engine's positions and orders
	Now     func() time.Time
}

// ref is an order the engine sent. It is written to the book BEFORE the
// order goes out (ID empty until the exchange answers), so a lost response
// or a crash leaves a trace to resolve instead of a blind resend.
type ref struct {
	ID, Symbol string
	State      exchange.OrderState
	Created    time.Time
	Stop       bool    // a protective stop
	Applied    float64 // net quantity already applied to the holding
}

type holding struct {
	Qty, Avg float64
	StopID   string // client ID of the resting stop
	Stop     float64
}

type book struct {
	Orders    map[string]ref
	Positions map[string]*holding
}

type Adapter struct {
	c       Client
	opt     Options
	markets map[string]lib.MarketInterface
	mu      sync.Mutex
	b       book
}

func New(c Client, o Options) (*Adapter, error) {
	if !o.Paper && o.Env != exchange.Real {
		return nil, errors.New("ccxt: live must be bound to the real environment")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	all, err := c.LoadMarkets()
	if err != nil {
		return nil, fmt.Errorf("ccxt: markets: %w", err)
	}
	ms := map[string]lib.MarketInterface{}
	for _, sym := range o.Symbols {
		m, ok := all[sym]
		if !ok {
			return nil, fmt.Errorf("ccxt: market %s not found", sym)
		}
		if m.Spot != nil && !*m.Spot {
			return nil, fmt.Errorf("ccxt: %s is not a spot market; only spot is traded", sym)
		}
		ms[sym] = m
	}
	a := &Adapter{c: c, opt: o, markets: ms, b: book{Orders: map[string]ref{}, Positions: map[string]*holding{}}}
	if raw, err := os.ReadFile(o.Book); err == nil {
		if err := json.Unmarshal(raw, &a.b); err != nil {
			return nil, fmt.Errorf("ccxt: book %s: %w", o.Book, err)
		}
	}
	return a, nil
}

func (a *Adapter) save() error {
	if a.opt.Book == "" {
		return nil
	}
	raw, err := json.MarshalIndent(a.b, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.opt.Book + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.opt.Book)
}

// do runs a blocking CCXT call under the adapter lock, but returns as soon as
// ctx is done; the call then finishes in the background and still updates
// the book, so a late fill is never lost.
func do[T any](ctx context.Context, a *Adapter, env exchange.Env, fn func() (T, error)) (T, error) {
	var zero T
	if err := exchange.CheckEnv(a.opt.Paper, env); err != nil {
		return zero, err
	}
	if env != a.opt.Env {
		return zero, fmt.Errorf("ccxt: adapter bound to %s, called for %s", a.opt.Env, env)
	}
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		v, err := fn()
		ch <- result{v, err}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

func timeframe(d time.Duration) (string, error) {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour)), nil
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour), nil
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute), nil
	}
	return "", fmt.Errorf("ccxt: interval %v is not whole minutes, hours or days", d)
}

// Candles returns the last n CLOSED candles: the exchange's still-forming
// candle is dropped.
func (a *Adapter) Candles(ctx context.Context, env exchange.Env, symbol string, interval time.Duration, n int) ([]market.Candle, error) {
	tf, err := timeframe(interval)
	if err != nil {
		return nil, err
	}
	return do(ctx, a, env, func() ([]market.Candle, error) {
		rows, err := a.c.FetchOHLCV(symbol, lib.WithFetchOHLCVTimeframe(tf), lib.WithFetchOHLCVLimit(int64(n+1)))
		if err != nil {
			return nil, err
		}
		now := a.opt.Now()
		var out []market.Candle
		for _, r := range rows {
			t := time.UnixMilli(r.Timestamp).UTC()
			if t.Add(interval).After(now) {
				continue
			}
			out = append(out, market.Candle{Time: t, Open: r.Open, High: r.High, Low: r.Low, Close: r.Close, Volume: r.Volume})
		}
		return out[max(0, len(out)-n):], nil
	})
}

func val(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func (a *Adapter) Quote(ctx context.Context, env exchange.Env, symbol string) (exchange.Quote, error) {
	return do(ctx, a, env, func() (exchange.Quote, error) { return a.quote(symbol) })
}

func (a *Adapter) quote(symbol string) (exchange.Quote, error) {
	t, err := a.c.FetchTicker(symbol)
	if err != nil {
		return exchange.Quote{}, err
	}
	return exchange.Quote{Bid: val(t.Bid), Ask: val(t.Ask), Last: val(t.Last)}, nil
}

func status(o lib.Order) exchange.Status {
	switch deref(o.Status) {
	case "closed":
		return exchange.Filled
	case "canceled", "cancelled":
		return exchange.Canceled
	case "rejected", "expired":
		return exchange.Rejected
	}
	return exchange.Open
}

func fillPrice(o lib.Order) float64 {
	if p := val(o.Average); p > 0 {
		return p
	}
	return val(o.Price)
}

func terminal(st exchange.Status) bool {
	return st == exchange.Filled || st == exchange.Canceled || st == exchange.Rejected
}

// PlaceOrder sends a market order with the client order ID. A client ID the
// book already knows is never sent again: it is resolved on the exchange. A
// fill not confirmed after a few looks comes back as Open, never as success.
func (a *Adapter) PlaceOrder(ctx context.Context, env exchange.Env, r exchange.OrderRequest) (exchange.OrderState, error) {
	return do(ctx, a, env, func() (exchange.OrderState, error) {
		if known, ok := a.b.Orders[r.ClientID]; ok {
			if known.State.Status == exchange.Rejected {
				return known.State, fmt.Errorf("ccxt: order %s was refused by the exchange", r.ClientID)
			}
			if !terminal(known.State.Status) {
				if err := a.refresh(r.ClientID); err != nil {
					return a.b.Orders[r.ClientID].State, errors.Join(err, a.save())
				}
			}
			return a.b.Orders[r.ClientID].State, a.save()
		}
		if r.Type != exchange.Market || r.AttachedStop > 0 {
			return exchange.OrderState{Status: exchange.Rejected}, errors.New("ccxt: only plain market orders are sent")
		}
		a.b.Orders[r.ClientID] = ref{Symbol: r.Symbol, Created: a.opt.Now(),
			State: exchange.OrderState{ClientID: r.ClientID, Symbol: r.Symbol, Side: r.Side, Qty: r.Qty, Status: exchange.Open}}
		if err := a.save(); err != nil {
			delete(a.b.Orders, r.ClientID)
			return exchange.OrderState{Status: exchange.Rejected}, err
		}
		o, err := a.c.CreateOrder(r.Symbol, "market", string(r.Side), r.Qty,
			lib.WithCreateOrderParams(map[string]any{"clientOrderId": r.ClientID}))
		if err != nil {
			if !refused(err) {
				return a.b.Orders[r.ClientID].State, err // fate unknown: resolved by client ID later
			}
			ref := a.b.Orders[r.ClientID]
			ref.State.Status = exchange.Rejected
			a.b.Orders[r.ClientID] = ref
			return ref.State, errors.Join(err, a.save())
		}
		a.settle(r.ClientID, o)
		for i := 1; i <= 5 && a.b.Orders[r.ClientID].State.Status == exchange.Open; i++ {
			time.Sleep(time.Duration(i) * 200 * time.Millisecond)
			if err := a.refresh(r.ClientID); err != nil {
				return a.b.Orders[r.ClientID].State, errors.Join(err, a.save())
			}
		}
		return a.b.Orders[r.ClientID].State, a.save()
	})
}

// refused reports an error by which the exchange answered that it did not
// take the order (CCXT's error hierarchy, lib.ErrorParents). Anything else,
// a network error or a generic ExchangeError, leaves the order's fate unknown.
func refused(err error) bool {
	var ce *lib.Error
	if !errors.As(err, &ce) || ce.Type == lib.DuplicateOrderIdErrType { // a duplicate ID means it exists
		return false
	}
	for t := ce.Type; t != ""; t = lib.ErrorParents[t] {
		switch t {
		case lib.InsufficientFundsErrType, lib.InvalidOrderErrType, lib.BadRequestErrType,
			lib.AuthenticationErrorErrType, lib.OperationRejectedErrType:
			return true
		case lib.ExchangeErrorErrType, lib.OperationFailedErrType:
			return false
		}
	}
	return false
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// refresh reads an order's current state: by exchange ID when known, else by
// client order ID among the symbol's recent orders. Not found means unknown.
func (a *Adapter) refresh(clientID string) error {
	r := a.b.Orders[clientID]
	if r.ID != "" {
		o, err := a.c.FetchOrder(r.ID, lib.WithFetchOrderSymbol(r.Symbol))
		if err != nil {
			return fmt.Errorf("ccxt: order %s: %w", clientID, err)
		}
		a.settle(clientID, o)
		return nil
	}
	since := r.Created.Add(-time.Hour).UnixMilli()
	open, oerr := a.c.FetchOpenOrders(lib.WithFetchOpenOrdersSymbol(r.Symbol), lib.WithFetchOpenOrdersSince(since))
	closed, cerr := a.c.FetchClosedOrders(lib.WithFetchClosedOrdersSymbol(r.Symbol), lib.WithFetchClosedOrdersSince(since))
	for _, o := range append(open, closed...) {
		if deref(o.ClientOrderId) == clientID {
			a.settle(clientID, o)
			return nil
		}
	}
	return fmt.Errorf("ccxt: order %s sent without an answer and not found by client order ID (%v)", clientID, errors.Join(oerr, cerr))
}

// settle records an order's state and applies the net quantity it filled
// since last seen: the executed amount, less any fee charged in the base asset.
func (a *Adapter) settle(clientID string, o lib.Order) {
	r := a.b.Orders[clientID]
	if id := deref(o.Id); id != "" {
		r.ID = id
	}
	r.State.Status = status(o)
	if p := fillPrice(o); p > 0 {
		r.State.FillPrice = p
	}
	filled := val(o.Filled)
	if o.Filled == nil && r.State.Status == exchange.Filled {
		filled = r.State.Qty
	}
	base := deref(a.markets[r.Symbol].BaseCurrency)
	fee := 0.0
	fees := o.Fees
	if len(fees) == 0 {
		fees = []lib.Fee{o.Fee}
	}
	for _, fe := range fees {
		if deref(fe.Currency) == base {
			fee += val(fe.Cost)
		}
	}
	net := filled - fee
	if r.State.Side == exchange.Sell {
		net = filled + fee
	} else {
		net = a.floorStep(r.Symbol, net)
	}
	if delta := net - r.Applied; delta > 0 {
		a.apply(r, delta)
		r.Applied = net
	}
	if r.State.Side == exchange.Buy && net > 0 {
		r.State.Qty = net
	}
	a.b.Orders[clientID] = r
}

func (a *Adapter) floorStep(symbol string, q float64) float64 {
	if step := val(a.markets[symbol].Precision.Amount); step > 0 {
		return math.Floor(q/step+1e-9) * step
	}
	return q
}

func (a *Adapter) apply(r ref, qty float64) {
	h := a.b.Positions[r.Symbol]
	if r.State.Side == exchange.Buy {
		if h == nil {
			h = &holding{}
			a.b.Positions[r.Symbol] = h
		}
		h.Avg = (h.Avg*h.Qty + r.State.FillPrice*qty) / (h.Qty + qty)
		h.Qty += qty
		return
	}
	if h != nil {
		if h.Qty -= qty; h.Qty <= 1e-12 {
			delete(a.b.Positions, r.Symbol)
		}
	}
}

func (a *Adapter) PlaceStop(ctx context.Context, env exchange.Env, clientID, symbol string, side exchange.Side, qty, stopPrice float64) (exchange.OrderState, error) {
	return do(ctx, a, env, func() (exchange.OrderState, error) {
		if known, ok := a.b.Orders[clientID]; ok {
			if known.State.Status == exchange.Rejected { // a retry must not take a refusal as placed
				return known.State, fmt.Errorf("ccxt: stop %s was refused by the exchange", clientID)
			}
			return known.State, nil
		}
		h := a.b.Positions[symbol]
		if h == nil || side != exchange.Sell || qty > h.Qty+1e-12 {
			return exchange.OrderState{Status: exchange.Rejected}, errors.New("ccxt: a stop must sell at most the engine's holding")
		}
		st := exchange.OrderState{ClientID: clientID, Symbol: symbol, Side: side, Qty: qty, StopPrice: stopPrice, Status: exchange.Open}
		a.b.Orders[clientID] = ref{Symbol: symbol, State: st, Created: a.opt.Now(), Stop: true}
		if err := a.save(); err != nil {
			return st, err
		}
		o, err := a.c.CreateStopMarketOrder(symbol, string(side), qty, stopPrice,
			lib.WithCreateStopMarketOrderParams(map[string]any{"clientOrderId": clientID}))
		if err != nil {
			st.Status = exchange.Rejected
			r := a.b.Orders[clientID]
			r.State = st
			a.b.Orders[clientID] = r
			return st, errors.Join(err, a.save())
		}
		r := a.b.Orders[clientID]
		r.ID = deref(o.Id)
		a.b.Orders[clientID] = r
		h.StopID, h.Stop = clientID, stopPrice
		return st, a.save()
	})
}

func (a *Adapter) CancelOrder(ctx context.Context, env exchange.Env, clientID string) (exchange.OrderState, error) {
	return do(ctx, a, env, func() (exchange.OrderState, error) {
		r, ok := a.b.Orders[clientID]
		if !ok {
			return exchange.OrderState{}, fmt.Errorf("ccxt: unknown order %s", clientID)
		}
		if terminal(r.State.Status) {
			return r.State, nil
		}
		if r.ID == "" {
			if err := a.refresh(clientID); err != nil {
				return r.State, errors.Join(err, a.save())
			}
			r = a.b.Orders[clientID]
		}
		o, err := a.c.CancelOrder(r.ID, lib.WithCancelOrderSymbol(r.Symbol))
		if err != nil {
			_ = a.refresh(clientID) // it may have filled meanwhile
			return a.b.Orders[clientID].State, errors.Join(err, a.save())
		}
		a.settle(clientID, o)
		if r := a.b.Orders[clientID]; r.State.Status == exchange.Open {
			r.State.Status = exchange.Canceled
			a.b.Orders[clientID] = r
		}
		if h := a.b.Positions[r.Symbol]; h != nil && h.StopID == clientID {
			h.StopID, h.Stop = "", 0
		}
		return a.b.Orders[clientID].State, a.save()
	})
}

// Positions returns the engine's holdings after bringing every open order up
// to date: late fills are applied, filled stops close their holding, and a
// stop the exchange dropped leaves its holding unprotected for reconcile.
func (a *Adapter) Positions(ctx context.Context, env exchange.Env) ([]exchange.Position, error) {
	return do(ctx, a, env, func() ([]exchange.Position, error) {
		var errs []error
		for id, r := range a.b.Orders {
			if !terminal(r.State.Status) {
				errs = append(errs, a.refresh(id))
			}
		}
		for _, h := range a.b.Positions {
			if h.StopID != "" && terminal(a.b.Orders[h.StopID].State.Status) {
				h.StopID, h.Stop = "", 0
			}
		}
		if err := errors.Join(append(errs, a.save())...); err != nil {
			return nil, err
		}
		out := []exchange.Position{}
		for sym, h := range a.b.Positions {
			out = append(out, exchange.Position{Symbol: sym, Qty: h.Qty, AvgPrice: h.Avg, StopClientID: h.StopID, StopPrice: h.Stop})
		}
		slices.SortFunc(out, func(x, y exchange.Position) int { return strings.Compare(x.Symbol, y.Symbol) })
		return out, nil
	})
}

// Balance: cash is the free quote currency of the traded markets; equity adds
// the engine's holdings at the last price.
func (a *Adapter) Balance(ctx context.Context, env exchange.Env) (exchange.Balance, error) {
	return do(ctx, a, env, func() (exchange.Balance, error) {
		bs, err := a.c.FetchBalance()
		if err != nil {
			return exchange.Balance{}, err
		}
		quote := ""
		if len(a.opt.Symbols) > 0 {
			quote = deref(a.markets[a.opt.Symbols[0]].QuoteCurrency)
		}
		b := exchange.Balance{Cash: val(bs.Free[quote])}
		b.Equity = b.Cash
		for sym, h := range a.b.Positions {
			q, err := a.quote(sym)
			if err != nil {
				return exchange.Balance{}, err
			}
			b.Equity += h.Qty * q.Last
		}
		return b, nil
	})
}

// Capabilities are conservative: no attached stops; the quantity step reads
// CCXT's amount precision as a tick size (CCXT's usual precision mode; to be
// verified per exchange).
func (a *Adapter) Capabilities() exchange.Capabilities {
	var c exchange.Capabilities
	for _, m := range a.markets {
		c.QtyStep = max(c.QtyStep, val(m.Precision.Amount))
		c.MinQty = max(c.MinQty, val(m.Limits.Amount.Min))
	}
	return c
}

// Lot is one market's quantity step and minimum (precision read as a tick
// size, CCXT's usual mode; to be verified per exchange).
func (a *Adapter) Lot(symbol string) (step, min float64) {
	m := a.markets[symbol]
	return val(m.Precision.Amount), val(m.Limits.Amount.Min)
}
