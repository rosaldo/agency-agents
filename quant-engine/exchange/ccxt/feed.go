package ccxt

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	lib "github.com/rosaldo/ccxt/v5"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/market"
)

// Watcher is the part of a CCXT Pro client that streams candles: each call
// returns once the venue pushed an update of symbol's candle (the forming
// one, or a late update of an older one).
type Watcher interface {
	WatchOHLCV(symbol string, options ...lib.WatchOHLCVOptions) ([]lib.OHLCV, error)
}

// reconnectBackoff paces the retries of a dropped stream; a var for tests.
var reconnectBackoff = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second}

var _ exchange.CandleFeed = (*Adapter)(nil)

// FeedIntervals lists the venue's stream intervals; none without a stream.
func (a *Adapter) FeedIntervals() []time.Duration {
	if a.opt.Feed == nil {
		return nil
	}
	return a.opt.Venue.FeedIntervals
}

// Subscribe streams the boundaries of symbols' candles (exchange.CandleFeed):
// one watcher per symbol over the client's shared connection. A failed watch
// reports LinkDown once, retries with backoff, and the first update after it
// reports LinkUp. The stream is the public mainnet one at every level: the
// candles always come from mainnet.
func (a *Adapter) Subscribe(ctx context.Context, env exchange.Env, symbols []string, interval time.Duration) (<-chan exchange.Boundary, error) {
	if err := exchange.CheckEnv(a.opt.Paper, env); err != nil {
		return nil, err
	}
	if a.opt.Feed == nil {
		return nil, errors.New("ccxt: this venue has no candle stream")
	}
	if !slices.Contains(a.FeedIntervals(), interval) {
		return nil, errors.New("ccxt: the venue's candle stream does not carry interval " + interval.String())
	}
	tf, err := timeframe(interval)
	if err != nil {
		return nil, err
	}
	if p, ok := a.opt.Feed.(interface{ Prepare() error }); ok { // what the watchers would otherwise set up concurrently
		if err := p.Prepare(); err != nil {
			return nil, errors.New("ccxt: candle stream: " + err.Error())
		}
	}
	out := make(chan exchange.Boundary, 64)
	l := &link{out: out}
	var wg sync.WaitGroup
	for _, sym := range symbols {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.watch(ctx, l, sym, tf, interval)
		}()
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out, nil
}

// watch runs one symbol's stream until ctx ends.
func (a *Adapter) watch(ctx context.Context, l *link, sym, tf string, interval time.Duration) {
	var tr tracker
	fails := 0
	for ctx.Err() == nil {
		rows, err := a.opt.Feed.WatchOHLCV(sym, lib.WithWatchOHLCVTimeframe(tf))
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			l.down(ctx, err.Error())
			select {
			case <-time.After(reconnectBackoff[min(fails, len(reconnectBackoff)-1)]):
			case <-ctx.Done():
				return
			}
			fails++
			continue
		}
		fails = 0
		l.up(ctx)
		for _, r := range rows {
			if b, ok := tr.update(sym, candleOf(r), interval); ok {
				l.send(ctx, b)
			}
		}
	}
}

func candleOf(r lib.OHLCV) market.Candle {
	return market.Candle{Time: time.UnixMilli(r.Timestamp).UTC(), Open: r.Open, High: r.High, Low: r.Low, Close: r.Close, Volume: r.Volume}
}

// tracker turns one symbol's updates into boundaries: the first update of a
// later candle ends the one before it. An update of an older candle, which
// the venue may push after the new one began, is ignored: it is no value,
// and the engine reads the closed candle over REST anyway.
type tracker struct {
	cur market.Candle // last update of the newest candle seen
}

func (t *tracker) update(sym string, c market.Candle, interval time.Duration) (exchange.Boundary, bool) {
	switch {
	case t.cur.Time.IsZero(): // the first update tells no boundary: the candle may have begun long ago
		t.cur = c
		return exchange.Boundary{}, false
	case c.Time.Before(t.cur.Time):
		return exchange.Boundary{}, false
	case c.Time.Equal(t.cur.Time):
		t.cur = c
		return exchange.Boundary{}, false
	}
	b := exchange.Boundary{Symbol: sym, Closed: c.Time.Add(-interval)}
	if t.cur.Time.Equal(b.Closed) { // a minute with no trade has no update: then nothing of it to show
		b.Last = t.cur
	}
	t.cur = c
	return b, true
}

// link is the stream's state shared by the symbols' watchers: one LinkDown
// per drop, one LinkUp per recovery.
type link struct {
	mu     sync.Mutex
	out    chan exchange.Boundary
	isDown bool
}

func (l *link) setDown(down bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	changed := l.isDown != down
	l.isDown = down
	return changed
}

func (l *link) down(ctx context.Context, reason string) {
	if l.setDown(true) {
		l.send(ctx, exchange.Boundary{Link: exchange.LinkDown, Reason: reason})
	}
}

func (l *link) up(ctx context.Context) {
	if l.setDown(false) {
		l.send(ctx, exchange.Boundary{Link: exchange.LinkUp})
	}
}

func (l *link) send(ctx context.Context, b exchange.Boundary) {
	select {
	case l.out <- b:
	case <-ctx.Done():
	}
}
