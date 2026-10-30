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
	"github.com/rosaldo/agency-agents/quant-engine/internal/ownbook"
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
	FetchFundingHistory(options ...lib.FetchFundingHistoryOptions) ([]lib.FundingHistory, error)
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
	// ReportsLiquidation: the venue's positions carry a liquidation price; a
	// position without one is then a violation (FR-P4).
	ReportsLiquidation bool
	// MaintenanceMargin is the venue's maintenance margin rate of a market,
	// as a fraction of the notional; ok=false = unknown. With it the adapter
	// estimates the liquidation distance (exchange.LiquidationEstimator).
	MaintenanceMargin func(m lib.MarketInterface) (rate float64, ok bool)
	// SetMargin, when set, applies margin mode and leverage on symbol in
	// place of CCXT's unified setMarginMode + setLeverage, for a venue whose
	// fork has no setMarginMode; an error refuses to start. Its positions
	// carry no margin mode, so the read-back keeps the requested one.
	SetMargin func(c Client, symbol string, want exchange.MarginSettings) error
	// NativeAmounts: on swap the fork passes this venue's order amounts,
	// fills, position sizes and amount limits through in the exchange's own
	// unit, the base asset, instead of CCXT's contracts: the adapter then does
	// not convert by the contract size.
	NativeAmounts bool
}

type Options struct {
	Venue   Venue
	Symbols []string     // the traded markets and the strategy's inputs
	Market  string       // "spot" | "swap" (linear); "" = spot
	Env     exchange.Env // the environment the client is bound to
	Paper   bool         // refuse exchange.Real
	Book    string       // file holding the engine's positions and orders
	Now     func() time.Time
	// Candles, when set, serves the candles in place of the trading client:
	// the testnet demo reads the signal from mainnet's public data (O4).
	Candles Client
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
	// Absent: a stop of unknown fate the venue said it does not have (lookup
	// by client order ID empty, cancel answered not found). It is taken as
	// cancelled, yet looked up until absentWatch after Created: if it shows
	// up, the orphan sweep cancels it.
	Absent bool
}

// absentWatch is how long an absent stop is still looked up for (the lookup
// window itself starts an hour before Created, refresh).
const absentWatch = time.Hour

// errNotFound: the venue answered both order lookups and has no order with
// the client order ID.
var errNotFound = errors.New("not found by client order ID")

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
	cuts    map[string]cut          // own-quantity drops seen once, awaiting confirmation (B1)
}

// cut is an own-quantity drop the account showed on one reading: own is the
// engine's quantity then, kept what the account left of it, since when first seen.
type cut struct {
	own, kept float64
	since     time.Time
}

// cutConfirm: an account reading lower than own (or flat, or opposite) cuts
// own_qty, and so its stop, only when a reading at least this long after the
// first one still agrees. A venue that reports a fresh own fill late (an
// empty or partial fetchPositions) must not cut what the engine holds:
// Positions is called several times per step, so two readings alone could
// both fall inside the lag; a grace alone could cut on one late reading.
const cutConfirm = time.Minute

// New binds c to the yml's markets. FR-P1: only spot and linear swap are
// traded, read from CCXT's market structure, never from the symbol's name;
// every symbol must be of the yml's market type.
func New(c Client, o Options) (*Adapter, error) {
	if !o.Paper && o.Env != exchange.Real {
		return nil, errors.New("ccxt: mainnet must be bound to the mainnet environment")
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
	a := &Adapter{c: c, opt: o, markets: ms, b: book{Orders: map[string]ref{}, Positions: map[string]*holding{}}, cuts: map[string]cut{}}
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
		if cs := val(m.ContractSize); cs <= 0 { // unknown: base quantities cannot be converted (fail closed)
			return "", fmt.Errorf("contract size %g: unknown, base quantities cannot be converted to contracts", cs)
		}
		return "swap", nil
	case flag(m.Spot) || typ == "spot":
		return "spot", nil
	}
	return "", fmt.Errorf("market type %q unknown", typ)
}

func (a *Adapter) swap() bool { return a.opt.Market == "swap" }

// contractSize is the base quantity of one contract of symbol: CCXT's unified
// amounts (order amount and filled, position contracts, amount precision and
// limits) are in contracts on swap, while the engine trades base quantities.
// Spot and a venue with NativeAmounts are 1. New refuses a swap market
// without a positive contract size.
func (a *Adapter) contractSize(symbol string) float64 {
	if cs := val(a.markets[symbol].ContractSize); a.swap() && cs > 0 && !a.opt.Venue.NativeAmounts {
		return cs
	}
	return 1
}

// toContracts is the amount sent for a base quantity: floored to the
// market's amount step in contracts, never above the quantity asked (risk).
// It also absorbs float noise (0.03/0.01 = 2.9999999999999996 is 3).
func (a *Adapter) toContracts(symbol string, qty float64) float64 {
	return floorTo(qty/a.contractSize(symbol), val(a.markets[symbol].Precision.Amount))
}

func floorTo(q, step float64) float64 {
	if step > 0 {
		return math.Floor(q/step+1e-9) * step
	}
	return q
}

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
	src := a.c
	if a.opt.Candles != nil {
		src = a.opt.Candles
	}
	return do(ctx, a, env, func() ([]market.Candle, error) {
		rows, err := src.FetchOHLCV(symbol, lib.WithFetchOHLCVTimeframe(tf), lib.WithFetchOHLCVLimit(int64(n+1)))
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
		if r.ReduceOnly { // FR-P8: an exit never closes more than the engine's own quantity
			r.Qty = min(r.Qty, a.b.Positions[r.Symbol].Qty)
		}
		amount := a.toContracts(r.Symbol, r.Qty)
		if amount <= 0 {
			return exchange.OrderState{Status: exchange.Rejected}, fmt.Errorf("ccxt: %s quantity %g is below one amount step", r.Symbol, r.Qty)
		}
		a.b.Orders[r.ClientID] = ref{Symbol: r.Symbol, Created: a.opt.Now(), Opens: !r.ReduceOnly && (a.swap() || r.Side == exchange.Buy),
			State: exchange.OrderState{ClientID: r.ClientID, Symbol: r.Symbol, Side: r.Side, Qty: min(r.Qty, amount*a.contractSize(r.Symbol)), Status: exchange.Open}}
		if err := a.save(); err != nil {
			delete(a.b.Orders, r.ClientID)
			return exchange.OrderState{Status: exchange.Rejected}, err
		}
		o, err := a.c.CreateOrder(r.Symbol, "market", string(r.Side), amount, lib.WithCreateOrderPrice(px),
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
	if oerr == nil && cerr == nil {
		return fmt.Errorf("ccxt: order %s sent without an answer: %w", clientID, errNotFound)
	}
	return fmt.Errorf("ccxt: order %s sent without an answer and not found by client order ID (%v)", clientID, errors.Join(oerr, cerr))
}

// settle records an order's state and applies the net quantity it filled
// since last seen: the executed amount (contracts on swap, made base), less
// any fee charged in the base asset.
func (a *Adapter) settle(clientID string, o lib.Order) {
	r := a.b.Orders[clientID]
	r.Absent = false // it exists after all
	if id := deref(o.Id); id != "" {
		r.ID = id
		r.State.ExchangeID = id
	}
	r.State.Status = status(o)
	if p := fillPrice(o); p > 0 {
		r.State.FillPrice = p
	}
	filled := val(o.Filled) * a.contractSize(r.Symbol)
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

// floorStep floors a base quantity to the market's amount step in base.
func (a *Adapter) floorStep(symbol string, q float64) float64 {
	step, _ := a.Lot(symbol)
	return floorTo(q, step)
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
		if h == nil || side != h.Side.Closing() {
			return exchange.OrderState{Status: exchange.Rejected}, errors.New("ccxt: a stop must close the engine's own position")
		}
		qty = min(qty, h.Qty) // FR-P8: never more than the engine's own quantity
		amount := a.toContracts(symbol, qty)
		if amount <= 0 {
			return exchange.OrderState{Status: exchange.Rejected}, fmt.Errorf("ccxt: %s stop quantity %g is below one amount step", symbol, qty)
		}
		st := exchange.OrderState{ClientID: clientID, Symbol: symbol, Side: side, Qty: min(qty, amount*a.contractSize(symbol)), StopPrice: stopPrice, Status: exchange.Open}
		a.b.Orders[clientID] = ref{Symbol: symbol, State: st, Created: a.opt.Now(), Stop: true}
		if err := a.save(); err != nil {
			return st, err
		}
		params := a.orderParams(clientID, true)
		params["triggerPrice"] = stopPrice
		// the trigger price doubles as the reference price of the market order it fires
		o, err := a.c.CreateOrder(symbol, "market", string(side), amount, lib.WithCreateOrderPrice(stopPrice), lib.WithCreateOrderParams(params))
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
			return exchange.OrderState{}, fmt.Errorf("%w: %s", exchange.ErrNotOwn, clientID)
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
	var ce *lib.Error
	if err != nil && r.Stop && errors.Is(lookup, errNotFound) && errors.As(err, &ce) && ce.Type == lib.OrderNotFoundErrType {
		// the venue has no such stop: resolved, not stale, and still watched (Absent)
		r.State.Status, r.Absent = exchange.Canceled, true
		a.b.Orders[clientID] = r
		return r.State, a.save()
	}
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
// Trouble in one symbol never fails the call (R1b): an own order of unknown
// fate that cannot be found blocks entries on its symbol (Position.Blocked,
// with Qty 0 when nothing is held). On swap the account is netted against the
// own holdings (FR-P8, ownbook.Reconcile), and the own stops left without a
// holding to protect are cancelled.
func (a *Adapter) Positions(ctx context.Context, env exchange.Env) ([]exchange.Position, error) {
	return do(ctx, a, env, func() ([]exchange.Position, error) {
		blocked := map[string]string{}
		now := a.opt.Now()
		for id, r := range a.b.Orders {
			watched := r.Absent && now.Before(r.Created.Add(absentWatch))
			if terminal(r.State.Status) && !watched {
				continue
			}
			if err := a.refresh(id); err != nil && !watched {
				blocked[r.Symbol] = fmt.Sprintf("own order %s of unknown fate: %v", id, err)
			}
		}
		for _, h := range a.b.Positions {
			if h.StopID != "" && terminal(a.b.Orders[h.StopID].State.Status) {
				h.StopID, h.Stop = "", 0
			}
		}
		var acct map[string]accountPos
		if a.swap() {
			acct = a.net(blocked)
		}
		a.sweep()
		if err := a.save(); err != nil {
			return nil, err
		}
		out := []exchange.Position{}
		for sym, h := range a.b.Positions {
			p := exchange.Position{Symbol: sym, Side: h.Side, Qty: h.Qty, AvgPrice: h.Avg,
				StopClientID: h.StopID, StopPrice: h.Stop, AccountQty: h.Qty, Blocked: blocked[sym]}
			if r, ok := a.b.Orders[h.StopID]; ok {
				p.StopQty = r.State.Qty - r.State.Filled
			}
			if ap, ok := acct[sym]; ok {
				p.AccountQty, p.Leverage, p.MarginMode, p.LiquidationPrice = ap.qty, ap.leverage, ap.marginMode, ap.liquidation
				if ap.side != "" && ap.side != p.PSide() {
					p.AccountQty = 0
				}
			}
			out = append(out, p)
			delete(blocked, sym)
		}
		for sym, why := range blocked { // nothing held, entries blocked all the same
			out = append(out, exchange.Position{Symbol: sym, Blocked: why})
		}
		slices.SortFunc(out, func(x, y exchange.Position) int { return strings.Compare(x.Symbol, y.Symbol) })
		return out, nil
	})
}

// sweep cancels the engine's own resting stops that protect no holding: a
// stop whose position closed outside the engine, an old stop a move or a
// resize left behind, or a stop of unknown fate that showed up after all.
// A failed cancel is tried again on the next look.
func (a *Adapter) sweep() {
	for id, r := range a.b.Orders {
		h := a.b.Positions[r.Symbol]
		if !r.Stop || r.ID == "" || r.State.Status != exchange.Open || (h != nil && h.StopID == id) {
			continue
		}
		o, err := a.c.CancelOrder(r.ID, lib.WithCancelOrderSymbol(r.Symbol))
		if err != nil {
			continue
		}
		a.settle(id, o)
		if r := a.b.Orders[id]; r.State.Status == exchange.Open {
			r.State.Status = exchange.Canceled
			a.b.Orders[id] = r
		}
	}
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
// verified per exchange), in base (Lot).
func (a *Adapter) Capabilities() exchange.Capabilities {
	// Netting on every swap: Hyperliquid is one-way (V13), and the adapter sends
	// no position side, so a CEX account in hedge mode is not traded as one
	c := exchange.Capabilities{Swap: a.swap(), ReportsLiquidation: a.swap() && a.opt.Venue.ReportsLiquidation, Netting: a.swap()}
	for sym := range a.markets {
		step, minQty := a.Lot(sym)
		c.QtyStep = max(c.QtyStep, step)
		c.MinQty = max(c.MinQty, minQty)
	}
	return c
}

// LiquidationDistance is the distance from entry to liquidation of a fresh
// isolated position, as a fraction of entry: the liquidation formula
// liq = entry - side*margin_available/size/(1 - mm*side), with
// margin_available = notional/leverage - mm*notional, gives
// (1/leverage - mm)/(1 - mm) for a long and (1/leverage - mm)/(1 + mm) for a
// short, mm being the venue's maintenance margin rate. Cross margin
// liquidates farther away. A leverage above the market's maximum gives 0
// (refused). ok=false: the venue has no maintenance margin rate for symbol.
func (a *Adapter) LiquidationDistance(symbol string, side exchange.PositionSide, _, leverage float64) (float64, bool) {
	m, found := a.markets[symbol]
	if !found || !a.swap() || a.opt.Venue.MaintenanceMargin == nil || leverage <= 0 {
		return 0, false
	}
	if hi := val(m.Limits.Leverage.Max); hi > 0 && leverage > hi {
		return 0, true
	}
	mm, ok := a.opt.Venue.MaintenanceMargin(m)
	if !ok || mm < 0 || mm >= 1 {
		return 0, false
	}
	if side == exchange.Short {
		return (1/leverage - mm) / (1 + mm), true
	}
	return (1/leverage - mm) / (1 - mm), true
}

// Lot is one market's quantity step and minimum in the base asset: CCXT
// gives them in contracts on swap (precision read as a tick size, CCXT's
// usual mode; to be verified per exchange).
func (a *Adapter) Lot(symbol string) (step, min float64) {
	m, cs := a.markets[symbol], a.contractSize(symbol)
	return val(m.Precision.Amount) * cs, val(m.Limits.Amount.Min) * cs
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

// accountPos is what the exchange reports for one symbol, quantity in base.
type accountPos struct {
	side                  exchange.PositionSide // "" = flat
	qty                   float64
	leverage, liquidation float64
	marginMode            string
	hedged                bool
}

// net reads the account's positions on every traded symbol and applies the
// netting rule (ownbook.Reconcile) to the engine's own holdings: a holding
// the account no longer covers shrinks (its stop is then resized or swept),
// and a symbol with foreign quantity, or a position on the other side, takes
// no new entry. A position with a leverage or margin mode other than
// configured blocks the symbol too; an unreadable answer blocks every symbol
// (fail closed) and changes nothing. A hedged account (positions per side)
// is not netted: only the engine's own fills count there.
func (a *Adapter) net(blocked map[string]string) map[string]accountPos {
	syms := slices.Clone(a.opt.Symbols)
	for sym := range a.b.Positions {
		if !slices.Contains(syms, sym) {
			syms = append(syms, sym)
		}
	}
	eps, err := a.c.FetchPositions(lib.WithFetchPositionsSymbols(syms))
	if err != nil {
		for _, sym := range syms {
			blocked[sym] = "exchange positions unreadable: " + err.Error()
		}
		return nil
	}
	acct := map[string]accountPos{}
	for _, ep := range eps {
		sym := deref(ep.Symbol)
		qty := val(ep.Contracts) * a.contractSize(sym) // CCXT positions count contracts
		if qty <= 0 {
			continue
		}
		ap := acct[sym]
		side := exchange.PositionSide(deref(ep.Side))
		ap.hedged = ap.hedged || flag(ep.Hedged)
		if h := a.b.Positions[sym]; ap.side == "" || (h != nil && side == h.Side) { // the own side's numbers win
			ap.side, ap.qty = side, qty
			ap.leverage, ap.marginMode, ap.liquidation = val(ep.Leverage), deref(ep.MarginMode), val(ep.LiquidationPrice)
		}
		acct[sym] = ap
	}
	for _, sym := range syms {
		ap := acct[sym]
		h := a.b.Positions[sym]
		own := ownbook.Holding{}
		if h != nil {
			own = ownbook.Holding{Side: h.Side, Qty: h.Qty}
		}
		if ap.hedged {
			if h != nil && ap.side != h.Side {
				ap = accountPos{} // the other side is not the engine's business on a hedged account
			}
		} else {
			kept, why := ownbook.Reconcile(own, ownbook.Holding{Side: ap.side, Qty: ap.qty})
			if h == nil || kept.Qty >= h.Qty {
				delete(a.cuts, sym) // the account agrees again: a pending drop was a late reading
			} else if !a.confirmCut(sym, h.Qty, kept.Qty) {
				// fail closed: own and its stop stay, entries wait for the second reading
				kept.Qty = h.Qty
				if why == "" {
					shows := "flat"
					if ap.qty > 0 {
						shows = fmt.Sprintf("%g %s", ap.qty, ap.side)
					}
					why = fmt.Sprintf("unconfirmed: the account shows %s against the engine's own %g; own and its stop kept until a later reading agrees", shows, h.Qty)
				}
			}
			if why != "" {
				blocked[sym] = why
			}
			if h != nil && kept.Qty < h.Qty {
				if h.Qty = kept.Qty; h.Qty <= 0 {
					delete(a.b.Positions, sym) // its stop is swept
				}
			}
		}
		if a.want.Leverage > 0 && ap.leverage > 0 && h != nil && (ap.leverage != float64(a.want.Leverage) ||
			(ap.marginMode != "" && ap.marginMode != a.want.MarginMode)) {
			blocked[sym] = fmt.Sprintf("the exchange applies leverage %g %s, the yml asks %d %s",
				ap.leverage, ap.marginMode, a.want.Leverage, a.want.MarginMode)
		}
		acct[sym] = ap
	}
	return acct
}

// confirmCut tells whether the drop of sym's own quantity from own to kept
// was already seen at least cutConfirm ago with the same numbers; any other
// reading starts the wait again.
func (a *Adapter) confirmCut(sym string, own, kept float64) bool {
	now := a.opt.Now()
	c, ok := a.cuts[sym]
	if ok && c.own == own && c.kept == kept {
		if !now.Before(c.since.Add(cutConfirm)) {
			delete(a.cuts, sym)
			return true
		}
		return false
	}
	a.cuts[sym] = cut{own: own, kept: kept, since: now}
	return false
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
		if set := a.opt.Venue.SetMargin; set != nil {
			if err := set(a.c, symbol, want); err != nil {
				return exchange.MarginSettings{}, fmt.Errorf("ccxt: %s: set %s leverage %d on %s: %w", a.opt.Venue.ID, want.MarginMode, want.Leverage, symbol, err)
			}
		} else {
			if _, err := a.c.SetMarginMode(want.MarginMode, lib.WithSetMarginModeSymbol(symbol),
				lib.WithSetMarginModeParams(map[string]any{"leverage": want.Leverage})); err != nil {
				return exchange.MarginSettings{}, fmt.Errorf("ccxt: set margin mode %s on %s: %w", want.MarginMode, symbol, err)
			}
			if _, err := a.c.SetLeverage(int64(want.Leverage), lib.WithSetLeverageSymbol(symbol),
				lib.WithSetLeverageParams(map[string]any{"marginMode": want.MarginMode})); err != nil {
				return exchange.MarginSettings{}, fmt.Errorf("ccxt: set leverage %d on %s: %w", want.Leverage, symbol, err)
			}
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
				if a.opt.Venue.SetMargin != nil && got.MarginMode == "" {
					got.MarginMode = want.MarginMode
				}
			}
		}
		return got, nil
	})
}

// Funding is the funding the venue reports on symbol since a time (FR-P5),
// for the journal; spot has none.
func (a *Adapter) Funding(ctx context.Context, env exchange.Env, symbol string, since time.Time) ([]exchange.FundingPayment, error) {
	if !a.swap() {
		return nil, nil
	}
	return do(ctx, a, env, func() ([]exchange.FundingPayment, error) {
		hs, err := a.c.FetchFundingHistory(lib.WithFetchFundingHistorySymbol(symbol), lib.WithFetchFundingHistorySince(since.UnixMilli()))
		if err != nil {
			return nil, fmt.Errorf("ccxt: funding %s: %w", symbol, err)
		}
		var out []exchange.FundingPayment
		for _, h := range hs {
			if h.Timestamp == nil || h.Amount == nil || (h.Symbol != nil && *h.Symbol != symbol) {
				continue
			}
			out = append(out, exchange.FundingPayment{Time: time.UnixMilli(*h.Timestamp).UTC(), Symbol: symbol, Amount: *h.Amount})
		}
		return out, nil
	})
}
