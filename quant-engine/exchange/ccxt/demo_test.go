package ccxt

import (
	"path/filepath"
	"testing"
	"time"

	lib "github.com/rosaldo/ccxt/v5"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
)

// The testnet demo trades the sandbox client and reads the signal's
// candles from the mainnet one; real stays refused.
func TestTestnetDemoReadsMainnetCandles(t *testing.T) {
	ts := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC).UnixMilli()
	testnet, mainnet := newStub(now), newStub(now)
	testnet.ohlcv = []lib.OHLCV{{Timestamp: ts, Open: 1, High: 1, Low: 1, Close: 1}}
	mainnet.ohlcv = []lib.OHLCV{{Timestamp: ts, Open: 2, High: 2, Low: 2, Close: 2}}
	a, err := New(testnet, Options{Symbols: []string{"AAA/USDT"}, Env: exchange.Demo, Paper: true, Candles: mainnet,
		Book: filepath.Join(t.TempDir(), "book.json"), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	cs, err := a.Candles(ctx, exchange.Demo, "AAA/USDT", time.Hour, 5)
	if err != nil || len(cs) != 1 || cs[0].Close != 2 {
		t.Fatalf("candles = %+v %v; want mainnet's", cs, err)
	}
	if q, err := a.Quote(ctx, exchange.Demo, "AAA/USDT"); err != nil || q.Last != 100 {
		t.Fatalf("quote = %+v %v", q, err)
	}
	if _, err := a.Candles(ctx, exchange.Real, "AAA/USDT", time.Hour, 5); err == nil {
		t.Fatal("real accepted in paper")
	}
}
