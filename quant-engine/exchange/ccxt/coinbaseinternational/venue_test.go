package coinbaseinternational

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	lib "github.com/rosaldo/ccxt/v5"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/exchange/ccxt"
)

const sym = "AAVE/USDC:USDC"

// gateway is the Coinbase gateway on loopback: no currencies, one linear perpetual, an
// accepted set_leverage, one position at leverage 5. Every other path fails the test.
type gateway struct {
	*httptest.Server
	mu       sync.Mutex
	leverage []url.Values // set_leverage queries
}

func newGateway(t *testing.T) *gateway {
	t.Helper()
	g := &gateway{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		switch r.URL.Path {
		case "/public/get_instruments":
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","result":[{"kind":"future","instrument_name":"AAVE_USDC-PERPETUAL",
				"base_currency":"AAVE","quote_currency":"USDC","counter_currency":"USDC","settlement_currency":"USDC",
				"instrument_type":"linear","future_type":"linear","settlement_period":"perpetual","is_active":true,"state":"open",
				"expiration_timestamp":32503708800000,"contract_size":1,"min_trade_amount":0.01,"tick_size":0.01,"max_leverage":50}]}`)
		case "/public/get_currencies":
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","result":[]}`)
		case "/private/set_leverage":
			g.leverage = append(g.leverage, r.URL.Query())
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":5}`)
		case "/private/get_positions":
			// a position at the leverage asked; the fork parses no margin mode (marginMode: nil)
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":[{"instrument_name":"AAVE_USDC-PERPETUAL","kind":"future","direction":"buy","size":1,"leverage":5}]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.Error(w, "unknown", http.StatusBadRequest)
		}
	}))
	t.Cleanup(g.Close)
	return g
}

// swapAdapter dials the venue as the cli does, points it at g with a cached
// gateway token (no public/auth, nothing signed leaves the process).
func swapAdapter(t *testing.T, g *gateway) *ccxt.Adapter {
	t.Helper()
	c, err := Venue.Dial(map[string]string{"API_KEY": "organizations/test/apiKeys/test", "API_SECRET": "test"}, false, []string{sym})
	if err != nil {
		t.Fatal(err)
	}
	x := c.(*lib.Coinbaseinternational)
	x.Urls["api"] = map[string]any{"rest": g.URL}
	x.Options.Store("v2AccessToken", "test-token")
	x.Options.Store("v2TokenExpires", int64(1<<62))
	ad, err := ccxt.New(c, ccxt.Options{Venue: Venue, Symbols: []string{sym}, Market: "swap",
		Env: exchange.Real, Book: filepath.Join(t.TempDir(), "book.json")})
	if err != nil {
		t.Fatal(err)
	}
	return ad
}

// Owner's decision 29/10: coinbaseinternational trades perp. The fork has no
// setMarginMode for it; isolated goes as params.isolated on setLeverage
// (coinbaseinternational.go:4083), and nothing else reaches the wire.
func TestIsolatedGoesOnSetLeverage(t *testing.T) {
	g := newGateway(t)
	ad := swapAdapter(t, g)
	want := exchange.MarginSettings{Leverage: 5, MarginMode: "isolated"}
	got, err := ad.ConfigureMarket(context.Background(), exchange.Real, sym, want)
	if err != nil {
		t.Fatalf("start swap refused: %v", err)
	}
	if got != want {
		t.Fatalf("applied %+v, want %+v", got, want)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.leverage) != 1 {
		t.Fatalf("set_leverage requests %d, want 1", len(g.leverage))
	}
	q := g.leverage[0]
	if q.Get("instrument_name") != "AAVE_USDC-PERPETUAL" || q.Get("leverage") != "5" || q.Get("isolated") != "true" || len(q) != 3 {
		t.Fatalf("set_leverage query %v, want instrument_name, leverage 5, isolated true and nothing else", q)
	}
}

// Cross is refused before anything is sent: the fork documents isolated
// only (params.isolated "set to true for isolated margin").
func TestCrossIsRefused(t *testing.T) {
	g := newGateway(t)
	ad := swapAdapter(t, g)
	_, err := ad.ConfigureMarket(context.Background(), exchange.Real, sym, exchange.MarginSettings{Leverage: 5, MarginMode: "cross"})
	if err == nil || !strings.Contains(err.Error(), "cross") {
		t.Fatalf("cross: err = %v, want a refusal naming cross", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.leverage) != 0 {
		t.Fatalf("set_leverage sent on a refused margin mode: %v", g.leverage)
	}
}
