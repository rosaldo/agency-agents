package risk

import (
	"errors"
	"testing"
	"time"
)

var mon = time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC) // a Monday

func TestDailyLimitHalts(t *testing.T) {
	b := &Breaker{DailyPct: 3, WeeklyPct: 6, Loc: time.UTC}
	for _, eq := range []float64{10_000, 9_900, 9_750} {
		if err := b.Allow(mon, eq); err != nil {
			t.Fatalf("equity %v halted too early: %v", eq, err)
		}
	}
	if err := b.Allow(mon.Add(time.Hour), 9_700); !errors.Is(err, ErrHalted) {
		t.Fatalf("3%% daily loss: err = %v, want ErrHalted", err)
	}
	if err := b.Allow(mon.Add(2*time.Hour), 10_500); !errors.Is(err, ErrHalted) {
		t.Fatal("a halt is final for this run, even if equity recovers")
	}
	if b.Halted() == "" {
		t.Fatal("halt reason missing")
	}
}

func TestNewDayResetsDailyButNotWeekly(t *testing.T) {
	b := &Breaker{DailyPct: 3, WeeklyPct: 6, Loc: time.UTC}
	steps := []struct {
		at     time.Time
		equity float64
		halt   bool
	}{
		{mon, 10_000, false},
		{mon.Add(5 * time.Hour), 9_750, false},  // day -2.5%
		{mon.Add(24 * time.Hour), 9_750, false}, // Tuesday opens at 9750
		{mon.Add(30 * time.Hour), 9_500, false}, // day -2.56%, week -5%
		{mon.Add(48 * time.Hour), 9_500, false}, // Wednesday
		{mon.Add(50 * time.Hour), 9_400, true},  // day -1.05%, week -6%
	}
	for i, s := range steps {
		err := b.Allow(s.at, s.equity)
		if (err != nil) != s.halt {
			t.Fatalf("step %d: err = %v, want halt %v", i, err, s.halt)
		}
	}
}

func TestNewWeekResetsWeekly(t *testing.T) {
	b := &Breaker{DailyPct: 3, WeeklyPct: 6, Loc: time.UTC}
	_ = b.Allow(mon, 10_000)
	_ = b.Allow(mon.Add(4*24*time.Hour), 9_500) // Friday: week -5%
	if err := b.Allow(mon.Add(7*24*time.Hour), 9_500); err != nil {
		t.Fatalf("next Monday must open a new week: %v", err)
	}
	if err := b.Allow(mon.Add(7*24*time.Hour+time.Hour), 9_250); err != nil { // old base: -7.5%; new week: -2.6%
		t.Fatalf("new week halted on the old base: %v", err)
	}
}

func TestBoundariesFollowTheTimezone(t *testing.T) {
	sp := time.FixedZone("UTC-3", -3*3600)
	b := &Breaker{DailyPct: 3, WeeklyPct: 6, Loc: sp}
	_ = b.Allow(time.Date(2026, 1, 6, 1, 0, 0, 0, time.UTC), 10_000) // Monday 22:00 local
	// 02:30 UTC is still Monday 23:30 local: same day, so -3% halts
	if err := b.Allow(time.Date(2026, 1, 6, 2, 30, 0, 0, time.UTC), 9_700); !errors.Is(err, ErrHalted) {
		t.Fatalf("local day not respected: %v", err)
	}
}

// A restarted run restores the bases and the halt: same day stays blocked,
// the next day clears a daily halt, only the next week clears a weekly one.
func TestRestoreSurvivesARestart(t *testing.T) {
	b := &Breaker{DailyPct: 3, WeeklyPct: 6, Loc: time.UTC}
	_ = b.Allow(mon, 10_000)
	if err := b.Allow(mon.Add(time.Hour), 9_600); !errors.Is(err, ErrHalted) {
		t.Fatal("setup: no daily halt")
	}
	restart := func() *Breaker {
		r := &Breaker{DailyPct: 3, WeeklyPct: 6, Loc: time.UTC}
		r.Restore(b.Snapshot())
		return r
	}
	if err := restart().Allow(mon.Add(2*time.Hour), 9_600); !errors.Is(err, ErrHalted) {
		t.Fatal("same-day restart lifted the daily halt")
	}
	if err := restart().Allow(mon.Add(2*time.Hour), 10_500); !errors.Is(err, ErrHalted) {
		t.Fatal("same-day restart lifted the halt on a recovered equity")
	}
	r := restart()
	if err := r.Allow(mon.Add(24*time.Hour), 9_600); err != nil {
		t.Fatalf("next day still blocked: %v", err)
	}
	if day, week := r.Opens(); day != 9_600 || week != 10_000 {
		t.Fatalf("bases after restart = %v/%v, want day 9600 (new day), week 10000 (kept)", day, week)
	}
}

func TestWeeklyHaltSurvivesTheDayBoundary(t *testing.T) {
	b := &Breaker{DailyPct: 3, WeeklyPct: 6, Loc: time.UTC}
	_ = b.Allow(mon, 10_000)
	_ = b.Allow(mon.Add(24*time.Hour), 9_750)
	_ = b.Allow(mon.Add(48*time.Hour), 9_500)
	if err := b.Allow(mon.Add(50*time.Hour), 9_400); !errors.Is(err, ErrHalted) {
		t.Fatal("setup: no weekly halt")
	}
	r := &Breaker{DailyPct: 3, WeeklyPct: 6, Loc: time.UTC}
	r.Restore(b.Snapshot())
	if err := r.Allow(mon.Add(72*time.Hour), 9_400); !errors.Is(err, ErrHalted) {
		t.Fatal("next day lifted a weekly halt")
	}
	if err := r.Allow(mon.Add(7*24*time.Hour), 9_400); err != nil {
		t.Fatalf("next week still blocked: %v", err)
	}
}

// Bases from another mode, or an equity that jumped outside the
// engine, are not reused; a halt is never lifted by either.
func TestRestoreForChecksModeAndEquity(t *testing.T) {
	now := time.Date(2026, 1, 7, 10, 0, 0, 0, time.UTC)
	paper := &Breaker{DailyPct: 3, WeeklyPct: 6, Loc: time.UTC}
	_ = paper.Allow(now, 100) // paper run with 100 of capital
	snap := paper.Snapshot()
	snap.Mode = "paper"

	live := &Breaker{DailyPct: 3, WeeklyPct: 6, Loc: time.UTC}
	if note := live.RestoreFor(snap, "mainnet", 10_000); note == "" {
		t.Error("a base from another mode must be replaced, and say so")
	}
	_ = live.Allow(now.Add(time.Hour), 10_000)
	if err := live.Allow(now.Add(2*time.Hour), 5_000); !errors.Is(err, ErrHalted) {
		t.Fatalf("live equity halved the same day: err = %v", err)
	}

	same := &Breaker{DailyPct: 3, WeeklyPct: 6, Loc: time.UTC}
	if note := same.RestoreFor(snap, "paper", 10_000); note == "" {
		t.Error("equity far above the last seen must re-base, and say so")
	}
	_ = same.Allow(now.Add(time.Hour), 10_000)
	if err := same.Allow(now.Add(2*time.Hour), 9_000); !errors.Is(err, ErrHalted) {
		t.Fatalf("10%% loss on the new base: err = %v", err)
	}

	halted := snap
	halted.Halt = "daily drawdown"
	h := &Breaker{DailyPct: 3, WeeklyPct: 6, Loc: time.UTC}
	h.RestoreFor(halted, "paper", 10_000)
	if err := h.Allow(now.Add(time.Hour), 10_000); !errors.Is(err, ErrHalted) {
		t.Fatal("a re-base lifted a halt")
	}
}

// A halt for a failed stop (Trip) is persisted like a daily halt.
func TestTripPersistsUntilTheNextDay(t *testing.T) {
	b := &Breaker{DailyPct: 3, WeeklyPct: 6, Loc: time.UTC}
	_ = b.Allow(mon, 10_000)
	b.Trip("stop for AAA failed")
	r := &Breaker{DailyPct: 3, WeeklyPct: 6, Loc: time.UTC}
	r.Restore(b.Snapshot())
	if err := r.Allow(mon.Add(time.Hour), 10_000); !errors.Is(err, ErrHalted) {
		t.Fatal("tripped halt lifted by a restart")
	}
	if err := r.Allow(mon.Add(24*time.Hour), 10_000); err != nil {
		t.Fatalf("next day still blocked: %v", err)
	}
}

// The fallback is 1/leverage minus the round trip; the stop must lie
// strictly between entry and liquidation, leaving the buffer free.
func TestStopBeforeLiquidation(t *testing.T) {
	if d := FallbackLiquidationDistance(4, RoundTripCost(0.05, 0.02, 0.04)); d != 0.25-0.002 {
		t.Fatalf("fallback %v, want 0.248", d)
	}
	for _, tc := range []struct {
		short           bool
		entry, stop, lq float64
		ok              bool
	}{
		{false, 100, 90, 80, true},   // 10 of 20, buffer 0.5 leaves 10
		{false, 100, 89, 80, false},  // 11 > 10
		{false, 100, 101, 80, false}, // stop above a long's entry
		{false, 100, 90, 101, false}, // liquidation above a long's entry
		{false, 100, 50, 0, true},    // 1x long: liquidation at 0
		{false, 100, 49, -5, false},  // below 0 counts as 0: 51 > 50
		{true, 100, 110, 120, true},
		{true, 100, 111, 120, false},
		{true, 100, 99, 120, false},
		{true, 100, 110, 95, false},
	} {
		err := StopBeforeLiquidation(tc.short, tc.entry, tc.stop, tc.lq, 0.5)
		if (err == nil) != tc.ok {
			t.Errorf("short=%v entry %g stop %g liq %g: err %v, want ok=%v", tc.short, tc.entry, tc.stop, tc.lq, err, tc.ok)
		}
	}
	if LiquidationPrice(false, 100, 0.25) != 75 || LiquidationPrice(true, 100, 0.25) != 125 {
		t.Fatal("liquidation price on the wrong side")
	}
}
