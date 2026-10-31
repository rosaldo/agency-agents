package market

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// A candle's snapshot is the one bound to it and read after its close,
// before the next candle closed (the step that decided on it). One read at
// or before the close, at the candle's open included, or in the next step,
// never counts for it.
func TestAlignDerivTakesTheSnapshotReadInTheCandlesStep(t *testing.T) {
	iv := time.Minute
	cs := []Candle{{Time: t0}, {Time: t0.Add(iv)}, {Time: t0.Add(2 * iv)}, {Time: t0.Add(3 * iv)}, {Time: t0.Add(4 * iv)}, {Time: t0.Add(5 * iv)}}
	snap := func(candle int, at time.Duration, oi float64) Deriv {
		return Deriv{Time: cs[candle].Time.Add(at), Candle: cs[candle].Time, OpenInterest: oi}
	}
	ds := []Deriv{
		snap(0, iv+5*time.Second, 1),   // candle 0: read 5s after its close
		snap(1, iv+time.Nanosecond, 2), // candle 1: just after the close
		snap(1, iv+9*time.Second, 3),   // candle 1 again: the latest wins
		snap(2, iv, 9),                 // candle 2: read exactly at the close, not after it
		snap(3, 0, 9),                  // candle 3: read at its open
		snap(4, 2*iv+time.Second, 9),   // candle 4: read in the next step
		snap(5, 2*iv, 6),               // candle 5: read as the next candle closed, still its step
	}
	got := AlignDeriv(cs, ds, iv)
	want := []float64{1, 3, 0, 0, 0, 6}
	if len(got) != len(cs) {
		t.Fatalf("len = %d, want one per candle (%d)", len(got), len(cs))
	}
	for i, d := range got {
		if d.OpenInterest != want[i] || d.Missing() != (want[i] == 0) {
			t.Fatalf("candle %d: %+v, want open interest %v (all: %+v)", i, d, want[i], got)
		}
	}
}

func TestDerivCSVRoundTrip(t *testing.T) {
	a := Deriv{Time: t0.Add(time.Minute + 1500*time.Millisecond), Candle: t0, OpenInterest: 12345.5, FundingRate: -0.0000125, MarkPrice: 101.25}
	b := Deriv{Time: t0.Add(2*time.Minute + time.Second), Candle: t0.Add(time.Minute), OpenInterest: 12000, FundingRate: 0.0001}
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
	row := func(at string) string { return at + ",AAA,2026-01-01T00:00:00Z,1,0,1\n" }
	for name, bad := range map[string]string{
		"header": "time,symbol,open_interest,funding_rate,mark_price\n",
		"time":   DerivCSVHeader + "\n" + row("yesterday"),
		"candle": DerivCSVHeader + "\n2026-01-01T00:01:00Z,AAA,later,1,0,1\n",
		"number": DerivCSVHeader + "\n2026-01-01T00:01:00Z,AAA,2026-01-01T00:00:00Z,x,0,1\n",
		"order":  DerivCSVHeader + "\n" + row("2026-01-01T00:02:00Z") + row("2026-01-01T00:01:00Z"),
		"repeat": DerivCSVHeader + "\n" + row("2026-01-01T00:01:00Z") + row("2026-01-01T00:01:00Z"),
		"torn":   DerivCSVHeader + "\n" + row("2026-01-01T00:01:00Z") + "2026-01-01T00:02:00Z,AAA,2026",
	} {
		if _, err := ReadDerivCSV(strings.NewReader(bad)); err == nil {
			t.Errorf("%s: accepted a bad file", name)
		}
	}
}

func same(a, b Deriv) bool {
	return a.Time.Equal(b.Time) && a.Candle.Equal(b.Candle) && a.OpenInterest == b.OpenInterest && a.FundingRate == b.FundingRate && a.MarkPrice == b.MarkPrice
}

// Hyperliquid fundingHistory pages, appended as fetched (each page repeats
// the previous one's last row): every candle gets the last rate settled at
// or before its close, as a snapshot read in its own step, with no open
// interest. A rate settled just after the close waits for the next candle,
// and a candle the history does not outlast gets none.
func TestReadFundingHistoryBindsTheRateSettledByTheClose(t *testing.T) {
	iv := time.Hour
	cs := []Candle{{Time: t0}, {Time: t0.Add(iv)}, {Time: t0.Add(2 * iv)}, {Time: t0.Add(3 * iv)}}
	row := func(h int, rate string) string {
		return `{"coin":"BTC","fundingRate":"` + rate + `","premium":"0","time":` + strconv.FormatInt(t0.Add(time.Duration(h)*iv+21*time.Millisecond).UnixMilli(), 10) + `}`
	}
	pages := "[" + row(0, "0.0001") + "," + row(1, "0.0002") + "]\n[" + row(1, "0.0002") + "," + row(2, "0.0003") + "," + row(3, "-0.0004") + "]"
	got, err := ReadFundingHistory(strings.NewReader(pages), cs, iv)
	if err != nil {
		t.Fatal(err)
	}
	al := AlignDeriv(cs, got, iv)
	for i, want := range []float64{0.0001, 0.0002, 0.0003} {
		if d := al[i]; d.Missing() || d.FundingRate != want || d.OpenInterest != 0 || d.MarkPrice != 0 {
			t.Fatalf("candle %d: %+v, want funding %v and no open interest", i, d, want)
		}
	}
	if !al[3].Missing() || len(got) != 3 {
		t.Fatalf("got %+v; the last candle closes after the history's last rate and must have none", got)
	}

	for name, in := range map[string]string{
		"two coins": "[" + row(0, "0.0001") + "," + strings.Replace(row(1, "0.0002"), "BTC", "ETH", 1) + "]",
		"bad rate":  "[" + row(0, "x") + "]",
		"not json":  "time,symbol",
	} {
		if _, err := ReadFundingHistory(strings.NewReader(in), cs, iv); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
