package loop

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/paper"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/market"
)

// Clock is the wall clock the FeedTicker waits on; nil = the real one.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time                         { return time.Now() }
func (wallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// FeedTicker paces the loop on the venue's candle stream (feed: websocket):
// Next returns once every symbol, inputs included, reported the end of the
// candle being waited on, or at its close plus Grace, whichever is first: a
// minute without a trade sends nothing, and a dropped stream never makes the
// loop later than RealTicker. Closed is the candle the step then decides on;
// its value is read over REST.
type FeedTicker struct {
	Interval, Grace time.Duration
	Clock           Clock

	symbols    []string
	report     func(kind, reason string)
	onRestored func()
	ch         <-chan exchange.Boundary // followed inside Next, on the loop's goroutine

	mu     sync.Mutex
	seen   map[string]time.Time     // symbol -> open time of the newest candle the stream ended
	last   map[string]market.Candle // symbol -> the stream's last update of that candle (status only, never a value)
	down   bool
	closed time.Time // the candle the last Next woke for
}

func (t *FeedTicker) clock() Clock {
	if t.Clock == nil {
		return wallClock{}
	}
	return t.Clock
}

func (t *FeedTicker) Now() time.Time { return t.clock().Now() }

// Closed is the open time of the candle the current step decides on.
func (t *FeedTicker) Closed() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// FeedState is what status shows of the stream: its state and, per symbol,
// the stream's last update of the newest candle it ended (the decision used
// that candle as REST returned it).
type FeedState struct {
	State string                   `json:"state"` // ok | degraded
	Last  map[string]market.Candle `json:"last,omitempty"`
}

// state is false before the subscription (and on a nil ticker: feed: rest).
func (t *FeedTicker) state() (FeedState, bool) {
	if t == nil {
		return FeedState{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seen == nil {
		return FeedState{}, false
	}
	st := FeedState{State: "ok", Last: maps.Clone(t.last)}
	if t.down {
		st.State = "degraded"
	}
	return st, true
}

// candleFeed is ex's stream port: the paper simulator streams its data
// adapter's public candles.
func candleFeed(ex exchange.Exchange) (exchange.CandleFeed, bool) {
	if sim, ok := ex.(*paper.Sim); ok {
		ex = sim.Data()
	}
	f, ok := ex.(exchange.CandleFeed)
	return f, ok
}

// checkFeed refuses, at load, feed: websocket on an adapter without a candle
// stream or with an interval its stream does not carry, or without the
// FeedTicker to pace on it.
func checkFeed(cfg *config.Config, ex exchange.Exchange, tick Ticker) (exchange.CandleFeed, *FeedTicker, error) {
	if cfg.Feed != "websocket" {
		return nil, nil, nil
	}
	f, ok := candleFeed(ex)
	if !ok || len(f.FeedIntervals()) == 0 {
		return nil, nil, errors.New("feed: websocket needs an adapter with a candle stream; this one has none (use feed: rest)")
	}
	if !slices.Contains(f.FeedIntervals(), cfg.Interval()) {
		return nil, nil, fmt.Errorf("feed: websocket: the venue's candle stream does not carry interval %s (use feed: rest or one of %v)",
			cfg.IntervalSpec, f.FeedIntervals())
	}
	ft, ok := tick.(*FeedTicker)
	if !ok {
		return nil, nil, errors.New("feed: websocket needs the stream's ticker")
	}
	return f, ft, nil
}

// start subscribes; Next follows the stream. report journals the link's
// changes; onRestored runs when the stream comes back (what closed meanwhile
// is read over REST again).
func (t *FeedTicker) start(ctx context.Context, f exchange.CandleFeed, env exchange.Env, symbols []string,
	report func(kind, reason string), onRestored func()) error {
	ch, err := f.Subscribe(ctx, env, symbols, t.Interval)
	if err != nil {
		return err
	}
	t.symbols, t.report, t.onRestored, t.ch = symbols, report, onRestored, ch
	t.mu.Lock()
	t.seen, t.last = map[string]time.Time{}, map[string]market.Candle{}
	t.closed = t.Now().Truncate(t.Interval).Add(-t.Interval) // the first step decides on the last closed candle
	t.mu.Unlock()
	return nil
}

func (t *FeedTicker) follow(b exchange.Boundary) {
	t.mu.Lock()
	var kind, reason string
	switch b.Link {
	case exchange.LinkDown:
		if !t.down {
			kind, reason = journal.FeedDegraded, "candle stream down ("+b.Reason+"); stepping at each boundary plus the grace"
		}
		t.down = true
	case exchange.LinkUp:
		if t.down {
			kind, reason = journal.FeedRestored, "candle stream back; the candle windows are read again in full over REST"
		}
		t.down = false
	default:
		if b.Closed.After(t.seen[b.Symbol]) {
			t.seen[b.Symbol] = b.Closed
			if !b.Last.Time.IsZero() {
				t.last[b.Symbol] = b.Last
			}
		}
	}
	t.mu.Unlock()
	// outside the lock: the report saves the state, which reads this ticker
	if kind == journal.FeedRestored && t.onRestored != nil {
		t.onRestored()
	}
	if kind != "" && t.report != nil {
		t.report(kind, reason)
	}
}

// ended: every symbol reported the end of candle due.
func (t *FeedTicker) ended(due time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.symbols {
		if t.seen[s].Before(due) {
			return false
		}
	}
	return true
}

// Next waits for the end of the candle after the last one stepped on.
func (t *FeedTicker) Next(ctx context.Context) error {
	c := t.clock()
	t.mu.Lock()
	due := c.Now().Truncate(t.Interval)
	if !due.After(t.closed) {
		due = t.closed.Add(t.Interval)
	}
	t.mu.Unlock()
	end := due.Add(t.Interval) // its close
	deadline := end.Add(t.Grace)
	for {
		now := c.Now()
		ended := t.ended(due)
		// never before the local close: a clock a little behind the venue
		// would not list the candle among REST's closed ones yet
		if ended && !now.Before(end) || !now.Before(deadline) {
			t.mu.Lock()
			t.closed = due
			t.mu.Unlock()
			return nil
		}
		wait := deadline.Sub(now)
		if ended {
			wait = end.Sub(now)
		}
		select {
		case <-c.After(wait):
		case b, ok := <-t.ch:
			if !ok { // ctx ended
				t.ch = nil
				continue
			}
			t.follow(b)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
