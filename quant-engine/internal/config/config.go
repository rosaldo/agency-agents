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
	AssetClass     string    `yaml:"asset_class"`
	NewsGuard      string    `yaml:"news_guard"`
	IntervalSpec   string    `yaml:"interval"`
	Timezone       string    `yaml:"timezone"`
	Symbols        []string  `yaml:"symbols"`
	StrategyParams yaml.Node `yaml:"strategy"` // opaque: validated by Strategy.Configure
	Perp           Perp      `yaml:"perp"`
	Risk           Risk      `yaml:"risk"`
	Costs          Costs     `yaml:"costs"`
	ValidationFile string    `yaml:"validation_file"`

	interval time.Duration
	location *time.Location
	name     string               // Strategy.Name()
	news     *strategy.NewsPolicy // nil = the strategy has no news policy
}

// Perp holds the perpetual-swap settings; for now only whether the strategy
// may go short (refused when false).
type Perp struct {
	AllowShort bool `yaml:"allow_short"`
}

type Risk struct {
	MaxRiskPerTradePct   float64 `yaml:"max_risk_per_trade_pct"`
	MaxDailyDrawdownPct  float64 `yaml:"max_daily_drawdown_pct"`
	MaxWeeklyDrawdownPct float64 `yaml:"max_weekly_drawdown_pct"`
	MinDemoPeriodDays    int     `yaml:"min_demo_period_days"`
	ConfirmedByUser      bool    `yaml:"confirmed_by_user"`
}

// Costs feed the backtest; slippage_pct also widens the per-trade stop distance in sizing.
type Costs struct {
	CommissionPct float64 `yaml:"commission_pct"`
	SpreadPct     float64 `yaml:"spread_pct"`
	SlippagePct   float64 `yaml:"slippage_pct"`
}

// defaults are the Quant Trader's suggested limits; the user confirms them.
func defaults() Config {
	return Config{
		Mode:     "paper",
		Timezone: "UTC",
		Risk:     Risk{MaxRiskPerTradePct: 1, MaxDailyDrawdownPct: 3, MaxWeeklyDrawdownPct: 6, MinDemoPeriodDays: 30},
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
	case c.Exchange != "fake" && c.AssetClass != "crypto":
		return errors.New("asset_class must be crypto: exchanges come through CCXT, which covers crypto only")
	case c.AssetClass != "crypto" && c.AssetClass != "equity":
		return fmt.Errorf("asset_class must be crypto or equity, got %q", c.AssetClass)
	case c.NewsGuard != "" && c.NewsGuard != "none" && c.NewsGuard != "required":
		return fmt.Errorf("news_guard must be none or required, got %q", c.NewsGuard)
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
	case c.Costs.CommissionPct < 0 || c.Costs.SpreadPct < 0 || c.Costs.SlippagePct < 0:
		return errors.New("costs must be >= 0")
	case !r.ConfirmedByUser:
		return errors.New("risk.confirmed_by_user is not true: review the four risk limits and confirm them")
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
	return nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
