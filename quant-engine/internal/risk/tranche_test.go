package risk_test

import (
	"math"
	"strings"
	"testing"

	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/risk"
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
)

// realFillCosts are the costs sizing counts on real fills with the
// pessimistic profile: floored at the real one.
func realFillCosts(t *testing.T) config.SimCosts {
	t.Helper()
	yml := "mode: paper\nexchange: hyperliquid\nmarket: swap\nasset_class: crypto\nsymbols: [SOL/USDC:USDC]\ninterval: 1h\n" +
		"perp: {leverage: 2, margin_mode: isolated}\nrisk: {confirmed_by_user: true}\n" +
		"sim: {profile: pessimistic, liquidity_class: liquid_alt}\n"
	c, err := config.Parse([]byte(yml), &scripted.Strategy{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckRealFills(); err != nil {
		t.Fatal(err)
	}
	return c.SizingCosts()["SOL/USDC:USDC"]
}

// lossAtStop is a tranche's loss when the stop fills, with both fills at
// their worst: slippage against each leg, commission on each notional.
func lossAtStop(qty, entry, stop float64, short bool, c config.SimCosts) float64 {
	adj := c.SlippagePct / 100
	buy, sell := entry*(1+adj), stop*(1-adj)
	if short {
		buy, sell = stop*(1+adj), entry*(1-adj)
	}
	return qty * (buy - sell + (buy+sell)*c.CommissionPct/100)
}

type tranche struct {
	f, equity, entry, stop float64
	refused                string // "" = taken
}

// Whatever the fractions, the prices, a stop tightened between
// tranches or the equity when a tranche comes, the loss of the AGGREGATE
// position at its stop stays within the budget fixed at the first tranche.
func TestAggregateLossAtTheStopStaysWithinTheTradeBudget(t *testing.T) {
	c := realFillCosts(t)
	cases := []struct {
		name  string
		short bool
		ts    []tranche
	}{
		{"one tranche is today's entry", false, []tranche{{0, 10_000, 100, 95, ""}}},
		{"three falling tranches", false, []tranche{{0.25, 10_000, 100, 90, ""}, {0.25, 10_000, 96, 90, ""}, {0.5, 10_000, 93, 90, ""}}},
		{"equity up between tranches does not grow the budget", false, []tranche{{0.5, 10_000, 100, 95, ""}, {0.5, 20_000, 98, 95, ""}}},
		{"equity down keeps the budget too", false, []tranche{{0.5, 10_000, 100, 95, ""}, {0.5, 9_000, 97, 95, ""}}},
		{"stop tightened by an add", false, []tranche{{0.4, 10_000, 100, 90, ""}, {0.6, 10_000, 97, 94, ""}}},
		{"fractions above the whole budget", false, []tranche{{0.6, 10_000, 100, 95, ""}, {0.6, 10_000, 98, 95, "above the whole trade budget"}}},
		{"after a full entry nothing is left", false, []tranche{{0, 10_000, 100, 95, ""}, {0.1, 10_000, 98, 95, "above the whole trade budget"}}},
		{"short tranches rising", true, []tranche{{0.5, 10_000, 100, 106, ""}, {0.3, 12_000, 102, 106, ""}, {0.3, 12_000, 103, 106, "above"}, {0.2, 12_000, 104, 106, ""}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tr risk.Trade
			type held struct{ qty, entry float64 }
			var pos []held
			budget := tc.ts[0].equity * 1 / 100
			for i, x := range tc.ts {
				next, qty, why := tr.Tranche(x.f, risk.SizeInput{Equity: x.equity, RiskPct: 1, Entry: x.entry, Stop: x.stop, Short: tc.short,
					SlippagePct: c.SlippagePct, CommissionPct: c.CommissionPct, QtyStep: 0.001, MinQty: 0.001})
				switch {
				case x.refused != "" && (qty != 0 || !strings.Contains(why, x.refused)):
					t.Fatalf("tranche %d: qty %v (%s), want refused with %q", i, qty, why, x.refused)
				case x.refused == "" && qty <= 0:
					t.Fatalf("tranche %d: refused (%s)", i, why)
				}
				if qty > 0 {
					pos = append(pos, held{qty, x.entry})
				}
				tr = next
			}
			stop := tc.ts[len(tc.ts)-1].stop // the position's single stop, as it ends
			var loss float64
			for _, p := range pos {
				loss += lossAtStop(p.qty, p.entry, stop, tc.short, c)
			}
			if loss > budget*(1+1e-9) || tr.Used > 1+1e-9 || tr.Budget != budget {
				t.Fatalf("aggregate loss %.6f, budget %.6f (trade %+v)", loss, budget, tr)
			}
		})
	}
}

// Fraction 0 (or 1) on a fresh trade sizes exactly as Size: the example
// strategy, which never sets it, keeps today's quantities bit for bit.
func TestWholeFirstTrancheIsTodaysSize(t *testing.T) {
	in := risk.SizeInput{Equity: 12_345.67, RiskPct: 0.7, Entry: 101.3, Stop: 97.1, SlippagePct: 0.08, CommissionPct: 0.045, QtyStep: 0.001, MinQty: 0.001}
	want, _ := risk.Size(in)
	for _, f := range []float64{0, 1} {
		next, qty, _ := risk.Trade{}.Tranche(f, in)
		if qty != want || next.Used != 1 || next.Budget != in.Equity*in.RiskPct/100 {
			t.Fatalf("fraction %v: qty %v trade %+v, want qty %v used 1", f, qty, next, want)
		}
	}
}

func TestFractionOutsideZeroOneIsRefused(t *testing.T) {
	in := risk.SizeInput{Equity: 10_000, RiskPct: 1, Entry: 100, Stop: 95, QtyStep: 0.001, MinQty: 0.001}
	for _, f := range []float64{-0.1, 1.5, math.NaN(), math.Inf(1)} {
		if next, qty, why := (risk.Trade{}).Tranche(f, in); qty != 0 || next.Used != 0 || !strings.Contains(why, "fraction") {
			t.Errorf("fraction %v: qty %v trade %+v (%s), want refused", f, qty, next, why)
		}
	}
}

// A tranche too small to trade is not counted: the fraction stays free.
func TestSkippedTrancheIsNotCounted(t *testing.T) {
	tr := risk.Trade{Budget: 100, Used: 0.5}
	next, qty, _ := tr.Tranche(0.5, risk.SizeInput{Equity: 1, RiskPct: 1, Entry: 100, Stop: 95, QtyStep: 100, MinQty: 100})
	if qty != 0 || next != tr {
		t.Fatalf("qty %v trade %+v, want nothing taken and %+v kept", qty, next, tr)
	}
}
