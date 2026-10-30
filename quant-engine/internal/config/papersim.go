package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// PaperSim is how the engine's paper simulator charges a fill (it applies
// only when paper runs without a testnet account): one named profile picks
// the order delay, the slippage and the taker fee; `profiles:` overrides the
// built-in numbers of a profile field by field.
type PaperSim struct {
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

// resolveSim checks paper_sim and computes the costs of every symbol. The
// fake exchange has its own fills, so only the profile name is checked there.
func (c *Config) resolveSim() error {
	p := &c.PaperSim
	if p.Profile == "" {
		p.Profile = "real"
	}
	i := slices.Index(SimProfiles, p.Profile)
	if i < 0 {
		return fmt.Errorf("paper_sim.profile %q unknown; use one of %s", p.Profile, strings.Join(SimProfiles, ", "))
	}
	for name, o := range p.Profiles {
		if !slices.Contains(SimProfiles, name) {
			return fmt.Errorf("paper_sim.profiles.%s unknown; use one of %s", name, strings.Join(SimProfiles, ", "))
		}
		if o.LatencyMS != nil && *o.LatencyMS < 0 || o.SlippagePct != nil && *o.SlippagePct < 0 || o.CommissionPct != nil && *o.CommissionPct < 0 {
			return fmt.Errorf("paper_sim.profiles.%s values must be >= 0", name)
		}
	}
	if c.Exchange == "fake" {
		return nil
	}
	o := p.Profiles[p.Profile]
	slip, ok := simSlippageBps[p.LiquidityClass]
	if !ok && o.SlippagePct == nil {
		return fmt.Errorf("paper_sim.liquidity_class %q must be one of %s (it sets the simulated slippage)", p.LiquidityClass, sortedKeys(simSlippageBps))
	}
	lat, okLat := simLatencyMS[c.Exchange]
	taker, okFee := simTakerPct[c.Exchange]
	if !okLat && o.LatencyMS == nil || !okFee && o.CommissionPct == nil {
		return fmt.Errorf("paper_sim has no built-in values for exchange %q: set paper_sim.profiles.%s.latency_ms and commission_pct", c.Exchange, p.Profile)
	}
	c.sim = map[string]SimCosts{}
	for _, sym := range c.Symbols {
		sc := SimCosts{Latency: time.Duration(lat[i]) * time.Millisecond, SlippagePct: slip[i] / 100, CommissionPct: taker}
		if c.Exchange == "hyperliquid" && hip3.MatchString(sym) {
			sc.CommissionPct = simHIP3TakerPct[i]
		}
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
	}
	return nil
}

// SimCosts are the paper simulator's costs per traded symbol; nil on the
// fake exchange.
func (c *Config) SimCosts() map[string]SimCosts { return c.sim }
