// Package paper trades on paper against a real market's public data: prices
// and candles come from another adapter (no credentials needed), fills and
// stops are simulated here. It is the paper mode when no demo account is
// configured; the stop then lives in this simulator, not on an exchange, which
// is acceptable only because no money is at stake. Live never uses it.
package paper

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

type hold struct {
	qty, avg float64
	stopID   string
	stop     float64
	since    time.Time // stop placement time: only later candles can trigger it
}

type Sim struct {
	data   exchange.Exchange
	now    func() time.Time
	mu     sync.Mutex
	cash   float64
	pos    map[string]*hold
	orders map[string]exchange.OrderState
}

func New(data exchange.Exchange, cash float64, now func() time.Time) *Sim {
	return &Sim{data: data, now: now, cash: cash, pos: map[string]*hold{}, orders: map[string]exchange.OrderState{}}
}

func check(env exchange.Env) error { return exchange.CheckEnv(true, env) }

func (s *Sim) Candles(ctx context.Context, env exchange.Env, symbol string, interval time.Duration, n int) ([]market.Candle, error) {
	if err := check(env); err != nil {
		return nil, err
	}
	cs, err := s.data.Candles(ctx, exchange.Demo, symbol, interval, n)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.pos[symbol]; h != nil && h.stopID != "" {
		for _, c := range cs {
			if c.Time.After(h.since) && c.Low <= h.stop {
				s.fillStop(symbol, h, min(c.Open, h.stop))
				break
			}
		}
	}
	return cs, nil
}

func (s *Sim) fillStop(symbol string, h *hold, px float64) {
	st := s.orders[h.stopID]
	st.Status, st.FillPrice = exchange.Filled, px
	s.orders[h.stopID] = st
	s.cash += h.qty * px
	delete(s.pos, symbol)
}

func (s *Sim) Quote(ctx context.Context, env exchange.Env, symbol string) (exchange.Quote, error) {
	if err := check(env); err != nil {
		return exchange.Quote{}, err
	}
	return s.data.Quote(ctx, exchange.Demo, symbol)
}

func (s *Sim) last(ctx context.Context, symbol string) (float64, error) {
	q, err := s.data.Quote(ctx, exchange.Demo, symbol)
	if err != nil {
		return 0, err
	}
	return q.Last, nil
}

func (s *Sim) PlaceOrder(ctx context.Context, env exchange.Env, r exchange.OrderRequest) (exchange.OrderState, error) {
	if err := check(env); err != nil {
		return exchange.OrderState{}, err
	}
	px, err := s.last(ctx, r.Symbol)
	if err != nil {
		return exchange.OrderState{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.orders[r.ClientID]; ok {
		return st, nil
	}
	st := exchange.OrderState{ClientID: r.ClientID, Symbol: r.Symbol, Side: r.Side, Qty: r.Qty, Status: exchange.Rejected}
	h := s.pos[r.Symbol]
	switch {
	case r.Type != exchange.Market || r.AttachedStop > 0 || r.Qty <= 0:
		return st, errors.New("paper: only plain market orders")
	case r.Side == exchange.Sell && (h == nil || r.Qty > h.qty):
		return st, errors.New("paper: sell exceeds the position")
	case r.Side == exchange.Buy:
		if h == nil {
			h = &hold{}
			s.pos[r.Symbol] = h
		}
		h.avg = (h.avg*h.qty + px*r.Qty) / (h.qty + r.Qty)
		h.qty += r.Qty
		s.cash -= px * r.Qty
	default:
		h.qty -= r.Qty
		s.cash += px * r.Qty
		if h.qty <= 0 {
			delete(s.pos, r.Symbol)
		}
	}
	st.Status, st.FillPrice = exchange.Filled, px
	s.orders[r.ClientID] = st
	return st, nil
}

func (s *Sim) PlaceStop(ctx context.Context, env exchange.Env, clientID, symbol string, side exchange.Side, qty, stopPrice float64) (exchange.OrderState, error) {
	if err := check(env); err != nil {
		return exchange.OrderState{}, err
	}
	px, err := s.last(ctx, symbol)
	if err != nil {
		return exchange.OrderState{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.orders[clientID]; ok {
		return st, nil
	}
	st := exchange.OrderState{ClientID: clientID, Symbol: symbol, Side: side, Qty: qty, StopPrice: stopPrice, Status: exchange.Rejected}
	h := s.pos[symbol]
	if h == nil || side != exchange.Sell || qty > h.qty || stopPrice >= px {
		return st, errors.New("paper: a stop must sell at most the position, below the price")
	}
	st.Status = exchange.Open
	s.orders[clientID] = st
	h.stopID, h.stop, h.since = clientID, stopPrice, s.now()
	return st, nil
}

func (s *Sim) CancelOrder(_ context.Context, env exchange.Env, clientID string) (exchange.OrderState, error) {
	if err := check(env); err != nil {
		return exchange.OrderState{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.orders[clientID]
	if !ok {
		return st, fmt.Errorf("paper: unknown order %s", clientID)
	}
	if h := s.pos[st.Symbol]; h != nil && h.stopID == clientID {
		h.stopID, h.stop = "", 0
		st.Status = exchange.Canceled
		s.orders[clientID] = st
	}
	return st, nil
}

// Positions also fills a stop the last price has already crossed.
func (s *Sim) Positions(ctx context.Context, env exchange.Env) ([]exchange.Position, error) {
	if err := check(env); err != nil {
		return nil, err
	}
	s.mu.Lock()
	syms := make([]string, 0, len(s.pos))
	for sym := range s.pos {
		syms = append(syms, sym)
	}
	s.mu.Unlock()
	lasts := map[string]float64{}
	for _, sym := range syms {
		px, err := s.last(ctx, sym)
		if err != nil {
			return nil, err
		}
		lasts[sym] = px
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []exchange.Position{}
	for sym, h := range s.pos {
		if px, ok := lasts[sym]; ok && h.stopID != "" && px <= h.stop {
			s.fillStop(sym, h, px)
			continue
		}
		out = append(out, exchange.Position{Symbol: sym, Qty: h.qty, AvgPrice: h.avg, StopClientID: h.stopID, StopPrice: h.stop})
	}
	slices.SortFunc(out, func(a, b exchange.Position) int { return strings.Compare(a.Symbol, b.Symbol) })
	return out, nil
}

func (s *Sim) Balance(ctx context.Context, env exchange.Env) (exchange.Balance, error) {
	ps, err := s.Positions(ctx, env)
	if err != nil {
		return exchange.Balance{}, err
	}
	s.mu.Lock()
	b := exchange.Balance{Cash: s.cash, Equity: s.cash}
	s.mu.Unlock()
	for _, p := range ps {
		px, err := s.last(ctx, p.Symbol)
		if err != nil {
			return exchange.Balance{}, err
		}
		b.Equity += p.Qty * px
	}
	return b, nil
}

func (s *Sim) Capabilities() exchange.Capabilities {
	c := s.data.Capabilities()
	c.AttachedStop = false
	return c
}

// Lot forwards the market data adapter's per-symbol lot, when it has one.
func (s *Sim) Lot(symbol string) (step, min float64) {
	if l, ok := s.data.(interface {
		Lot(string) (float64, float64)
	}); ok {
		return l.Lot(symbol)
	}
	c := s.data.Capabilities()
	return c.QtyStep, c.MinQty
}
