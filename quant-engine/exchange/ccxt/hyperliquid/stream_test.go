package hyperliquid

import (
	"bufio"
	"context"
	"encoding/json"
	pemlib "encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/exchange/ccxt"
)

// newCandleServer is a loopback websocket that answers the fork's candle
// subscriptions with the messages of testdata/hyperliquid_candle_ws.jsonl,
// in their order, once BTC and ETH are both subscribed. Each message waits
// its recorded gap after the one before, capped at maxGap: the fork wakes
// only a watch already waiting, so messages sent back to back would race
// the watchers' re-arm.
func newCandleServer(t *testing.T) *httptest.Server {
	t.Helper()
	const maxGap = 250 * time.Millisecond
	var msgs [][]byte
	var gaps []time.Duration
	prev := int64(0)
	fh, err := os.Open(filepath.Join("..", "..", "..", "testdata", "hyperliquid_candle_ws.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		var l struct {
			RecvMs int64 `json:"recv_ms"`
			Msg    json.RawMessage
		}
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		if prev == 0 {
			prev = l.RecvMs
		}
		msgs = append(msgs, l.Msg)
		gaps = append(gaps, min(time.Duration(l.RecvMs-prev)*time.Millisecond, maxGap))
		prev = l.RecvMs
	}
	up := websocket.Upgrader{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		subs := map[string]bool{}
		for len(subs) < 2 {
			_, b, err := c.ReadMessage()
			if err != nil {
				return
			}
			var m struct {
				Method       string
				Subscription struct{ Type, Coin, Interval string }
			}
			if json.Unmarshal(b, &m) == nil && m.Method == "subscribe" && m.Subscription.Type == "candle" && m.Subscription.Interval == "1m" {
				subs[m.Subscription.Coin] = true
			}
		}
		for i, m := range msgs {
			time.Sleep(gaps[i])
			if c.WriteMessage(websocket.TextMessage, m) != nil {
				return
			}
		}
		for { // keep the link open until the client goes
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	// the fork dials ws:// only through a proxy and trusts the system roots:
	// the loopback stream is wss:// with its certificate as the only root.
	// the roots may be read once per process: one server per test binary run,
	// set before its first TLS dial
	pem := filepath.Join(t.TempDir(), "root.pem")
	if err := os.WriteFile(pem, pemOf(srv.Certificate().Raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", pem)
	return srv
}

func pemOf(der []byte) []byte {
	return pemlib.EncodeToMemory(&pemlib.Block{Type: "CERTIFICATE", Bytes: der})
}

// The recorded candle messages, through the fork's own parser (WatchOHLCV
// and its candle handler) and the adapter's stream, end the 16:22 candle
// once per symbol, each boundary showing the stream's last update of that
// candle (ETH 2250.7, never a value: REST gives 2250.8). Both
// symbols on one stream, as the loop subscribes them: their first watches
// start together.
//
// BTC's Last depends on goroutine scheduling: the fork wakes only a watch
// already waiting, so when BTC's watcher has not re-armed by the 16:23
// candle's first update, that wake-up is lost and the next BTC message (the
// late update of 16:22) returns the cache with 16:22 already overwritten by
// it. Both are the stream's last update the adapter saw; the test takes
// either.
func TestRecordedCandleMessagesThroughTheFork(t *testing.T) {
	ended := time.UnixMilli(1793463720000).UTC()
	want := map[string][][2]float64{
		"BTC/USDC:USDC": {{77598, 0.16004}, {77598.5, 0.16104}},
		"ETH/USDC:USDC": {{2250.7, 4.6282}},
	}
	got := stream(t, newCandleServer(t), 2, "BTC/USDC:USDC", "ETH/USDC:USDC")
	if len(got) != 2 {
		t.Fatalf("boundaries %+v; want one per symbol", got)
	}
	for _, b := range got {
		ok := false
		for _, w := range want[b.Symbol] {
			ok = ok || b.Last.Close == w[0] && b.Last.Volume == w[1]
		}
		if !b.Closed.Equal(ended) || !ok {
			t.Fatalf("boundary %+v; want the 16:22 candle ended at one of (close, volume) %v", b, want[b.Symbol])
		}
		delete(want, b.Symbol)
	}
	if len(want) != 0 {
		t.Fatalf("no boundary for %v", want)
	}
}

// stream replays the recording to the adapter subscribed to syms and
// returns the boundaries it gave: n of them, or what came within 10 s.
func stream(t *testing.T, ws *httptest.Server, n int, syms ...string) []exchange.Boundary {
	t.Helper()
	venue := newVenueServer(t)
	w, err := dialFeed(syms)
	if err != nil {
		t.Fatal(err)
	}
	api := w.(feedClient).Urls["api"].(map[string]any)
	api["public"], api["private"] = venue.URL, venue.URL
	api["ws"].(map[string]any)["public"] = "wss" + strings.TrimPrefix(ws.URL, "https")
	// no keep-alive ping: the replay is over well within one, and a client
	// left over from an earlier run of the test would ping its closed
	// server and fail that run with the fork's own teardown race
	if _, ok := w.(feedClient).Options.Load("ws"); ok {
		t.Fatal("the fork now sets ws options: merge the keep-alive into them")
	}
	w.(feedClient).Options.Store("ws", map[string]any{"keepAlive": int64(0)})
	a, err := ccxt.New(dialTo(t, venue, syms), ccxt.Options{Venue: Venue, Symbols: syms, Market: "swap",
		Env: exchange.Real, Book: filepath.Join(t.TempDir(), "book.json"), Feed: w})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := a.Subscribe(ctx, exchange.Real, syms, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var got []exchange.Boundary
	timeout := time.After(10 * time.Second)
	for {
		select {
		case b := <-ch:
			if b.Link == exchange.LinkDown {
				t.Fatalf("stream down: %s", b.Reason)
			}
			if b.Link == "" {
				got = append(got, b)
			}
			if len(got) == n {
				return got
			}
		case <-timeout:
			return got
		}
	}
}
