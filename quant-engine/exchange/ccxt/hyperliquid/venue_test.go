package hyperliquid

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	lib "github.com/rosaldo/ccxt/v5"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/exchange/ccxt"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exec"
	"github.com/rosaldo/agency-agents/quant-engine/internal/risk"
)

// testKey is a throwaway secp256k1 key for signing in tests (well-known
// documentation key, holds nothing); no request leaves the process.
const testKey = "0x4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"

// venueServer is the Hyperliquid API on loopback: /info answers from
// testdata (public responses captured 2026-10-29), /exchange accepts orders.
type venueServer struct {
	*httptest.Server
	mu       sync.Mutex
	exchange []string // raw /exchange bodies
	info     []string // raw /info bodies
	nextOid  int
	status   map[int]string // oid -> filled | open | canceled
	held     float64        // the account's BTC position, moved by filled market orders
}

func newVenueServer(t *testing.T) *venueServer {
	t.Helper()
	v := &venueServer{nextOid: 10, status: map[int]string{}}
	v.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		v.mu.Lock()
		defer v.mu.Unlock()
		if r.URL.Path == "/exchange" {
			v.exchange = append(v.exchange, string(body))
			_, _ = io.WriteString(w, v.answer(string(body)))
			return
		}
		v.info = append(v.info, string(body))
		var q struct {
			Type, Dex string
			Oid       any
		}
		_ = json.Unmarshal(body, &q)
		if q.Type == "orderStatus" {
			oid := int(q.Oid.(float64))
			left := "0.0" // remaining size: none once filled
			if v.status[oid] != "filled" {
				left = "0.002"
			}
			_, _ = io.WriteString(w, `{"status":"order","order":{"order":{"coin":"BTC","side":"B","limitPx":"80000.0","sz":"`+left+`","oid":`+
				itoa(oid)+`,"timestamp":1793282400000,"origSz":"0.002"},"status":"`+v.status[oid]+`","statusTimestamp":1793282400000}}`)
			return
		}
		name := q.Type
		if q.Dex != "" {
			name += "_" + q.Dex
		}
		switch q.Type {
		case "clearinghouseState":
			pos := ""
			if v.held != 0 { // the account must show what the engine holds: the adapter nets against it (FR-P8)
				pos = `{"type":"oneWay","position":{"coin":"BTC","szi":"` + strconv.FormatFloat(v.held, 'f', -1, 64) +
					`","entryPx":"80000.0","leverage":{"type":"isolated","value":2},"liquidationPx":"40000.0","positionValue":"160.0","unrealizedPnl":"0.0","marginUsed":"80.0","maxLeverage":40}}`
			}
			_, _ = io.WriteString(w, `{"marginSummary":{"accountValue":"1000.0","totalMarginUsed":"0.0"},"withdrawable":"1000.0","assetPositions":[`+pos+`],"time":1793282400000}`)
			return
		case "userAbstraction":
			_, _ = io.WriteString(w, `"default"`)
			return
		}
		b, err := os.ReadFile(filepath.Join("testdata", name+".json"))
		if err != nil {
			t.Errorf("unexpected /info request %s", body)
			http.Error(w, "unknown", http.StatusBadRequest)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(v.Close)
	return v
}

// answer fills market orders, rests triggers, cancels cancels.
func (v *venueServer) answer(body string) string {
	v.nextOid++
	switch {
	case strings.Contains(body, `"type":"cancel`):
		for oid, st := range v.status {
			if st == "open" {
				v.status[oid] = "canceled"
			}
		}
		return `{"status":"ok","response":{"type":"cancel","data":{"statuses":["success"]}}}`
	case strings.Contains(body, `"trigger"`):
		v.status[v.nextOid] = "open"
		return `{"status":"ok","response":{"type":"order","data":{"statuses":[{"resting":{"oid":` + itoa(v.nextOid) + `}}]}}}`
	case strings.Contains(body, `"type":"updateLeverage"`):
		return `{"status":"ok","response":{"type":"default"}}`
	}
	v.status[v.nextOid] = "filled"
	if strings.Contains(body, `"b":true`) {
		v.held += 0.002
	} else {
		v.held -= 0.002
	}
	return `{"status":"ok","response":{"type":"order","data":{"statuses":[{"filled":{"totalSz":"0.002","avgPx":"80000.0","oid":` + itoa(v.nextOid) + `}}]}}}`
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// dialTo dials the venue as the cli does, then points it at srv.
func dialTo(t *testing.T, srv *venueServer, symbols []string) ccxt.Client {
	t.Helper()
	c, err := Venue.Dial(map[string]string{"WALLET_ADDRESS": "0x2c7536E3605D9C16a7a3D7b1898e529396a65c23", "PRIVATE_KEY": testKey}, false, symbols)
	if err != nil {
		t.Fatal(err)
	}
	hl(c).Urls["api"] = map[string]any{"public": srv.URL, "private": srv.URL}
	return c
}

// NFR-0 through the real fork: every /exchange request an entry, its stop,
// leverage settings and the exit produce carries no builder, broker or
// referral field; client order IDs are 0x + 128-bit hex; stop and exit are
// reduce-only. Nothing leaves the process (loopback server).
func TestNoBuilderOrReferralReachesTheWire(t *testing.T) {
	srv := newVenueServer(t)
	sym := "BTC/USDC:USDC"
	ad, err := ccxt.New(dialTo(t, srv, []string{sym}), ccxt.Options{Venue: Venue, Symbols: []string{sym}, Market: "swap",
		Env: exchange.Real, Book: filepath.Join(t.TempDir(), "book.json")})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := ad.ConfigureMarket(ctx, exchange.Real, sym, exchange.MarginSettings{Leverage: 2, MarginMode: "isolated"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC)
	e := &exec.Executor{Name: "test", Ex: ad, Env: exchange.Real, Now: func() time.Time { return now },
		Risk: &risk.Breaker{DailyPct: 100, WeeklyPct: 100, Loc: time.UTC}}
	if err := e.Enter(ctx, sym, now, exec.Plan{Side: exchange.Long, Entry: 80000, Stop: 78000, Qty: 0.002}); err != nil {
		t.Fatal(err)
	}
	ps, err := ad.Positions(ctx, exchange.Real)
	if err != nil || len(ps) != 1 {
		t.Fatalf("positions %+v %v", ps, err)
	}
	if err := e.Exit(ctx, ps[0], now.Add(time.Hour), "exit"); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.exchange) != 6 { // margin mode, leverage, entry, stop, stop cancel, exit
		t.Fatalf("/exchange requests %d, want 6: %v", len(srv.exchange), srv.exchange)
	}
	orders := 0
	for _, body := range srv.exchange {
		low := strings.ToLower(body)
		for _, banned := range []string{"builder", "broker", "referr", "partner", `"ref"`} {
			if strings.Contains(low, banned) {
				t.Errorf("request carries %q: %s", banned, body)
			}
		}
		var req struct {
			Action struct {
				Type   string
				Orders []struct {
					C string
					R bool
					T map[string]any
				}
			}
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatal(err)
		}
		for _, o := range req.Action.Orders {
			orders++
			if !strings.HasPrefix(o.C, "0x") || len(o.C) != 34 {
				t.Errorf("cloid %q, want 0x + 32 hex", o.C)
			}
			_, trigger := o.T["trigger"]
			if entry := orders == 1; o.R == entry || (orders == 2) != trigger {
				t.Errorf("order %d: reduceOnly %v trigger %v", orders, o.R, trigger)
			}
		}
	}
	if orders != 3 {
		t.Fatalf("orders on the wire %d, want 3", orders)
	}
}

// hl is the fork's client under the venue's dex binding.
func hl(c ccxt.Client) *lib.Hyperliquid { return c.(*client).Hyperliquid }

// FR-P4 (V2): Hyperliquid positions carry liquidationPx, and its maintenance
// margin is half the initial margin at the market's max leverage (HIP-3
// xyz:GOOGL: 20x ⇒ 2.5%).
func TestHyperliquidLiquidationInputs(t *testing.T) {
	if !Venue.ReportsLiquidation || Venue.MaintenanceMargin == nil {
		t.Fatal("hyperliquid venue does not declare liquidation reports and maintenance margin")
	}
	max := 20.0
	if mm, ok := Venue.MaintenanceMargin(lib.MarketInterface{Limits: lib.Limits{Leverage: lib.MinMax{Max: &max}}}); !ok || mm != 0.025 {
		t.Fatalf("maintenance margin %v %v, want 0.025", mm, ok)
	}
	if _, ok := Venue.MaintenanceMargin(lib.MarketInterface{}); ok {
		t.Fatal("maintenance margin without a max leverage")
	}
}
