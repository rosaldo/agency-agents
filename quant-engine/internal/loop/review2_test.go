package loop

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
)

// runMode runs the loop on the crash fixture (a drawdown halt) in mode.
func runMode(t *testing.T, mode, dir string) (string, *fake.Fake) {
	t.Helper()
	s := example.New()
	c, err := config.Parse([]byte(cfgYAML+"mode: "+mode+"\nvalidation_file: v.yml\n"), s)
	if err != nil {
		t.Fatal(err)
	}
	cs, _ := market.ReadCSVFile("../../testdata/loop_crash.csv")
	f := fake.New(fake.Options{Paper: mode == "paper", Cash: 10_000, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs}})
	env := exchange.Demo
	if mode == "live" {
		env = exchange.Real
	}
	return Run(context.Background(), c, s, f, env, &FakeTicker{Ex: f, Candles: cs}, dir), f
}

// Review 2 N4: paper and live keep separate state; a paper run in the same
// directory never lifts the live halt of the day.
func TestPaperRunKeepsTheLiveHalt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".example-sma-cross")
	if first, _ := runMode(t, "live", dir); !strings.Contains(first, "drawdown") {
		t.Fatalf("setup: %q", first)
	}
	runMode(t, "paper", dir)
	again, f := runMode(t, "live", dir)
	if !strings.Contains(again, "drawdown") || f.Calls("PlaceOrder") > 0 {
		t.Fatalf("live after paper: %q, orders %d", again, f.Calls("PlaceOrder"))
	}
	if s, _ := ReadStatus(dir); s.Mode != "live" {
		t.Fatalf("status shows %q, want the last run (live)", s.Mode)
	}
}

// Review 2 N7: a valid but emptied state file is refused like a missing one.
func TestEmptiedStateIsFailClosed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".example-sma-cross")
	if first, _ := runMode(t, "live", dir); !strings.Contains(first, "drawdown") {
		t.Fatalf("setup: %q", first)
	}
	_ = os.WriteFile(filepath.Join(dir, "state-live.json"), []byte("{}"), 0o600)
	again, f := runMode(t, "live", dir)
	if !strings.HasPrefix(again, "refused") || f.Calls("PlaceOrder") > 0 {
		t.Fatalf("emptied state: %q, orders %d", again, f.Calls("PlaceOrder"))
	}
}

// lateFill leaves the entry unconfirmed, then lets its fill appear on the
// second look at the positions after that (the first is the executor's own).
type lateFill struct {
	*fake.Fake
	pending     bool
	looks, buys int
}

func (l *lateFill) PlaceOrder(ctx context.Context, env exchange.Env, r exchange.OrderRequest) (exchange.OrderState, error) {
	if r.Side == exchange.Buy && !l.pending {
		l.pending = true
		return exchange.OrderState{ClientID: r.ClientID, Symbol: r.Symbol, Side: r.Side, Qty: r.Qty, Status: exchange.Open}, nil
	}
	if r.Side == exchange.Buy {
		l.buys++
	}
	return l.Fake.PlaceOrder(ctx, env, r)
}

func (l *lateFill) Positions(ctx context.Context, env exchange.Env) ([]exchange.Position, error) {
	if l.pending {
		if l.looks++; l.looks == 2 {
			l.SetPosition("AAA", 3, 100)
		}
	}
	return l.Fake.Positions(ctx, env)
}

// Review 2 (B2 residue): after the halt for an unconfirmed entry the loop
// keeps guarding: a fill that lands later still gets its stop, and nothing
// new is bought.
func TestFillAfterAnUnconfirmedHaltGetsAStop(t *testing.T) {
	c, s, f, tick, dir := setup(t, "loop_two_days.csv", 0)
	ex := &lateFill{Fake: f}
	reason := Run(context.Background(), c, s, ex, exchange.Demo, tick, dir)
	if !strings.Contains(reason, "not confirmed") {
		t.Fatalf("reason = %q", reason)
	}
	ps, _ := f.Positions(context.Background(), exchange.Demo)
	protected := f.Calls("PlaceStop") > 0 || f.Calls("PlaceOrder") > 0 // a stop, or flattened when none fits
	if !protected || ex.buys != 0 || (len(ps) == 1 && ps[0].StopClientID == "") {
		t.Fatalf("late fill: positions %+v, stops %d, buys after the halt %d", ps, f.Calls("PlaceStop"), ex.buys)
	}
}

type noStops struct{ *fake.Fake }

func (n noStops) PlaceStop(context.Context, exchange.Env, string, string, exchange.Side, float64, float64) (exchange.OrderState, error) {
	return exchange.OrderState{Status: exchange.Rejected}, errors.New("stop refused")
}

// Review minor 2 (integration): a failed-stop halt survives a restart the same day.
func TestFailedStopHaltSurvivesARestart(t *testing.T) {
	c, s, f, tick, dir := setup(t, "loop_two_days.csv", 0)
	if first := Run(context.Background(), c, s, noStops{f}, exchange.Demo, tick, dir); !strings.Contains(first, "halt") {
		t.Fatalf("setup: %q", first)
	}
	c2, s2, f2, tick2, _ := setup(t, "loop_two_days.csv", 0)
	Run(context.Background(), c2, s2, f2, exchange.Demo, tick2, dir)
	if f2.Calls("PlaceOrder") != 0 {
		t.Fatal("restart traded after a failed-stop halt the same day")
	}
}

// dropsStop takes stops until refuse is set, then refuses them.
type dropsStop struct {
	*fake.Fake
	refuse bool
}

func (d *dropsStop) PlaceStop(ctx context.Context, env exchange.Env, id, sym string, side exchange.Side, qty, px float64) (exchange.OrderState, error) {
	if d.refuse {
		return exchange.OrderState{Status: exchange.Rejected}, errors.New("stop refused")
	}
	return d.Fake.PlaceStop(ctx, env, id, sym, side, qty, px)
}

// Re-review 3, R3: during the guard after a halt, status shows the halt
// (last_event and breaker) and the current positions and equity, not the
// position the halt already closed.
func TestStatusDuringTheGuardShowsTheHalt(t *testing.T) {
	c, s, f, tick, dir := setup(t, "loop_two_days.csv", 0)
	ex := &dropsStop{Fake: f}
	var during *Status
	tick.OnStep = func(int) {
		ctx := context.Background()
		ps, _ := f.Positions(ctx, exchange.Demo)
		switch {
		case !ex.refuse && len(ps) == 1 && ps[0].StopClientID != "":
			_, _ = f.CancelOrder(ctx, exchange.Demo, ps[0].StopClientID) // the exchange drops the stop...
			ex.refuse = true                                             // ...and takes no new one: flatten + halt
		case ex.refuse && during == nil:
			st, err := ReadStatus(dir)
			if err != nil {
				t.Error(err)
			}
			during = &st
			_ = RequestStop(dir, false)
		}
	}
	reason := Run(context.Background(), c, s, ex, exchange.Demo, tick, dir)
	if !strings.Contains(reason, "halt") || during == nil {
		t.Fatalf("setup: reason %q, status read %v", reason, during != nil)
	}
	b, _ := json.Marshal(*during)
	bal, _ := f.Balance(context.Background(), exchange.Demo)
	if !strings.Contains(string(b), `"halt":"exec: halt`) || !strings.HasPrefix(during.LastEvent, "halt ") ||
		len(during.Positions) != 0 || during.Equity != bal.Equity {
		t.Fatalf("status during the guard = %s; want the halt, last_event halt, no position, equity %v", b, bal.Equity)
	}
}
