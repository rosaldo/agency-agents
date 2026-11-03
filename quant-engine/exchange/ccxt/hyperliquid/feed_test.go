package hyperliquid

import (
	"slices"
	"testing"
	"time"
)

// The stream pings before the venue's 60 s idle close, carries the 1m and
// 5m candles, and builds without touching the network.
func TestCandleStreamIsDialledWithAPingUnderAMinute(t *testing.T) {
	if feedKeepAlive <= 0 || feedKeepAlive >= time.Minute {
		t.Fatalf("keepAlive %v; want a ping before 60 s of silence", feedKeepAlive)
	}
	for _, iv := range []time.Duration{time.Minute, 5 * time.Minute} {
		if !slices.Contains(Venue.FeedIntervals, iv) {
			t.Fatalf("intervals %v miss %v", Venue.FeedIntervals, iv)
		}
	}
	if w, err := Venue.DialFeed([]string{"BTC/USDC:USDC", "XYZ-XYZ100/USDC:USDC"}); err != nil || w == nil {
		t.Fatalf("dial: %v %v", w, err)
	}
}
