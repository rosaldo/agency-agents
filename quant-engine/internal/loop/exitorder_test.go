package loop

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// crashEx dies (simulated kill -9) at the emergency exit's market order: it
// reads what is on disk at that instant, refuses the order and asks a stop;
// after that nothing more reaches the exchange.
type crashEx struct {
	*fake.Fake
	dir   string
	armed atomic.Bool
	dead  atomic.Bool
	mu    sync.Mutex
	disk  State
	read  bool
}

func (c *crashEx) PlaceOrder(ctx context.Context, env exchange.Env, r exchange.OrderRequest) (exchange.OrderState, error) {
	if r.ReduceOnly && r.Type == exchange.Market && c.armed.CompareAndSwap(true, false) {
		st, err := LoadState(c.dir, "paper")
		c.mu.Lock()
		c.disk, c.read = st, err == nil
		c.mu.Unlock()
		_ = RequestStop(c.dir, false)
		c.dead.Store(true)
		return exchange.OrderState{}, errors.New("killed")
	}
	if c.dead.Load() {
		return exchange.OrderState{}, errors.New("killed")
	}
	return c.Fake.PlaceOrder(ctx, env, r)
}

// hackSetup builds a crypto run over flat hourly candles at hours, entering
// long whenever flat, with a news feed served by handler.
func hackSetup(t *testing.T, handler http.HandlerFunc, hours []int) (*config.Config, newsStrategy, *fake.Fake, []market.Candle, string) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t0 := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for _, h := range hours {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(h) * time.Hour), Open: 100, High: 100, Low: 100, Close: 100, Volume: 10})
	}
	s := newsStrategy{&scripted.Strategy{Decider: func(in strategy.Input) strategy.Decision {
		if in.Position != nil {
			return strategy.Decision{}
		}
		return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 90}
	}}, srv.URL}
	c, err := config.Parse([]byte(newsYAML), s)
	if err != nil {
		t.Fatal(err)
	}
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 0.001, MinQty: 0.001, Candles: map[string][]market.Candle{"AAA": cs}})
	return c, s, f, cs, filepath.Join(t.TempDir(), ".scripted")
}

func rssHack(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, `<rss><channel><item><title>AAA exchange hack</title></item></channel></rss>`)
}

// A kill at the emergency exit's order finds the news block already on
// disk: the restart within the lookback retries the exit first and never
// re-enters, whether the stop was still on the exchange or already
// cancelled (killed between the cancel and the order).
func TestKillAtTheExitOrder(t *testing.T) {
	for _, naked := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop_kept", true: "stop_cancelled"}[naked], func(t *testing.T) { killAtTheExitOrder(t, naked) })
	}
}

func killAtTheExitOrder(t *testing.T, naked bool) {
	c, s, f, cs, dir := hackSetup(t, rssHack, []int{0, 1, 2, 3, 25})
	cx := &crashEx{Fake: f, dir: dir}
	cx.armed.Store(true)
	tick := &FakeTicker{Ex: f, Candles: cs, Delay: 500 * time.Millisecond}
	Run(context.Background(), c, s, cx, exchange.Demo, tick, dir)
	cx.mu.Lock()
	disk, read := cx.disk, cx.read
	cx.mu.Unlock()
	if !read || !disk.NewsUntil["AAA"].After(cs[0].Time) {
		t.Fatalf("on disk at the exit order: read %v news_until %v; want the block saved before the order", read, disk.NewsUntil)
	}
	ps, _ := f.Positions(context.Background(), exchange.Demo)
	if len(ps) != 1 || ps[0].Qty <= 0 {
		t.Fatalf("positions after the kill %+v; want the position still open", ps)
	}
	if naked && ps[0].StopClientID != "" { // a real kill restores nothing: take the stop away
		_, _ = f.CancelOrder(context.Background(), exchange.Demo, ps[0].StopClientID)
	}
	before := len(journalOf(t, dir))
	tick.OnStep = nil
	Run(context.Background(), c, s, f, exchange.Demo, tick, dir)
	var buys []time.Time
	retried := false
	for _, e := range journalOf(t, dir)[before:] {
		switch {
		case e.Kind == journal.Filled && e.Side == "buy":
			buys = append(buys, e.Time)
		case e.Kind == journal.Filled && e.Side == "sell":
			retried = true
		}
	}
	for _, b := range buys {
		if b.Before(cs[4].Time) {
			t.Fatalf("re-entry at %v within the lookback; buys %v", b, buys)
		}
	}
	if !retried {
		t.Fatalf("no retried emergency exit after the restart; journal %+v", journalOf(t, dir)[before:])
	}
}

// The news block's save fails (the state directory turned read-only): the
// failure goes to the journal and the emergency exit still goes out.
func TestNewsBlockSaveFailureIsJournaledAndTheExitStillGoes(t *testing.T) {
	var dir string
	var once sync.Once
	h := func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { _ = os.Chmod(dir, 0o500) })
		rssHack(w, r)
	}
	c, s, f, cs, d := hackSetup(t, h, []int{0, 1, 2, 3})
	dir = d
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	Run(context.Background(), c, s, f, exchange.Demo, &FakeTicker{Ex: f, Candles: cs, Delay: 500 * time.Millisecond}, dir)
	_ = os.Chmod(dir, 0o700)
	if st, _ := LoadState(dir, "paper"); len(st.NewsUntil) != 0 {
		t.Fatalf("news_until saved %v; the test needs the save to fail", st.NewsUntil)
	}
	if ps, _ := f.Positions(context.Background(), exchange.Demo); len(ps) != 0 {
		t.Fatalf("positions %+v; want the emergency exit done although the save failed", ps)
	}
	for _, e := range journalOf(t, dir) {
		if e.Kind == Error && strings.HasPrefix(e.Reason, "news block not saved: ") {
			return
		}
	}
	t.Fatalf("no journaled save failure; journal %+v", journalOf(t, dir))
}
