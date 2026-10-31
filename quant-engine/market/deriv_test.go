package market

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// The snapshot of a candle is the latest one read after its open and at or
// before its close: one read exactly at a candle's close belongs to it, one
// read a nanosecond later to the next candle (off-by-one), and none read
// after the last close is ever used (future leak).
func TestAlignDerivTakesTheLatestSnapshotAtOrBeforeTheClose(t *testing.T) {
	iv := time.Minute
	cs := []Candle{{Time: t0}, {Time: t0.Add(iv)}, {Time: t0.Add(2 * iv)}, {Time: t0.Add(3 * iv)}}
	snap := func(at time.Duration, oi float64) Deriv { return Deriv{Time: t0.Add(at), OpenInterest: oi} }
	ds := []Deriv{
		snap(-time.Second, 1),       // before the first candle opened: none
		snap(10*time.Second, 2),     // candle 0
		snap(iv, 3),                 // exactly at candle 0's close: candle 0, the latest
		snap(iv+time.Nanosecond, 4), // candle 1
		// candle 2: none
		snap(3*iv+5*time.Second, 5), // candle 3
		snap(4*iv+time.Second, 6),   // after the last close: never
	}
	got := AlignDeriv(cs, ds, iv)
	want := []float64{3, 4, 0, 5}
	if len(got) != len(cs) {
		t.Fatalf("len = %d, want one per candle (%d)", len(got), len(cs))
	}
	for i, d := range got {
		if d.OpenInterest != want[i] {
			t.Fatalf("candle %d: open interest %v, want %v (all: %+v)", i, d.OpenInterest, want[i], got)
		}
		if want[i] == 0 != d.Missing() {
			t.Fatalf("candle %d: missing = %v", i, d.Missing())
		}
		if !d.Missing() && d.Time.After(cs[i].Time.Add(iv)) {
			t.Fatalf("candle %d: snapshot at %v is after the close %v", i, d.Time, cs[i].Time.Add(iv))
		}
	}
}

func TestDerivCSVRoundTrip(t *testing.T) {
	a := Deriv{Time: t0.Add(1500 * time.Millisecond), OpenInterest: 12345.5, FundingRate: -0.0000125, MarkPrice: 101.25}
	b := Deriv{Time: t0.Add(time.Minute), OpenInterest: 12000, FundingRate: 0.0001}
	file := DerivCSVHeader + "\n" + DerivCSVRow("BTC/USDC:USDC", a) + DerivCSVRow("XYZ-XYZ100/USDC:USDC", b) + DerivCSVRow("BTC/USDC:USDC", b)
	got, err := ReadDerivCSV(strings.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	if bt := got["BTC/USDC:USDC"]; len(bt) != 2 || !same(bt[0], a) || !same(bt[1], b) {
		t.Fatalf("BTC = %+v", bt)
	}
	if x := got["XYZ-XYZ100/USDC:USDC"]; len(x) != 1 || !same(x[0], b) {
		t.Fatalf("XYZ = %+v", x)
	}
	if got, err := ReadDerivCSV(strings.NewReader("")); err != nil || len(got) != 0 {
		t.Fatalf("an empty file = %v, %v; want no rows", got, err)
	}
	row := func(at string) string { return at + ",AAA,1,0,1\n" }
	for name, bad := range map[string]string{
		"header": "time,open,high,low,close,volume\n",
		"time":   DerivCSVHeader + "\n" + row("yesterday"),
		"number": DerivCSVHeader + "\n2026-01-01T00:00:00Z,AAA,x,0,1\n",
		"order":  DerivCSVHeader + "\n" + row("2026-01-01T00:01:00Z") + row("2026-01-01T00:00:00Z"),
		"repeat": DerivCSVHeader + "\n" + row("2026-01-01T00:00:00Z") + row("2026-01-01T00:00:00Z"),
		"short":  DerivCSVHeader + "\n2026-01-01T00:00:00Z,AAA,1\n",
	} {
		if _, err := ReadDerivCSV(strings.NewReader(bad)); err == nil {
			t.Errorf("%s: accepted a bad file", name)
		}
	}
}

func same(a, b Deriv) bool {
	return a.Time.Equal(b.Time) && a.OpenInterest == b.OpenInterest && a.FundingRate == b.FundingRate && a.MarkPrice == b.MarkPrice
}
