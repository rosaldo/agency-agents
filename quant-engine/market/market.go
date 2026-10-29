// Package market holds the price types shared by every engine package.
package market

import "time"

// Candle is one closed bar of an interval.
type Candle struct {
	Time                           time.Time
	Open, High, Low, Close, Volume float64
}
