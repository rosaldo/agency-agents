package ccxt

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	lib "github.com/rosaldo/ccxt/v5"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
)

// levStub is the swap stub whose AAA/USDC:USDC market caps leverage at 20.
type levStub struct{ *swapStub }

func (c *levStub) LoadMarkets(...any) (map[string]lib.MarketInterface, error) {
	ms, _ := c.swapStub.LoadMarkets()
	m := ms["AAA/USDC:USDC"]
	m.Limits.Leverage.Max = f(20)
	ms["AAA/USDC:USDC"] = m
	return ms, nil
}

func liqAdapter(t *testing.T, v Venue) *Adapter {
	t.Helper()
	a, err := New(&levStub{newSwapStub()}, Options{Venue: v, Symbols: []string{"AAA/USDC:USDC"}, Market: "swap", Env: exchange.Real,
		Book: filepath.Join(t.TempDir(), "book.json"), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// With the venue's maintenance margin rate the adapter estimates the
// isolated liquidation distance (long (1/L-mm)/(1-mm), short (1/L-mm)/(1+mm));
// leverage above the market's maximum gives 0; without a rate, no estimate.
func TestLiquidationDistanceFromTheMaintenanceMargin(t *testing.T) {
	a := liqAdapter(t, Venue{ReportsLiquidation: true,
		MaintenanceMargin: func(m lib.MarketInterface) (float64, bool) { return 1 / (2 * *m.Limits.Leverage.Max), true }})
	var est exchange.LiquidationEstimator = a
	if !a.Capabilities().ReportsLiquidation {
		t.Error("swap venue that reports liquidation: capability off")
	}
	for _, tc := range []struct {
		side exchange.PositionSide
		lev  float64
		want float64
	}{
		{exchange.Long, 3, (1.0/3 - 0.025) / 0.975},
		{exchange.Short, 3, (1.0/3 - 0.025) / 1.025},
		{exchange.Long, 21, 0},
	} {
		got, ok := est.LiquidationDistance("AAA/USDC:USDC", tc.side, 100, tc.lev)
		if !ok || math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("%s %gx: %v %v, want %v", tc.side, tc.lev, got, ok, tc.want)
		}
	}
	if _, ok := liqAdapter(t, Venue{}).LiquidationDistance("AAA/USDC:USDC", exchange.Long, 100, 3); ok {
		t.Error("estimate without a maintenance margin rate")
	}
	if liqAdapter(t, Venue{}).Capabilities().ReportsLiquidation {
		t.Error("venue without liquidation reports: capability on")
	}
}
