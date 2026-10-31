package market

import (
	"cmp"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Deriv is a derivatives snapshot of a swap market: open interest in the base
// asset, the funding rate of the current period and the mark price (0 = not
// reported; a funding-only history reports no open interest either, so 0
// there too). Time is when it was read, never before the data it holds; a zero
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
	return d.Time.After(candle.Add(interval)) && !d.Time.After(StepEnd(candle, interval))
}

// StepEnd is the end of the step window of the candle opened at candle: the
// next candle's close. No snapshot read later belongs to it, so a read for
// it is not waited on past this point.
func StepEnd(candle time.Time, interval time.Duration) time.Time { return candle.Add(2 * interval) }

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

// ReadFundingHistory parses Hyperliquid fundingHistory responses, one JSON
// array per call appended as fetched (a page repeats the previous page's last
// row, which binds alike), and binds them to cs: each candle gets the
// last rate settled at or before its close, as a snapshot read just after
// the close, with no open interest and no mark price. A rate settled after
// the close waits for the next candle, so none leaks from the future, and a
// candle closing after the history's last rate gets none: the history must
// outlast the candles. All rows must be of one coin.
func ReadFundingHistory(r io.Reader, cs []Candle, interval time.Duration) ([]Deriv, error) {
	type row struct {
		Coin        string `json:"coin"`
		FundingRate string `json:"fundingRate"`
		Time        int64  `json:"time"`
	}
	var rows []row
	dec := json.NewDecoder(r)
	for {
		var page []row
		if err := dec.Decode(&page); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("funding history: %w", err)
		}
		rows = append(rows, page...)
	}
	slices.SortStableFunc(rows, func(a, b row) int { return cmp.Compare(a.Time, b.Time) })
	rates := make([]float64, len(rows))
	for i, r := range rows {
		if r.Coin != rows[0].Coin {
			return nil, fmt.Errorf("funding history: coins %s and %s in one file", rows[0].Coin, r.Coin)
		}
		var err error
		if rates[i], err = strconv.ParseFloat(r.FundingRate, 64); err != nil {
			return nil, fmt.Errorf("funding history: bad rate %q", r.FundingRate)
		}
	}
	var out []Deriv
	i := -1 // the last row settled by the candle's close
	for _, c := range cs {
		end := c.Time.Add(interval)
		for i+1 < len(rows) && !time.UnixMilli(rows[i+1].Time).After(end) {
			i++
		}
		if i < 0 || i == len(rows)-1 { // before the first rate, or no later rate shows the history reaches past this close
			continue
		}
		out = append(out, Deriv{Time: end.Add(time.Nanosecond), Candle: c.Time, FundingRate: rates[i]})
	}
	return out, nil
}
