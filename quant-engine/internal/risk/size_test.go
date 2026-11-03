package risk

import (
	"math"
	"math/rand"
	"testing"
)

// Quant Trader "Position Sizing": size = floor(capital x risk / (|entry - stop| + slippage)).
func TestSizeFromTheRiskBudget(t *testing.T) {
	in := SizeInput{Equity: 10_000, RiskPct: 1, Entry: 50, Stop: 48, QtyStep: 1, MinQty: 1}
	if qty, why := Size(in); qty != 50 {
		t.Fatalf("qty = %v (%s), want 50", qty, why)
	}
	in.SlippagePct = 0.1 // both legs: 50.05 - 47.952 = 2.098 per unit, 100 / 2.098 = 47.66
	if qty, _ := Size(in); qty != 47 {
		t.Fatalf("qty with slippage = %v, want 47", qty)
	}
	in.SlippagePct, in.QtyStep, in.MinQty, in.Equity = 0, 0.01, 0.01, 1_000 // 10 / 2 = 5
	if qty, _ := Size(in); math.Abs(qty-5) > 1e-9 {
		t.Fatalf("fractional qty = %v, want 5", qty)
	}
}

func TestSizeBelowTheMinimumIsZero(t *testing.T) {
	if qty, why := Size(SizeInput{Equity: 100, RiskPct: 1, Entry: 50, Stop: 48, QtyStep: 1, MinQty: 1}); qty != 0 || why == "" { // 1 / 2 = 0.5
		t.Fatalf("qty = %v (%q), want 0 with a reason", qty, why)
	}
}

func TestStopOffTheLongSideIsZero(t *testing.T) {
	for _, stop := range []float64{0, -1, 50, 51} {
		if qty, why := Size(SizeInput{Equity: 10_000, RiskPct: 1, Entry: 50, Stop: stop, QtyStep: 1, MinQty: 1}); qty != 0 || why == "" {
			t.Errorf("stop %v: qty = %v (%q), want 0 with a reason", stop, qty, why)
		}
	}
}

// Property: the currency loss at the stop never exceeds the per-trade risk budget.
func TestRiskNeverExceedsTheBudget(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	steps := []float64{1, 0.1, 0.001, 0.00001}
	for i := 0; i < 5000; i++ {
		entry := 1 + r.Float64()*1000
		in := SizeInput{Equity: 10 + r.Float64()*1e6, RiskPct: 0.1 + r.Float64()*2, Entry: entry,
			Stop: entry * (1 - (0.1+r.Float64()*20)/100), SlippagePct: r.Float64() * 0.5, QtyStep: steps[r.Intn(len(steps))]}
		in.MinQty = in.QtyStep
		qty, _ := Size(in)
		budget := in.Equity * in.RiskPct / 100
		if loss := qty * (entry - in.Stop); loss > budget*(1+1e-9) {
			t.Fatalf("case %d: loss %.6f exceeds budget %.6f (%+v)", i, loss, budget, in)
		}
	}
}

// FR-R1: the round-trip commission (entry and stop fill) counts against the
// budget, on the notional.
func TestSizeCountsRoundTripFees(t *testing.T) {
	// per unit: 2 + (50+48) x 0.1% = 2.098; 100 / 2.098 = 47.66
	in := SizeInput{Equity: 10_000, RiskPct: 1, Entry: 50, Stop: 48, CommissionPct: 0.1, QtyStep: 1, MinQty: 1}
	if qty, why := Size(in); qty != 47 {
		t.Fatalf("qty = %v (%s), want 47", qty, why)
	}
}

// Property: loss at the stop plus slippage plus round-trip fees never exceeds the budget.
func TestRiskWithCostsNeverExceedsTheBudget(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	for i := 0; i < 5000; i++ {
		entry := 1 + r.Float64()*1000
		in := SizeInput{Equity: 10 + r.Float64()*1e6, RiskPct: 0.1 + r.Float64()*2, Entry: entry,
			Stop: entry * (1 - (0.1+r.Float64()*20)/100), SlippagePct: r.Float64() * 0.7, CommissionPct: r.Float64() * 0.2, QtyStep: 0.001, MinQty: 0.001}
		qty, _ := Size(in)
		cost := qty * worstLoss(in)
		if budget := in.Equity * in.RiskPct / 100; cost > budget*(1+1e-9) {
			t.Fatalf("case %d: cost %.6f exceeds budget %.6f (%+v)", i, cost, budget, in)
		}
	}
}

// worstLoss is the loss per unit when both fills pay the slippage (buy
// higher, stop sells lower) and both pay the commission: the way the fake
// and the backtest charge them.
func worstLoss(in SizeInput) float64 {
	a := in.SlippagePct / 100
	buy, sell := in.Entry*(1+a), in.Stop*(1-a)
	return buy - sell + (buy+sell)*in.CommissionPct/100
}

// FR-R1 hard cap: slippage counts on both legs, entry and stop.
func TestSizeCountsSlippageOnBothLegs(t *testing.T) {
	// a = 0.1%: buy 105.105, stop fill 100.6992, commission 0.1% on both:
	// 4.4058 + 0.2058042 = 4.6116042 per unit; 100 / 4.6116042 = 21.68
	in := SizeInput{Equity: 10_000, RiskPct: 1, Entry: 105, Stop: 100.8, SlippagePct: 0.1, CommissionPct: 0.1, QtyStep: 1, MinQty: 1}
	if qty, why := Size(in); qty != 21 {
		t.Fatalf("qty = %v (%s), want 21", qty, why)
	}
}

// A short mirrors the long: it sells the entry and buys back at the stop,
// costs against both legs.
func TestSizeIsSideAware(t *testing.T) {
	// a = 0.1%: sell 104.895 (105*0.999), buy back at 109.3092 (109.2*1.001):
	// 4.4142 + 0.2142042 commission = 4.6284042 per unit; 100 / 4.6284042 = 21.6
	in := SizeInput{Equity: 10_000, RiskPct: 1, Entry: 105, Stop: 109.2, Short: true, SlippagePct: 0.1, CommissionPct: 0.1, QtyStep: 1, MinQty: 1}
	if qty, why := Size(in); qty != 21 {
		t.Fatalf("short qty = %v (%s), want 21", qty, why)
	}
	in.Stop = 100 // a short's stop below the entry is on the wrong side
	if qty, _ := Size(in); qty != 0 {
		t.Fatalf("short with stop below entry sized %v, want 0", qty)
	}
	in.Short, in.Stop = false, 109.2 // and a long's stop above it
	if qty, _ := Size(in); qty != 0 {
		t.Fatalf("long with stop above entry sized %v, want 0", qty)
	}
}
