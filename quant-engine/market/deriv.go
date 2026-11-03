package market

import (
	"bytes"
	"cmp"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Deriv is a derivatives snapshot of a swap market: open interest in the base
// asset, the funding rate of the current period and the mark price (0 = not
// reported). HasOpenInterest says the venue reported the open interest: a
// funding-only history has none, and OpenInterest is then 0, not a reading. A
// rule on open interest must treat a snapshot without it as not passing.
// Time is when it was read, never before the data it holds; a zero
// Time marks no snapshot. Candle is the open time of the closed candle it is
// bound to: the one the step that read it decided on.
type Deriv struct {
	Time                                 time.Time
	Candle                               time.Time
	OpenInterest, FundingRate, MarkPrice float64
	HasOpenInterest                      bool
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
// the symbol and the open time of the candle the snapshot is bound to. An
// empty open_interest is one the venue did not report.
const DerivCSVHeader = "time,symbol,candle,open_interest,funding_rate,mark_price"

// DerivCSVRow is one line of the recorder's file, newline included.
func DerivCSVRow(symbol string, d Deriv) string {
	var b strings.Builder
	w := csv.NewWriter(&b)
	g := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	ts := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
	oi := ""
	if d.HasOpenInterest {
		oi = g(d.OpenInterest)
	}
	_ = w.Write([]string{ts(d.Time), symbol, ts(d.Candle), oi, g(d.FundingRate), g(d.MarkPrice)})
	w.Flush()
	return b.String()
}

// ReadDerivCSV parses the recorder's file into each symbol's snapshots,
// oldest first; read times must increase per symbol and every number must be
// finite. An empty file has none.
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
		hasOI := row[3] != ""
		for j := range v {
			if j == 0 && !hasOI {
				continue
			}
			if v[j], err = strconv.ParseFloat(row[j+3], 64); err != nil || math.IsNaN(v[j]) || math.IsInf(v[j], 0) {
				return nil, fmt.Errorf("derivatives: line %d: bad number %q (finite numbers only)", i+2, row[j+3])
			}
		}
		sym := row[1]
		if prev := out[sym]; len(prev) > 0 && !ts[0].After(prev[len(prev)-1].Time) {
			return nil, fmt.Errorf("derivatives: line %d: time not after the previous %s snapshot", i+2, sym)
		}
		out[sym] = append(out[sym], Deriv{Time: ts[0], Candle: ts[1], OpenInterest: v[0], HasOpenInterest: hasOI, FundingRate: v[1], MarkPrice: v[2]})
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
// the close, with no open interest (HasOpenInterest false) and no mark price.
// A rate settled after the close waits for the next candle, so none leaks from
// the future, and a candle closing after the history's last rate gets none:
// the history must outlast the candles. Every row must be of symbol's coin,
// with a finite rate; an error names the line the row starts on.
func ReadFundingHistory(r io.Reader, symbol string, cs []Candle, interval time.Duration) ([]Deriv, error) {
	type row struct {
		Coin        string `json:"coin"`
		FundingRate string `json:"fundingRate"`
		Time        int64  `json:"time"`
		line        int    // where the row starts in the input, for errors
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("funding history: %w", err)
	}
	line, counted := 1, 0
	lineAt := func(off int64) int { // the line of the first value at or after off; offsets only grow, so each byte is counted once
		start := len(b) - len(bytes.TrimLeft(b[off:], " \t\r\n,"))
		line += bytes.Count(b[counted:start], []byte("\n"))
		counted = start
		return line
	}
	var rows []row
	dec := json.NewDecoder(bytes.NewReader(b))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("funding history: %w", err)
		}
		if tok != json.Delim('[') {
			return nil, fmt.Errorf("funding history: want a JSON array per call, got %v", tok)
		}
		for dec.More() {
			at := lineAt(dec.InputOffset())
			var x row
			if err := dec.Decode(&x); err != nil {
				return nil, fmt.Errorf("funding history: line %d: %w", at, err)
			}
			x.line = at
			rows = append(rows, x)
		}
		if _, err := dec.Token(); err != nil { // the closing ]
			return nil, fmt.Errorf("funding history: %w", err)
		}
	}
	slices.SortStableFunc(rows, func(a, b row) int { return cmp.Compare(a.Time, b.Time) })
	coin := hyperliquidCoin(symbol)
	rates := make([]float64, len(rows))
	for i, r := range rows {
		if !strings.EqualFold(r.Coin, coin) {
			return nil, fmt.Errorf("funding history: a row of coin %s; %s is coin %s", r.Coin, symbol, coin)
		}
		var err error
		if rates[i], err = strconv.ParseFloat(r.FundingRate, 64); err != nil || math.IsNaN(rates[i]) || math.IsInf(rates[i], 0) {
			return nil, fmt.Errorf("funding history: line %d: bad rate %q (finite numbers only)", r.line, r.FundingRate)
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

// hip3Base is a HIP-3 symbol's base as the ccxt fork builds it: the dex
// upper-cased, a dash, the ticker (xyz:GOOGL -> XYZ-GOOGL/USDC:USDC).
var hip3Base = regexp.MustCompile(`^([A-Z0-9]+)-([A-Z0-9]+)$`)

// hyperliquidCoin is the venue's coin of a symbol: the base, or dex:ticker on
// a HIP-3 market.
func hyperliquidCoin(symbol string) string {
	base, _, _ := strings.Cut(symbol, "/")
	if m := hip3Base.FindStringSubmatch(base); m != nil {
		return strings.ToLower(m[1]) + ":" + m[2]
	}
	return base
}
