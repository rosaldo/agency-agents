package loop

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

const feedYAML = `
exchange: fake
asset_class: crypto
interval: 1m
feed: websocket
symbols: [ETH]
risk: {confirmed_by_user: true}
`

// The ETH 16:22 candle of testdata/hyperliquid_candle_ws.jsonl: the
// stream's last pushed state closes at 2250.7, REST's final candle at 2250.8.
var (
	eth1622   = time.Date(2026, 10, 31, 16, 22, 0, 0, time.UTC)
	ethWSLast = market.Candle{Time: eth1622, Open: 2250.7, High: 2250.7, Low: 2250.7, Close: 2250.7, Volume: 4.6282}
)

// ethMinutes are 1m candles from 16:20; the one at 16:22 is REST's final one.
func ethMinutes() []market.Candle {
	var cs []market.Candle
	for i, c := range []float64{2249.9, 2250.1, 2250.8, 2250.5, 2250.6, 2250.9, 2251.2, 2251.0} {
		cs = append(cs, market.Candle{Time: eth1622.Add(time.Duration(i-2) * time.Minute), Open: c, High: c, Low: c, Close: c, Volume: 4.6728})
	}
	return cs
}

// decided is what one Decide saw: the clock, the candle, and whether its
// window is the one a full REST read gives at that moment.
type decided struct {
	at, candle time.Time
	close      float64
	whole      bool
}

type feedRun struct {
	ff     *FakeFeed
	dir    string
	reason string
	seen   []decided
}

// runFeed runs the loop on feed: websocket over cs, the stream scripted by
// minute, the strategy never trading.
func runFeed(t *testing.T, cs []market.Candle, minute func(int) Minute) *feedRun {
	t.Helper()
	r := &feedRun{dir: filepath.Join(t.TempDir(), ".scripted")}
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 0.001, MinQty: 0.001, Candles: map[string][]market.Candle{"ETH": cs}})
	var times []time.Time
	for _, c := range cs {
		times = append(times, c.Time)
	}
	r.ff = NewFakeFeed(f, times, []string{"ETH"}, time.Minute, 700*time.Millisecond)
	r.ff.Minute = minute
	s := &scripted.Strategy{Decider: func(in strategy.Input) strategy.Decision {
		full, _ := f.Candles(context.Background(), exchange.Demo, "ETH", time.Minute, len(in.Candles))
		last := in.Candles[len(in.Candles)-1]
		r.seen = append(r.seen, decided{r.ff.Now(), last.Time, last.Close, slices.Equal(full, in.Candles)})
		return strategy.Decision{}
	}}
	c, err := config.Parse([]byte(feedYAML), s)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.ff.OnEnd = cancel
	r.reason = Run(ctx, c, s, r.ff, exchange.Demo, &FeedTicker{Interval: time.Minute, Grace: 5 * time.Second, Clock: r.ff}, r.dir)
	return r
}

func (r *feedRun) events(t *testing.T, kind string) []journal.Event {
	t.Helper()
	evs, _, err := journal.Read(filepath.Join(r.dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []journal.Event
	for _, e := range evs {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func (r *feedRun) at(candle time.Time) (decided, bool) {
	for _, d := range r.seen {
		if d.candle.Equal(candle) {
			return d, true
		}
	}
	return decided{}, false
}

// The stream's first update of the new candle wakes the step (0.7 s after
// the close, not the 5 s grace), and the value decided on is REST's candle,
// even when the stream's last update of it differs (ETH 16:22: 2250.7 pushed,
// 2250.8 final). Status shows the feed and that last pushed state.
func TestStreamWakesTheStepAndRESTGivesTheValue(t *testing.T) {
	r := runFeed(t, ethMinutes(), func(i int) Minute {
		if i == 2 {
			return Minute{Last: map[string]market.Candle{"ETH": ethWSLast}}
		}
		return Minute{}
	})
	d, ok := r.at(eth1622)
	if !ok {
		t.Fatalf("no decision on 16:22: %+v (%s)", r.seen, r.reason)
	}
	if d.close != 2250.8 || !d.whole {
		t.Fatalf("decided on close %v (window = a full REST read: %v); want REST's 2250.8, not the stream's 2250.7", d.close, d.whole)
	}
	if want := eth1622.Add(time.Minute + 700*time.Millisecond); !d.at.Equal(want) {
		t.Fatalf("stepped at %s; want %s, on the stream", d.at, want)
	}
	if len(r.seen) != len(ethMinutes()) {
		t.Fatalf("%d decisions over %d candles", len(r.seen), len(ethMinutes()))
	}
	st, err := ReadStatus(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Feed != "websocket" || st.FeedState == nil || st.FeedState.State != "ok" || st.FeedState.Last["ETH"].Time.IsZero() {
		t.Fatalf("status feed %q state %+v; want websocket, ok, the stream's last candle", st.Feed, st.FeedState)
	}
}

// A minute without a trade sends nothing: the step runs at the close plus
// the grace, as with feed: rest, on that candle.
func TestSilentMinuteStepsAtTheDeadline(t *testing.T) {
	r := runFeed(t, ethMinutes(), func(i int) Minute {
		if i == 3 {
			return Minute{Silent: []string{"ETH"}}
		}
		return Minute{}
	})
	c := ethMinutes()[3]
	d, ok := r.at(c.Time)
	if want := c.Time.Add(time.Minute + 5*time.Second); !ok || !d.at.Equal(want) || !d.whole {
		t.Fatalf("decision %+v (found %v); want it on %s at %s", d, ok, c.Time, want)
	}
}

// REST does not list the candle the stream ended yet: no decision on that
// symbol (candle_not_final), and the next step decides as usual.
func TestRESTCandleOtherThanTheStreamsIsNotDecidedOn(t *testing.T) {
	r := runFeed(t, ethMinutes(), func(i int) Minute { return Minute{Stale: i == 3} })
	nf := r.events(t, journal.CandleNotFinal)
	if len(nf) != 1 || nf[0].Symbol != "ETH" || !strings.Contains(nf[0].Reason, "16:22:00Z") || !strings.Contains(nf[0].Reason, "16:23:00Z") {
		t.Fatalf("candle_not_final events %+v; want one naming REST's 16:22 and the stream's 16:23", nf)
	}
	for _, d := range r.seen {
		if !d.whole {
			t.Fatalf("decision %+v on a window other than REST's", d)
		}
	}
	if _, ok := r.at(ethMinutes()[3].Time); ok {
		t.Fatalf("decided on 16:23 although REST did not list it in its step: %+v", r.seen)
	}
	if _, ok := r.at(ethMinutes()[4].Time); !ok {
		t.Fatalf("no decision on 16:24 after the late candle: %+v", r.seen)
	}
}

// The stream drops: feed_degraded, the steps run at the deadline; it comes
// back: feed_restored, and the windows are read again in full, so the
// candles closed meanwhile are neither doubled nor skipped.
func TestDroppedStreamStepsAtTheDeadlineAndRefillsOnReturn(t *testing.T) {
	r := runFeed(t, ethMinutes(), func(i int) Minute {
		switch i {
		case 2:
			return Minute{Down: "read: connection reset", Silent: []string{"ETH"}}
		case 3:
			return Minute{Silent: []string{"ETH"}}
		case 4:
			return Minute{Up: true}
		}
		return Minute{}
	})
	if n, m := len(r.events(t, journal.FeedDegraded)), len(r.events(t, journal.FeedRestored)); n != 1 || m != 1 {
		t.Fatalf("feed_degraded %d, feed_restored %d; want one each", n, m)
	}
	cs := ethMinutes()
	for i, d := range r.seen {
		if !d.whole || !d.candle.Equal(cs[i].Time) {
			t.Fatalf("decision %d %+v; want every candle once, in order, on REST's window", i, d)
		}
	}
	// warmup 2: the first two steps read the whole window (one candle exists
	// at start, short of it), so does the one after the return; every other
	// step reads the one candle closed since
	if got, want := r.ff.Reads(), []int{2, 2, 1, 1, 2, 1, 1, 1}; !slices.Equal(got, want) {
		t.Fatalf("REST reads of %v candles; want %v", got, want)
	}
	for _, i := range []int{2, 3} {
		if d, _ := r.at(cs[i].Time); !d.at.Equal(cs[i].Time.Add(time.Minute + 5*time.Second)) {
			t.Fatalf("decision on %s at %s; want the deadline while the stream is down", cs[i].Time, d.at)
		}
	}
}

// feed: websocket on an adapter without a stream, or on an interval its
// stream does not carry, is refused before anything runs.
func TestWebsocketFeedIsRefusedWithoutAFittingStream(t *testing.T) {
	s := &scripted.Strategy{Decider: func(strategy.Input) strategy.Decision { return strategy.Decision{} }}
	cs := ethMinutes()
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, Candles: map[string][]market.Candle{"ETH": cs}})
	c, err := config.Parse([]byte(feedYAML), s)
	if err != nil {
		t.Fatal(err)
	}
	ft := &FeedTicker{Interval: time.Minute, Grace: 5 * time.Second}
	if r := Run(context.Background(), c, s, f, exchange.Demo, ft, t.TempDir()); !strings.HasPrefix(r, "refused: feed: websocket needs an adapter with a candle stream") {
		t.Fatalf("no stream: %q", r)
	}
	c5, err := config.Parse([]byte(strings.Replace(feedYAML, "interval: 1m", "interval: 5m", 1)), s)
	if err != nil {
		t.Fatal(err)
	}
	ff := NewFakeFeed(f, []time.Time{cs[0].Time}, []string{"ETH"}, time.Minute, 0)
	if r := Run(context.Background(), c5, s, ff, exchange.Demo, ft, t.TempDir()); !strings.Contains(r, "does not carry interval 5m") {
		t.Fatalf("5m on a 1m stream: %q", r)
	}
	if len(s.Log) != 0 {
		t.Fatalf("decided %d times before the refusal", len(s.Log))
	}
}
