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

// upAndDown enters long on a rising close (target 5% up) and leaves through
// Exit on a falling one: deterministic given its Input.
func upAndDown(in strategy.Input) strategy.Decision {
	if len(in.Candles) < 2 {
		return strategy.Decision{}
	}
	last, prev := in.Candles[len(in.Candles)-1].Close, in.Candles[len(in.Candles)-2].Close
	switch {
	case in.Position == nil && last > prev:
		return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: last * 0.5, Target: last * 1.05, Reason: "up"}
	case in.Position != nil && last < prev:
		return strategy.Decision{Action: strategy.Exit, Reason: "down"}
	}
	return strategy.Decision{}
}

func parityCandles() []market.Candle {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for i, c := range []float64{100, 100, 105, 111, 100, 100, 101, 102, 101, 100, 100} {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(i) * time.Hour), Open: c, High: c, Low: c, Close: c, Volume: 10})
	}
	return cs
}

const parityYAML = `
exchange: fake
asset_class: crypto
interval: 1h
symbols: [AAA]
risk: {confirmed_by_user: true}
`

// Backtest and loop call the same Decide through the same engine
// step: the same candles give the same strategy calls and the same orders.
func TestBacktestAndLoopDecideAlike(t *testing.T) {
	cs := parityCandles()
	bs := &scripted.Strategy{Decider: upAndDown}
	res, err := Run(context.Background(), bs, []byte(parityYAML), Input{Candles: cs, Capital: 10_000, QtyStep: 1, MinQty: 1})
	if err != nil {
		t.Fatal(err)
	}
	var want, reasons []string
	for _, tr := range res.Trades {
		want = append(want, fmt.Sprintf("buy %v %s", tr.Qty, tr.EntryTime.Format(time.RFC3339)),
			fmt.Sprintf("sell %v %s", tr.Qty, tr.ExitTime.Format(time.RFC3339)))
		reasons = append(reasons, tr.ExitReason)
	}
	if strings.Join(reasons, ",") != "target,exit" {
		t.Fatalf("backtest trades %+v, want a target exit, then a strategy exit", res.Trades)
	}

	ls := &scripted.Strategy{Decider: upAndDown}
	c, err := config.Parse([]byte(parityYAML), ls)
	if err != nil {
		t.Fatal(err)
	}
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs}})
	dir := filepath.Join(t.TempDir(), ".scripted")
	loop.Run(context.Background(), c, ls, f, exchange.Demo, &loop.FakeTicker{Ex: f, Candles: cs}, dir)
	evs, _, err := journal.Read(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range evs {
		if e.Kind == journal.Filled {
			got = append(got, fmt.Sprintf("%s %v %s", e.Side, e.Qty, e.Time.Format(time.RFC3339)))
		}
	}
	if !slices.Equal(bs.Log, ls.Log) {
		t.Fatalf("Decide calls differ:\nbacktest %q\nloop     %q", bs.Log, ls.Log)
	}
	if !slices.Equal(want, got) {
		t.Fatalf("orders differ:\nbacktest %q\nloop     %q", want, got)
	}
}

// trailer enters long on the first rise, stop 90, and trails 3 below the close.
type trailer struct{ *scripted.Strategy }

func (trailer) Trail(in strategy.Input) (float64, bool) {
	return in.Candles[len(in.Candles)-1].Close - 3, true
}

func enterOnRise(in strategy.Input) strategy.Decision {
	if len(in.Candles) < 2 || in.Position != nil {
		return strategy.Decision{}
	}
	if last, prev := in.Candles[len(in.Candles)-1].Close, in.Candles[len(in.Candles)-2].Close; last > prev && in.Now.Hour() == 1 {
		return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 90, Reason: "up"}
	}
	return strategy.Decision{}
}

// FR-P9 parity: the same Trail on the same candles moves the same stops in
// the backtest and in the loop (each Decide sees the stop), and the stop
// moved at 110 (107) is the one the drop fills, on the next candle.
func TestBacktestAndLoopTrailAlike(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for i, b := range [][4]float64{{100, 100, 100, 100}, {101, 101, 101, 101}, {105, 105, 105, 105}, {110, 110, 110, 110},
		{110, 110, 100, 100}, {100, 100, 100, 100}} {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(i) * time.Hour), Open: b[0], High: b[1], Low: b[2], Close: b[3], Volume: 10})
	}
	bs := trailer{&scripted.Strategy{Decider: enterOnRise}}
	res, err := Run(context.Background(), bs, []byte(parityYAML), Input{Candles: cs, Capital: 10_000, QtyStep: 1, MinQty: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Trades) != 1 || res.Trades[0].ExitReason != "stop" || res.Trades[0].ExitPrice != 107 || res.Trades[0].Stop != 90 {
		t.Fatalf("backtest trades %+v; want one stop exit at the trailed 107, R on the initial 90", res.Trades)
	}

	ls := trailer{&scripted.Strategy{Decider: enterOnRise}}
	c, err := config.Parse([]byte(parityYAML), ls)
	if err != nil {
		t.Fatal(err)
	}
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs}})
	dir := filepath.Join(t.TempDir(), ".scripted")
	loop.Run(context.Background(), c, ls, f, exchange.Demo, &loop.FakeTicker{Ex: f, Candles: cs}, dir)
	if !slices.Equal(bs.Log, ls.Log) {
		t.Fatalf("Decide calls (stops included) differ:\nbacktest %q\nloop     %q", bs.Log, ls.Log)
	}
	st, err := loop.LoadState(dir, "paper")
	if err != nil || st.Equity != res.Summary.EndEquity || len(st.Positions) != 0 {
		t.Fatalf("loop state %+v (%v); want flat at the backtest's end equity %v", st, err, res.Summary.EndEquity)
	}
}
