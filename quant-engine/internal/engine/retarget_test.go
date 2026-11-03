package engine

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/paper"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// retargeting is a scripted strategy whose target is pct above the
// position's average price as the engine reports it after the fill.
type retargeting struct {
	*scripted.Strategy
	pct  float64
	seen []strategy.PositionView // one per Retarget call
}

func (r *retargeting) Retarget(in strategy.Input) float64 {
	r.seen = append(r.seen, *in.Position)
	return in.Position.AvgPrice * (1 + r.pct/100)
}

// answering returns one decision per Decide, then None; it records the
// target each Decide was told.
func answering(told *[]float64, answers ...strategy.Decision) *scripted.Strategy {
	return &scripted.Strategy{Decider: func(in strategy.Input) strategy.Decision {
		if in.Position != nil {
			*told = append(*told, in.Position.Target)
		}
		if len(answers) == 0 {
			return strategy.Decision{}
		}
		d := answers[0]
		answers = answers[1:]
		return d
	}}
}

// retargetCandles: closes 100 100 100 105 103 then 102 for the rest.
func retargetCandles() []market.Candle {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for k, x := range []float64{100, 100, 100, 105, 103, 102, 102, 102} {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(k) * time.Hour), Open: x, High: x, Low: x, Close: x, Volume: 10})
	}
	return cs
}

// retargetRun steps s on ex (holding data advanced to candle 3) n times;
// before each step, hook (if set) gets the step index and the engine.
func retargetRun(t *testing.T, s strategy.Strategy, ex exchange.Exchange, data *fake.Fake, cs []market.Candle, n int, hook func(int, *Engine)) *Engine {
	t.Helper()
	c, err := config.Parse([]byte(scriptedYAML), s)
	if err != nil {
		t.Fatal(err)
	}
	for k := 0; k < 3; k++ {
		data.Advance()
	}
	now := cs[3].Time
	e := New(c, s, ex, exchange.Demo, &events{}, func() time.Time { return now })
	for k := 0; k < n; k++ {
		if k > 0 {
			data.Advance()
			now = cs[3+k].Time
		}
		if hook != nil {
			hook(k, e)
		}
		if err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func ownPos(t *testing.T, ex exchange.Exchange) exchange.Position {
	t.Helper()
	ps, _ := ex.Positions(context.Background(), exchange.Demo)
	if len(ps) != 1 {
		t.Fatalf("positions %+v, want one", ps)
	}
	return ps[0]
}

func entryAt(target float64) strategy.Decision {
	return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 100, Target: target, Fraction: 0.5}
}

func addAt(target float64) strategy.Decision {
	return strategy.Decision{Action: strategy.Add, Side: strategy.Long, Target: target, Fraction: 0.5}
}

func retargets(e *Engine) string {
	var rs []string
	for _, ev := range *journalOf(e) {
		if ev.Kind == journal.Retarget {
			rs = append(rs, ev.Reason)
		}
	}
	return strings.Join(rs, "; ")
}

// Fills 0.1% above the close: after the Enter and after the Add, the target
// is 2% above the average price the exchange reports, not the close's nor
// the one before the Add's fill; the Decision's absolute target is replaced.
func TestRetargetUsesTheAveragePriceAsFilled(t *testing.T) {
	var told []float64
	s := &retargeting{Strategy: answering(&told, entryAt(999), addAt(999)), pct: 2}
	cs := retargetCandles()
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 0.001, MinQty: 0.001, SlippagePct: 0.1, Candles: map[string][]market.Candle{"AAA": cs}})
	var afterEntry exchange.Position
	e := retargetRun(t, s, f, f, cs, 2, func(k int, e *Engine) {
		if k == 1 {
			afterEntry = ownPos(t, f)
			if got, want := e.Targets()["AAA"], afterEntry.AvgPrice*1.02; got != want {
				t.Fatalf("target after the entry %v, want %v (2%% above the filled %v)", got, want, afterEntry.AvgPrice)
			}
		}
	})
	p := ownPos(t, f)
	if p.AvgPrice == afterEntry.AvgPrice || afterEntry.AvgPrice == 105 {
		t.Fatalf("setup: fills %v then %v must differ from the close and from each other", afterEntry.AvgPrice, p.AvgPrice)
	}
	if got, want := e.Targets()["AAA"], p.AvgPrice*1.02; got != want {
		t.Fatalf("target after the add %v, want %v (2%% above the filled average %v)", got, want, p.AvgPrice)
	}
	if len(s.seen) != 2 || s.seen[1].Qty != p.Qty || s.seen[1].Tranches != 2 || s.seen[1].BudgetUsed != 1 || s.seen[1].Target != 999 || s.seen[1].Stop != 100 {
		t.Fatalf("views given to Retarget %+v, want the position after the add (qty %v, 2 tranches, budget used, the Add's target 999)", s.seen, p.Qty)
	}
	if !strings.Contains(retargets(e), "999 -> ") {
		t.Fatalf("retargets journaled %q, want the move from 999", retargets(e))
	}
}

// partialAdd fills only half of the second market order (the Add).
type partialAdd struct {
	*fake.Fake
	n int
}

func (x *partialAdd) PlaceOrder(ctx context.Context, env exchange.Env, r exchange.OrderRequest) (exchange.OrderState, error) {
	if r.Type == exchange.Market && !r.ReduceOnly {
		if x.n++; x.n == 2 {
			r.Qty = math.Floor(r.Qty/2*1000) / 1000
		}
	}
	return x.Fake.PlaceOrder(ctx, env, r)
}

// An Add filled for half its quantity: the target follows the position's
// real quantity and average price.
func TestRetargetAfterAPartialAdd(t *testing.T) {
	var told []float64
	s := &retargeting{Strategy: answering(&told, entryAt(0), addAt(0)), pct: 2}
	cs := retargetCandles()
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 0.001, MinQty: 0.001, Candles: map[string][]market.Candle{"AAA": cs}})
	e := retargetRun(t, s, &partialAdd{Fake: f}, f, cs, 2, nil)
	p := ownPos(t, f)
	if math.Abs(p.Qty-(10+8.333)) > 1e-9 {
		t.Fatalf("setup: qty %v, want 10 + 8.333 (half of the add)", p.Qty)
	}
	if got, want := e.Targets()["AAA"], p.AvgPrice*1.02; got != want || s.seen[1].Qty != p.Qty {
		t.Fatalf("target %v on qty %v, want %v on the filled qty %v", got, s.seen[1].Qty, want, p.Qty)
	}
}

// A strategy without Retargeter keeps the Decision's target; a 0 answer
// keeps it too, and a non-price is refused and journaled.
func TestRetargetZeroOrNotAPriceKeepsTheTarget(t *testing.T) {
	for name, answer := range map[string]float64{"zero": 0, "negative": -1, "nan": math.NaN(), "inf": math.Inf(1)} {
		t.Run(name, func(t *testing.T) {
			var told []float64
			s := &fixedRetarget{Strategy: answering(&told, entryAt(120)), answer: answer}
			cs := retargetCandles()
			f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 0.001, MinQty: 0.001, Candles: map[string][]market.Candle{"AAA": cs}})
			e := retargetRun(t, s, f, f, cs, 1, nil)
			if got := e.Targets()["AAA"]; got != 120 {
				t.Fatalf("target %v, want the Decision's 120 kept", got)
			}
			if refused := strings.Contains(retargets(e), "refused"); refused != (name != "zero") {
				t.Fatalf("retargets journaled %q", retargets(e))
			}
		})
	}
}

type fixedRetarget struct {
	*scripted.Strategy
	answer float64
}

func (r *fixedRetarget) Retarget(strategy.Input) float64 { return r.answer }

// The position cannot be read right after the entry's fill: the target the
// Decision gave stands, the failure is journaled, and the retarget is owed
// until the next step, which makes it on the position read then, before
// the decision (which is told the new target).
func TestRetargetIsRetriedWhenThePositionIsUnreadable(t *testing.T) {
	var told []float64
	s := &retargeting{Strategy: answering(&told, entryAt(120)), pct: 2}
	cs := retargetCandles()
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 0.001, MinQty: 0.001, SlippagePct: 0.1, Candles: map[string][]market.Candle{"AAA": cs}})
	var owed []string
	var kept float64
	e := retargetRun(t, s, &failPositionsAfterFill{Fake: f}, f, cs, 2, func(k int, e *Engine) {
		if k == 1 {
			owed, kept = e.RetargetDue(), e.Targets()["AAA"]
		}
	})
	if !slices.Equal(owed, []string{"AAA"}) || kept != 120 {
		t.Fatalf("after the unreadable fill: owed %v target %v, want AAA owed and the Decision's 120 kept", owed, kept)
	}
	p := ownPos(t, f)
	if !strings.Contains(retargets(e), "position unreadable after the fill") {
		t.Fatalf("retargets journaled %q, want the failed read", retargets(e))
	}
	if got, want := e.Targets()["AAA"], p.AvgPrice*1.02; got != want || len(e.RetargetDue()) != 0 {
		t.Fatalf("target %v (owed %v), want %v made at the next step", got, e.RetargetDue(), want)
	}
	if len(told) != 1 || told[0] != p.AvgPrice*1.02 {
		t.Fatalf("decisions told targets %v, want the retargeted %v", told, p.AvgPrice*1.02)
	}
}

// failPositionsAfterFill fails the first Positions read after the first
// order is placed.
type failPositionsAfterFill struct {
	*fake.Fake
	done bool
}

func (x *failPositionsAfterFill) Positions(ctx context.Context, env exchange.Env) ([]exchange.Position, error) {
	if !x.done && x.Fake.Calls("PlaceOrder") > 0 {
		x.done = true
		return nil, errors.New("positions unavailable")
	}
	return x.Fake.Positions(ctx, env)
}

// The owed retarget is kept until made; it survives a restart through
// RestoreRetarget, and forgetting the position drops it.
func TestRetargetOwedIsRestoredAndForgotten(t *testing.T) {
	c, err := config.Parse([]byte(scriptedYAML), &scripted.Strategy{})
	if err != nil {
		t.Fatal(err)
	}
	e := New(c, &scripted.Strategy{}, fake.New(fake.Options{}), exchange.Demo, nil, time.Now)
	e.RestoreRetarget([]string{"BBB", "AAA"})
	if got := e.RetargetDue(); !slices.Equal(got, []string{"AAA", "BBB"}) {
		t.Fatalf("owed %v, want AAA BBB", got)
	}
	e.forget("AAA")
	if got := e.RetargetDue(); !slices.Equal(got, []string{"BBB"}) {
		t.Fatalf("owed %v after forgetting AAA, want BBB", got)
	}
}

// The backtest's exchange (the fake) and the paper simulator, with the same
// slippage, set the same targets on the same fills.
func TestRetargetBacktestAndPaperAgree(t *testing.T) {
	run := func(paperSim bool) (map[string]float64, float64) {
		var told []float64
		s := &retargeting{Strategy: answering(&told, entryAt(0), addAt(0)), pct: 2}
		cs := retargetCandles()
		if paperSim {
			data := fake.New(fake.Options{Paper: true, QtyStep: 0.001, MinQty: 0.001, Candles: map[string][]market.Candle{"AAA": cs}})
			now := cs[3].Time
			sim := paper.New(data, 10_000, func() time.Time { return now }, paper.Profile{Name: "t", Costs: map[string]config.SimCosts{"AAA": {SlippagePct: 0.1}}})
			e := retargetRun(t, s, sim, data, cs, 2, func(k int, _ *Engine) { now = cs[3+k].Time })
			return e.Targets(), ownPos(t, sim).AvgPrice
		}
		f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 0.001, MinQty: 0.001, SlippagePct: 0.1, Candles: map[string][]market.Candle{"AAA": cs}})
		e := retargetRun(t, s, f, f, cs, 2, nil)
		return e.Targets(), ownPos(t, f).AvgPrice
	}
	bt, btAvg := run(false)
	pp, ppAvg := run(true)
	if math.Abs(bt["AAA"]-pp["AAA"]) > 1e-9 || math.Abs(btAvg-ppAvg) > 1e-9 || bt["AAA"] == 0 {
		t.Fatalf("backtest target %v (avg %v), paper %v (avg %v); want equal", bt, btAvg, pp, ppAvg)
	}
}

// stopDown refuses the next down stop placements.
type stopDown struct {
	*fake.Fake
	down int
}

func (x *stopDown) PlaceStop(ctx context.Context, env exchange.Env, id, sym string, side exchange.Side, qty, px float64) (exchange.OrderState, error) {
	if x.down > 0 {
		x.down--
		return exchange.OrderState{}, exchange.ErrRefused
	}
	return x.Fake.PlaceStop(ctx, env, id, sym, side, qty, px)
}

// An Add that filled but whose stop swap failed returns an error without a
// halt: the retarget stays owed and the next step makes it on the position
// as filled.
func TestRetargetOwedWhenTheAddsStopSwapFails(t *testing.T) {
	var told []float64
	s := &retargeting{Strategy: answering(&told, entryAt(0), addAt(0)), pct: 2}
	cs := retargetCandles()
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 0.001, MinQty: 0.001, SlippagePct: 0.1, Candles: map[string][]market.Candle{"AAA": cs}})
	c, _ := config.Parse([]byte(scriptedYAML), s)
	for k := 0; k < 3; k++ {
		f.Advance()
	}
	now := cs[3].Time
	x := &stopDown{Fake: f}
	e := New(c, s, x, exchange.Demo, &events{}, func() time.Time { return now })
	ctx := context.Background()
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	f.Advance()
	now = cs[4].Time
	x.down = 3 // every attempt of the swap's new stop
	if err := e.Step(ctx); err == nil || IsHalt(err) {
		t.Fatalf("setup: add step error %v, want a non-halt error after the fill", err)
	}
	if got := e.RetargetDue(); !slices.Equal(got, []string{"AAA"}) {
		t.Fatalf("owed %v after the failed swap, want AAA", got)
	}
	f.Advance()
	now = cs[5].Time
	_ = e.Step(ctx)
	if p := ownPos(t, f); e.Targets()["AAA"] != p.AvgPrice*1.02 || len(e.RetargetDue()) != 0 {
		t.Fatalf("target %v owed %v, want %v (2%% above the filled average %v) and nothing owed", e.Targets()["AAA"], e.RetargetDue(), p.AvgPrice*1.02, p.AvgPrice)
	}
}

// An owed retarget is made before the step's target check: the Decision's
// 103 is reached at 104, but the exact target (10% above the fill) is not.
func TestOwedRetargetIsMadeBeforeTheTargetCheck(t *testing.T) {
	var told []float64
	s := &retargeting{Strategy: answering(&told, strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 95, Target: 103, Fraction: 0.5}), pct: 10}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for k, x := range []float64{100, 100, 100, 100, 104, 104, 104} {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(k) * time.Hour), Open: x, High: x, Low: x, Close: x, Volume: 10})
	}
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 0.001, MinQty: 0.001, Candles: map[string][]market.Candle{"AAA": cs}})
	e := retargetRun(t, s, &failPositionsAfterFill{Fake: f}, f, cs, 2, nil)
	p := ownPos(t, f)
	if e.Targets()["AAA"] != p.AvgPrice*1.10 {
		t.Fatalf("target %v, want %v", e.Targets()["AAA"], p.AvgPrice*1.10)
	}
}

// A retarget owed by a run whose strategy had Retargeter, restored for one
// that has not: dropped and journaled, no panic.
func TestOwedRetargetWithoutRetargeterIsDropped(t *testing.T) {
	var told []float64
	s := answering(&told, entryAt(0))
	cs := retargetCandles()
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 0.001, MinQty: 0.001, Candles: map[string][]market.Candle{"AAA": cs}})
	e := retargetRun(t, s, f, f, cs, 2, func(k int, e *Engine) {
		if k == 1 {
			e.RestoreRetarget([]string{"AAA"})
		}
	})
	if len(e.RetargetDue()) != 0 || !strings.Contains(retargets(e), "retarget owed dropped") {
		t.Fatalf("owed %v, journal %q; want it dropped and journaled", e.RetargetDue(), retargets(e))
	}
}

// closeRetarget sets the target 5% above the last close: a rule on the
// candles, which a call without a fill would move.
type closeRetarget struct {
	*scripted.Strategy
	calls int
}

func (r *closeRetarget) Retarget(in strategy.Input) float64 {
	r.calls++
	return in.Candles[len(in.Candles)-1].Close * 1.05
}

// rejectAdd rejects every market order after the first (the Add and its
// resends) at the exchange.
type rejectAdd struct {
	*fake.Fake
	n int
}

func (x *rejectAdd) PlaceOrder(ctx context.Context, env exchange.Env, r exchange.OrderRequest) (exchange.OrderState, error) {
	if r.Type == exchange.Market && !r.ReduceOnly {
		if x.n++; x.n >= 2 {
			return exchange.OrderState{ClientID: r.ClientID, Symbol: r.Symbol, Status: exchange.Rejected}, exchange.ErrRefused
		}
	}
	return x.Fake.PlaceOrder(ctx, env, r)
}

// An Add that filled nothing on an open position (never sent: the balance
// is unreadable; or rejected by the exchange) owes no retarget: on the next
// candle Retarget is not called and the target does not move.
func TestAddThatFilledNothingOwesNoRetarget(t *testing.T) {
	for _, name := range []string{"not sent", "rejected"} {
		t.Run(name, func(t *testing.T) {
			cs := retargetCandles()
			f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 0.001, MinQty: 0.001, Candles: map[string][]market.Candle{"AAA": cs}})
			answers := []strategy.Decision{entryAt(0), addAt(0)}
			s := &closeRetarget{Strategy: &scripted.Strategy{Decider: func(strategy.Input) strategy.Decision {
				if len(answers) == 0 {
					return strategy.Decision{}
				}
				d := answers[0]
				if answers = answers[1:]; d.Action == strategy.Add && name == "not sent" {
					f.FailNext("Balance", errors.New("balance unavailable"), false) // the executor's read before the order
				}
				return d
			}}}
			var ex exchange.Exchange = f
			if name == "rejected" {
				ex = &rejectAdd{Fake: f}
			}
			c, _ := config.Parse([]byte(scriptedYAML), s)
			for k := 0; k < 3; k++ {
				f.Advance()
			}
			now := cs[3].Time
			e := New(c, s, ex, exchange.Demo, &events{}, func() time.Time { return now })
			var afterAdd []string
			var target float64
			for k := 0; k < 3; k++ {
				if k > 0 {
					f.Advance()
					now = cs[3+k].Time
				}
				err := e.Step(context.Background())
				switch {
				case k == 1 && (err == nil || IsHalt(err)):
					t.Fatalf("setup: the add step returned %v, want a non-halt error", err)
				case k != 1 && err != nil:
					t.Fatal(err)
				case k == 0:
					target = e.Targets()["AAA"]
				case k == 1:
					afterAdd = e.RetargetDue()
				}
			}
			if p := ownPos(t, f); math.Abs(p.Qty-10) > 1e-9 {
				t.Fatalf("setup: qty %v, want only the entry's 10", p.Qty)
			}
			if s.calls != 1 || e.Targets()["AAA"] != target || len(afterAdd) != 0 {
				t.Fatalf("Retarget calls %d, target %v (was %v), owed after the add %v; want 1 call, the target unmoved, nothing owed",
					s.calls, e.Targets()["AAA"], target, afterAdd)
			}
		})
	}
}

// A tranche whose save fails sends no order and owes no retarget.
func TestFailedSaveOwesNoRetarget(t *testing.T) {
	for _, failAt := range []strategy.Action{strategy.Enter, strategy.Add} {
		t.Run(map[strategy.Action]string{strategy.Enter: "enter", strategy.Add: "add"}[failAt], func(t *testing.T) {
			cs := retargetCandles()
			f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 0.001, MinQty: 0.001, Candles: map[string][]market.Candle{"AAA": cs}})
			var fail bool
			answers := []strategy.Decision{entryAt(0), addAt(0)}
			s := &closeRetarget{Strategy: &scripted.Strategy{Decider: func(strategy.Input) strategy.Decision {
				if len(answers) == 0 {
					return strategy.Decision{}
				}
				d := answers[0]
				answers = answers[1:]
				fail = d.Action == failAt
				return d
			}}}
			c, _ := config.Parse([]byte(scriptedYAML), s)
			for k := 0; k < 3; k++ {
				f.Advance()
			}
			now := cs[3].Time
			e := New(c, s, f, exchange.Demo, &events{}, func() time.Time { return now })
			e.OnTrade = func() error {
				if fail {
					return errors.New("disk full")
				}
				return nil
			}
			ctx := context.Background()
			steps := 1
			if failAt == strategy.Add {
				steps = 2
			}
			for k := 0; k < steps; k++ {
				if k > 0 {
					f.Advance()
					now = cs[3+k].Time
				}
				if err := e.Step(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if want := steps - 1; f.Calls("PlaceOrder") != want || len(e.RetargetDue()) != 0 {
				t.Fatalf("orders %d owed %v, want %d orders and nothing owed", f.Calls("PlaceOrder"), e.RetargetDue(), want)
			}
		})
	}
}

// An Enter never sent (the balance is unreadable) leaves nothing owed.
func TestEnterNotSentOwesNoRetarget(t *testing.T) {
	cs := retargetCandles()
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 0.001, MinQty: 0.001, Candles: map[string][]market.Candle{"AAA": cs}})
	s := &closeRetarget{Strategy: &scripted.Strategy{Decider: func(strategy.Input) strategy.Decision {
		f.FailNext("Balance", errors.New("balance unavailable"), false) // the executor's read before the order
		return entryAt(0)
	}}}
	c, _ := config.Parse([]byte(scriptedYAML), s)
	for k := 0; k < 3; k++ {
		f.Advance()
	}
	e := New(c, s, f, exchange.Demo, &events{}, func() time.Time { return cs[3].Time })
	if err := e.Step(context.Background()); err == nil || f.Calls("PlaceOrder") != 0 {
		t.Fatalf("setup: step error %v, orders %d; want the entry refused before sending", err, f.Calls("PlaceOrder"))
	}
	if got := e.RetargetDue(); len(got) != 0 {
		t.Fatalf("owed %v after an entry never sent, want nothing", got)
	}
}
