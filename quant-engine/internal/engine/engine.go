// Package engine runs one decision step per closed candle — exits, then the
// strategy's decision, sizing, risk and order — the same code for backtest and
// live.
package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exec"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/news"
	"github.com/rosaldo/agency-agents/quant-engine/internal/risk"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

type Engine struct {
	Cfg      *config.Config
	Strategy strategy.Strategy
	Ex       exchange.Exchange
	Env      exchange.Env
	Exec     *exec.Executor
	Risk     *risk.Breaker
	// News is the pre-entry check used when news_guard is required; nil means
	// no checker is configured, so such entries are refused.
	News        NewsChecker
	NewsTimeout time.Duration // 0 = 10s

	targets map[string]float64
	stops   map[string]float64 // stop given at entry, re-placed if the exchange drops it
}

// New wires the executor and the breaker from the config; cfg must have been
// parsed for s. j may be nil.
func New(cfg *config.Config, s strategy.Strategy, ex exchange.Exchange, env exchange.Env, j exec.Recorder, now func() time.Time) *Engine {
	br := &risk.Breaker{DailyPct: cfg.Risk.MaxDailyDrawdownPct, WeeklyPct: cfg.Risk.MaxWeeklyDrawdownPct, Loc: cfg.Location()}
	e := &Engine{Cfg: cfg, Strategy: s, Ex: ex, Env: env, Risk: br, targets: map[string]float64{}, stops: map[string]float64{},
		Exec: &exec.Executor{Ex: ex, Env: env, Now: now, Risk: br, Journal: j}}
	if n := cfg.NewsPolicy(); n != nil && len(n.Feeds) > 0 {
		var kw []string
		for _, k := range n.Keywords {
			kw = append(kw, strings.ToLower(k))
		}
		lookback := n.Lookback
		if lookback <= 0 {
			lookback = 24 * time.Hour
		}
		e.News = &news.RSS{Feeds: n.Feeds, Keywords: kw, Aliases: n.Aliases,
			Lookback: lookback, Client: &http.Client{Timeout: 10 * time.Second}}
	}
	return e
}

// NewsChecker says whether recent news leaves an entry on symbol clear. It is
// a port: no news source is built in.
type NewsChecker interface {
	Clear(ctx context.Context, symbol string, at time.Time) (clear bool, reason string, err error)
}

// newsBlocks applies news_guard fail-closed: no checker, an error, a timeout
// or a not-clear verdict all keep the engine out.
func (e *Engine) newsBlocks(ctx context.Context, symbol string, at time.Time) string {
	if e.Cfg.NewsGuard != "required" {
		return ""
	}
	if e.News == nil {
		return "news_guard is required and no news checker is configured"
	}
	timeout := e.NewsTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ok, reason, err := e.News.Clear(cctx, symbol, at)
	switch {
	case err != nil:
		return "news check failed: " + err.Error()
	case !ok:
		return "news check not clear: " + reason
	}
	return ""
}

// IsHalt reports an error that must end the run.
func IsHalt(err error) bool { return errors.Is(err, exec.ErrHalt) || errors.Is(err, risk.ErrHalted) }

// Target is the exit target of an open position, if this run opened it.
func (e *Engine) Target(symbol string) (float64, bool) {
	t, ok := e.targets[symbol]
	return t, ok
}

// Targets returns a copy of the open positions' targets.
func (e *Engine) Targets() map[string]float64 {
	out := make(map[string]float64, len(e.targets))
	for k, v := range e.targets {
		out[k] = v
	}
	return out
}

// Restore reloads the targets and the positions' stops saved by a previous run.
func (e *Engine) Restore(targets map[string]float64, positions []exchange.Position) {
	for k, v := range targets {
		e.targets[k] = v
	}
	for _, p := range positions {
		if p.StopPrice > 0 {
			e.stops[p.Symbol] = p.StopPrice
		}
	}
}

// Flatten closes every position at market, cancelling its stop first.
func (e *Engine) Flatten(ctx context.Context) error {
	ps, err := e.Ex.Positions(ctx, e.Env)
	if err != nil {
		return fmt.Errorf("flatten: %w", err)
	}
	var errs []error
	for _, p := range ps {
		if err := e.Exec.Exit(ctx, p, e.Exec.Now(), "flatten"); err != nil {
			errs = append(errs, err)
			continue
		}
		delete(e.targets, p.Symbol)
		delete(e.stops, p.Symbol)
	}
	return errors.Join(errs...)
}

// Reconcile protects positions left without a stop, before any decision: the
// stop goes back at the price the strategy gave at entry. A position whose
// stop the engine does not know gets stop 0, which the executor flattens.
func (e *Engine) Reconcile(ctx context.Context) error {
	return e.Exec.Reconcile(ctx, func(p exchange.Position) float64 { return e.stops[p.Symbol] })
}

// Step evaluates the last closed candle of every symbol.
func (e *Engine) Step(ctx context.Context) error {
	bal, err := e.Ex.Balance(ctx, e.Env)
	if err != nil {
		return fmt.Errorf("step: balance: %w", err)
	}
	if err := e.Risk.Allow(e.Exec.Now(), bal.Equity); err != nil {
		return err
	}
	// every step, not only at start: a late fill or a stop the exchange
	// dropped gets protected before anything else
	if err := e.Reconcile(ctx); err != nil {
		return err
	}
	ps, err := e.Ex.Positions(ctx, e.Env)
	if err != nil {
		return fmt.Errorf("step: positions: %w", err)
	}
	held := map[string]exchange.Position{}
	for _, p := range ps {
		held[p.Symbol] = p
	}
	var errs []error
	for _, sym := range e.Cfg.Symbols {
		err := e.stepSymbol(ctx, sym, held, bal.Equity)
		if IsHalt(err) {
			return err
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (e *Engine) stepSymbol(ctx context.Context, sym string, held map[string]exchange.Position, equity float64) error {
	n := max(e.Strategy.Warmup(), 1)
	cs, err := e.Ex.Candles(ctx, e.Env, sym, e.Cfg.Interval(), n)
	if err != nil || len(cs) == 0 {
		return fmt.Errorf("step: candles %s: %v", sym, err)
	}
	last := cs[len(cs)-1]
	if p, ok := held[sym]; ok {
		if t, ok := e.targets[sym]; ok && last.Close >= t {
			if err := e.Exec.Exit(ctx, p, last.Time, "target"); err != nil {
				return err // the target stays for the next step
			}
			delete(e.targets, sym)
			delete(e.stops, sym)
		}
		return nil // the exchange stop guards it
	}
	delete(e.targets, sym) // closed by its stop
	delete(e.stops, sym)
	in := strategy.Input{Symbol: sym, Now: last.Time, Candles: cs}
	for _, aux := range e.Strategy.Inputs() {
		if in.Aux == nil {
			in.Aux = map[string][]market.Candle{}
		}
		if in.Aux[aux], err = e.Ex.Candles(ctx, e.Env, aux, e.Cfg.Interval(), n); err != nil {
			return fmt.Errorf("step: candles %s: %w", aux, err)
		}
	}
	d := e.Strategy.Decide(in)
	if d.Action != strategy.Enter {
		return nil
	}
	if d.Side != strategy.Long {
		e.skip(sym, fmt.Sprintf("side %q not supported", d.Side))
		return nil
	}
	if why := e.newsBlocks(ctx, sym, last.Time); why != "" {
		e.skip(sym, why)
		return nil
	}
	caps := e.Ex.Capabilities()
	step, minQty := caps.QtyStep, caps.MinQty
	if l, ok := e.Ex.(interface {
		Lot(string) (float64, float64)
	}); ok {
		step, minQty = l.Lot(sym) // per market when the adapter knows it
	}
	qty, why := risk.Size(risk.SizeInput{Equity: equity, RiskPct: e.Cfg.Risk.MaxRiskPerTradePct, Entry: last.Close,
		Stop: d.Stop, SlippagePct: e.Cfg.Costs.SlippagePct, QtyStep: step, MinQty: minQty})
	if qty == 0 {
		e.skip(sym, why)
		return nil
	}
	if err := e.Exec.Enter(ctx, sym, last.Time, exec.Plan{Entry: last.Close, Stop: d.Stop, Target: d.Target, Qty: qty}); err != nil {
		return err
	}
	e.stops[sym] = d.Stop
	if d.Target > 0 {
		e.targets[sym] = d.Target
	}
	return nil
}

// skip journals a signal that did not become an order.
func (e *Engine) skip(sym, why string) {
	if e.Exec.Journal != nil {
		_ = e.Exec.Journal.Append(journal.Event{Time: e.Exec.Now(), Kind: journal.Rejected, Symbol: sym, Reason: "signal skipped: " + why})
	}
}
