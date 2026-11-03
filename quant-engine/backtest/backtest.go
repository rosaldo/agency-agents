// Package backtest replays candle files through the live engine step against
// the fake exchange, with the sim profile's slippage and fee (candles carry
// no price inside the candle, so the profile's latency is not modelled), and
// reports the trades.
package backtest

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	cfgpkg "github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/engine"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

type Input struct {
	Candles         []market.Candle
	Aux             map[string][]market.Candle // one series per Strategy.Inputs(), aligned with Candles
	Capital         float64
	QtyStep, MinQty float64
	// Deriv is the symbol's derivatives snapshots, oldest first, bound to
	// their candles as the recorder writes them (market.ReadDerivCSV, or
	// market.ReadFundingHistory). A DerivativesUser is refused unless they
	// span every candle; any other strategy ignores them.
	Deriv []market.Deriv
}

type Trade struct {
	Side                          strategy.Side
	EntryTime, ExitTime           time.Time
	Qty, EntryPrice, Stop, Target float64
	ExitPrice                     float64
	ExitReason                    string // target, exit (the strategy's), stop, open_at_end
	PnL, R                        float64
}

// Summary carries the Quant Trader "Backtest Report" fields the engine can
// fill; the walk-forward split and the verdict belong to the Statistician.
type Summary struct {
	ParamsHash          string  `json:"params_hash"`
	PeriodStart         string  `json:"period_start"`
	PeriodEnd           string  `json:"period_end"`
	CommissionPct       float64 `json:"commission_pct"`
	SimProfile          string  `json:"sim_profile"`
	SlippagePct         float64 `json:"slippage_pct"`
	ConfigurationsTried int     `json:"configurations_tried"`
	Trades              int     `json:"trades"`
	WinRate             float64 `json:"win_rate"`
	PayoffRatio         float64 `json:"payoff_ratio"`
	ExpectancyR         float64 `json:"expectancy_r"`
	MaxDrawdownPct      float64 `json:"max_drawdown_pct"`
	LongestDrawdown     string  `json:"longest_drawdown"`
	StartEquity         float64 `json:"start_equity"`
	EndEquity           float64 `json:"end_equity"`
	Halt                string  `json:"halt,omitempty"`
	// CandlesWithoutDerivatives counts the candles a DerivativesUser got no
	// snapshot for (recorded gaps); nil for any other strategy.
	CandlesWithoutDerivatives *int `json:"candles_without_derivatives,omitempty"`
	// News says what the backtest cannot do with news_guard required.
	News   string `json:"news,omitempty"`
	Status string `json:"status"`
}

type Result struct {
	Trades  []Trade
	Summary Summary
}

// Run replays in through the same engine step as live, against the fake
// exchange, with s configured from the `strategy:` block of the yml config.
func Run(ctx context.Context, s strategy.Strategy, config []byte, in Input) (Result, error) {
	c, err := cfgpkg.Parse(config, s)
	if err != nil {
		return Result{}, err
	}
	return run(ctx, c, s, in)
}

func run(ctx context.Context, c *cfgpkg.Config, s strategy.Strategy, in Input) (Result, error) {
	if len(c.Symbols) != 1 {
		return Result{}, errors.New("backtest: the yml must list exactly one symbol")
	}
	if len(in.Candles) == 0 {
		return Result{}, errors.New("backtest: no candles")
	}
	sym := c.Symbols[0]
	candles := map[string][]market.Candle{sym: in.Candles}
	for _, aux := range s.Inputs() {
		if len(in.Aux[aux]) != len(in.Candles) {
			return Result{}, fmt.Errorf("backtest: the strategy needs %s candles aligned with the symbol's", aux)
		}
		candles[aux] = in.Aux[aux]
	}
	ex := fake.New(fake.Options{Paper: true, QtyStep: in.QtyStep, MinQty: in.MinQty, Cash: in.Capital, Candles: candles,
		SlippagePct: c.SimCosts()[sym].SlippagePct, CommissionPct: c.SimCosts()[sym].CommissionPct, Shorts: c.Market == "swap"})
	var now time.Time
	en := engine.New(c, s, ex, exchange.Demo, nil, func() time.Time { return now })
	if engine.NeedsDerivatives(s) {
		if err := engine.CheckDerivatives(c, s, ex); err != nil {
			return Result{}, fmt.Errorf("backtest: %w", err)
		}
		oi, ok := s.(strategy.OpenInterestUser)
		if err := derivCovers(in.Candles, in.Deriv, c.Interval(), sym, ok && oi.NeedsOpenInterest()); err != nil {
			return Result{}, err
		}
		// replayed, never read: the binding is the recording's (market.AlignDeriv), with no wall-clock deadline
		en.SeedDeriv(map[string][]market.Deriv{sym: in.Deriv})
		en.DerivReplay = true
	}
	en.Simulated = true              // simulated fills: the active profile sizes as it is, never floored at the real one
	en.News, en.PastNews = nil, true // past headlines are not replayable: see Summary.News

	var (
		res          Result
		open         *Trade
		stopID       string
		equityBefore float64
		curve        []float64
	)
	equity := func() float64 { b, _ := ex.Balance(ctx, exchange.Demo); return b.Equity }
	position := func() (exchange.Position, bool) {
		ps, _ := ex.Positions(ctx, exchange.Demo)
		for _, p := range ps {
			if p.Symbol == sym {
				return p, true
			}
		}
		return exchange.Position{}, false
	}
	closeTrade := func(price float64, reason string) {
		open.ExitTime, open.ExitPrice, open.ExitReason = now, price, reason
		open.PnL = equity() - equityBefore
		open.R = open.PnL / (open.Qty * math.Abs(open.EntryPrice-open.Stop))
		res.Trades = append(res.Trades, *open)
		open = nil
	}
	for i, candle := range in.Candles {
		now = candle.Time
		if i > 0 {
			ex.Advance()
			if _, held := position(); open != nil && !held {
				st, _ := ex.Order(stopID)
				closeTrade(st.FillPrice, "stop")
			}
		}
		if open == nil {
			equityBefore = equity()
		}
		target, _ := en.Target(sym)
		err := en.Step(ctx)
		if engine.IsHalt(err) {
			res.Summary.Halt = err.Error()
		} else if err != nil {
			return Result{}, fmt.Errorf("backtest at %s: %w", now.Format(time.RFC3339), err)
		}
		p, held := position()
		switch {
		case open == nil && held:
			t, _ := en.Target(sym)
			side := strategy.Long
			if p.PSide() == exchange.Short {
				side = strategy.Short
			}
			open = &Trade{Side: side, EntryTime: now, Qty: p.Qty, EntryPrice: p.AvgPrice, Stop: p.StopPrice, Target: t}
		case open != nil && held && p.Qty != open.Qty: // a further tranche: the trade is the aggregate position
			open.Qty, open.EntryPrice, open.Stop = p.Qty, p.AvgPrice, p.StopPrice // R against the stop in force
			open.Target, _ = en.Target(sym)                                       // the Add's, or a Retargeter's on the new average
		case open != nil && !held:
			st, _ := ex.Order(en.Exec.ID(sym, now, "exit"))
			reason := "exit" // the strategy's Exit
			if engine.TargetHit(open.Side, candle.Close, target) {
				reason = "target"
			}
			closeTrade(st.FillPrice, reason)
		}
		if open != nil {
			stopID = p.StopClientID
		}
		curve = append(curve, equity())
		if res.Summary.Halt != "" {
			break
		}
	}
	if open != nil {
		closeTrade(in.Candles[len(curve)-1].Close, "open_at_end")
	}
	res.Summary = summarize(c, in, res, curve)
	if engine.NeedsDerivatives(s) {
		gaps := 0
		for _, d := range market.AlignDeriv(in.Candles[:len(curve)], in.Deriv, c.Interval()) {
			if d.Missing() {
				gaps++
			}
		}
		res.Summary.CandlesWithoutDerivatives = &gaps
	}
	return res, nil
}

// derivCovers refuses snapshots that do not span the candles, counting only
// those read in their candle's step, and, when needOI, only those with open
// interest: the strategy never runs as if its filter passed, and the venue
// keeps no open interest history to fill in.
func derivCovers(cs []market.Candle, ds []market.Deriv, interval time.Duration, sym string, needOI bool) error {
	var from, to time.Time
	for _, d := range ds {
		if !d.InStep(d.Candle, interval) || needOI && !d.HasOpenInterest {
			continue
		}
		if from.IsZero() || d.Candle.Before(from) {
			from = d.Candle
		}
		if d.Candle.After(to) {
			to = d.Candle
		}
	}
	first, last := cs[0].Time, cs[len(cs)-1].Time
	if !from.IsZero() && !from.After(first) && !to.Before(last) {
		return nil
	}
	need, have := "derivatives data (open interest, funding)", "has none for "+sym
	if needOI {
		need, have = "open interest", "has no open interest for "+sym+" (a funding-only history carries none)"
	}
	if !from.IsZero() {
		have = fmt.Sprintf("covers %s to %s for %s", from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339), sym)
	}
	return fmt.Errorf("backtest: the strategy needs %s for every candle from %s to %s; the data %s. "+
		"Open interest exists only in our own recording (deriv-<level>.csv): replay candles inside it",
		need, first.UTC().Format(time.RFC3339), last.UTC().Format(time.RFC3339), have)
}

func summarize(c *cfgpkg.Config, in Input, res Result, curve []float64) Summary {
	s := Summary{
		ParamsHash: c.ParamsHash(), PeriodStart: in.Candles[0].Time.Format(time.RFC3339),
		PeriodEnd:     in.Candles[len(curve)-1].Time.Format(time.RFC3339),
		CommissionPct: c.SimCosts()[c.Symbols[0]].CommissionPct, SlippagePct: c.SimCosts()[c.Symbols[0]].SlippagePct, SimProfile: c.Sim.Profile,
		ConfigurationsTried: 1, Trades: len(res.Trades), StartEquity: in.Capital, EndEquity: curve[len(curve)-1],
		Halt: res.Summary.Halt, Status: "not validated",
	}
	if c.NewsGuard == "required" {
		s.News = map[string]string{
			"crypto": "entered and never exited on news: past headlines are not replayable, so no emergency exit is simulated",
			"equity": "no entry taken: news is checked before an equity entry and past headlines are not replayable",
		}[c.AssetClass]
	}
	var wins, losses, winSum, lossSum, rSum float64
	for _, t := range res.Trades {
		rSum += t.R
		if t.PnL > 0 {
			wins, winSum = wins+1, winSum+t.PnL
		} else {
			losses, lossSum = losses+1, lossSum-t.PnL
		}
	}
	if n := float64(len(res.Trades)); n > 0 {
		s.WinRate, s.ExpectancyR = wins/n, rSum/n
	}
	if wins > 0 && losses > 0 && lossSum > 0 {
		s.PayoffRatio = (winSum / wins) / (lossSum / losses)
	}
	peak, peakAt := in.Capital, in.Candles[0].Time
	var longest time.Duration
	for i, eq := range curve {
		if eq >= peak {
			peak, peakAt = eq, in.Candles[i].Time
			continue
		}
		s.MaxDrawdownPct = math.Max(s.MaxDrawdownPct, (peak-eq)/peak*100)
		longest = max(longest, in.Candles[i].Time.Sub(peakAt))
	}
	s.LongestDrawdown = longest.String()
	return s
}

// Write puts trades.csv and summary.json in dir.
func (r Result) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("backtest: %w", err)
	}
	f, err := os.Create(filepath.Join(dir, "trades.csv"))
	if err != nil {
		return fmt.Errorf("backtest: %w", err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	num := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	_ = w.Write([]string{"side", "entry_time", "entry_price", "qty", "stop", "target", "exit_time", "exit_price", "exit_reason", "pnl", "r_multiple"})
	for _, t := range r.Trades {
		_ = w.Write([]string{string(t.Side), t.EntryTime.Format(time.RFC3339), num(t.EntryPrice), num(t.Qty), num(t.Stop), num(t.Target),
			t.ExitTime.Format(time.RFC3339), num(t.ExitPrice), t.ExitReason, num(t.PnL), num(t.R)})
	}
	if w.Flush(); w.Error() != nil {
		return fmt.Errorf("backtest: %w", w.Error())
	}
	b, err := json.MarshalIndent(r.Summary, "", "  ")
	if err != nil {
		return fmt.Errorf("backtest: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, "summary.json"), append(b, '\n'), 0o600)
}
