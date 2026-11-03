package ccxt

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	lib "github.com/rosaldo/ccxt/v5"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
)

// wsLine is one line of the recorded candle channel.
type wsLine struct {
	Msg struct {
		Data struct {
			T          int64  `json:"t"`
			End        int64  `json:"T"` // declared so "T" never lands in t
			S          string `json:"s"`
			O, H, L, C string
			V          string `json:"v"`
		} `json:"data"`
	} `json:"msg"`
}

// recorded reads testdata/hyperliquid_candle_ws.jsonl as the fork hands
// the messages over: symbol and one OHLCV row each, in receive order.
func recorded(t *testing.T) (syms []string, rows []lib.OHLCV) {
	t.Helper()
	fh, err := os.Open(filepath.Join("..", "..", "testdata", "hyperliquid_candle_ws.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	num := func(s string) float64 {
		x, err := strconv.ParseFloat(s, 64)
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		var l wsLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		d := l.Msg.Data
		syms = append(syms, d.S)
		rows = append(rows, lib.OHLCV{Timestamp: d.T, Open: num(d.O), High: num(d.H), Low: num(d.L), Close: num(d.C), Volume: num(d.V)})
	}
	return syms, rows
}

var minute1622 = time.UnixMilli(1793463720000).UTC()

// The recorded stream: the first update of the 16:23 candle ends the 16:22
// one; updates of the 16:22 candle before it change nothing and the one
// after it is ignored; ETH's boundary shows its last pushed state (close
// 2250.7), which is not the final candle (REST: 2250.8) and is never a value.
func TestTrackerSignalsTheBoundaryOnTheFirstUpdateOfTheNewCandle(t *testing.T) {
	syms, rows := recorded(t)
	trs := map[string]*tracker{}
	var got []exchange.Boundary
	for i, r := range rows {
		if trs[syms[i]] == nil {
			trs[syms[i]] = &tracker{}
		}
		if b, ok := trs[syms[i]].update(syms[i], candleOf(r), time.Minute); ok {
			got = append(got, b)
		}
	}
	if len(got) != 2 || got[0].Symbol != "BTC" || got[1].Symbol != "ETH" {
		t.Fatalf("boundaries %+v; want BTC then ETH, one each", got)
	}
	for _, b := range got {
		if !b.Closed.Equal(minute1622) {
			t.Fatalf("boundary %+v; want the 16:22 candle ended", b)
		}
	}
	if got[0].Last.Close != 77598 || got[1].Last.Close != 2250.7 || got[1].Last.Volume != 4.6282 {
		t.Fatalf("last pushed states %+v / %+v; want BTC 77598 (the update before the boundary), ETH 2250.7", got[0].Last, got[1].Last)
	}
	if c := trs["BTC"].cur; !c.Time.Equal(minute1622.Add(time.Minute)) || c.Close != 77598 {
		t.Fatalf("BTC tracker at %+v; the late update of 16:22 must not take it back", c)
	}
}

// scriptedWatcher replays per symbol a list of watch results (a row or an
// error), then blocks until done.
type scriptedWatcher struct {
	mu    sync.Mutex
	steps map[string][]watchStep
	done  chan struct{}
}

type watchStep struct {
	row lib.OHLCV
	err error
}

func (w *scriptedWatcher) WatchOHLCV(symbol string, opts ...lib.WatchOHLCVOptions) ([]lib.OHLCV, error) {
	var o lib.WatchOHLCVOptionsStruct
	for _, fn := range opts {
		fn(&o)
	}
	if o.Timeframe == nil || *o.Timeframe != "1m" {
		return nil, errors.New("timeframe not 1m")
	}
	w.mu.Lock()
	q := w.steps[symbol]
	if len(q) > 0 {
		w.steps[symbol] = q[1:]
	}
	w.mu.Unlock()
	if len(q) == 0 {
		<-w.done
		return nil, errors.New("closed")
	}
	if q[0].err != nil {
		return nil, q[0].err
	}
	return []lib.OHLCV{q[0].row}, nil
}

func feedAdapter(t *testing.T, w Watcher) *Adapter {
	t.Helper()
	a, err := New(newStub(now), Options{Symbols: []string{"AAA/USDT"}, Env: exchange.Real, Book: filepath.Join(t.TempDir(), "book.json"),
		Now: func() time.Time { return now }, Feed: w, Venue: Venue{FeedIntervals: []time.Duration{time.Minute}}})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func row(minute int, c float64) lib.OHLCV {
	return lib.OHLCV{Timestamp: minute1622.Add(time.Duration(minute) * time.Minute).UnixMilli(), Open: c, High: c, Low: c, Close: c, Volume: 1}
}

// A dropped watch reports the link down once, the first update after it
// the link up, and the candles go on from where they were: each boundary
// once, none skipped, a candle seen again after the reconnection is no new one.
func TestSubscribeReportsTheLinkAndReconnectsWithoutDuplicates(t *testing.T) {
	old := reconnectBackoff
	reconnectBackoff = []time.Duration{time.Millisecond}
	t.Cleanup(func() { reconnectBackoff = old })
	w := &scriptedWatcher{done: make(chan struct{}), steps: map[string][]watchStep{"AAA/USDT": {
		{row: row(0, 1)}, {row: row(1, 2)}, {err: errors.New("connection reset")}, {err: errors.New("dial: refused")},
		{row: row(1, 3)}, {row: row(2, 4)}, {row: row(4, 5)},
	}}}
	t.Cleanup(func() { close(w.done) })
	a := feedAdapter(t, w)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := a.Subscribe(ctx, exchange.Real, []string{"AAA/USDT"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for len(got) < 5 {
		select {
		case b := <-ch:
			if b.Link != "" {
				got = append(got, string(b.Link))
			} else {
				got = append(got, b.Closed.Format("15:04"))
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("got %q, then nothing", got)
		}
	}
	want := []string{"16:22", "down", "up", "16:23", "16:25"} // 16:24 had no trade: the 16:26 update ends 16:25, and 16:24 with it
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

// No stream, or an interval the stream does not carry: refused.
func TestSubscribeRefusesWithoutAStreamOrForAnotherInterval(t *testing.T) {
	if _, err := adapter(t, newStub(now)).Subscribe(context.Background(), exchange.Real, []string{"AAA/USDT"}, time.Minute); err == nil {
		t.Fatal("subscribed without a stream")
	}
	if iv := adapter(t, newStub(now)).FeedIntervals(); len(iv) != 0 {
		t.Fatalf("intervals %v without a stream", iv)
	}
	w := &scriptedWatcher{done: make(chan struct{})}
	defer close(w.done)
	if _, err := feedAdapter(t, w).Subscribe(context.Background(), exchange.Real, []string{"AAA/USDT"}, 2*time.Minute); err == nil {
		t.Fatal("subscribed to 2m on a 1m-only stream")
	}
}
