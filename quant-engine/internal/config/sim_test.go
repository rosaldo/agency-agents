package config

import (
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
)

// simYAML is a real venue's yml with the sim block appended by each test.
const simYAML = `
exchange: hyperliquid
market: swap
asset_class: crypto
symbols: [BTC/USDC:USDC, XYZ-GOOGL/USDC:USDC]
interval: 1h
strategy: {period: 20, stop_pct: 4, target_pct: 5}
risk: {confirmed_by_user: true}
`

func simCosts(t *testing.T, block string) (*Config, error) {
	t.Helper()
	return Parse([]byte(simYAML+block), example.New())
}

// FR-P10: the profile defaults to real, and each profile carries the
// built-in numbers (venue latency and taker fee, liquidity class slippage;
// HIP-3 markets their own fee).
func TestPaperSimProfilesCarryTheBuiltInNumbers(t *testing.T) {
	for _, tc := range []struct {
		block     string
		profile   string
		lat       time.Duration
		slip, fee float64
		hip3Fee   float64
	}{
		{"sim: {liquidity_class: major}\n", "real", 550 * time.Millisecond, 0.01, 0.045, 0.009},
		{"sim: {profile: optimistic, liquidity_class: major}\n", "optimistic", 250 * time.Millisecond, 0.002, 0.045, 0.009},
		{"sim: {profile: pessimistic, liquidity_class: thin}\n", "pessimistic", 1500 * time.Millisecond, 0.6, 0.045, 0.09},
	} {
		c, err := simCosts(t, tc.block)
		if err != nil {
			t.Fatalf("%s: %v", tc.block, err)
		}
		got, hip3 := c.SimCosts()["BTC/USDC:USDC"], c.SimCosts()["XYZ-GOOGL/USDC:USDC"]
		if c.Sim.Profile != tc.profile || got != (SimCosts{tc.lat, tc.slip, tc.fee}) || hip3.CommissionPct != tc.hip3Fee {
			t.Errorf("%s: profile %q costs %+v hip3 %+v", tc.block, c.Sim.Profile, got, hip3)
		}
	}
}

func TestPaperSimRefusesUnknownProfilesAndMissingClass(t *testing.T) {
	for block, want := range map[string]string{
		"sim: {profile: realistic, liquidity_class: major}\n":                   "use one of optimistic, real, pessimistic",
		"sim: {liquidity_class: major, profiles: {best: {latency_ms: 1}}}\n":    "sim.profiles.best unknown",
		"sim: {liquidity_class: major, profiles: {real: {slippage_pct: -1}}}\n": ">= 0",
		"sim: {profile: real}\n":                                      "liquidity_class",
		"sim: {liquidity_class: huge}\n":                              "liquidity_class",
		"sim: {liquidity_class: major, profiles: {real: {fee: 1}}}\n": "fee",
	} {
		if _, err := simCosts(t, block); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want it naming %q", block, err, want)
		}
	}
	// the fake exchange fills by itself: only the profile name is checked
	if _, err := Parse([]byte(validYAML), example.New()); err != nil {
		t.Fatalf("fake without sim: %v", err)
	}
	if _, err := Parse([]byte(validYAML+"sim: {profile: worst}\n"), example.New()); err == nil {
		t.Error("fake accepted an unknown profile")
	}
}

// profiles: overrides the active profile field by field.
func TestPaperSimOverride(t *testing.T) {
	c, err := simCosts(t, "sim: {liquidity_class: major, profiles: {real: {latency_ms: 100, commission_pct: 0.02}, pessimistic: {latency_ms: 9}}}\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.SimCosts()["BTC/USDC:USDC"]; got != (SimCosts{100 * time.Millisecond, 0.01, 0.02}) {
		t.Fatalf("costs %+v; want latency and fee overridden, slippage kept", got)
	}
}

// The profile and its costs are in the parameter hash: the gate ties the
// paper run to the live one.
func TestPaperSimInTheHash(t *testing.T) {
	base := hashOf(t, simYAML+"sim: {liquidity_class: major}\n")
	if hashOf(t, simYAML+"sim: {profile: real, liquidity_class: major}\n") != base {
		t.Error("the explicit default profile changed the hash")
	}
	for _, block := range []string{
		"sim: {profile: pessimistic, liquidity_class: major}\n",
		"sim: {liquidity_class: thin}\n",
		"sim: {liquidity_class: major, profiles: {real: {latency_ms: 1}}}\n",
	} {
		if hashOf(t, simYAML+block) == base {
			t.Errorf("%s kept the hash", block)
		}
	}
	if hashOf(t, validYAML) == hashOf(t, validYAML+"sim: {profile: optimistic}\n") {
		t.Error("the profile is not in the hash")
	}
}

// The profile is the single source of costs: the old blocks are refused,
// pointing at sim; on the fake exchange the costs are the overrides.
func TestSimIsTheOnlySourceOfCosts(t *testing.T) {
	for block, want := range map[string]string{
		"costs: {commission_pct: 0.045}\n":  "costs come from the sim profile",
		"paper_sim: {profile: real}\n":      "paper_sim is now sim",
		"costs: {}\nsim: {profile: real}\n": "costs come from the sim profile",
	} {
		if _, err := Parse([]byte(validYAML+block), example.New()); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want it naming %q", block, err, want)
		}
	}
	c, err := Parse([]byte(validYAML+"sim: {profiles: {real: {commission_pct: 0.1, slippage_pct: 0.2}}}\n"), example.New())
	if err != nil {
		t.Fatal(err)
	}
	if got := c.SimCosts()["AAA-USD"]; got != (SimCosts{0, 0.2, 0.1}) {
		t.Fatalf("fake costs %+v; want the overrides", got)
	}
}

// Every built-in number against the public research table, written here by
// hand so a changed number fails: latency per venue, slippage per liquidity
// class and taker fee per venue (HIP-3 included), in each profile.
func TestSimBuiltInNumbersMatchTheResearch(t *testing.T) {
	venues := map[string]struct {
		lat [3]time.Duration
		fee float64
	}{
		"hyperliquid":           {[3]time.Duration{250, 550, 1500}, 0.045},
		"okx":                   {[3]time.Duration{150, 400, 1200}, 0.05},
		"binanceusdm":           {[3]time.Duration{150, 350, 1200}, 0.05},
		"coinbaseinternational": {[3]time.Duration{150, 300, 1000}, 0.035},
	}
	classes := map[string][3]float64{ // percent (research table in bps / 100)
		"major":      {0.002, 0.01, 0.04},
		"liquid_alt": {0.005, 0.02, 0.08},
		"mid_alt":    {0.02, 0.05, 0.15},
		"thin":       {0.05, 0.25, 0.6},
	}
	hip3Fee := [3]float64{0.009, 0.009, 0.09}
	for venue, v := range venues {
		for class, slip := range classes {
			for i, profile := range SimProfiles {
				yml := strings.Replace(simYAML, "hyperliquid", venue, 1) +
					"sim: {profile: " + profile + ", liquidity_class: " + class + "}\n"
				c, err := Parse([]byte(yml), example.New())
				if err != nil {
					t.Fatalf("%s %s %s: %v", venue, class, profile, err)
				}
				want := SimCosts{v.lat[i] * time.Millisecond, slip[i], v.fee}
				if got := c.SimCosts()["BTC/USDC:USDC"]; got != want {
					t.Errorf("%s %s %s: costs %+v, want %+v", venue, class, profile, got, want)
				}
				fee := v.fee
				if venue == "hyperliquid" {
					fee = hip3Fee[i]
				}
				if got := c.SimCosts()["XYZ-GOOGL/USDC:USDC"].CommissionPct; got != fee {
					t.Errorf("%s %s HIP-3 fee %v, want %v", venue, profile, got, fee)
				}
			}
		}
	}
}

// Every numeric override is refused below zero, naming the field.
func TestSimRefusesNegativeOverrides(t *testing.T) {
	for _, field := range []string{"latency_ms", "slippage_pct", "commission_pct"} {
		_, err := simCosts(t, "sim: {liquidity_class: major, profiles: {pessimistic: {"+field+": -1}}}\n")
		if err == nil || !strings.Contains(err.Error(), "sim.profiles.pessimistic."+field+" must be >= 0") {
			t.Errorf("%s -1: err %v, want it refused naming the field", field, err)
		}
	}
}

// A missing class says it is required, naming the key and the exchange.
func TestSimMissingClassIsRequired(t *testing.T) {
	_, err := simCosts(t, "sim: {profile: real}\n")
	if err == nil || !strings.Contains(err.Error(), `sim.liquidity_class is required on exchange "hyperliquid"`) {
		t.Fatalf("err %v", err)
	}
}

// FR-R1: real fills (live, testnet) are sized with, field by field, the
// larger of the active profile (overrides included) and the built-in real
// profile, so the fee never falls below the venue's contract; the simulator
// charges the active profile as it is.
func TestSizingCostsAreFlooredAtTheRealProfile(t *testing.T) {
	real, err := simCosts(t, "sim: {liquidity_class: major}\n")
	if err != nil {
		t.Fatal(err)
	}
	c, err := simCosts(t, "sim: {profile: optimistic, liquidity_class: major, profiles: {optimistic: {commission_pct: 0}}}\n")
	if err != nil {
		t.Fatal(err)
	}
	for sym, want := range real.SimCosts() {
		if got := c.SizingCosts()[sym]; got != want {
			t.Errorf("%s: sizing %+v, want the real profile %+v", sym, got, want)
		}
		if got := c.SimCosts()[sym]; got.CommissionPct != 0 || got.SlippagePct != 0.002 {
			t.Errorf("%s: simulator %+v, want the optimistic profile with fee 0", sym, got)
		}
	}
	p, err := simCosts(t, "sim: {profile: pessimistic, liquidity_class: major, profiles: {pessimistic: {commission_pct: 0}}}\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.SizingCosts()["BTC/USDC:USDC"]; got != (SimCosts{1500 * time.Millisecond, 0.04, 0.045}) {
		t.Errorf("pessimistic with fee 0: sizing %+v, want its own latency and slippage, the real fee", got)
	}
}

// With real fills (live, testnet) the liquidity class is required even
// with a slippage_pct override, so the real profile's slippage floor always
// exists; the simulator (paper without testnet, backtest) still accepts the
// override alone.
func TestRealFillsRequireTheLiquidityClass(t *testing.T) {
	override := "sim: {profiles: {real: {slippage_pct: 0}}}\n"
	if _, err := simCosts(t, override); err != nil {
		t.Fatalf("simulation with a slippage override and no class: %v", err)
	}
	if _, err := simCosts(t, "demo: {source: paper}\n"+override); err != nil {
		t.Fatalf("explicit paper level with a slippage override and no class: %v", err)
	}
	for _, level := range []string{"mode: live\n", "demo: {source: testnet}\n"} {
		if _, err := simCosts(t, level+override); err == nil || !strings.Contains(err.Error(), "sim.liquidity_class is required with real fills") {
			t.Errorf("%s: err %v, want the class required", strings.TrimSpace(level), err)
		}
		if _, err := simCosts(t, level+"sim: {liquidity_class: major, profiles: {real: {slippage_pct: 0}}}\n"); err != nil {
			t.Errorf("%s with the class: %v", strings.TrimSpace(level), err)
		}
	}
}

// On an exchange with no built-in numbers the overrides
// are the only floor, so real fills refuse a fee or slippage of 0.
func TestRealFillsRefuseZeroCostsWithoutBuiltIns(t *testing.T) {
	yml := strings.Replace(simYAML, "exchange: hyperliquid", "exchange: kraken", 1)
	parse := func(block string) error {
		_, err := Parse([]byte(yml+block), example.New())
		return err
	}
	ok := "sim: {liquidity_class: major, profiles: {real: {latency_ms: 300, commission_pct: 0.05, slippage_pct: 0.01}}}\n"
	for _, level := range []string{"", "mode: live\n", "demo: {source: testnet}\n"} {
		if err := parse(level + ok); err != nil {
			t.Errorf("%q: positive overrides refused: %v", level, err)
		}
	}
	for _, f := range []string{"commission_pct: 0.05", "slippage_pct: 0.01"} {
		zero := strings.Replace(ok, f, strings.Split(f, ":")[0]+": 0", 1)
		if err := parse(zero); err != nil {
			t.Errorf("simulation with %s 0: %v", f, err)
		}
		for _, level := range []string{"mode: live\n", "demo: {source: testnet}\n"} {
			if err := parse(level + zero); err == nil || !strings.Contains(err.Error(), strings.Split(f, ":")[0]+" must be > 0 with real fills") {
				t.Errorf("%s with %s 0: err %v, want a refusal", strings.TrimSpace(level), f, err)
			}
		}
	}
}
