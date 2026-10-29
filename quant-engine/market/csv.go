package market

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

const csvHeader = "time,open,high,low,close,volume"

// ReadCSV parses candles with the header time,open,high,low,close,volume and
// RFC 3339 times, oldest first.
func ReadCSV(r io.Reader) ([]Candle, error) {
	rows, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("candles: %w", err)
	}
	if len(rows) == 0 || strings.Join(rows[0], ",") != csvHeader {
		return nil, fmt.Errorf("candles: header must be %q", csvHeader)
	}
	out := make([]Candle, 0, len(rows)-1)
	for i, row := range rows[1:] {
		t, err := time.Parse(time.RFC3339, row[0])
		if err != nil {
			return nil, fmt.Errorf("candles: line %d: bad time %q", i+2, row[0])
		}
		var v [5]float64
		for j := range v {
			if v[j], err = strconv.ParseFloat(row[j+1], 64); err != nil {
				return nil, fmt.Errorf("candles: line %d: bad number %q", i+2, row[j+1])
			}
		}
		if len(out) > 0 && !t.After(out[len(out)-1].Time) {
			return nil, fmt.Errorf("candles: line %d: time not after the previous candle", i+2)
		}
		out = append(out, Candle{Time: t, Open: v[0], High: v[1], Low: v[2], Close: v[3], Volume: v[4]})
	}
	return out, nil
}

func ReadCSVFile(path string) ([]Candle, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("candles: %w", err)
	}
	defer f.Close()
	return ReadCSV(f)
}
