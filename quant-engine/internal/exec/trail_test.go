package exec

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/market"
)

// spy records the order of stop calls and lets a test act on the cancel of
// one order (the old stop firing in between, for one).
type spy struct {
	*fake.Fake
	calls    []string
	onCancel func(id string) (exchange.OrderState, error, bool)
}

func (s *spy) PlaceStop(ctx context.Context, env exchange.Env, id, sym string, side exchange.Side, qty, px float64) (exchange.OrderState, error) {
	s.calls = append(s.calls, "place "+id)
	return s.Fake.PlaceStop(ctx, env, id, sym, side, qty, px)
}

func (s *spy) CancelOrder(ctx context.Context, env exchange.Env, id string) (exchange.OrderState, error) {
	s.calls = append(s.calls, "cancel "+id)
	if s.onCancel != nil {
		if st, err, ok := s.onCancel(id); ok {
			return st, err
		}
	}
	return s.Fake.CancelOrder(ctx, env, id)
}

// entered is setup with the plan's long held (stop 90 at price 100) behind
// a spy; swap makes the fake a linear swap, where a stray sell opens a short.
func entered(t *testing.T, swap ...bool) (*fake.Fake, *spy, *Executor, exchange.Position) {
	t.Helper()
	f, e := setup(t, false)
	if len(swap) > 0 {
		cs, _ := market.ReadCSVFile("../../testdata/stop_hit.csv")
		f = fake.New(fake.Options{Paper: true, QtyStep: 1, MinQty: 1, Cash: 10_000, Shorts: true, Candles: map[string][]market.Candle{"AAA": cs}})
		e.Ex = f
	}
	if err := e.Enter(ctx, "AAA", t0, plan); err != nil {
		t.Fatal(err)
	}
	s := &spy{Fake: f}
	e.Ex, e.Retries = s, 1
	return f, s, e, onlyPosition(t, f)
}

var t1 = t0.Add(time.Hour)

// FR-P9 (d): the new stop goes first, the old one is cancelled after; the
// moved stop is the one the next candle (low 94) fills.
func TestMoveStopPlacesTheNewStopBeforeCancellingTheOld(t *testing.T) {
	f, s, e, p := entered(t)
	stale, err := e.MoveStop(ctx, p, 95, t1)
	if err != nil || stale != "" {
		t.Fatalf("stale %q, err %v", stale, err)
	}
	newID := e.ID("AAA", t1, "trail")
	if got := strings.Join(s.calls, ","); got != "place "+newID+",cancel "+p.StopClientID {
		t.Fatalf("calls = %s, want the new stop placed before the old one is cancelled", got)
	}
	if q := onlyPosition(t, f); q.StopClientID != newID || q.StopPrice != 95 {
		t.Fatalf("position = %+v", q)
	}
	assertProtected(t, f)
	if st, _ := f.Order(newID); st.Status != exchange.Filled || st.FillPrice != 95 {
		t.Fatalf("moved stop = %+v, want filled at 95", st)
	}
}

// FR-P9 (d): the venue refuses the new stop: the old one stays, nothing is cancelled.
func TestMoveStopRefusedKeepsTheOldStop(t *testing.T) {
	f, s, e, p := entered(t)
	f.FailNext("PlaceStop", errors.Join(exchange.ErrRefused, errors.New("too many orders")), false)
	if _, err := e.MoveStop(ctx, p, 95, t1); err == nil {
		t.Fatal("refused move must surface")
	}
	if q := onlyPosition(t, f); q.StopClientID != p.StopClientID || q.StopPrice != 90 {
		t.Fatalf("position = %+v, want the old stop", q)
	}
	for _, c := range s.calls {
		if c == "cancel "+p.StopClientID {
			t.Fatal("the old stop was cancelled after a refused move")
		}
	}
	assertProtected(t, f)
}

// FR-P9 (d): the new stop's fate is unknown (it landed, the answer was lost):
// it is cancelled, and a position the venue left naked gets the old price back.
func TestMoveStopOfUnknownFateLeavesAStop(t *testing.T) {
	f, _, e, p := entered(t)
	f.FailNext("PlaceStop", errors.New("timeout"), true)
	if _, err := e.MoveStop(ctx, p, 95, t1); err == nil {
		t.Fatal("unknown move must surface")
	}
	if q := onlyPosition(t, f); q.StopClientID == "" || q.StopPrice != 90 {
		t.Fatalf("position = %+v, want a stop at the old price", q)
	}
	assertProtected(t, f)
}

// FR-P9 (d): the old stop's cancel fails: the move holds, two stops guard
// the position for now, and the old one comes back as stale.
func TestMoveStopOldCancelFailsIsStale(t *testing.T) {
	f, _, e, p := entered(t)
	f.FailNext("CancelOrder", errors.New("timeout"), false)
	stale, err := e.MoveStop(ctx, p, 95, t1)
	if err != nil || stale != p.StopClientID {
		t.Fatalf("stale %q, err %v; want the old stop %s", stale, err, p.StopClientID)
	}
	if q := onlyPosition(t, f); q.StopPrice != 95 {
		t.Fatalf("position = %+v", q)
	}
	if !e.CancelStale(ctx, "AAA", stale) {
		t.Fatal("retry of the stale cancel did not clear it")
	}
	assertProtected(t, f)
}

// FR-P9 (d): the old stop fires while the new one is placed: the position is
// gone, the new stop is cancelled, no order is left and no position opens
// (a swap fake: a stray sell would open a short).
func TestMoveStopOldFilledCancelsTheNew(t *testing.T) {
	f, s, e, p := entered(t, true)
	s.onCancel = func(id string) (exchange.OrderState, error, bool) {
		if id != p.StopClientID {
			return exchange.OrderState{}, nil, false
		}
		// the old stop fills: the position is sold
		if _, err := f.PlaceOrder(ctx, exchange.Demo, exchange.OrderRequest{ClientID: "old-fill", Symbol: "AAA", Side: exchange.Sell,
			Qty: p.Qty, Type: exchange.Market, ReduceOnly: true}); err != nil {
			t.Fatal(err)
		}
		return exchange.OrderState{ClientID: id, Status: exchange.Filled}, nil, true
	}
	stale, err := e.MoveStop(ctx, p, 95, t1)
	if !errors.Is(err, ErrStopFilled) || stale != "" {
		t.Fatalf("stale %q, err %v; want ErrStopFilled", stale, err)
	}
	if ps, _ := f.Positions(ctx, exchange.Demo); len(ps) != 0 {
		t.Fatalf("positions = %+v, want none", ps)
	}
	if st, _ := f.Order(e.ID("AAA", t1, "trail")); st.Status == exchange.Open {
		t.Fatalf("new stop left resting: %+v", st)
	}
	if s.calls[len(s.calls)-1] != "cancel "+e.ID("AAA", t1, "trail") {
		t.Fatalf("calls = %v, want the new stop cancelled last", s.calls)
	}
}

// FR-P9 (e): a move is journaled: proposed cur -> next, the new stop sent and
// resting, the old one sent for cancel and cancelled.
func TestMoveStopIsJournaled(t *testing.T) {
	_, _, e, p := entered(t)
	m := &memJournal{}
	e.Journal = m
	if _, err := e.MoveStop(ctx, p, 95, t1); err != nil {
		t.Fatal(err)
	}
	if got := kinds(m); got != "proposed,sent,open,sent,canceled" {
		t.Fatalf("journal kinds = %s", got)
	}
	if !strings.Contains(m.evs[0].Reason, "trail 90 -> 95") || m.evs[2].ClientID != e.ID("AAA", t1, "trail") || m.evs[4].ClientID != p.StopClientID {
		t.Fatalf("events = %+v", m.evs)
	}
}
