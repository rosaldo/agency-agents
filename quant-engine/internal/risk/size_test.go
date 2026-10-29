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
	in.SlippagePct = 0.1 // 0.05 per unit: 100 / 2.05 = 48.78
	if qty, _ := Size(in); qty != 48 {
		t.Fatalf("qty with slippage = %v, want 48", qty)
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
