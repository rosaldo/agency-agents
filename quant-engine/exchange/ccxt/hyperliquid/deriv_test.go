package hyperliquid

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/exchange/ccxt"
)

// Through the real fork against the recorded venue answers (loopback, no
// network): open interest, funding rate and mark price of a main-dex and a
// HIP-3 market.
func TestSnapshotThroughTheFork(t *testing.T) {
	srv := newVenueServer(t)
	for sym, want := range map[string][3]float64{
		"BTC/USDC:USDC":       {43268.37306, 0.0000125, 81490},
		"XYZ-GOOGL/USDC:USDC": {993116.9920000001, 0.0000459668, 285.08},
	} {
		a, err := ccxt.New(dialTo(t, srv, []string{sym}), ccxt.Options{Venue: Venue, Symbols: []string{sym}, Market: "swap",
			Env: exchange.Real, Book: filepath.Join(t.TempDir(), "book.json")})
		if err != nil {
			t.Fatal(err)
		}
		d, err := a.Snapshot(context.Background(), exchange.Real, sym)
		if err != nil {
			t.Fatalf("%s: %v", sym, err)
		}
		if got := [3]float64{d.OpenInterest, d.FundingRate, d.MarkPrice}; got != want || d.Missing() {
			t.Fatalf("%s: snapshot %+v, want %v", sym, d, want)
		}
	}
}
