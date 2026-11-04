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

	"github.com/rosaldo/agency-agents/quant-engine/internal/news"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// exchangeID is "fake" or a CCXT exchange id (checked against CCXT at start).
var exchangeID = regexp.MustCompile(`^[a-z0-9]+$`)

type Config struct {
	Mode         string `yaml:"mode"`
	Exchange     string `yaml:"exchange"`
	Market       string `yaml:"market"` // spot | swap (linear)
	AssetClass   string `yaml:"asset_class"`
	NewsGuard    string `yaml:"news_guard"`
	IntervalSpec string `yaml:"interval"`
	// Feed is how the loop learns a candle closed: rest polls at the boundary
	// plus a grace; websocket wakes on the venue's stream, the value still
	// read over REST. Latency only, not a rule: outside the parameter hash.
	Feed           string    `yaml:"feed"`
	Timezone       string    `yaml:"timezone"`
	Symbols        []string  `yaml:"symbols"`
	StrategyParams yaml.Node `yaml:"strategy"` // opaque: validated by Strategy.Configure
	Perp           Perp      `yaml:"perp"`
	Risk           Risk      `yaml:"risk"`
	Demo           Demo      `yaml:"demo"` // optional; picks the demo level of mode: paper
	Gate           Gate      `yaml:"gate"`
	Sim            Sim       `yaml:"sim"`
	ValidationFile string    `yaml:"validation_file"`

	interval time.Duration
	location *time.Location
	testnet  bool                 // set at start when testnet credentials pick the venue's sandbox
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
	MaxLeverage          int     `yaml:"max_leverage"`           // hard ceiling for perp.leverage
	MinLiquidationBuffer float64 `yaml:"min_liquidation_buffer"` // share of the distance to liquidation the stop leaves free
	ConfirmedByUser      bool    `yaml:"confirmed_by_user"`
}

// Demo picks the level mode: paper runs at. source: paper is the engine's
// simulator on the venue's public mainnet data, with no credential, even on a
// venue that has a testnet; source: testnet trades the venue's sandbox with
// the testnet credentials and reads the signal's candles from mainnet.
// Without source, testnet credentials pick the testnet and their absence the
// simulator. Candles always come from mainnet.
type Demo struct {
	Source  string `yaml:"source"`  // paper | testnet
	Candles string `yaml:"candles"` // mainnet
}

// Gate holds what mainnet needs beyond the walk-forward validation and the
// typed confirmation: MinDemoDays > 0 also requires a demo journal with the
// same parameter hash on that many calendar days; 0 (default) requires none.
type Gate struct {
	MinDemoDays int `yaml:"min_demo_days"`
}

// defaults are the Quant Trader's suggested limits; the user confirms them.
func defaults() Config {
	return Config{
		Mode:     "paper",
		Feed:     "rest",
		Market:   "spot",
		Timezone: "UTC",
		Perp:     Perp{Leverage: 1},
		Risk: Risk{MaxRiskPerTradePct: 1, MaxDailyDrawdownPct: 3, MaxWeeklyDrawdownPct: 6,
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
		if err := checkClassifier(p.Classifier); err != nil {
			return nil, fmt.Errorf("config: strategy %s: news classifier: %w", s.Name(), err)
		}
		if p.Classifier != nil && c.NewsGuard != "required" { // nothing would ever ask it
			return nil, fmt.Errorf("config: strategy %s: news classifier: news_guard is %s, so it would never be asked; set news_guard: required or remove the classifier", s.Name(), c.NewsGuard)
		}
		c.news = &p
		if c.NewsGuard == "required" && len(p.Feeds) > 0 {
			if key, bad := news.IgnoredAliasKey(p.Aliases); bad {
				return nil, fmt.Errorf("config: strategy %s: alias key %q is never read: a key is one uppercase name, the base or, on HIP-3, the ticker or the dex (for XYZ-GOOGL/USDC:USDC: GOOGL or XYZ)", s.Name(), key)
			}
			for _, sym := range c.Symbols {
				if name := news.NameNeedingAlias(sym, p.Aliases); name != "" {
					return nil, fmt.Errorf("config: strategy %s: symbol %q: a name of one character cannot be searched in headlines, so the news guard would find nothing; add its names to the news policy's aliases, one word each, such as {%s: [name]}", s.Name(), sym, name)
				}
				if alias, bad := news.UnsearchableAlias(sym, p.Aliases); bad {
					return nil, fmt.Errorf("config: strategy %s: symbol %q: alias %q can never match: a headline is compared one word at a time, letters and digits only, so the news guard would not find it; list each name as one such word", s.Name(), sym, alias)
				}
			}
		}
	}
	if _, err := c.paramsHash(); err != nil {
		return nil, fmt.Errorf("config: a key that is not a string (quote true, false, null and numbers used as keys) or a .inf or .nan value cannot be part of a run's parameters: %s", oneLine(err.Error()))
	}
	return &c, nil
}

func (c *Config) Interval() time.Duration { return c.interval }

// Level is the trading level a run is on: paper (the engine's simulator),
// testnet (the venue's sandbox) or mainnet (real money). Each level keeps its
// own state and book, so one never restores another's breaker or positions.
func (c *Config) Level() string {
	switch {
	case c.Mode == "mainnet":
		return "mainnet"
	case c.Demo.Source == "testnet" || c.testnet:
		return "testnet"
	}
	return "paper"
}

// UseTestnet records that testnet credentials picked the sandbox for a
// mode: paper run without demo.source.
func (c *Config) UseTestnet()              { c.testnet = true }
func (c *Config) Location() *time.Location { return c.location }

// StrategyName is the Name() of the strategy the config was parsed for.
func (c *Config) StrategyName() string { return c.name }

// NewsPolicy is the strategy's news policy; nil when it has none.
func (c *Config) NewsPolicy() *strategy.NewsPolicy { return c.news }

func (c *Config) validate() error {
	r := c.Risk
	var err error
	switch {
	case c.Mode == "live":
		return errors.New("mode: live is gone; the real-money level is now mode: mainnet (rename the value)")
	case c.Mode != "paper" && c.Mode != "mainnet":
		return fmt.Errorf("mode must be paper or mainnet, got %q", c.Mode)
	case !exchangeID.MatchString(c.Exchange):
		return fmt.Errorf("exchange %q must be fake or a CCXT exchange id such as kraken", c.Exchange)
	case c.Market != "spot" && c.Market != "swap":
		return fmt.Errorf("market must be spot or swap (linear), got %q", c.Market)
	case c.Market == "spot" && (c.Perp != Perp{Leverage: 1}):
		return errors.New("perp settings apply to market: swap only; remove the perp block on spot")
	case c.Market == "swap" && c.Perp.MarginMode != "" && c.Perp.MarginMode != "isolated" && c.Perp.MarginMode != "cross":
		return fmt.Errorf("perp.margin_mode must be isolated or cross, got %q", c.Perp.MarginMode)
	case c.Feed != "rest" && c.Feed != "websocket":
		return fmt.Errorf("feed must be rest or websocket, got %q", c.Feed)
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
	case c.Gate.MinDemoDays < 0:
		return errors.New("gate.min_demo_days must be >= 0")
	case r.MaxLeverage < 1:
		return errors.New("risk.max_leverage must be >= 1")
	case r.MinLiquidationBuffer < 0 || r.MinLiquidationBuffer >= 1:
		return errors.New("risk.min_liquidation_buffer must be >= 0 and < 1")
	case c.Perp.Leverage < 1:
		return errors.New("perp.leverage must be >= 1")
	case c.Perp.Leverage > r.MaxLeverage:
		return fmt.Errorf("perp.leverage %d is above risk.max_leverage %d", c.Perp.Leverage, r.MaxLeverage)
	case c.Demo.Source != "" && c.Demo.Source != "paper" && c.Demo.Source != "testnet":
		return fmt.Errorf("demo.source must be paper (the engine's simulator, no credential) or testnet (the venue's sandbox, testnet credentials), got %q", c.Demo.Source)
	case c.Demo.Candles != "" && c.Demo.Candles != "mainnet":
		return fmt.Errorf("demo.candles must be mainnet (the only source of signal candles implemented), got %q", c.Demo.Candles)
	case !r.ConfirmedByUser:
		return errors.New("risk.confirmed_by_user is not true: review the five risk limits and confirm them")
	}
	if c.Market == "swap" && c.Perp.MarginMode == "" { // isolated caps a position's loss at its own margin
		c.Perp.MarginMode = "isolated"
	}
	if c.NewsGuard == "" { // equities check news before entering; crypto news lags the price
		c.NewsGuard = map[string]string{"crypto": "none", "equity": "required"}[c.AssetClass]
	}
	if c.AssetClass == "equity" && c.Exchange != "fake" { // the fake adapter's symbols are made up
		for _, sym := range c.Symbols {
			if _, _, ok := market.HIP3(sym); !ok {
				return fmt.Errorf("asset_class equity: symbol %q is not in the form DEX-TICKER/QUOTE:SETTLE with letters and digits only, such as XYZ-GOOGL/USDC:USDC: the news guard could not search its ticker", sym)
			}
		}
	}
	if c.interval, err = time.ParseDuration(c.IntervalSpec); err != nil || c.interval <= 0 {
		return fmt.Errorf("interval %q must be a positive duration such as 15m or 1h", c.IntervalSpec)
	}
	if c.location, err = time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("timezone %q unknown", c.Timezone)
	}
	if err := c.resolveSim(); err != nil {
		return err
	}
	if c.Mode == "mainnet" || c.Mode == "paper" && c.Demo.Source == "testnet" {
		return c.CheckRealFills()
	}
	return nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// checkClassifier refuses a news classifier the engine cannot ask: a noul
// question only, with its text, a model and a threshold in (0, 1].
func checkClassifier(c *strategy.NewsClassifier) error {
	switch {
	case c == nil:
		return nil
	case c.Type != "noul":
		return fmt.Errorf("type %q is not supported (noul only)", c.Type)
	case strings.TrimSpace(c.Question) == "":
		return errors.New("no question")
	case c.Model == "":
		return errors.New("no model")
	case !(c.Threshold > 0 && c.Threshold <= 1):
		return fmt.Errorf("threshold %v is outside (0, 1]", c.Threshold)
	}
	for k := range c.Criteria {
		if k != "true" && k != "false" {
			return fmt.Errorf("criteria key %q: a noul question takes true and false", k)
		}
	}
	return nil
}
