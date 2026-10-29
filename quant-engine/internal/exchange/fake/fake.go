// Package fake is an in-memory, deterministic exchange for tests and dry
// runs. Prices come from fixture candles; Advance moves to the next candle
// and triggers resting stops.
package fake

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/market"
)

type Options struct {
	Paper        bool // refuse exchange.Real
	AttachedStop bool // support entry orders with an attached stop
	QtyStep      float64
	MinQty       float64
	Cash         float64
	Candles      map[string][]market.Candle
	// Costs, in percent: fills move against the order by half the spread
	// plus slippage; commission is charged on each fill's notional.
	SpreadPct, SlippagePct, CommissionPct float64
	Latency                               time.Duration // per call; honours ctx cancellation
}

type position struct {
	qty, avg float64
	stopID   string
}

type failure struct {
	err     error
	applied bool
}

type Fake struct {
	mu          sync.Mutex
	opt         Options
	cursor      int
	cash        float64
	pos         map[string]*position
	orders      map[string]exchange.OrderState
	failures    map[string]failure
	calls       map[string]int
	unprotected int
}

func New(o Options) *Fake {
	return &Fake{opt: o, cash: o.Cash, pos: map[string]*position{}, orders: map[string]exchange.OrderState{},
		failures: map[string]failure{}, calls: map[string]int{}}
}

// FailNext makes the next call to method return err. applied=true performs
// the call first (the exchange acted but the response was lost).
func (f *Fake) FailNext(method string, err error, applied bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[method] = failure{err, applied}
}

func (f *Fake) Calls(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

// Unprotected counts candles that passed while some position had no stop.
func (f *Fake) Unprotected() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unprotected
}

// SetPosition plants a holding without a stop, as left by a crash.
func (f *Fake) SetPosition(symbol string, qty, price float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pos[symbol] = &position{qty: qty, avg: price}
}

// Advance moves every symbol to its next candle, fills stops the candle
// crossed (at the stop, or the open on a gap down) and reports false when
// the candles are exhausted.
func (f *Fake) Advance() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.hasCandle(f.cursor + 1) {
		return false
	}
	f.cursor++
	for sym, p := range f.pos {
		c := f.opt.Candles[sym][f.cursor]
		if p.stopID == "" {
			f.unprotected++
			continue
		}
		st := f.orders[p.stopID]
		if c.Low <= st.StopPrice {
			st.FillPrice, st.Status = f.fill(min(c.Open, st.StopPrice), exchange.Sell, p.qty), exchange.Filled
			f.orders[p.stopID] = st
			f.cash += p.qty * st.FillPrice
			delete(f.pos, sym)
		}
	}
	return true
}

// fill applies spread and slippage to a price and charges the commission.
func (f *Fake) fill(px float64, side exchange.Side, qty float64) float64 {
	adj := (f.opt.SpreadPct/2 + f.opt.SlippagePct) / 100
	if side == exchange.Buy {
		px *= 1 + adj
	} else {
		px *= 1 - adj
	}
	f.cash -= px * qty * f.opt.CommissionPct / 100
	return px
}

// Order returns a known order's state (test and backtest accessor).
func (f *Fake) Order(clientID string) (exchange.OrderState, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.orders[clientID]
	return st, ok
}

func (f *Fake) hasCandle(i int) bool {
	for _, cs := range f.opt.Candles {
		if i >= len(cs) {
			return false
		}
	}
	return len(f.opt.Candles) > 0
}

// begin counts the call, checks env and takes an injected failure.
// late is an injected error to return after the call has taken effect.
func (f *Fake) begin(ctx context.Context, method string, env exchange.Env) (late, err error) {
	f.calls[method]++
	if f.opt.Latency > 0 {
		f.mu.Unlock()
		select {
		case <-time.After(f.opt.Latency):
		case <-ctx.Done():
		}
		f.mu.Lock()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if err := exchange.CheckEnv(f.opt.Paper, env); err != nil {
		return nil, err
	}
	fl, ok := f.failures[method]
	if !ok {
		return nil, nil
	}
	delete(f.failures, method)
	if !fl.applied {
		return nil, fl.err
	}
	return fl.err, nil
}

func (f *Fake) price(symbol string) (float64, error) {
	cs, ok := f.opt.Candles[symbol]
	if !ok || f.cursor >= len(cs) {
		return 0, fmt.Errorf("fake: no price for %s", symbol)
	}
	return cs[f.cursor].Close, nil
}

func (f *Fake) Candles(ctx context.Context, env exchange.Env, symbol string, _ time.Duration, n int) ([]market.Candle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	late, err := f.begin(ctx, "Candles", env)
	if err != nil {
		return nil, err
	}
	cs, ok := f.opt.Candles[symbol]
	if !ok {
		return nil, fmt.Errorf("fake: unknown symbol %s", symbol)
	}
	end := min(f.cursor+1, len(cs))
	return slices.Clone(cs[max(0, end-n):end]), late
}

func (f *Fake) Quote(ctx context.Context, env exchange.Env, symbol string) (exchange.Quote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	late, err := f.begin(ctx, "Quote", env)
	if err != nil {
		return exchange.Quote{}, err
	}
	p, err := f.price(symbol)
	if err != nil {
		return exchange.Quote{}, err
	}
	return exchange.Quote{Bid: p, Ask: p, Last: p}, late
}

func (f *Fake) PlaceOrder(ctx context.Context, env exchange.Env, r exchange.OrderRequest) (exchange.OrderState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	late, err := f.begin(ctx, "PlaceOrder", env)
	if err != nil {
		return exchange.OrderState{}, err
	}
	if st, ok := f.orders[r.ClientID]; ok {
		return st, late // idempotent: a known client ID is never executed twice
	}
	st, err := f.place(r)
	if err != nil {
		return st, err
	}
	return st, late
}

func (f *Fake) place(r exchange.OrderRequest) (exchange.OrderState, error) {
	st := exchange.OrderState{ClientID: r.ClientID, Symbol: r.Symbol, Side: r.Side, Qty: r.Qty, Status: exchange.Rejected}
	px, err := f.price(r.Symbol)
	switch {
	case err != nil:
		return st, err
	case r.Type != exchange.Market:
		return st, errors.New("fake: only market orders are supported")
	case r.Qty <= 0 || r.Qty < f.opt.MinQty:
		return st, fmt.Errorf("fake: quantity %v below the minimum %v", r.Qty, f.opt.MinQty)
	case r.AttachedStop > 0 && !f.opt.AttachedStop:
		return st, errors.New("fake: attached stops not supported")
	case r.AttachedStop > 0 && (r.Side != exchange.Buy || r.AttachedStop >= px):
		return st, errors.New("fake: attached stop must be below the price of a buy")
	}
	p := f.pos[r.Symbol]
	if r.Side == exchange.Sell && (p == nil || r.Qty > p.qty) {
		return st, errors.New("fake: sell exceeds the position")
	}
	px = f.fill(px, r.Side, r.Qty)
	if r.Side == exchange.Buy {
		if p == nil {
			p = &position{}
			f.pos[r.Symbol] = p
		}
		p.avg = (p.avg*p.qty + px*r.Qty) / (p.qty + r.Qty)
		p.qty += r.Qty
		f.cash -= px * r.Qty
	} else {
		p.qty -= r.Qty
		f.cash += px * r.Qty
		if p.qty == 0 {
			f.cancelStop(p)
			delete(f.pos, r.Symbol)
		}
	}
	st.FillPrice, st.Status = px, exchange.Filled
	f.orders[r.ClientID] = st
	if r.AttachedStop > 0 {
		id := r.ClientID + "-stop"
		f.orders[id] = exchange.OrderState{ClientID: id, Symbol: r.Symbol, Side: exchange.Sell, Qty: p.qty, StopPrice: r.AttachedStop, Status: exchange.Open}
		p.stopID = id
	}
	return st, nil
}

func (f *Fake) cancelStop(p *position) {
	if st, ok := f.orders[p.stopID]; ok && st.Status == exchange.Open {
		st.Status = exchange.Canceled
		f.orders[p.stopID] = st
	}
	p.stopID = ""
}

func (f *Fake) PlaceStop(ctx context.Context, env exchange.Env, clientID, symbol string, side exchange.Side, qty, stopPrice float64) (exchange.OrderState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	late, err := f.begin(ctx, "PlaceStop", env)
	if err != nil {
		return exchange.OrderState{}, err
	}
	if st, ok := f.orders[clientID]; ok {
		return st, late
	}
	st := exchange.OrderState{ClientID: clientID, Symbol: symbol, Side: side, Qty: qty, StopPrice: stopPrice, Status: exchange.Rejected}
	p := f.pos[symbol]
	px, err := f.price(symbol)
	switch {
	case err != nil:
		return st, err
	case side != exchange.Sell || p == nil || qty <= 0 || qty > p.qty:
		return st, errors.New("fake: a stop must sell at most the held quantity")
	case stopPrice >= px:
		return st, errors.New("fake: stop must be below the current price")
	}
	f.cancelStop(p)
	st.Status = exchange.Open
	f.orders[clientID] = st
	p.stopID = clientID
	return st, late
}

func (f *Fake) CancelOrder(ctx context.Context, env exchange.Env, clientID string) (exchange.OrderState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	late, err := f.begin(ctx, "CancelOrder", env)
	if err != nil {
		return exchange.OrderState{}, err
	}
	st, ok := f.orders[clientID]
	if !ok {
		return st, fmt.Errorf("fake: unknown order %s", clientID)
	}
	if p := f.pos[st.Symbol]; p != nil && p.stopID == clientID {
		f.cancelStop(p)
	}
	return f.orders[clientID], late
}

func (f *Fake) Positions(ctx context.Context, env exchange.Env) ([]exchange.Position, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	late, err := f.begin(ctx, "Positions", env)
	if err != nil {
		return nil, err
	}
	out := []exchange.Position{}
	for sym, p := range f.pos {
		ep := exchange.Position{Symbol: sym, Qty: p.qty, AvgPrice: p.avg}
		if p.stopID != "" {
			ep.StopClientID, ep.StopPrice = p.stopID, f.orders[p.stopID].StopPrice
		}
		out = append(out, ep)
	}
	slices.SortFunc(out, func(a, b exchange.Position) int { return strings.Compare(a.Symbol, b.Symbol) })
	return out, late
}

func (f *Fake) Balance(ctx context.Context, env exchange.Env) (exchange.Balance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	late, err := f.begin(ctx, "Balance", env)
	if err != nil {
		return exchange.Balance{}, err
	}
	b := exchange.Balance{Cash: f.cash, Equity: f.cash}
	for sym, p := range f.pos {
		px, _ := f.price(sym)
		b.Equity += p.qty * px
	}
	return b, late
}

func (f *Fake) Capabilities() exchange.Capabilities {
	return exchange.Capabilities{AttachedStop: f.opt.AttachedStop, Demo: true, QtyStep: f.opt.QtyStep, MinQty: f.opt.MinQty}
}
