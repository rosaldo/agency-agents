package config

import (
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
)

// simYAML is a real venue's yml with the paper_sim block appended by each test.
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
		{"paper_sim: {liquidity_class: major}\n", "real", 550 * time.Millisecond, 0.01, 0.045, 0.009},
		{"paper_sim: {profile: optimistic, liquidity_class: major}\n", "optimistic", 250 * time.Millisecond, 0.002, 0.045, 0.009},
		{"paper_sim: {profile: pessimistic, liquidity_class: thin}\n", "pessimistic", 1500 * time.Millisecond, 0.6, 0.045, 0.09},
	} {
		c, err := simCosts(t, tc.block)
		if err != nil {
			t.Fatalf("%s: %v", tc.block, err)
		}
		got, hip3 := c.SimCosts()["BTC/USDC:USDC"], c.SimCosts()["XYZ-GOOGL/USDC:USDC"]
		if c.PaperSim.Profile != tc.profile || got != (SimCosts{tc.lat, tc.slip, tc.fee}) || hip3.CommissionPct != tc.hip3Fee {
			t.Errorf("%s: profile %q costs %+v hip3 %+v", tc.block, c.PaperSim.Profile, got, hip3)
		}
	}
}

func TestPaperSimRefusesUnknownProfilesAndMissingClass(t *testing.T) {
	for block, want := range map[string]string{
		"paper_sim: {profile: realistic, liquidity_class: major}\n":                   "use one of optimistic, real, pessimistic",
		"paper_sim: {liquidity_class: major, profiles: {best: {latency_ms: 1}}}\n":    "paper_sim.profiles.best unknown",
		"paper_sim: {liquidity_class: major, profiles: {real: {slippage_pct: -1}}}\n": ">= 0",
		"paper_sim: {profile: real}\n":                                                "liquidity_class",
		"paper_sim: {liquidity_class: huge}\n":                                        "liquidity_class",
		"paper_sim: {liquidity_class: major, profiles: {real: {fee: 1}}}\n":           "fee",
	} {
		if _, err := simCosts(t, block); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want it naming %q", block, err, want)
		}
	}
	// the fake exchange fills by itself: only the profile name is checked
	if _, err := Parse([]byte(validYAML), example.New()); err != nil {
		t.Fatalf("fake without paper_sim: %v", err)
	}
	if _, err := Parse([]byte(validYAML+"paper_sim: {profile: worst}\n"), example.New()); err == nil {
		t.Error("fake accepted an unknown profile")
	}
}

// profiles: overrides the active profile field by field.
func TestPaperSimOverride(t *testing.T) {
	c, err := simCosts(t, "paper_sim: {liquidity_class: major, profiles: {real: {latency_ms: 100, commission_pct: 0.02}, pessimistic: {latency_ms: 9}}}\n")
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
	base := hashOf(t, simYAML+"paper_sim: {liquidity_class: major}\n")
	if hashOf(t, simYAML+"paper_sim: {profile: real, liquidity_class: major}\n") != base {
		t.Error("the explicit default profile changed the hash")
	}
	for _, block := range []string{
		"paper_sim: {profile: pessimistic, liquidity_class: major}\n",
		"paper_sim: {liquidity_class: thin}\n",
		"paper_sim: {liquidity_class: major, profiles: {real: {latency_ms: 1}}}\n",
	} {
		if hashOf(t, simYAML+block) == base {
			t.Errorf("%s kept the hash", block)
		}
	}
	if hashOf(t, validYAML) == hashOf(t, validYAML+"paper_sim: {profile: optimistic}\n") {
		t.Error("the profile is not in the hash")
	}
}
