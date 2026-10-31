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
// Time marks no snapshot.
type Deriv struct {
	Time                                 time.Time
	OpenInterest, FundingRate, MarkPrice float64
}

// Missing reports no snapshot.
func (d Deriv) Missing() bool { return d.Time.IsZero() }

// AlignDeriv returns one snapshot per candle, same index: the latest of ds
// read after the candle opened and at or before its close (Time + interval).
// A snapshot read after the close never counts for that candle: it would put
// the future in the decision. ds is oldest first.
func AlignDeriv(cs []Candle, ds []Deriv, interval time.Duration) []Deriv {
	out := make([]Deriv, len(cs))
	j := 0
	for i, c := range cs {
		for ; j < len(ds) && !ds[j].Time.After(c.Time.Add(interval)); j++ {
			if ds[j].Time.After(c.Time) {
				out[i] = ds[j]
			}
		}
	}
	return out
}

// DerivCSVHeader is the first line of the recorder's file.
const DerivCSVHeader = "time,symbol,open_interest,funding_rate,mark_price"

// DerivCSVRow is one line of the recorder's file, newline included.
func DerivCSVRow(symbol string, d Deriv) string {
	var b strings.Builder
	w := csv.NewWriter(&b)
	g := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	_ = w.Write([]string{d.Time.UTC().Format(time.RFC3339Nano), symbol, g(d.OpenInterest), g(d.FundingRate), g(d.MarkPrice)})
	w.Flush()
	return b.String()
}

// ReadDerivCSV parses the recorder's file into each symbol's snapshots,
// oldest first; times must increase per symbol. An empty file has none.
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
		t, err := time.Parse(time.RFC3339Nano, row[0])
		if err != nil {
			return nil, fmt.Errorf("derivatives: line %d: bad time %q", i+2, row[0])
		}
		var v [3]float64
		for j := range v {
			if v[j], err = strconv.ParseFloat(row[j+2], 64); err != nil {
				return nil, fmt.Errorf("derivatives: line %d: bad number %q", i+2, row[j+2])
			}
		}
		sym := row[1]
		if prev := out[sym]; len(prev) > 0 && !t.After(prev[len(prev)-1].Time) {
			return nil, fmt.Errorf("derivatives: line %d: time not after the previous %s snapshot", i+2, sym)
		}
		out[sym] = append(out[sym], Deriv{Time: t, OpenInterest: v[0], FundingRate: v[1], MarkPrice: v[2]})
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
