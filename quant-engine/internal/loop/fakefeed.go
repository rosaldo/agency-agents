package loop

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/market"
)

// FakeFeed is the fake exchange with a candle stream, and the clock a
// FeedTicker waits on, for tests: each wait of a new candle's length closes
// the next of Times on the fake exchange and streams its boundary for every
// symbol, Latency after the close; a shorter wait (the stream left a symbol
// silent) jumps the clock to its end, the deadline.
type FakeFeed struct {
	*fake.Fake
	Times    []time.Time // open times of the candles, in order; Times[0] is closed at start
	Symbols  []string
	Interval time.Duration
	Latency  time.Duration
	Minute   func(i int) Minute // nil: every symbol signals every candle
	OnEnd    func()             // Times ran out

	mu    sync.Mutex
	reads []int // n of every Candles read
	now   time.Time
	i     int
	lag   int // candles the stream ended that the fake exchange has not closed yet
	ch    chan exchange.Boundary
}

// Minute scripts what the stream does around the close of Times[i].
type Minute struct {
	Silent []string                 // symbols that send nothing (no trade that minute)
	Last   map[string]market.Candle // the stream's last update of the candle, per symbol
	Stale  bool                     // the boundary arrives while REST does not list the candle yet
	Down   string                   // non-empty: the link drops first, with this reason
	Up     bool                     // the link comes back first
}

// NewFakeFeed starts the clock Latency after the close of Times[0].
func NewFakeFeed(f *fake.Fake, times []time.Time, symbols []string, interval, latency time.Duration) *FakeFeed {
	return &FakeFeed{Fake: f, Times: times, Symbols: symbols, Interval: interval, Latency: latency,
		now: times[0].Add(interval + latency), ch: make(chan exchange.Boundary, 1024)}
}

// Candles records how many candles each REST read asks for.
func (f *FakeFeed) Candles(ctx context.Context, env exchange.Env, symbol string, interval time.Duration, n int) ([]market.Candle, error) {
	f.mu.Lock()
	f.reads = append(f.reads, n)
	f.mu.Unlock()
	return f.Fake.Candles(ctx, env, symbol, interval, n)
}

// Reads is the n of every Candles read so far.
func (f *FakeFeed) Reads() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reads)
}

func (f *FakeFeed) FeedIntervals() []time.Duration { return []time.Duration{f.Interval} }

func (f *FakeFeed) Subscribe(context.Context, exchange.Env, []string, time.Duration) (<-chan exchange.Boundary, error) {
	return f.ch, nil
}

func (f *FakeFeed) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

var fired = func() chan time.Time { c := make(chan time.Time); close(c); return c }()

func (f *FakeFeed) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ch) > 0 { // the ticker has boundaries to read first
		return nil
	}
	if d < f.Interval { // the rest of this candle's wait: its deadline
		f.now = f.now.Add(d)
		return fired
	}
	if f.i+1 >= len(f.Times) {
		if f.OnEnd != nil {
			f.OnEnd()
		}
		return nil
	}
	f.i++
	var m Minute
	if f.Minute != nil {
		m = f.Minute(f.i)
	}
	if m.Stale {
		f.lag++
	} else {
		for range 1 + f.lag {
			f.Fake.Advance()
		}
		f.lag = 0
	}
	start := f.now
	f.now = f.Times[f.i].Add(f.Interval + f.Latency)
	if m.Down != "" {
		f.ch <- exchange.Boundary{Link: exchange.LinkDown, Reason: m.Down}
	}
	if m.Up {
		f.ch <- exchange.Boundary{Link: exchange.LinkUp}
	}
	for _, s := range f.Symbols {
		if !slices.Contains(m.Silent, s) {
			f.ch <- exchange.Boundary{Symbol: s, Closed: f.Times[f.i], Last: m.Last[s]}
		}
	}
	if len(f.ch) == 0 { // nothing streamed: the wait runs to its deadline
		f.now = start.Add(d)
		return fired
	}
	return nil
}
