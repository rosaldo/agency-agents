package config

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// refuseOldCostKeys refuses the keys `sim:` replaced, so no yml keeps two
// sources of costs.
func refuseOldCostKeys(b []byte) error {
	var top map[string]yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(b)).Decode(&top); err != nil {
		return nil // Parse reports the syntax error
	}
	if _, ok := top["costs"]; ok {
		return errors.New("config: the costs block is gone; costs come from the sim profile (sim.profile, sim.liquidity_class, sim.profiles.<profile>.commission_pct and slippage_pct)")
	}
	if r, ok := top["risk"]; ok {
		var risk map[string]yaml.Node
		if r.Decode(&risk) == nil {
			if _, ok := risk["min_demo_period_days"]; ok {
				return errors.New("config: risk.min_demo_period_days is gone; set gate.min_demo_days instead (optional; 0, the default, requires no demo days before live)")
			}
		}
	}
	if _, ok := top["paper_sim"]; ok {
		return errors.New("config: paper_sim is now sim (it also sets the backtest's and sizing's costs); rename the key")
	}
	return nil
}

// Sim is the single source of trading costs: one named profile picks the
// order delay, the slippage and the taker fee that the paper simulator (paper
// without a testnet account) and the backtest charge, and that sizing counts
// in the risk of a trade (FR-R1). `profiles:` overrides the built-in numbers
// of a profile field by field. The backtest has only candles, so it charges
// the slippage and the fee but cannot model the delay.
type Sim struct {
	Profile        string                 `yaml:"profile"`         // optimistic | real (default) | pessimistic
	LiquidityClass string                 `yaml:"liquidity_class"` // major | liquid_alt | mid_alt | thin
	Profiles       map[string]SimOverride `yaml:"profiles"`
}

// SimOverride replaces a built-in value of one profile; nil keeps it.
type SimOverride struct {
	LatencyMS     *int     `yaml:"latency_ms"`
	SlippagePct   *float64 `yaml:"slippage_pct"`
	CommissionPct *float64 `yaml:"commission_pct"`
}

// SimCosts is what the simulator charges every fill of one symbol: the fill
// takes the price Latency after the order, moved SlippagePct against the
// trader, and pays CommissionPct of its notional.
type SimCosts struct {
	Latency       time.Duration
	SlippagePct   float64
	CommissionPct float64
}

// SimProfiles are the profile names, from the most to the least favourable.
var SimProfiles = []string{"optimistic", "real", "pessimistic"}

// The built-in numbers, one per profile in SimProfiles order. Sources (read
// 2026-10-30): taker fees from each venue's public fee page at the base tier
// (hyperliquid.gitbook.io trading/fees, okx.com/fees, binance.com/en/fee/futureFee,
// support.deribit.com Fees for the Coinbase International perp route);
// Hyperliquid latency from its docs (hypercore/overview: median 0.2 s, p99
// 0.9 s co-located) plus a measured round trip to each venue's public API;
// slippage from half spread plus impact measured on the public order books
// for a 10k USD order. Pessimistic values add margin for stressed markets,
// which were not measured.
var (
	simLatencyMS = map[string][3]int{
		"hyperliquid":           {250, 550, 1500},
		"okx":                   {150, 400, 1200},
		"binanceusdm":           {150, 350, 1200},
		"coinbaseinternational": {150, 300, 1000},
	}
	simSlippageBps = map[string][3]float64{
		"major":      {0.2, 1, 4}, // BTC, ETH, the most liquid HIP-3 index
		"liquid_alt": {0.5, 2, 8}, // e.g. SOL
		"mid_alt":    {2, 5, 15},  // e.g. AVAX
		"thin":       {5, 25, 60}, // small HIP-3 markets
	}
	simTakerPct = map[string]float64{
		"hyperliquid":           0.045,
		"okx":                   0.05,
		"binanceusdm":           0.05,
		"coinbaseinternational": 0.035,
	}
	// HIP-3 (Hyperliquid builder-deployed perps, fee scale 1): growth mode on
	// (most markets as read) in optimistic and real, off in pessimistic.
	// shortcut: fixed numbers, the adapter does not read growthMode and
	// deployerFeeScale from the venue's meta; compute them from it when it does.
	simHIP3TakerPct = [3]float64{0.009, 0.009, 0.090}
)

// hip3 matches a HIP-3 symbol as the Hyperliquid venue names it
// (DEX-TICKER/QUOTE:SETTLE).
var hip3 = regexp.MustCompile(`^[A-Z0-9]+-[A-Z0-9]+/`)

func sortedKeys[V any](m map[string]V) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return strings.Join(ks, ", ")
}

// resolveSim checks sim and computes the costs of every symbol. The fake
// exchange has no built-in numbers: its costs are the liquidity class's
// slippage, if set, and the profile's overrides, else zero.
func (c *Config) resolveSim() error {
	p := &c.Sim
	if p.Profile == "" {
		p.Profile = "real"
	}
	i := slices.Index(SimProfiles, p.Profile)
	if i < 0 {
		return fmt.Errorf("sim.profile %q unknown; use one of %s", p.Profile, strings.Join(SimProfiles, ", "))
	}
	for name, o := range p.Profiles {
		if !slices.Contains(SimProfiles, name) {
			return fmt.Errorf("sim.profiles.%s unknown; use one of %s", name, strings.Join(SimProfiles, ", "))
		}
		for _, f := range []struct {
			name string
			neg  bool
		}{
			{"latency_ms", o.LatencyMS != nil && *o.LatencyMS < 0},
			{"slippage_pct", o.SlippagePct != nil && *o.SlippagePct < 0},
			{"commission_pct", o.CommissionPct != nil && *o.CommissionPct < 0},
		} {
			if f.neg {
				return fmt.Errorf("sim.profiles.%s.%s must be >= 0", name, f.name)
			}
		}
	}
	fake := c.Exchange == "fake"
	o := p.Profiles[p.Profile]
	slip, ok := simSlippageBps[p.LiquidityClass]
	switch {
	case ok || o.SlippagePct != nil || fake && p.LiquidityClass == "":
	case p.LiquidityClass == "":
		return fmt.Errorf("sim.liquidity_class is required on exchange %q; use one of %s (it sets the simulated slippage)", c.Exchange, sortedKeys(simSlippageBps))
	default:
		return fmt.Errorf("sim.liquidity_class %q must be one of %s (it sets the simulated slippage)", p.LiquidityClass, sortedKeys(simSlippageBps))
	}
	lat, okLat := simLatencyMS[c.Exchange]
	taker, okFee := simTakerPct[c.Exchange]
	if !fake && (!okLat && o.LatencyMS == nil || !okFee && o.CommissionPct == nil) {
		return fmt.Errorf("sim has no built-in values for exchange %q: set sim.profiles.%s.latency_ms and commission_pct", c.Exchange, p.Profile)
	}
	builtIn := func(i int, sym string) SimCosts {
		sc := SimCosts{Latency: time.Duration(lat[i]) * time.Millisecond, SlippagePct: slip[i] / 100, CommissionPct: taker}
		if c.Exchange == "hyperliquid" && hip3.MatchString(sym) {
			sc.CommissionPct = simHIP3TakerPct[i]
		}
		return sc
	}
	c.sim, c.sizing = map[string]SimCosts{}, map[string]SimCosts{}
	for _, sym := range c.Symbols {
		sc := builtIn(i, sym)
		if o.LatencyMS != nil {
			sc.Latency = time.Duration(*o.LatencyMS) * time.Millisecond
		}
		if o.SlippagePct != nil {
			sc.SlippagePct = *o.SlippagePct
		}
		if o.CommissionPct != nil {
			sc.CommissionPct = *o.CommissionPct
		}
		c.sim[sym] = sc
		r := builtIn(1, sym) // the real profile; zero where nothing is built in
		c.sizing[sym] = SimCosts{max(sc.Latency, r.Latency), max(sc.SlippagePct, r.SlippagePct), max(sc.CommissionPct, r.CommissionPct)}
	}
	return nil
}

// CheckRealFills is what sim must hold when fills are real (live, testnet),
// so that sizing always has a floor: the liquidity class sets the
// real profile's slippage, so it is required even with a slippage_pct
// override; on an exchange with no built-in numbers the overrides are the
// only floor, so a fee or slippage of 0 is refused.
func (c *Config) CheckRealFills() error {
	if c.Exchange == "fake" {
		return nil
	}
	p := c.Sim
	if _, ok := simSlippageBps[p.LiquidityClass]; !ok {
		return fmt.Errorf("sim.liquidity_class is required with real fills (live, testnet), even with a slippage_pct override: it sets the real profile's slippage that sizing never goes below; use one of %s", sortedKeys(simSlippageBps))
	}
	if _, ok := simTakerPct[c.Exchange]; ok {
		return nil
	}
	o := p.Profiles[p.Profile]
	for _, f := range []struct {
		name string
		v    *float64
	}{{"commission_pct", o.CommissionPct}, {"slippage_pct", o.SlippagePct}} {
		if f.v != nil && *f.v == 0 {
			return fmt.Errorf("sim.profiles.%s.%s must be > 0 with real fills on exchange %q: it has no built-in value, so the override is the only floor sizing counts", p.Profile, f.name, c.Exchange)
		}
	}
	return nil
}

// SimCosts are the profile's costs per traded symbol.
func (c *Config) SimCosts() map[string]SimCosts { return c.sim }

// SizingCosts are the costs sizing counts when fills are real (live,
// testnet): field by field the larger of the active profile and the built-in
// real one, so a favourable profile or a low override never shrinks the risk
// of a real trade and the fee never falls below the venue's (FR-R1).
func (c *Config) SizingCosts() map[string]SimCosts { return c.sizing }
