// Package ccxt is the one generic exchange adapter: spot or linear swap
// markets of any exchange CCXT supports, through the fork
// github.com/rosaldo/ccxt/v5. No code is specific to an exchange: a venue
// (exchange/ccxt/<id>) only builds its client. The adapter keeps a book of
// the orders the engine sent and the positions they opened (its own
// quantity, never the account's), persisted to a file; on swap the
// exchange's position adds what only it knows (leverage, liquidation price).
package ccxt

import (
	"context"
	"encoding/hex"
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
	"github.com/rosaldo/agency-agents/quant-engine/market"
)

// Client is the part of a CCXT exchange the adapter uses. Stops go through
// CreateOrder with params.triggerPrice (CCXT's unified trigger), since not
// every exchange implements createStopMarketOrder (Hyperliquid does not).
type Client interface {
	LoadMarkets(params ...any) (map[string]lib.MarketInterface, error)
	FetchOHLCV(symbol string, options ...lib.FetchOHLCVOptions) ([]lib.OHLCV, error)
	FetchTicker(symbol string, options ...lib.FetchTickerOptions) (lib.Ticker, error)
	FetchBalance(params ...any) (lib.Balances, error)
	CreateOrder(symbol, typeVar, side string, amount float64, options ...lib.CreateOrderOptions) (lib.Order, error)
	CancelOrder(id string, options ...lib.CancelOrderOptions) (lib.Order, error)
	FetchOrder(id string, options ...lib.FetchOrderOptions) (lib.Order, error)
	FetchOpenOrders(options ...lib.FetchOpenOrdersOptions) ([]lib.Order, error)
	FetchClosedOrders(options ...lib.FetchClosedOrdersOptions) ([]lib.Order, error)
	FetchPositions(options ...lib.FetchPositionsOptions) ([]lib.Position, error)
	SetMarginMode(marginMode string, options ...lib.SetMarginModeOptions) (map[string]any, error)
	SetLeverage(leverage int64, options ...lib.SetLeverageOptions) (map[string]any, error)
}

// Venue is one exchange variant, chosen by the strategy's main at build time
// (one package per venue under exchange/ccxt/): the binary links only the
// constructor its venue references.
type Venue struct {
	ID          string   // CCXT id; the yml `exchange:` must equal it
	Credentials []string // env suffixes, read as <STRATEGY>_<ID>[_TESTNET]_<SUFFIX>
	Optional    []string // suffixes that may be absent
	// Dial builds the client: cred keyed by suffix (nil = public data only),
	// sandbox = testnet, symbols = the yml's, so a venue can scope what
	// LoadMarkets fetches.
	Dial func(cred map[string]string, sandbox bool, symbols []string) (Client, error)
	// ClientID formats a 16-byte order digest the way the venue accepts it;
	// nil = 32 hex characters.
	ClientID func(digest [16]byte) string
	// Equity: the venue lists equity markets; asset_class equity is refused
	// on a venue without them.
	Equity bool
	// Check runs on the loaded markets of the yml's symbols before trading;
	// an error refuses to start (venue rules the generic adapter cannot know).
	Check func(markets map[string]lib.MarketInterface) error
}

type Options struct {
	Venue   Venue
	Symbols []string     // the traded markets and the strategy's inputs
	Market  string       // "spot" | "swap" (linear); "" = spot
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
	Opens      bool    // opens or grows a position (else reduces it)
	Applied    float64 // net quantity already applied to the holding
}

type holding struct {
	Side     exchange.PositionSide
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
	want    exchange.MarginSettings // applied by ConfigureMarket; checked on every Positions
}

// New binds c to the yml's markets. FR-P1: only spot and linear swap are
// traded, read from CCXT's market structure, never from the symbol's name;
// every symbol must be of the yml's market type.
func New(c Client, o Options) (*Adapter, error) {
	if !o.Paper && o.Env != exchange.Real {
		return nil, errors.New("ccxt: live must be bound to the real environment")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Market == "" {
		o.Market = "spot"
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
		kind, err := marketKind(m)
		if err != nil {
			return nil, fmt.Errorf("ccxt: %s: %w", sym, err)
		}
		if kind != o.Market {
			return nil, fmt.Errorf("ccxt: %s is a %s market and the yml trades market: %s", sym, kind, o.Market)
		}
		ms[sym] = m
	}
	if o.Venue.Check != nil {
		if err := o.Venue.Check(ms); err != nil {
			return nil, fmt.Errorf("ccxt: %s: %w", o.Venue.ID, err)
		}
	}
	a := &Adapter{c: c, opt: o, markets: ms, b: book{Orders: map[string]ref{}, Positions: map[string]*holding{}}}
	if raw, err := os.ReadFile(o.Book); err == nil {
		if err := json.Unmarshal(raw, &a.b); err != nil {
			return nil, fmt.Errorf("ccxt: book %s: %w", o.Book, err)
		}
	}
	return a, nil
}

func flag(p *bool) bool { return p != nil && *p }

// marketKind is "spot" or "swap" for the markets the engine trades; future,
// option, margin and inverse swap are refused with the reason.
func marketKind(m lib.MarketInterface) (string, error) {
	typ := deref(m.Type)
	switch {
	case flag(m.Future) || flag(m.Option) || typ == "future" || typ == "option":
		return "", fmt.Errorf("market type %q is refused: only spot and linear swap are traded", typ)
	case typ == "margin":
		return "", errors.New("margin markets are refused: only spot and linear swap are traded")
	case flag(m.Swap) || typ == "swap":
		if flag(m.Inverse) || !flag(m.Linear) {
			return "", errors.New("inverse swap is refused: only linear swap is traded")
		}
		if cs := val(m.ContractSize); cs != 0 && cs != 1 {
			return "", fmt.Errorf("contract size %g: quantities would be in contracts, not in the base asset", cs)
		}
		return "swap", nil
	case flag(m.Spot) || typ == "spot":
		return "spot", nil
	}
	return "", fmt.Errorf("market type %q unknown", typ)
}

func (a *Adapter) swap() bool { return a.opt.Market == "swap" }

// FormatClientID shapes client order IDs for the venue (exchange.IDFormatter).
func (a *Adapter) FormatClientID(d [16]byte) string {
	if a.opt.Venue.ClientID != nil {
		return a.opt.Venue.ClientID(d)
	}
	return hex.EncodeToString(d[:])
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

// PlaceOrder sends a market order with the client order ID, a reference
// price (some venues build a market order as an IOC limit around it, V5) and,
// on swap, reduceOnly for exits. A client ID the book already knows is never
// sent again: it is resolved on the exchange. A fill not confirmed after a
// few looks comes back as Open, never as success.
func (a *Adapter) PlaceOrder(ctx context.Context, env exchange.Env, r exchange.OrderRequest) (exchange.OrderState, error) {
	return do(ctx, a, env, func() (exchange.OrderState, error) {
		if known, ok := a.b.Orders[r.ClientID]; ok {
			if known.State.Status == exchange.Rejected {
				return known.State, fmt.Errorf("%w: ccxt: order %s was refused by the exchange", exchange.ErrRefused, r.ClientID)
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
		if err := a.mayOpen(r.Symbol, r.Side, r.ReduceOnly); err != nil {
			return exchange.OrderState{Status: exchange.Rejected}, err
		}
		q, err := a.quote(r.Symbol)
		if err != nil {
			return exchange.OrderState{Status: exchange.Rejected}, fmt.Errorf("ccxt: reference price: %w", err)
		}
		px := q.Ask
		if r.Side == exchange.Sell {
			px = q.Bid
		}
		if px <= 0 {
			px = q.Last
		}
		if px <= 0 {
			return exchange.OrderState{Status: exchange.Rejected}, fmt.Errorf("ccxt: no reference price for %s", r.Symbol)
		}
		a.b.Orders[r.ClientID] = ref{Symbol: r.Symbol, Created: a.opt.Now(), Opens: !r.ReduceOnly && (a.swap() || r.Side == exchange.Buy),
			State: exchange.OrderState{ClientID: r.ClientID, Symbol: r.Symbol, Side: r.Side, Qty: r.Qty, Status: exchange.Open}}
		if err := a.save(); err != nil {
			delete(a.b.Orders, r.ClientID)
			return exchange.OrderState{Status: exchange.Rejected}, err
		}
		o, err := a.c.CreateOrder(r.Symbol, "market", string(r.Side), r.Qty, lib.WithCreateOrderPrice(px),
			lib.WithCreateOrderParams(a.orderParams(r.ClientID, r.ReduceOnly)))
		if err != nil {
			if !refused(err) {
				return a.b.Orders[r.ClientID].State, err // fate unknown: resolved by client ID later
			}
			ref := a.b.Orders[r.ClientID]
			ref.State.Status = exchange.Rejected
			a.b.Orders[r.ClientID] = ref
			return ref.State, errors.Join(exchange.ErrRefused, err, a.save())
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

// orderParams are the only extra fields an order carries: its client order
// ID and, on swap, reduceOnly. Nothing else (no builder, broker or referral
// field: NFR-0).
func (a *Adapter) orderParams(clientID string, reduceOnly bool) map[string]any {
	p := map[string]any{"clientOrderId": clientID}
	if a.swap() && reduceOnly {
		p["reduceOnly"] = true
	}
	return p
}

// mayOpen refuses, before sending, an order that would grow a position on
// the other side of the engine's own (inversion goes through flat, FR-R4),
// a reduce-only order with nothing to reduce, and a short on spot.
func (a *Adapter) mayOpen(symbol string, side exchange.Side, reduceOnly bool) error {
	h := a.b.Positions[symbol]
	switch {
	case reduceOnly && (h == nil || side != h.Side.Closing()):
		return errors.New("ccxt: a reduce-only order must close the engine's own position")
	case reduceOnly:
		return nil
	case !a.swap() && side == exchange.Sell:
		return errors.New("ccxt: spot sells only reduce the engine's holding")
	case h != nil && side != h.Side.Opening():
		return errors.New("ccxt: an entry on the other side of an open position (reverse through flat)")
	}
	return nil
}

// refused reports an error by which the exchange answered that it did not
// take the order (CCXT's error hierarchy, lib.ErrorParents). Anything else,
// a network error or a generic ExchangeError, leaves the order's fate unknown.
// DDoSProtection, RateLimitExceeded and InvalidNonce are answers sent before
// the venue accepts a request (CCXT's routerIsOutcomeUnknownError lists them
// as known outcomes), so they are refusals too (R1a).
func refused(err error) bool {
	var ce *lib.Error
	if !errors.As(err, &ce) || ce.Type == lib.DuplicateOrderIdErrType { // a duplicate ID means it exists
		return false
	}
	switch ce.Type { // exactly these: their subtypes (e.g. ChecksumError) stay unknown
	case lib.DDoSProtectionErrType, lib.RateLimitExceededErrType, lib.InvalidNonceErrType:
		return true
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
		r.State.ExchangeID = id
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
	net := filled + fee // a reduction also takes the fee out of the position
	if r.Opens {
		net = a.floorStep(r.Symbol, filled-fee)
	}
	if delta := net - r.Applied; delta > 0 {
		a.apply(r, delta)
		r.Applied = net
	}
	r.State.Filled = filled
	if r.Opens && net > 0 {
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
	if r.Opens {
		if h == nil {
			h = &holding{Side: exchange.Long}
			if r.State.Side == exchange.Sell {
				h.Side = exchange.Short
			}
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
				return known.State, fmt.Errorf("%w: ccxt: stop %s was refused by the exchange", exchange.ErrRefused, clientID)
			}
			return known.State, nil
		}
		h := a.b.Positions[symbol]
		if h == nil || side != h.Side.Closing() || qty > h.Qty+1e-12 {
			return exchange.OrderState{Status: exchange.Rejected}, errors.New("ccxt: a stop must close at most the engine's own position")
		}
		st := exchange.OrderState{ClientID: clientID, Symbol: symbol, Side: side, Qty: qty, StopPrice: stopPrice, Status: exchange.Open}
		a.b.Orders[clientID] = ref{Symbol: symbol, State: st, Created: a.opt.Now(), Stop: true}
		if err := a.save(); err != nil {
			return st, err
		}
		params := a.orderParams(clientID, true)
		params["triggerPrice"] = stopPrice
		// the trigger price doubles as the reference price of the market order it fires
		o, err := a.c.CreateOrder(symbol, "market", string(side), qty, lib.WithCreateOrderPrice(stopPrice), lib.WithCreateOrderParams(params))
		if err != nil {
			st.Status = exchange.Unknown // it may have landed: R2 resolves it by client ID
			if refused(err) {
				st.Status, err = exchange.Rejected, errors.Join(exchange.ErrRefused, err)
			}
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
				return a.cancelByClientID(clientID, err)
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

// cancelByClientID cancels an order whose exchange ID never came back (its
// send failed with an unknown outcome and a lookup did not find it), by its
// client order ID (CCXT's unified clientOrderId param). Success means it
// rested and is now gone; failure leaves it unknown.
func (a *Adapter) cancelByClientID(clientID string, lookup error) (exchange.OrderState, error) {
	r := a.b.Orders[clientID]
	o, err := a.c.CancelOrder("", lib.WithCancelOrderSymbol(r.Symbol), lib.WithCancelOrderParams(map[string]any{"clientOrderId": clientID}))
	if err != nil {
		return r.State, errors.Join(lookup, err, a.save())
	}
	a.settle(clientID, o)
	r = a.b.Orders[clientID]
	if !terminal(r.State.Status) {
		r.State.Status = exchange.Canceled
		a.b.Orders[clientID] = r
	}
	return r.State, a.save()
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
			out = append(out, exchange.Position{Symbol: sym, Side: h.Side, Qty: h.Qty, AvgPrice: h.Avg,
				StopClientID: h.StopID, StopPrice: h.Stop, AccountQty: h.Qty})
		}
		slices.SortFunc(out, func(x, y exchange.Position) int { return strings.Compare(x.Symbol, y.Symbol) })
		if a.swap() && len(out) > 0 {
			a.account(out)
		}
		return out, nil
	})
}

// Balance: on spot, cash is the free quote currency of the traded markets
// and equity adds the engine's holdings at the last price; on swap both come
// from the exchange in the settle currency: free margin and account value
// with unrealized P&L and funding (FR-P5).
func (a *Adapter) Balance(ctx context.Context, env exchange.Env) (exchange.Balance, error) {
	return do(ctx, a, env, func() (exchange.Balance, error) {
		if a.swap() {
			return a.swapBalance()
		}
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
	c := exchange.Capabilities{Swap: a.swap()}
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

// settleCurrency is the settle currency of the traded swap markets; they
// must share one, as free margin is read in it.
func (a *Adapter) settleCurrency() (string, error) {
	cur := ""
	for _, sym := range a.opt.Symbols {
		c := deref(a.markets[sym].Settle)
		if cur != "" && c != cur {
			return "", fmt.Errorf("ccxt: swap markets settle in %s and %s; one settle currency per config", cur, c)
		}
		cur = c
	}
	if cur == "" {
		return "", errors.New("ccxt: swap market without a settle currency")
	}
	return cur, nil
}

func (a *Adapter) swapBalance() (exchange.Balance, error) {
	cur, err := a.settleCurrency()
	if err != nil {
		return exchange.Balance{}, err
	}
	bs, err := a.c.FetchBalance(map[string]any{"type": "swap"})
	if err != nil {
		return exchange.Balance{}, err
	}
	free, total := bs.Free[cur], bs.Total[cur]
	if free == nil || total == nil {
		return exchange.Balance{}, fmt.Errorf("ccxt: the exchange reports no free and total %s margin", cur)
	}
	return exchange.Balance{Cash: *free, Equity: *total}, nil
}

// account adds to the engine's own positions what the exchange reports for
// the symbol: account quantity on the same side, leverage, margin mode and
// liquidation price. A position the exchange reports on the other side, or
// with a leverage or margin mode other than configured, blocks entries on the
// symbol; an unreadable answer blocks them all (fail closed).
func (a *Adapter) account(own []exchange.Position) {
	syms := make([]string, len(own))
	for i, p := range own {
		syms[i] = p.Symbol
	}
	eps, err := a.c.FetchPositions(lib.WithFetchPositionsSymbols(syms))
	if err != nil {
		for i := range own {
			own[i].Blocked = "exchange positions unreadable: " + err.Error()
		}
		return
	}
	for i := range own {
		p := &own[i]
		p.AccountQty = 0
		for _, ep := range eps {
			if deref(ep.Symbol) != p.Symbol {
				continue
			}
			if exchange.PositionSide(deref(ep.Side)) != p.PSide() {
				p.Blocked = fmt.Sprintf("the exchange reports a %s position against the engine's %s", deref(ep.Side), p.PSide())
				continue
			}
			p.AccountQty += val(ep.Contracts)
			p.Leverage, p.MarginMode, p.LiquidationPrice = val(ep.Leverage), deref(ep.MarginMode), val(ep.LiquidationPrice)
		}
		if a.want.Leverage > 0 && p.Leverage > 0 && (p.Leverage != float64(a.want.Leverage) ||
			(p.MarginMode != "" && p.MarginMode != a.want.MarginMode)) {
			p.Blocked = fmt.Sprintf("the exchange applies leverage %g %s, the yml asks %d %s",
				p.Leverage, p.MarginMode, a.want.Leverage, a.want.MarginMode)
		}
	}
}

// ConfigureMarket sets margin mode and leverage on a swap market (FR-P3).
// Both calls pass both values: on some venues setLeverage alone resets the
// margin mode (Hyperliquid: cross, V10). Neither answer echoes what was
// applied, so it is read back from the exchange's position when one exists;
// otherwise the accepted request is the answer, and every later Positions
// checks the position against it. Spot has nothing to set.
func (a *Adapter) ConfigureMarket(ctx context.Context, env exchange.Env, symbol string, want exchange.MarginSettings) (exchange.MarginSettings, error) {
	return do(ctx, a, env, func() (exchange.MarginSettings, error) {
		if !a.swap() {
			return exchange.MarginSettings{Leverage: 1}, nil
		}
		if _, ok := a.markets[symbol]; !ok {
			return exchange.MarginSettings{}, fmt.Errorf("ccxt: market %s not loaded", symbol)
		}
		if _, err := a.c.SetMarginMode(want.MarginMode, lib.WithSetMarginModeSymbol(symbol),
			lib.WithSetMarginModeParams(map[string]any{"leverage": want.Leverage})); err != nil {
			return exchange.MarginSettings{}, fmt.Errorf("ccxt: set margin mode %s on %s: %w", want.MarginMode, symbol, err)
		}
		if _, err := a.c.SetLeverage(int64(want.Leverage), lib.WithSetLeverageSymbol(symbol),
			lib.WithSetLeverageParams(map[string]any{"marginMode": want.MarginMode})); err != nil {
			return exchange.MarginSettings{}, fmt.Errorf("ccxt: set leverage %d on %s: %w", want.Leverage, symbol, err)
		}
		a.want = want
		eps, err := a.c.FetchPositions(lib.WithFetchPositionsSymbols([]string{symbol}))
		if err != nil {
			return exchange.MarginSettings{}, fmt.Errorf("ccxt: read back %s: %w", symbol, err)
		}
		got := want
		for _, ep := range eps {
			if deref(ep.Symbol) == symbol && val(ep.Leverage) > 0 {
				got = exchange.MarginSettings{Leverage: int(val(ep.Leverage)), MarginMode: deref(ep.MarginMode)}
			}
		}
		return got, nil
	})
}
