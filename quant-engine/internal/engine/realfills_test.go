package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/paper"
	"github.com/rosaldo/agency-agents/quant-engine/internal/risk"
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
)

// optimisticZeroFee is a Hyperliquid swap yml whose active profile is
// optimistic with a zero fee: below the real profile in every cost.
func optimisticZeroFee(t *testing.T) *config.Config {
	t.Helper()
	yml := strings.Replace(scriptedYAML, "exchange: fake", "exchange: hyperliquid\nmarket: swap\nperp: {leverage: 2}", 1) +
		"sim: {profile: optimistic, liquidity_class: major, profiles: {optimistic: {commission_pct: 0}}}\n"
	c, err := config.Parse([]byte(yml), &scripted.Strategy{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The real-cost floor in the liquidation check: with real fills the round-trip cost
// taken off the distance to liquidation is the floored one (the real
// profile), not the optimistic zero-fee profile the simulator charges.
func TestRealFillsLiquidationDistanceIsFloored(t *testing.T) {
	c := optimisticZeroFee(t)
	e := New(c, &scripted.Strategy{}, fake.New(fake.Options{}), exchange.Real, nil, time.Now)
	e.Simulated = false
	floor := c.SizingCosts()["AAA"]
	want := risk.FallbackLiquidationDistance(2, risk.RoundTripCost(floor.CommissionPct, 0, floor.SlippagePct))
	if got := e.liquidationDistance("AAA", exchange.Long, 105, 2); got != want {
		t.Fatalf("real fills: distance %v, want %v (round trip at the real profile)", got, want)
	}
	e.Simulated = true
	if got := e.liquidationDistance("AAA", exchange.Long, 105, 2); got <= want {
		t.Fatalf("simulated fills: distance %v, want more than the floored %v (the optimistic profile as is)", got, want)
	}
}

// withSimNote is a non-simulator adapter that happens to have SimNote.
type withSimNote struct{ *fake.Fake }

func (withSimNote) SimNote() string { return "not a simulator" }

// Only the engine's own simulator counts as simulated fills: an adapter is
// never taken for it by the methods it has, so it never loses the floor.
func TestOnlyThePaperSimulatorIsSimulated(t *testing.T) {
	c := optimisticZeroFee(t)
	f := fake.New(fake.Options{})
	if e := New(c, &scripted.Strategy{}, withSimNote{f}, exchange.Real, nil, time.Now); e.Simulated {
		t.Fatal("an adapter with SimNote was taken for the simulator")
	}
	sim := paper.New(f, 10_000, time.Now, paper.Profile{Name: "optimistic", Costs: c.SimCosts()})
	if e := New(c, &scripted.Strategy{}, sim, exchange.Demo, nil, time.Now); !e.Simulated {
		t.Fatal("the paper simulator was not taken as simulated")
	}
}
