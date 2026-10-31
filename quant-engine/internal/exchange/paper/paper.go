// Package paper trades on paper against a real market's public data: prices
// and candles come from another adapter (no credentials needed), fills and
// stops are simulated here. It is the paper mode when no demo account is
// configured; the stop then lives in this simulator, not on an exchange, which
// is acceptable only because no money is at stake. Mainnet never uses it.
//
// On a swap market it simulates a linear perp: long and short, the margin
// locked at the leverage ConfigureMarket set, and an ESTIMATED liquidation
// (the venue's formula when the data adapter has one, else 1/leverage) at
// which the position is closed and its margin lost. Funding is not modelled
// (FR-P5): FundingNote says so in status.
//
// Every fill pays the profile's costs (config.Sim): it takes the price
// read after the profile's latency, moved by its slippage against the
// trader (a triggered stop and an exit too), and pays its taker fee out of
// cash. shortcut: fixed slippage per liquidity class, not a walk of the live
// order book at the fill's size; walk the book when a stressed-market model
// is needed.
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
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/risk"
	"github.com/rosaldo/agency-agents/quant-engine/market"
)

type hold struct {
	side     exchange.PositionSide
	qty, avg float64
	margin   float64 // locked: notional/leverage at entry (the whole notional on spot)
	liq      float64 // estimated liquidation price; 0 = none (spot, or leverage 1 long)
	stopID   string
	stop     float64
	since    time.Time // stop placement time: only later candles can trigger it
}

// sign is +1 for a long, -1 for a short.
func (h *hold) sign() float64 {
	if h.side == exchange.Short {
		return -1
	}
	return 1
}

// crossed reports a price at or beyond level on h's losing side.
func (h *hold) crossed(px, level float64) bool {
	if h.side == exchange.Short {
		return px >= level
	}
	return px <= level
}

type Sim struct {
	data   exchange.Exchange
	now    func() time.Time
	mu     sync.Mutex
	cash   float64 // free cash (spot) or free margin (swap)
	pos    map[string]*hold
	orders map[string]exchange.OrderState
	margin map[string]exchange.MarginSettings // what ConfigureMarket set; leverage 1 when unset
	prof   Profile
	sleep  func(context.Context, time.Duration) error
}

// Profile is the sim profile the simulator charges: its name and the
// costs per symbol. An empty Costs charges nothing (tests); with Costs set,
// an order on a symbol without them is refused.
type Profile struct {
	Name  string
	Costs map[string]config.SimCosts
}

func New(data exchange.Exchange, cash float64, now func() time.Time, prof Profile) *Sim {
	return &Sim{data: data, now: now, cash: cash, pos: map[string]*hold{}, orders: map[string]exchange.OrderState{},
		margin: map[string]exchange.MarginSettings{}, prof: prof, sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SimNote is what status and the journal show about the costs charged.
func (s *Sim) SimNote() string {
	if s.prof.Name == "" {
		return ""
	}
	syms := make([]string, 0, len(s.prof.Costs))
	for sym := range s.prof.Costs {
		syms = append(syms, sym)
	}
	slices.Sort(syms)
	parts := []string{"sim profile " + s.prof.Name}
	for _, sym := range syms {
		c := s.prof.Costs[sym]
		parts = append(parts, fmt.Sprintf("%s: latency %v, slippage %g%%, taker fee %g%%", sym, c.Latency, c.SlippagePct, c.CommissionPct))
	}
	return strings.Join(parts, "; ")
}

func (s *Sim) costs(symbol string) (config.SimCosts, error) {
	c, ok := s.prof.Costs[symbol]
	if !ok && s.prof.Costs != nil {
		return c, fmt.Errorf("paper: no simulated costs for %s", symbol)
	}
	return c, nil
}

// slipped is px moved pct against the side that trades at it.
func slipped(px float64, side exchange.Side, pct float64) float64 {
	if side == exchange.Buy {
		return px * (1 + pct/100)
	}
	return px * (1 - pct/100)
}

// fee is the taker fee of a fill.
func fee(c config.SimCosts, qty, px float64) float64 { return qty * px * c.CommissionPct / 100 }

func (s *Sim) swap() bool { return s.data.Capabilities().Swap }

// FundingNote is what status shows about funding (FR-P5): the simulator
// leaves it out of P&L and drawdown; "" on spot, which pays none.
func (s *Sim) FundingNote() string {
	if !s.swap() {
		return ""
	}
	return "funding not modelled: the paper simulator leaves it out of P&L and drawdown"
}

func (s *Sim) leverage(symbol string) float64 {
	if l := s.margin[symbol].Leverage; l > 0 {
		return float64(l)
	}
	return 1
}

// liquidation estimates the liquidation price of a fresh position: the data
// adapter's venue formula when it has one, else at the loss of all margin.
func (s *Sim) liquidation(symbol string, side exchange.PositionSide, entry, lev float64) float64 {
	if !s.swap() {
		return 0
	}
	d, ok := s.LiquidationDistance(symbol, side, entry, lev)
	if !ok {
		d = risk.FallbackLiquidationDistance(lev, 0)
	}
	if side == exchange.Long && d >= 1 {
		return 0 // a long at leverage 1 cannot lose more than its margin
	}
	return risk.LiquidationPrice(side == exchange.Short, entry, d)
}

// LiquidationDistance forwards the data adapter's venue formula (FR-P4).
func (s *Sim) LiquidationDistance(symbol string, side exchange.PositionSide, entry, leverage float64) (float64, bool) {
	if est, ok := s.data.(exchange.LiquidationEstimator); ok {
		return est.LiquidationDistance(symbol, side, entry, leverage)
	}
	return 0, false
}

// Data is the adapter whose public data the simulator trades on.
func (s *Sim) Data() exchange.Exchange { return s.data }

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
	if h := s.pos[symbol]; h != nil {
		for _, c := range cs {
			worst, gap := c.Low, min(c.Open, h.stop)
			if h.side == exchange.Short {
				worst, gap = c.High, max(c.Open, h.stop)
			}
			if c.Time.After(h.since) && s.trigger(symbol, h, worst, gap) {
				break
			}
		}
	}
	return cs, nil
}

// trigger closes h when price px crossed its stop (filled at fill) or its
// liquidation, whichever the price reached first on the losing side: a stop
// past the liquidation never fills. It reports whether h was closed.
func (s *Sim) trigger(symbol string, h *hold, px, fill float64) bool {
	stopHit := h.stopID != "" && h.crossed(px, h.stop)
	switch {
	case h.liq > 0 && h.crossed(px, h.liq) && (!stopHit || h.crossed(fill, h.liq)):
		s.liquidate(symbol, h)
	case stopHit:
		s.fillStop(symbol, h, fill)
	default:
		return false
	}
	return true
}

// fillStop fills h's stop at px moved by the slippage, less the fee.
func (s *Sim) fillStop(symbol string, h *hold, px float64) {
	c, _ := s.costs(symbol) // h exists, so its symbol's order passed costs()
	st := s.orders[h.stopID]
	px = slipped(px, h.side.Closing(), c.SlippagePct)
	st.Status, st.FillPrice, st.Filled = exchange.Filled, px, h.qty
	s.orders[h.stopID] = st
	s.cash += h.margin + h.sign()*h.qty*(px-h.avg) - fee(c, h.qty, px)
	delete(s.pos, symbol)
}

// liquidate closes h at its estimated liquidation: the margin is lost and
// its stop is gone with it.
func (s *Sim) liquidate(symbol string, h *hold) {
	if h.stopID != "" {
		st := s.orders[h.stopID]
		st.Status = exchange.Canceled
		s.orders[h.stopID] = st
	}
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
	c, err := s.costs(r.Symbol)
	if err != nil {
		return exchange.OrderState{}, err
	}
	if err := s.sleep(ctx, c.Latency); err != nil { // the fill takes the price after the order's delay
		return exchange.OrderState{}, err
	}
	px, err := s.last(ctx, r.Symbol)
	if err != nil {
		return exchange.OrderState{}, err
	}
	px = slipped(px, r.Side, c.SlippagePct)
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
	case h != nil && r.Side == h.side.Closing():
		if r.Qty > h.qty {
			return st, errors.New("paper: the order closes more than the position")
		}
		freed := h.margin * r.Qty / h.qty
		s.cash += freed + h.sign()*r.Qty*(px-h.avg) - fee(c, r.Qty, px)
		h.qty, h.margin = h.qty-r.Qty, h.margin-freed
		if h.qty <= 0 {
			delete(s.pos, r.Symbol)
		}
	case r.ReduceOnly:
		return st, errors.New("paper: reduce-only order without a position to reduce")
	case r.Side == exchange.Sell && !s.swap():
		return st, errors.New("paper: sell exceeds the position")
	default:
		side := exchange.Long
		if r.Side == exchange.Sell {
			side = exchange.Short
		}
		if h == nil {
			h = &hold{side: side}
			s.pos[r.Symbol] = h
		}
		lev := s.leverage(r.Symbol)
		m := px * r.Qty / lev
		h.avg = (h.avg*h.qty + px*r.Qty) / (h.qty + r.Qty)
		h.qty += r.Qty
		h.margin += m
		h.liq = s.liquidation(r.Symbol, side, h.avg, lev)
		s.cash -= m + fee(c, r.Qty, px)
	}
	st.Status, st.FillPrice, st.Filled = exchange.Filled, px, r.Qty
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
	if h == nil || side != h.side.Closing() || qty > h.qty || h.side == exchange.Long && stopPrice >= px ||
		h.side == exchange.Short && stopPrice <= px {
		return st, errors.New("paper: a stop must close at most the position, on its losing side of the price")
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
	lasts, err := s.lasts(ctx, syms)
	if err != nil {
		return nil, err
	}
	// a stop the price crossed fills at the price after the order's delay
	fills := lasts
	if hit := s.stopsHit(lasts); len(hit) > 0 {
		var delay time.Duration
		for _, sym := range hit {
			c, _ := s.costs(sym)
			delay = max(delay, c.Latency)
		}
		if err := s.sleep(ctx, delay); err != nil {
			return nil, err
		}
		if fills, err = s.lasts(ctx, hit); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []exchange.Position{}
	for sym, h := range s.pos {
		px, ok := lasts[sym]
		fill, delayed := fills[sym]
		if !delayed {
			fill = px
		}
		if ok && s.trigger(sym, h, px, fill) {
			continue
		}
		p := exchange.Position{Symbol: sym, Side: h.side, Qty: h.qty, AvgPrice: h.avg, StopClientID: h.stopID, StopPrice: h.stop}
		if h.stopID != "" {
			p.StopQty = s.orders[h.stopID].Qty
		}
		if s.swap() {
			p.Leverage, p.MarginMode, p.LiquidationPrice = s.leverage(sym), s.margin[sym].MarginMode, h.liq
		}
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b exchange.Position) int { return strings.Compare(a.Symbol, b.Symbol) })
	return out, nil
}

func (s *Sim) lasts(ctx context.Context, syms []string) (map[string]float64, error) {
	out := map[string]float64{}
	for _, sym := range syms {
		px, err := s.last(ctx, sym)
		if err != nil {
			return nil, err
		}
		out[sym] = px
	}
	return out, nil
}

// stopsHit lists the symbols whose resting stop px has crossed.
func (s *Sim) stopsHit(px map[string]float64) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var hit []string
	for sym, h := range s.pos {
		if p, ok := px[sym]; ok && h.stopID != "" && h.crossed(p, h.stop) {
			hit = append(hit, sym)
		}
	}
	return hit
}

func (s *Sim) Balance(ctx context.Context, env exchange.Env) (exchange.Balance, error) {
	ps, err := s.Positions(ctx, env)
	if err != nil {
		return exchange.Balance{}, err
	}
	lasts := map[string]float64{}
	for _, p := range ps {
		px, err := s.last(ctx, p.Symbol)
		if err != nil {
			return exchange.Balance{}, err
		}
		lasts[p.Symbol] = px
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := exchange.Balance{Cash: s.cash, Equity: s.cash}
	for sym, h := range s.pos {
		if px, ok := lasts[sym]; ok {
			b.Equity += h.margin + h.sign()*h.qty*(px-h.avg)
		}
	}
	return b, nil
}

// Capabilities are the data adapter's, minus attached stops, and without a
// reported liquidation: the simulator's is an estimate.
func (s *Sim) Capabilities() exchange.Capabilities {
	c := s.data.Capabilities()
	c.AttachedStop, c.ReportsLiquidation, c.Netting = false, false, false
	return c
}

// ConfigureMarket applies the settings to the simulated account: entries
// lock notional/leverage of margin. Spot keeps leverage 1, no margin mode.
func (s *Sim) ConfigureMarket(_ context.Context, env exchange.Env, symbol string, want exchange.MarginSettings) (exchange.MarginSettings, error) {
	if err := check(env); err != nil {
		return exchange.MarginSettings{}, err
	}
	if !s.swap() {
		return exchange.MarginSettings{Leverage: 1}, nil
	}
	if want.Leverage < 1 {
		return exchange.MarginSettings{}, errors.New("paper: leverage must be >= 1")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.margin[symbol] = want
	return want, nil
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
