package engine

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/market"
)

// CandleBuffer keeps the last candles of each symbol (and input) between
// steps, so a step reads over REST only the candles closed since the last
// one; it holds what a full read of the same window returns. Reset makes the
// next read fill the whole window again (start, and after the stream came
// back: candles may have closed meanwhile).
type CandleBuffer struct {
	mu     sync.Mutex
	gen    int
	series map[string][]market.Candle
}

// Reset drops every series: the next read is a full one.
func (b *CandleBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gen++
	b.series = nil
}

// read returns sym's last n closed candles, want being the open time of the
// newest one the step expects.
func (b *CandleBuffer) read(ctx context.Context, ex exchange.Exchange, env exchange.Env, sym string, interval time.Duration, n int, want time.Time) ([]market.Candle, error) {
	b.mu.Lock()
	gen, s := b.gen, b.series[sym]
	b.mu.Unlock()
	k := n
	if len(s) == n {
		if gap := int(want.Sub(s[n-1].Time) / interval); gap >= 0 && gap < n {
			k = gap
		}
	}
	if k == 0 {
		return slices.Clone(s), nil
	}
	got, err := ex.Candles(ctx, env, sym, interval, k)
	if err != nil {
		return nil, err
	}
	if k < n { // the new ones only: a candle not newer than the buffer's last is already in it
		next := slices.Clone(s)
		for _, c := range got {
			if c.Time.After(next[len(next)-1].Time) {
				next = append(next, c)
			}
		}
		got = next[len(next)-n:]
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.gen == gen { // a Reset meanwhile: the next read fills again
		if b.series == nil {
			b.series = map[string][]market.Candle{}
		}
		b.series[sym] = got
	}
	return slices.Clone(got), nil
}
