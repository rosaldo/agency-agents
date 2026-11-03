package backtest

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/loop"
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

const parityWSYAML = `
exchange: fake
asset_class: crypto
interval: 1m
feed: websocket
symbols: [AAA]
risk: {confirmed_by_user: true}
`

// asMinutes moves cs onto consecutive minutes, prices unchanged.
func asMinutes(cs []market.Candle) []market.Candle {
	out := slices.Clone(cs)
	for i := range out {
		out[i].Time = cs[0].Time.Add(time.Duration(i) * time.Minute)
	}
	return out
}

// runOnStream runs the loop on feed: websocket over cs through the fake
// stream (every candle signalled 0.7 s after its close) and returns the
// journal.
func runOnStream(t *testing.T, s strategy.Strategy, cs []market.Candle) []journal.Event {
	t.Helper()
	c, err := config.Parse([]byte(parityWSYAML), s)
	if err != nil {
		t.Fatal(err)
	}
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs}})
	var times []time.Time
	for _, x := range cs {
		times = append(times, x.Time)
	}
	ff := loop.NewFakeFeed(f, times, []string{"AAA"}, time.Minute, 700*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ff.OnEnd = cancel
	dir := filepath.Join(t.TempDir(), ".scripted")
	loop.Run(ctx, c, s, ff, exchange.Demo, &loop.FeedTicker{Interval: time.Minute, Grace: 5 * time.Second, Clock: ff}, dir)
	evs, _, err := journal.Read(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// On a 1m series, the loop woken by the stream makes the same Decide calls
// and the same orders as the backtest: the stream only says when, the
// candles are REST's.
func TestBacktestAndStreamedLoopDecideAlike(t *testing.T) {
	cs := asMinutes(parityCandles())
	bs := &scripted.Strategy{Decider: upAndDown}
	res, err := Run(context.Background(), bs, []byte(parityWSYAML), Input{Candles: cs, Capital: 10_000, QtyStep: 1, MinQty: 1})
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, tr := range res.Trades {
		want = append(want, fmt.Sprintf("buy %v", tr.Qty), fmt.Sprintf("sell %v", tr.Qty))
	}
	if len(res.Trades) != 2 {
		t.Fatalf("backtest trades %+v, want two", res.Trades)
	}
	ls := &scripted.Strategy{Decider: upAndDown}
	var got []string
	for _, e := range runOnStream(t, ls, cs) {
		if e.Kind == journal.Filled {
			got = append(got, fmt.Sprintf("%s %v", e.Side, e.Qty))
		}
	}
	if !slices.Equal(bs.Log, ls.Log) {
		t.Fatalf("Decide calls differ:\nbacktest %q\nstream   %q", bs.Log, ls.Log)
	}
	if !slices.Equal(want, got) {
		t.Fatalf("orders differ:\nbacktest %q\nstream   %q", want, got)
	}
}

// enterThenAddByMinute is enterThenAdd on a 1m series: enter at minute 1,
// add at minute 2.
func enterThenAddByMinute(in strategy.Input) strategy.Decision {
	return enterThenAdd(strategy.Input{Position: in.Position, Now: time.Date(2026, 1, 1, in.Now.Minute(), 0, 0, 0, time.UTC)})
}

// Graduated entry on the stream: Enter then Add follow the backtest's path,
// and the add's step swaps the stop for the whole quantity.
func TestBacktestAndStreamedLoopAddAlike(t *testing.T) {
	var cs []market.Candle
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, b := range [][4]float64{{100, 100, 100, 100}, {101, 101, 101, 101}, {103, 103, 103, 103}, {104, 104, 104, 104},
		{104, 104, 85, 85}, {85, 85, 85, 85}} {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(i) * time.Minute), Open: b[0], High: b[1], Low: b[2], Close: b[3], Volume: 10})
	}
	bs := &scripted.Strategy{Decider: enterThenAddByMinute}
	res, err := Run(context.Background(), bs, []byte(parityWSYAML), Input{Candles: cs, Capital: 10_000, QtyStep: 1, MinQty: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Trades) != 1 || res.Trades[0].Qty != 7 || res.Trades[0].ExitReason != "stop" {
		t.Fatalf("backtest trades %+v; want one of 7 (4 + 3) closed by the stop", res.Trades)
	}
	ls := &scripted.Strategy{Decider: enterThenAddByMinute}
	var fills []string
	whole := false
	for _, e := range runOnStream(t, ls, cs) {
		switch {
		case e.Kind == journal.Filled:
			fills = append(fills, fmt.Sprintf("%s %v", e.Side, e.Qty))
		case e.Kind == journal.Resting && e.Qty == 7 && e.Time.Equal(cs[2].Time.Add(time.Minute+700*time.Millisecond)):
			whole = true
		}
	}
	if !slices.Equal(bs.Log, ls.Log) {
		t.Fatalf("Decide calls differ:\nbacktest %s\nstream   %s", strings.Join(bs.Log, "\n"), strings.Join(ls.Log, "\n"))
	}
	if want := []string{"buy 4", "buy 3"}; !slices.Equal(fills, want) || !whole {
		t.Fatalf("stream fills %q (a resting stop for 7 in the add's step: %v); want %q", fills, whole, want)
	}
}
