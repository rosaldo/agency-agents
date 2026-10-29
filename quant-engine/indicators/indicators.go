// Package indicators computes price and volume statistics over closed candles.
// Every function reports ok=false instead of dividing by zero or reading past
// the data it was given.
package indicators

import (
	"math"

	"github.com/rosaldo/agency-agents/quant-engine/market"
)

// Window is a fixed-size ring of the most recent candles.
type Window struct {
	buf  []market.Candle
	next int
	full bool
}

func NewWindow(size int) *Window { return &Window{buf: make([]market.Candle, size)} }

func (w *Window) Push(c market.Candle) {
	w.buf[w.next] = c
	w.next = (w.next + 1) % len(w.buf)
	w.full = w.full || w.next == 0
}

// Candles returns the window's candles, oldest first.
func (w *Window) Candles() []market.Candle {
	if !w.full {
		return append([]market.Candle(nil), w.buf[:w.next]...)
	}
	return append(append([]market.Candle(nil), w.buf[w.next:]...), w.buf[:w.next]...)
}

func SMA(xs []float64) (float64, bool) {
	if len(xs) == 0 {
		return 0, false
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs)), true
}

// reference returns the lookback candles before the last one: the baseline
// the last candle is compared against.
func reference(cs []market.Candle, lookback int) ([]market.Candle, bool) {
	if lookback < 1 || len(cs) < lookback+1 {
		return nil, false
	}
	return cs[len(cs)-1-lookback : len(cs)-1], true
}

func avg(cs []market.Candle, lookback int, f func(market.Candle) float64) (float64, bool) {
	ref, ok := reference(cs, lookback)
	if !ok {
		return 0, false
	}
	xs := make([]float64, len(ref))
	for i, c := range ref {
		xs[i] = f(c)
	}
	return SMA(xs)
}

// DropPct is how far, in percent, the last close sits below the reference
// average close.
func DropPct(cs []market.Candle, lookback int) (float64, bool) {
	a, ok := avg(cs, lookback, func(c market.Candle) float64 { return c.Close })
	if !ok || a <= 0 {
		return 0, false
	}
	return (a - cs[len(cs)-1].Close) / a * 100, true
}

// AvgClose is the reference average close.
func AvgClose(cs []market.Candle, lookback int) (float64, bool) {
	return avg(cs, lookback, func(c market.Candle) float64 { return c.Close })
}

// VolRatio is the last volume over the reference average volume.
func VolRatio(cs []market.Candle, lookback int) (float64, bool) {
	a, ok := avg(cs, lookback, func(c market.Candle) float64 { return c.Volume })
	if !ok || a <= 0 {
		return 0, false
	}
	return cs[len(cs)-1].Volume / a, true
}

// AvgQuoteVolume is the reference average of close x volume per candle.
func AvgQuoteVolume(cs []market.Candle, lookback int) (float64, bool) {
	return avg(cs, lookback, func(c market.Candle) float64 { return c.Close * c.Volume })
}

// ATR is the simple mean of the last `periods` true ranges.
func ATR(cs []market.Candle, periods int) (float64, bool) {
	if periods < 1 || len(cs) < periods+1 {
		return 0, false
	}
	trs := make([]float64, 0, periods)
	for i := len(cs) - periods; i < len(cs); i++ {
		c, prev := cs[i], cs[i-1].Close
		trs = append(trs, math.Max(c.High-c.Low, math.Max(math.Abs(c.High-prev), math.Abs(c.Low-prev))))
	}
	return SMA(trs)
}
