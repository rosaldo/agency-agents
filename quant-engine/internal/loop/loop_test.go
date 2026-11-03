package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
)

const cfgYAML = `
exchange: fake
asset_class: crypto
interval: 1h
symbols: [AAA]
strategy: {period: 3, stop_pct: 4, target_pct: 5}
risk: {confirmed_by_user: true}
`

func setup(t *testing.T, file string, latency time.Duration) (*config.Config, strategy.Strategy, *fake.Fake, *FakeTicker, string) {
	t.Helper()
	s := example.New()
	c, err := config.Parse([]byte(cfgYAML), s)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := market.ReadCSVFile("../../testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 1, MinQty: 1, Latency: latency,
		Candles: map[string][]market.Candle{"AAA": cs}})
	return c, s, f, &FakeTicker{Ex: f, Candles: cs}, filepath.Join(t.TempDir(), ".example-sma-cross")
}

func kinds(t *testing.T, dir string) string {
	t.Helper()
	evs, _, err := journal.Read(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var ks []string
	for _, e := range evs {
		ks = append(ks, e.Kind)
	}
	return strings.Join(ks, ",")
}

func TestRunsUntilStopAndRecordsWhy(t *testing.T) {
	c, s, f, tick, dir := setup(t, "loop_two_days.csv", 0)
	tick.OnStep = func(i int) {
		if i == 30 {
			_ = RequestStop(dir, false)
		}
	}
	reason := Run(context.Background(), c, s, f, exchange.Demo, tick, dir)
	if !strings.Contains(reason, "stop command") {
		t.Fatalf("reason = %q", reason)
	}
	st, err := LoadState(dir, "paper")
	if err != nil || st.Running || st.StopReason != reason || st.Pid != os.Getpid() {
		t.Fatalf("state = %+v, %v", st, err)
	}
	ks := kinds(t, dir)
	if !strings.HasPrefix(ks, "start,heartbeat") || !strings.HasSuffix(ks, ",stop") || !strings.Contains(ks, "filled") {
		t.Fatalf("journal = %s", ks)
	}
	if n := strings.Count(ks, "heartbeat"); n < 2 {
		t.Fatalf("a quiet day must still leave heartbeats, got %d", n)
	}
}

func TestHaltStopsTheLoopWithItsReason(t *testing.T) {
	c, s, f, tick, dir := setup(t, "loop_crash.csv", 0)
	reason := Run(context.Background(), c, s, f, exchange.Demo, tick, dir)
	if !strings.Contains(reason, "drawdown") {
		t.Fatalf("reason = %q, want the drawdown halt", reason)
	}
	if st, _ := LoadState(dir, "paper"); st.Running || st.StopReason != reason {
		t.Fatalf("state = %+v", st)
	}
	if !strings.Contains(kinds(t, dir), "limit") {
		t.Fatal("halt not journaled as a limit event")
	}
}

func TestTargetSurvivesARestart(t *testing.T) {
	c, s, f, tick, dir := setup(t, "loop_two_days.csv", 0)
	tick.OnStep = func(i int) {
		if i == 10 { // entered at the crossing candle
			_ = RequestStop(dir, false)
		}
	}
	Run(context.Background(), c, s, f, exchange.Demo, tick, dir)
	st, _ := LoadState(dir, "paper")
	if st.Targets["AAA"] == 0 || len(st.Positions) != 1 || st.Positions[0].StopClientID == "" {
		t.Fatalf("state after stop = %+v; want the target saved and the position left with its exchange stop", st)
	}
	tick.OnStep = nil
	Run(context.Background(), c, s, f, exchange.Demo, tick, dir) // restart: the target exit must still happen
	if st, _ = LoadState(dir, "paper"); len(st.Positions) != 0 {
		t.Fatalf("position not closed at the restored target: %+v", st.Positions)
	}
}

func TestStopIsPromptWithASlowExchange(t *testing.T) {
	c, s, f, tick, dir := setup(t, "loop_two_days.csv", 10*time.Second)
	done := make(chan string)
	go func() { done <- Run(context.Background(), c, s, f, exchange.Demo, tick, dir) }()
	time.Sleep(200 * time.Millisecond)
	t0 := time.Now()
	if err := RequestStop(dir, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		if time.Since(t0) > time.Second {
			t.Fatalf("loop took %v to stop", time.Since(t0))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop within 2s")
	}
}

func TestStopWithFlattenClosesPositions(t *testing.T) {
	c, s, f, tick, dir := setup(t, "loop_two_days.csv", 0)
	tick.OnStep = func(i int) {
		if i == 10 { // a position is open with its stop
			_ = RequestStop(dir, true)
		}
	}
	reason := Run(context.Background(), c, s, f, exchange.Demo, tick, dir)
	if !strings.Contains(reason, "flatten") {
		t.Fatalf("reason = %q", reason)
	}
	if ps, _ := f.Positions(context.Background(), exchange.Demo); len(ps) != 0 {
		t.Fatalf("positions left open: %+v", ps)
	}
	if st, _ := LoadState(dir, "paper"); len(st.Positions) != 0 || len(st.Targets) != 0 {
		t.Fatalf("state = %+v", st)
	}
	if ks := kinds(t, dir); !strings.Contains(ks, "canceled,sent,filled") {
		t.Fatalf("journal = %s; want the stop cancelled, then the market exit filled", ks)
	}
}

func TestHaltSurvivesARestartTheSameDay(t *testing.T) {
	c, s, f, tick, dir := setup(t, "loop_crash.csv", 0)
	first := Run(context.Background(), c, s, f, exchange.Demo, tick, dir)
	if !strings.Contains(first, "drawdown") {
		t.Fatalf("setup: %q", first)
	}
	sent := f.Calls("PlaceOrder")
	again := Run(context.Background(), c, s, f, exchange.Demo, tick, dir) // stop + start, same day
	if !strings.Contains(again, "drawdown") || f.Calls("PlaceOrder") != sent {
		t.Fatalf("restart same day: reason %q, orders %d -> %d", again, sent, f.Calls("PlaceOrder"))
	}
	if st, _ := LoadState(dir, "paper"); st.Breaker.Halt == "" || st.Breaker.DayOpen == 0 {
		t.Fatalf("breaker not persisted: %+v", st.Breaker)
	}
}

// A lost or corrupt state.json never lifts a halt: the loop
// refuses to trade until it is dealt with explicitly.
func TestMissingOrCorruptStateIsFailClosed(t *testing.T) {
	for _, mut := range []string{"delete", "corrupt"} {
		c, s, f, tick, dir := setup(t, "loop_crash.csv", 0)
		if first := Run(context.Background(), c, s, f, exchange.Demo, tick, dir); !strings.Contains(first, "drawdown") {
			t.Fatalf("setup: %q", first)
		}
		if mut == "delete" {
			_ = os.Remove(filepath.Join(dir, "state-paper.json"))
		} else {
			_ = os.WriteFile(filepath.Join(dir, "state-paper.json"), []byte("{garbage"), 0o600)
		}
		sent := f.Calls("PlaceOrder")
		again := Run(context.Background(), c, s, f, exchange.Demo, tick, dir)
		if !strings.Contains(again, "state-paper.json") || f.Calls("PlaceOrder") != sent {
			t.Fatalf("%s state.json: second run %q, orders %d -> %d", mut, again, sent, f.Calls("PlaceOrder"))
		}
	}
}

// tickerFrom is a FakeTicker resuming at candle i (a restart).
func tickerFrom(f *fake.Fake, cs []market.Candle, i int64) *FakeTicker {
	t := &FakeTicker{Ex: f, Candles: cs}
	t.i.Store(i)
	return t
}
