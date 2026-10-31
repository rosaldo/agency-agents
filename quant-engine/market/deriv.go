package market

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Deriv is a derivatives snapshot of a swap market: open interest in the base
// asset, the funding rate of the current period and the mark price (0 = not
// reported). Time is when it was read, never before the data it holds; a zero
// Time marks no snapshot. Candle is the open time of the closed candle it is
// bound to: the one the step that read it decided on.
type Deriv struct {
	Time                                 time.Time
	Candle                               time.Time
	OpenInterest, FundingRate, MarkPrice float64
}

// Missing reports no snapshot.
func (d Deriv) Missing() bool { return d.Time.IsZero() }

// InStep reports whether d may belong to the closed candle opened at candle:
// read after that candle's close and no later than the next close, the
// window of the step that decides on it. A read at or before the close is
// stale for the step; one after the next close belongs to a later step.
func (d Deriv) InStep(candle time.Time, interval time.Duration) bool {
	end := candle.Add(interval)
	return d.Time.After(end) && !d.Time.After(end.Add(interval))
}

// AlignDeriv returns one snapshot per candle, same index: the latest of ds
// bound to that candle and read in its step (InStep). The engine reads it
// after the candle closed and before deciding on it, and the recorder keeps
// the same binding, so a replay hands the strategy what the live step had.
// ds is oldest first.
func AlignDeriv(cs []Candle, ds []Deriv, interval time.Duration) []Deriv {
	out := make([]Deriv, len(cs))
	at := make(map[time.Time]int, len(cs))
	for i, c := range cs {
		at[c.Time.UTC()] = i
	}
	for _, d := range ds {
		if i, ok := at[d.Candle.UTC()]; ok && d.InStep(cs[i].Time, interval) {
			out[i] = d
		}
	}
	return out
}

// DerivCSVHeader is the first line of the recorder's file: the read time,
// the symbol and the open time of the candle the snapshot is bound to.
const DerivCSVHeader = "time,symbol,candle,open_interest,funding_rate,mark_price"

// DerivCSVRow is one line of the recorder's file, newline included.
func DerivCSVRow(symbol string, d Deriv) string {
	var b strings.Builder
	w := csv.NewWriter(&b)
	g := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	ts := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
	_ = w.Write([]string{ts(d.Time), symbol, ts(d.Candle), g(d.OpenInterest), g(d.FundingRate), g(d.MarkPrice)})
	w.Flush()
	return b.String()
}

// ReadDerivCSV parses the recorder's file into each symbol's snapshots,
// oldest first; read times must increase per symbol. An empty file has none.
func ReadDerivCSV(r io.Reader) (map[string][]Deriv, error) {
	rows, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("derivatives: %w", err)
	}
	out := map[string][]Deriv{}
	if len(rows) == 0 {
		return out, nil
	}
	if strings.Join(rows[0], ",") != DerivCSVHeader {
		return nil, fmt.Errorf("derivatives: header must be %q", DerivCSVHeader)
	}
	for i, row := range rows[1:] {
		var ts [2]time.Time
		for j, col := range []int{0, 2} {
			if ts[j], err = time.Parse(time.RFC3339Nano, row[col]); err != nil {
				return nil, fmt.Errorf("derivatives: line %d: bad time %q", i+2, row[col])
			}
		}
		var v [3]float64
		for j := range v {
			if v[j], err = strconv.ParseFloat(row[j+3], 64); err != nil {
				return nil, fmt.Errorf("derivatives: line %d: bad number %q", i+2, row[j+3])
			}
		}
		sym := row[1]
		if prev := out[sym]; len(prev) > 0 && !ts[0].After(prev[len(prev)-1].Time) {
			return nil, fmt.Errorf("derivatives: line %d: time not after the previous %s snapshot", i+2, sym)
		}
		out[sym] = append(out[sym], Deriv{Time: ts[0], Candle: ts[1], OpenInterest: v[0], FundingRate: v[1], MarkPrice: v[2]})
	}
	return out, nil
}

// ReadDerivCSVFile reads the recorder's file; a file that does not exist yet
// has no snapshots.
func ReadDerivCSVFile(path string) (map[string][]Deriv, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string][]Deriv{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("derivatives: %w", err)
	}
	defer f.Close()
	return ReadDerivCSV(f)
}
