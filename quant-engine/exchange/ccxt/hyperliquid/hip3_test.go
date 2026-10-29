package hyperliquid

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	lib "github.com/rosaldo/ccxt/v5"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/exchange/ccxt"
)

// fetchMarkets reads what the dialed client will load.
func fetchMarkets(t *testing.T, c ccxt.Client) map[string]any {
	t.Helper()
	v, _ := hl(c).Options.Load("fetchMarkets")
	m, _ := v.(map[string]any)
	return m
}

// D10: HIP-3 perp dexes load only when the yml trades one, and then exactly
// the dexes of its symbols (never the first 10 of perpDexs in silence).
func TestDialScopesTheHip3Dexes(t *testing.T) {
	c, _ := Venue.Dial(nil, false, []string{"BTC/USDC:USDC"})
	if fm := fetchMarkets(t, c); slices.Contains(fm["types"].([]any), any("hip3")) {
		t.Fatalf("main dex only: fetchMarkets %v loads hip3", fm)
	}
	c, _ = Venue.Dial(nil, false, []string{"XYZ-GOOGL/USDC:USDC", "XYZ-MU/USDC:USDC"})
	fm := fetchMarkets(t, c)
	hip3, _ := fm["hip3"].(map[string]any)
	if !slices.Contains(fm["types"].([]any), any("hip3")) || hip3 == nil || !slices.Equal(hip3["dexes"].([]any), []any{"xyz"}) {
		t.Fatalf("fetchMarkets %v, want hip3 with dexes [xyz]", fm)
	}
}

func newHip3(t *testing.T, srv *venueServer, symbols ...string) (*ccxt.Adapter, error) {
	t.Helper()
	return ccxt.New(dialTo(t, srv, symbols), ccxt.Options{Venue: Venue, Symbols: symbols, Market: "swap",
		Env: exchange.Real, Book: filepath.Join(t.TempDir(), "book.json")})
}

// D10 decisions (t9-hip3.md §6): a USDC dex market loads with its real
// symbol; a dex whose collateral is not USDC and symbols of two dexes are
// refused at start; the balance is read from the dex's clearinghouse.
func TestHip3MarketsFailClosed(t *testing.T) {
	srv := newVenueServer(t)
	a, err := newHip3(t, srv, "XYZ-GOOGL/USDC:USDC")
	if err != nil {
		t.Fatalf("xyz:GOOGL refused: %v", err)
	}
	if _, err := a.Balance(context.Background(), exchange.Real); err != nil {
		t.Fatal(err)
	}
	if !srv.sawInfo(t, "clearinghouseState", "xyz") {
		t.Fatal("balance not read from the xyz clearinghouse")
	}
	for syms, want := range map[string]string{
		"FLX-CRCL/USDH:USDH":                     "USDH",
		"CASH-TSLA/USDT0:USDT0":                  "USDT0",
		"XYZ-GOOGL/USDC:USDC,BTC/USDC:USDC":      "one dex",
		"XYZ-GOOGL/USDC:USDC,FLX-CRCL/USDH:USDH": "one dex",
	} {
		if _, err := newHip3(t, srv, strings.Split(syms, ",")...); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want it refused naming %q", syms, err, want)
		}
	}
}

// sawInfo reports an /info request of type typ for dex.
func (v *venueServer) sawInfo(t *testing.T, typ, dex string) bool {
	t.Helper()
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, b := range v.info {
		var q struct{ Type, Dex string }
		_ = json.Unmarshal([]byte(b), &q)
		if q.Type == typ && q.Dex == dex {
			return true
		}
	}
	return false
}

var _ = lib.NewHyperliquid
