// Package config loads and validates the yml a strategy binary runs with: the
// engine's own schema plus an opaque `strategy:` block handed to the strategy.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// exchangeID is "fake" or a CCXT exchange id (checked against CCXT at start).
var exchangeID = regexp.MustCompile(`^[a-z0-9]+$`)

type Config struct {
	Mode           string    `yaml:"mode"`
	Exchange       string    `yaml:"exchange"`
	Market         string    `yaml:"market"` // spot | swap (linear)
	AssetClass     string    `yaml:"asset_class"`
	NewsGuard      string    `yaml:"news_guard"`
	IntervalSpec   string    `yaml:"interval"`
	Timezone       string    `yaml:"timezone"`
	Symbols        []string  `yaml:"symbols"`
	StrategyParams yaml.Node `yaml:"strategy"` // opaque: validated by Strategy.Configure
	Perp           Perp      `yaml:"perp"`
	Risk           Risk      `yaml:"risk"`
	Demo           Demo      `yaml:"demo"` // optional; only the O4 value is implemented
	Sim            Sim       `yaml:"sim"`
	ValidationFile string    `yaml:"validation_file"`

	interval time.Duration
	location *time.Location
	name     string               // Strategy.Name()
	news     *strategy.NewsPolicy // nil = the strategy has no news policy
	sim      map[string]SimCosts  // the profile's costs per symbol (paper simulator, backtest, sizing)
	sizing   map[string]SimCosts  // sim floored at the built-in real profile: sizing on real fills
}

// Perp holds the perpetual-swap settings: the leverage (it sets the margin
// locked, never the size), the margin mode and whether the strategy may go
// short. Leverage 1 and no margin mode on spot.
type Perp struct {
	Leverage   int    `yaml:"leverage"`
	MarginMode string `yaml:"margin_mode"` // isolated | cross; swap only
	AllowShort bool   `yaml:"allow_short"`
}

type Risk struct {
	MaxRiskPerTradePct   float64 `yaml:"max_risk_per_trade_pct"`
	MaxDailyDrawdownPct  float64 `yaml:"max_daily_drawdown_pct"`
	MaxWeeklyDrawdownPct float64 `yaml:"max_weekly_drawdown_pct"`
	MinDemoPeriodDays    int     `yaml:"min_demo_period_days"`
	MaxLeverage          int     `yaml:"max_leverage"`           // hard ceiling for perp.leverage
	MinLiquidationBuffer float64 `yaml:"min_liquidation_buffer"` // share of the distance to liquidation the stop leaves free
	ConfirmedByUser      bool    `yaml:"confirmed_by_user"`
}

// Demo is where the testnet demo trades and reads the signal's candles (O4).
// The engine implements only {source: testnet, candles: mainnet}; the key
// is optional and documents that choice in the yml.
type Demo struct {
	Source  string `yaml:"source"`
	Candles string `yaml:"candles"`
}

var o4 = Demo{Source: "testnet", Candles: "mainnet"}

// defaults are the Quant Trader's suggested limits; the user confirms them.
func defaults() Config {
	return Config{
		Mode:     "paper",
		Market:   "spot",
		Timezone: "UTC",
		Perp:     Perp{Leverage: 1},
		Risk: Risk{MaxRiskPerTradePct: 1, MaxDailyDrawdownPct: 3, MaxWeeklyDrawdownPct: 6, MinDemoPeriodDays: 30,
			MaxLeverage: 3, MinLiquidationBuffer: 0.5},
	}
}

func Load(path string, s strategy.Strategy) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return Parse(b, s)
}

// strategyName is what Strategy.Name() must match: it becomes a directory,
// an environment prefix and part of every client ID.
var strategyName = regexp.MustCompile(`^[a-z0-9-]+$`)

// Parse decodes over the defaults, so an absent key keeps its default and an
// explicit 0 still fails validation. The `strategy:` block goes to
// s.Configure, which fails early on a bad block.
func Parse(b []byte, s strategy.Strategy) (*Config, error) {
	if s == nil {
		return nil, errors.New("config: no strategy")
	}
	if !strategyName.MatchString(s.Name()) {
		return nil, fmt.Errorf("config: strategy name %q must match %s", s.Name(), strategyName)
	}
	if err := refuseSecrets(b); err != nil {
		return nil, err
	}
	if err := refuseOldCostKeys(b); err != nil {
		return nil, err
	}
	c := defaults()
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %s", oneLine(err.Error()))
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := s.Configure(c.StrategyParams); err != nil {
		return nil, fmt.Errorf("config: strategy %s: %s", s.Name(), oneLine(err.Error()))
	}
	c.name = s.Name()
	if ns, ok := s.(strategy.NewsSource); ok {
		p := ns.NewsPolicy()
		if len(p.Feeds) > 0 && len(p.Keywords) == 0 { // no keyword matches nothing: the guard would always clear
			return nil, fmt.Errorf("config: strategy %s: news policy has feeds but no keywords", s.Name())
		}
		c.news = &p
	}
	return &c, nil
}

func (c *Config) Interval() time.Duration  { return c.interval }
func (c *Config) Location() *time.Location { return c.location }

// StrategyName is the Name() of the strategy the config was parsed for.
func (c *Config) StrategyName() string { return c.name }

// NewsPolicy is the strategy's news policy; nil when it has none.
func (c *Config) NewsPolicy() *strategy.NewsPolicy { return c.news }

func (c *Config) validate() error {
	r := c.Risk
	var err error
	switch {
	case c.Mode != "paper" && c.Mode != "live":
		return fmt.Errorf("mode must be paper or live, got %q", c.Mode)
	case !exchangeID.MatchString(c.Exchange):
		return fmt.Errorf("exchange %q must be fake or a CCXT exchange id such as kraken", c.Exchange)
	case c.Market != "spot" && c.Market != "swap":
		return fmt.Errorf("market must be spot or swap (linear), got %q", c.Market)
	case c.Market == "spot" && (c.Perp != Perp{Leverage: 1}):
		return errors.New("perp settings apply to market: swap only; remove the perp block on spot")
	case c.Market == "swap" && c.Perp.MarginMode != "" && c.Perp.MarginMode != "isolated" && c.Perp.MarginMode != "cross":
		return fmt.Errorf("perp.margin_mode must be isolated or cross, got %q", c.Perp.MarginMode)
	case c.AssetClass != "crypto" && c.AssetClass != "equity":
		return fmt.Errorf("asset_class must be crypto or equity, got %q", c.AssetClass)
	case c.NewsGuard != "" && c.NewsGuard != "none" && c.NewsGuard != "required":
		return fmt.Errorf("news_guard must be none or required, got %q", c.NewsGuard)
	case c.AssetClass == "equity" && c.NewsGuard == "none":
		return errors.New("news_guard none is refused for asset_class equity: equities check news before entering")
	case len(c.Symbols) == 0 || slices.Contains(c.Symbols, ""):
		return errors.New("symbols must list at least one non-empty symbol")
	case r.MaxRiskPerTradePct <= 0:
		return errors.New("risk.max_risk_per_trade_pct must be > 0")
	case r.MaxDailyDrawdownPct <= 0:
		return errors.New("risk.max_daily_drawdown_pct must be > 0")
	case r.MaxWeeklyDrawdownPct <= 0:
		return errors.New("risk.max_weekly_drawdown_pct must be > 0")
	case r.MinDemoPeriodDays <= 0:
		return errors.New("risk.min_demo_period_days must be > 0")
	case r.MaxLeverage < 1:
		return errors.New("risk.max_leverage must be >= 1")
	case r.MinLiquidationBuffer < 0 || r.MinLiquidationBuffer >= 1:
		return errors.New("risk.min_liquidation_buffer must be >= 0 and < 1")
	case c.Perp.Leverage < 1:
		return errors.New("perp.leverage must be >= 1")
	case c.Perp.Leverage > r.MaxLeverage:
		return fmt.Errorf("perp.leverage %d is above risk.max_leverage %d", c.Perp.Leverage, r.MaxLeverage)
	case c.Demo != (Demo{}) && c.Demo != o4:
		return fmt.Errorf("demo must be {source: testnet, candles: mainnet} (the only demo implemented: testnet orders, mainnet candles), got {source: %q, candles: %q}", c.Demo.Source, c.Demo.Candles)
	case !r.ConfirmedByUser:
		return errors.New("risk.confirmed_by_user is not true: review the four risk limits and confirm them")
	}
	if c.Market == "swap" && c.Perp.MarginMode == "" { // isolated caps a position's loss at its own margin
		c.Perp.MarginMode = "isolated"
	}
	if c.NewsGuard == "" { // equities check news before entering; crypto news lags the price
		c.NewsGuard = map[string]string{"crypto": "none", "equity": "required"}[c.AssetClass]
	}
	if c.interval, err = time.ParseDuration(c.IntervalSpec); err != nil || c.interval <= 0 {
		return fmt.Errorf("interval %q must be a positive duration such as 15m or 1h", c.IntervalSpec)
	}
	if c.location, err = time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("timezone %q unknown", c.Timezone)
	}
	return c.resolveSim()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
