package engine

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/market"
)

// sizes records the n of every Candles read.
type sizes struct {
	exchange.Exchange
	ns []int
}

func (s *sizes) Candles(ctx context.Context, env exchange.Env, sym string, iv time.Duration, n int) ([]market.Candle, error) {
	s.ns = append(s.ns, n)
	return s.Exchange.Candles(ctx, env, sym, iv, n)
}

func minutes(n int) []market.Candle {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for i := range n {
		x := 100 + float64(i)
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(i) * time.Minute), Open: x, High: x, Low: x, Close: x, Volume: 1})
	}
	return cs
}

// The buffer fills the window over REST once, then reads only the candles
// closed since; after a Reset (the stream came back) it fills it again. Every
// read equals a full read of the same window: nothing doubled, nothing skipped.
func TestBufferReadsOnlyTheNewCandlesAndRefillsAfterAReset(t *testing.T) {
	cs := minutes(20)
	f := fake.New(fake.Options{Paper: true, Cash: 1, Candles: map[string][]market.Candle{"AAA": cs}})
	for range 5 {
		f.Advance()
	}
	ex := &sizes{Exchange: f}
	var b CandleBuffer
	at := 5
	check := func(want []int) {
		t.Helper()
		got, err := b.read(context.Background(), ex, exchange.Demo, "AAA", time.Minute, 4, cs[at].Time)
		if err != nil {
			t.Fatal(err)
		}
		full, _ := f.Candles(context.Background(), exchange.Demo, "AAA", time.Minute, 4)
		if !slices.Equal(got, full) {
			t.Fatalf("at %d: buffer %v, full read %v", at, got, full)
		}
		if !slices.Equal(ex.ns, want) {
			t.Fatalf("at %d: reads of %v, want %v", at, ex.ns, want)
		}
	}
	check([]int{4}) // start: the whole window
	f.Advance()
	at++
	check([]int{4, 1}) // one candle closed
	f.Advance()
	f.Advance()
	at += 2
	check([]int{4, 1, 2}) // a step missed: the two closed since
	b.Reset()
	for range 3 { // the stream was down meanwhile
		f.Advance()
		at++
	}
	check([]int{4, 1, 2, 4}) // whole window again
	check([]int{4, 1, 2, 4}) // nothing new: no read
	for range 6 {
		f.Advance()
		at++
	}
	check([]int{4, 1, 2, 4, 4}) // more than a window closed: a full read
}
