package indicators

import (
	"math"
	"testing"

	"github.com/rosaldo/agency-agents/quant-engine/market"
)

func closes(vs ...float64) []market.Candle {
	cs := make([]market.Candle, len(vs))
	for i, v := range vs {
		cs[i] = market.Candle{Open: v, High: v, Low: v, Close: v, Volume: 10}
	}
	return cs
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestSMA(t *testing.T) {
	if v, ok := SMA([]float64{1, 2, 3, 4, 5}); !ok || v != 3 {
		t.Fatalf("SMA(1..5) = %v %v, want 3", v, ok)
	}
	if _, ok := SMA(nil); ok {
		t.Fatal("SMA of nothing must be no data")
	}
}

func TestWindowKeepsTheLastN(t *testing.T) {
	w := NewWindow(3)
	if len(w.Candles()) != 0 {
		t.Fatal("new window must be empty")
	}
	for _, c := range closes(1, 2, 3, 4, 5) {
		w.Push(c)
	}
	got := w.Candles()
	if len(got) != 3 || got[0].Close != 3 || got[2].Close != 5 {
		t.Fatalf("window = %+v, want closes 3,4,5 oldest first", got)
	}
}

// The reference average covers the lookback candles before the last one, so
// the last candle does not dilute its own baseline.
func TestDropAndVolumeRatio(t *testing.T) {
	cs := closes(10, 10, 10, 9)
	cs[3].Volume = 40
	drop, ok := DropPct(cs, 3)
	if !ok || !near(drop, 10) {
		t.Fatalf("drop = %v %v, want 10", drop, ok)
	}
	vr, ok := VolRatio(cs, 3)
	if !ok || !near(vr, 4) {
		t.Fatalf("vol ratio = %v %v, want 4", vr, ok)
	}
	qv, ok := AvgQuoteVolume(cs, 3)
	if !ok || !near(qv, 100) {
		t.Fatalf("avg quote volume = %v %v, want 100", qv, ok)
	}
}

func TestNoDataBelowLookback(t *testing.T) {
	cs := closes(10, 10, 9)
	if _, ok := DropPct(cs, 3); ok {
		t.Error("drop with too few candles must be no data")
	}
	if _, ok := VolRatio(cs, 3); ok {
		t.Error("vol ratio with too few candles must be no data")
	}
	if _, ok := AvgQuoteVolume(cs, 3); ok {
		t.Error("quote volume with too few candles must be no data")
	}
	if _, ok := ATR(cs, 3); ok {
		t.Error("ATR with too few candles must be no data")
	}
}

func TestZeroAverageIsNoSignal(t *testing.T) {
	cs := closes(0, 0, 0, 0)
	for i := range cs {
		cs[i].Volume = 0
	}
	if _, ok := VolRatio(cs, 3); ok {
		t.Error("zero average volume must be no data, not a division by zero")
	}
	if _, ok := DropPct(cs, 3); ok {
		t.Error("zero average price must be no data")
	}
}

func TestATR(t *testing.T) {
	if v, ok := ATR(closes(5, 5, 5, 5), 3); !ok || v != 0 {
		t.Fatalf("ATR of a constant series = %v %v, want 0", v, ok)
	}
	cs := []market.Candle{
		{High: 10, Low: 9, Close: 10},
		{High: 12, Low: 10, Close: 11}, // TR 2
		{High: 11, Low: 8, Close: 9},   // TR 3
		{High: 13, Low: 12, Close: 12}, // TR max(1, |13-9|, |12-9|) = 4
	}
	if v, ok := ATR(cs, 3); !ok || !near(v, 3) {
		t.Fatalf("ATR = %v %v, want 3", v, ok)
	}
}
