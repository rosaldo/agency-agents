package exec

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/risk"
	"github.com/rosaldo/agency-agents/quant-engine/market"
)

var (
	ctx = context.Background()
	t0  = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
)

func setup(t *testing.T, attached bool) (*fake.Fake, *Executor) {
	t.Helper()
	cs, err := market.ReadCSVFile("../../testdata/stop_hit.csv")
	if err != nil {
		t.Fatal(err)
	}
	f := fake.New(fake.Options{Paper: true, AttachedStop: attached, QtyStep: 1, MinQty: 1, Cash: 10_000,
		Candles: map[string][]market.Candle{"AAA": cs}})
	return f, &Executor{Ex: f, Env: exchange.Demo, Now: func() time.Time { return t0 },
		Risk: &risk.Breaker{DailyPct: 100, WeeklyPct: 100, Loc: time.UTC}}
}

var plan = Plan{Entry: 100, Stop: 90, Target: 105, Qty: 10}

// assertProtected: no observable instant with a position and no stop on the
// exchange — after the call returns and while the following candles pass.
func assertProtected(t *testing.T, f *fake.Fake) {
	t.Helper()
	ps, err := f.Positions(ctx, exchange.Demo)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.Qty > 0 && p.StopClientID == "" {
			t.Fatalf("position without a stop on the exchange: %+v", p)
		}
	}
	for f.Advance() {
	}
	if n := f.Unprotected(); n != 0 {
		t.Fatalf("%d candle(s) passed with an unprotected position", n)
	}
}

func onlyPosition(t *testing.T, f *fake.Fake) exchange.Position {
	t.Helper()
	ps, _ := f.Positions(ctx, exchange.Demo)
	if len(ps) != 1 {
		t.Fatalf("positions = %+v, want exactly one", ps)
	}
	return ps[0]
}

func TestAttachedStopPath(t *testing.T) {
	f, e := setup(t, true)
	if err := e.Enter(ctx, "AAA", t0, plan); err != nil {
		t.Fatal(err)
	}
	if p := onlyPosition(t, f); p.Qty != 10 || p.StopPrice != 90 {
		t.Fatalf("position = %+v", p)
	}
	if f.Calls("PlaceStop") != 0 {
		t.Fatal("attached path must not send a separate stop")
	}
	assertProtected(t, f)
}

func TestTwoStepPath(t *testing.T) {
	f, e := setup(t, false)
	if err := e.Enter(ctx, "AAA", t0, plan); err != nil {
		t.Fatal(err)
	}
	if p := onlyPosition(t, f); p.Qty != 10 || p.StopPrice != 90 {
		t.Fatalf("position = %+v", p)
	}
	assertProtected(t, f)
}

func TestStopFailureFlattensAndHalts(t *testing.T) {
	f, e := setup(t, false)
	bad := plan
	bad.Stop = 101 // above the market: the exchange rejects it every time
	if err := e.Enter(ctx, "AAA", t0, bad); !errors.Is(err, ErrHalt) {
		t.Fatalf("err = %v, want ErrHalt", err)
	}
	if ps, _ := f.Positions(ctx, exchange.Demo); len(ps) != 0 {
		t.Fatalf("position not flattened: %+v", ps)
	}
}

func TestResendAfterTimeoutDoesNotDuplicate(t *testing.T) {
	f, e := setup(t, false)
	f.FailNext("PlaceOrder", errors.New("timeout"), true)
	if err := e.Enter(ctx, "AAA", t0, plan); err != nil {
		t.Fatal(err)
	}
	if err := e.Enter(ctx, "AAA", t0, plan); err != nil { // same candle again
		t.Fatal(err)
	}
	if p := onlyPosition(t, f); p.Qty != 10 {
		t.Fatalf("qty = %v, want 10 (no duplicate)", p.Qty)
	}
}

func TestEntryErrorStillProtects(t *testing.T) {
	f, e := setup(t, false)
	e.Retries = 1
	f.FailNext("PlaceOrder", errors.New("timeout"), true) // filled, response lost, no retry left
	if err := e.Enter(ctx, "AAA", t0, plan); err == nil {
		t.Fatal("entry error must surface")
	}
	assertProtected(t, f)
}

func TestReconcileProtectsAnOrphan(t *testing.T) {
	f, e := setup(t, false)
	f.SetPosition("AAA", 5, 100)
	if err := e.Reconcile(ctx, func(p exchange.Position) float64 { return p.AvgPrice * 0.9 }); err != nil {
		t.Fatal(err)
	}
	if p := onlyPosition(t, f); p.StopPrice != 90 {
		t.Fatalf("orphan not protected: %+v", p)
	}
	assertProtected(t, f)
}

func TestReconcileFlattensWhenNoStopFits(t *testing.T) {
	f, e := setup(t, false)
	f.SetPosition("AAA", 5, 120)
	err := e.Reconcile(ctx, func(p exchange.Position) float64 { return p.AvgPrice * 0.9 }) // 108 > market 100
	if !errors.Is(err, ErrHalt) {
		t.Fatalf("err = %v, want ErrHalt", err)
	}
	if ps, _ := f.Positions(ctx, exchange.Demo); len(ps) != 0 {
		t.Fatalf("position not flattened: %+v", ps)
	}
}

func TestClientIDIsDeterministic(t *testing.T) {
	a, b := ClientID("AAA", t0, "entry"), ClientID("AAA", t0, "entry")
	if a != b || a == ClientID("AAA", t0, "stop") || a == ClientID("AAA", t0.Add(time.Hour), "entry") || len(a) > 32 {
		t.Fatalf("client IDs: %s %s", a, b)
	}
}

func TestLossesCrossingTheDailyLimitBlockNewEntries(t *testing.T) {
	f, e := setup(t, false)
	e.Risk = &risk.Breaker{DailyPct: 3, WeeklyPct: 6, Loc: time.UTC}
	big := Plan{Entry: 100, Stop: 96, Target: 105, Qty: 100}
	if err := e.Enter(ctx, "AAA", t0, big); err != nil {
		t.Fatal(err)
	}
	f.Advance() // stop at 96: -400 on 10000 = -4%
	sent := f.Calls("PlaceOrder")
	err := e.Enter(ctx, "AAA", t0.Add(time.Hour), plan)
	if !errors.Is(err, risk.ErrHalted) {
		t.Fatalf("err = %v, want risk.ErrHalted", err)
	}
	if f.Calls("PlaceOrder") != sent {
		t.Fatal("an entry was sent after the daily limit")
	}
}

func TestNoRiskGateNoEntry(t *testing.T) {
	f, e := setup(t, false)
	e.Risk = nil
	if err := e.Enter(ctx, "AAA", t0, plan); err == nil {
		t.Fatal("entry without a risk gate must be refused")
	}
	if f.Calls("PlaceOrder") != 0 {
		t.Fatal("an entry bypassed risk.Allow")
	}
}

type memJournal struct{ evs []journal.Event }

func (m *memJournal) Append(e journal.Event) error { m.evs = append(m.evs, e); return nil }

func kinds(m *memJournal) string {
	var ks []string
	for _, e := range m.evs {
		ks = append(ks, e.Kind)
	}
	return strings.Join(ks, ",")
}

func TestEveryOrderIsJournaled(t *testing.T) {
	_, e := setup(t, false)
	m := &memJournal{}
	e.Journal = m
	if err := e.Enter(ctx, "AAA", t0, plan); err != nil {
		t.Fatal(err)
	}
	if got := kinds(m); got != "proposed,sent,filled,sent,open" {
		t.Fatalf("journal kinds = %s", got)
	}
	if m.evs[2].ClientID != ClientID("AAA", t0, "entry") || m.evs[4].ClientID != ClientID("AAA", t0, "stop") || m.evs[2].Price != 100 {
		t.Fatalf("events = %+v", m.evs)
	}
}

func TestHaltAndLimitAreJournaled(t *testing.T) {
	_, e := setup(t, false)
	m := &memJournal{}
	e.Journal = m
	bad := plan
	bad.Stop = 101
	_ = e.Enter(ctx, "AAA", t0, bad)
	if got := kinds(m); got != "proposed,sent,filled,sent,rejected,sent,filled,halt" {
		t.Fatalf("journal kinds = %s", got)
	}
	m.evs = nil
	e.Risk = &risk.Breaker{DailyPct: 3, WeeklyPct: 6, Loc: time.UTC}
	_ = e.Risk.Allow(t0, 20_000)
	_ = e.Enter(ctx, "AAA", t0, plan)
	if got := kinds(m); got != "limit" {
		t.Fatalf("journal kinds = %s", got)
	}
}

func TestExitRestoresTheStopWhenTheSellFails(t *testing.T) {
	f, e := setup(t, false)
	if err := e.Enter(ctx, "AAA", t0, plan); err != nil {
		t.Fatal(err)
	}
	p := onlyPosition(t, f)
	e.Retries = 1
	f.FailNext("PlaceOrder", errors.New("down"), false)
	if err := e.Exit(ctx, p, t0.Add(time.Hour), "target"); err == nil {
		t.Fatal("failed exit must surface")
	}
	if q := onlyPosition(t, f); q.StopClientID == "" || q.StopPrice != 90 {
		t.Fatalf("stop not restored: %+v", q)
	}
	if err := e.Exit(ctx, onlyPosition(t, f), t0.Add(2*time.Hour), "target"); err != nil {
		t.Fatal(err)
	}
	if ps, _ := f.Positions(ctx, exchange.Demo); len(ps) != 0 {
		t.Fatalf("position still open: %+v", ps)
	}
}
