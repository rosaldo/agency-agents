package loop

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
)

// schedClock is a Clock whose After(d) advances the time by d unless a
// boundary is scheduled earlier: then it jumps to that time and delivers the
// boundary on ch.
type schedClock struct {
	mu    sync.Mutex
	now   time.Time
	sched []schedEvent
	ch    chan exchange.Boundary
}

type schedEvent struct {
	at time.Time
	b  exchange.Boundary
}

func (c *schedClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *schedClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.now.Add(d)
	if len(c.sched) > 0 && !c.sched[0].at.After(t) {
		c.now = c.sched[0].at
		c.ch <- c.sched[0].b
		c.sched = c.sched[1:]
		return nil
	}
	c.now = t
	out := make(chan time.Time, 1)
	out <- t
	return out
}

type chanFeed struct{ ch chan exchange.Boundary }

func (f chanFeed) FeedIntervals() []time.Duration { return []time.Duration{time.Minute} }
func (f chanFeed) Subscribe(context.Context, exchange.Env, []string, time.Duration) (<-chan exchange.Boundary, error) {
	return f.ch, nil
}

type step struct{ at, closed time.Time }

// steps runs n Next calls of a 1m FeedTicker (5 s grace) on ETH over the
// scheduled boundaries, the clock starting at start.
func feedSteps(t *testing.T, start time.Time, sched []schedEvent, n int) []step {
	t.Helper()
	ch := make(chan exchange.Boundary, 16)
	c := &schedClock{now: start, sched: sched, ch: ch}
	ft := &FeedTicker{Interval: time.Minute, Grace: 5 * time.Second, Clock: c}
	if err := ft.start(context.Background(), chanFeed{ch}, exchange.Demo, []string{"ETH"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	var got []step
	for range n {
		if err := ft.Next(context.Background()); err != nil {
			t.Fatal(err)
		}
		got = append(got, step{c.Now(), ft.Closed()})
	}
	return got
}

func ended(at, open time.Time) schedEvent {
	return schedEvent{at, exchange.Boundary{Symbol: "ETH", Closed: open}}
}

func wantSteps(t *testing.T, got, want []step) {
	t.Helper()
	for i := range want {
		if !got[i].at.Equal(want[i].at) || !got[i].closed.Equal(want[i].closed) {
			t.Fatalf("steps %v; want %v", got, want)
		}
	}
}

var t1623 = time.Date(2026, 10, 31, 16, 23, 0, 0, time.UTC)

// A boundary of candle k arriving after k's deadline already stepped must
// not wake an extra or early step: one step per candle, never before its close.
func TestLateBoundaryAfterTheDeadlineWakesNoExtraStep(t *testing.T) {
	t0 := t1623
	got := feedSteps(t, t0.Add(500*time.Millisecond), []schedEvent{
		ended(t0.Add(time.Minute+6*time.Second), t0), // 16:23 ended, after its deadline 16:24:05
		ended(t0.Add(2*time.Minute+700*time.Millisecond), t0.Add(time.Minute)),
		ended(t0.Add(3*time.Minute+700*time.Millisecond), t0.Add(2*time.Minute)),
	}, 3)
	wantSteps(t, got, []step{
		{t0.Add(time.Minute + 5*time.Second), t0},                               // 16:23 at its deadline
		{t0.Add(2*time.Minute + 700*time.Millisecond), t0.Add(time.Minute)},     // 16:24 on its boundary
		{t0.Add(3*time.Minute + 700*time.Millisecond), t0.Add(2 * time.Minute)}, // 16:25 on its boundary
	})
}

// The stream's boundary and the deadline at the same instant: one step.
func TestBoundaryAtTheDeadlineInstantIsOneStep(t *testing.T) {
	t0 := t1623
	got := feedSteps(t, t0.Add(500*time.Millisecond), []schedEvent{
		ended(t0.Add(time.Minute+5*time.Second), t0),
		ended(t0.Add(2*time.Minute+time.Second), t0.Add(time.Minute)),
	}, 2)
	wantSteps(t, got, []step{
		{t0.Add(time.Minute + 5*time.Second), t0},
		{t0.Add(2*time.Minute + time.Second), t0.Add(time.Minute)},
	})
}

// A boundary that arrives before the local close (the local clock a little
// behind the venue) waits for the close: REST would not list the candle yet.
func TestBoundaryBeforeTheLocalCloseWaitsForTheClose(t *testing.T) {
	t0 := t1623
	got := feedSteps(t, t0.Add(500*time.Millisecond), []schedEvent{
		ended(t0.Add(59500*time.Millisecond), t0),
	}, 1)
	wantSteps(t, got, []step{{t0.Add(time.Minute), t0}})
}
