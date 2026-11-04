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
	"sync/atomic"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/news"
	"github.com/rosaldo/agency-agents/quant-engine/internal/news/typesafe"
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

const newsYAML = `
exchange: fake
asset_class: crypto
news_guard: required
interval: 1h
symbols: [AAA]
risk: {confirmed_by_user: true}
`

// newsStrategy enters long at every candle it is flat on, with the news
// policy of one feed and the keyword "hack" (no classifier: a match is a
// catastrophe).
type newsStrategy struct {
	*scripted.Strategy
	feed string
}

// extraFeed, when set, is a second feed of every newsStrategy.
var extraFeed string

func (s newsStrategy) NewsPolicy() strategy.NewsPolicy {
	feeds := []string{s.feed}
	if extraFeed != "" {
		feeds = append(feeds, extraFeed)
	}
	return strategy.NewsPolicy{Feeds: feeds, Keywords: []string{"hack"}, Lookback: 24 * time.Hour}
}

// newsRun runs the loop over four flat hourly candles with the feed
// answering title (undated), or HTTP 500 when title is empty.
func newsRun(t *testing.T, title string) (dir string, f *fake.Fake, cs []market.Candle) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if title == "" {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `<rss><channel><item><title>`+title+`</title></item></channel></rss>`)
	}))
	t.Cleanup(srv.Close)
	t0 := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	for k := range 4 {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(k) * time.Hour), Open: 100, High: 100, Low: 100, Close: 100, Volume: 10})
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
	f = fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 0.001, MinQty: 0.001, Candles: map[string][]market.Candle{"AAA": cs}})
	dir = filepath.Join(t.TempDir(), ".scripted")
	// the delay leaves the worker time to answer before the next candle
	if r := Run(context.Background(), c, s, f, exchange.Demo, &FakeTicker{Ex: f, Candles: cs, Delay: 500 * time.Millisecond}, dir); r != "candles exhausted" {
		t.Fatalf("run ended: %s", r)
	}
	return dir, f, cs
}

func journalOf(t *testing.T, dir string) []journal.Event {
	t.Helper()
	evs, _, err := journal.Read(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// A crypto enters without waiting for the news; the check after the entry
// finds a catastrophe and exits in the same cycle (stamped with the entry's
// candle, before the next one), then the symbol takes no entry while the
// headline is within the lookback.
func TestCatastropheAfterTheEntryExitsAtOnceAndBlocksTheSymbol(t *testing.T) {
	dir, _, cs := newsRun(t, "AAA exchange hack")
	var entries, exits int
	var exitAt time.Time
	var blocked bool
	for _, e := range journalOf(t, dir) {
		switch {
		case e.Kind == journal.Filled && e.Side == "buy":
			entries++
		case e.Kind == journal.Filled && e.Side == "sell":
			exits++
		case e.Kind == journal.EmergencyExit:
			exitAt = e.Time
		case e.Kind == journal.Rejected && strings.Contains(e.Reason, "blocked by a news catastrophe"):
			blocked = true
		}
	}
	if entries != 1 || exits != 1 || !exitAt.Equal(cs[0].Time) || !blocked {
		t.Fatalf("entries %d, exits %d, emergency exit at %v (want %v), re-entry blocked %v; want one entry, its exit in the same cycle, no re-entry",
			entries, exits, exitAt, cs[0].Time, blocked)
	}
}

// An unreadable feed after the entry: news_check_failed, the position stays.
func TestUnreadableFeedAfterTheEntryKeepsThePosition(t *testing.T) {
	dir, f, _ := newsRun(t, "")
	failed, exits := 0, 0
	for _, e := range journalOf(t, dir) {
		switch {
		case e.Kind == journal.NewsCheckFailed:
			failed++
		case e.Kind == journal.Filled && e.Side == "sell", e.Kind == journal.EmergencyExit:
			exits++
		}
	}
	ps, _ := f.Positions(context.Background(), exchange.Demo)
	if failed == 0 || exits != 0 || len(ps) != 1 || ps[0].Qty <= 0 {
		t.Fatalf("news_check_failed %d, exits %d, positions %+v; want the failure journaled and the position kept", failed, exits, ps)
	}
}

// A crypto position with one feed down: a catastrophe in the other feed still
// exits in the same cycle, and the failure is journaled.
func TestAFailingFeedDoesNotHideACatastropheAfterTheEntry(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusInternalServerError) }))
	t.Cleanup(down.Close)
	extraFeed = down.URL
	t.Cleanup(func() { extraFeed = "" })
	dir, _, cs := newsRun(t, "AAA exchange hack")
	failed, exits := 0, 0
	var exitAt time.Time
	for _, e := range journalOf(t, dir) {
		switch e.Kind {
		case journal.NewsCheckFailed:
			failed++
		case journal.EmergencyExit:
			exits++
			exitAt = e.Time
		}
	}
	if failed == 0 || exits != 1 || !exitAt.Equal(cs[0].Time) {
		t.Fatalf("news_check_failed %d, emergency exits %d at %v; want the failure journaled and one exit at %v", failed, exits, exitAt, cs[0].Time)
	}
}

// The classifier's key never reaches the journal: a failing classifier's
// verdict is journaled with the error, without the key.
func TestTheClassifierKeyStaysOutOfTheJournal(t *testing.T) {
	const key = "ts-secret-key-do-not-print-7f3a"
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<rss><channel><item><title>AAA exchange hack</title></item></channel></rss>`)
	}))
	defer feed.Close()
	jev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized "+r.Header.Get("Authorization"), http.StatusUnauthorized) // echoes the key back
	}))
	defer jev.Close()
	g := &news.Guard{RSS: &news.RSS{Feeds: []string{feed.URL}, Keywords: []string{"hack"}, Lookback: time.Hour, Client: http.DefaultClient},
		Classifier: &typesafe.Client{URL: jev.URL, Key: config.Secret(key), KeyVar: "S_TYPESAFE_API_KEY", HTTP: http.DefaultClient},
		Question:   news.Question{Type: "noul", Instructions: "Catastrophe?", Model: "jev-latest"}, Threshold: 0.9}
	fs, err := g.Check(context.Background(), "AAA", time.Now())
	if err != nil || len(fs) != 1 || !fs[0].Catastrophe || fs[0].Err == nil {
		t.Fatalf("findings %+v %v; want one catastrophe from the failing classifier", fs, err)
	}
	dir := t.TempDir()
	j, err := journal.Open(dir, "paper", "paper", "h")
	if err != nil {
		t.Fatal(err)
	}
	note := func(ev journal.Event) { ev.Time = time.Now(); _ = j.Append(ev) }
	exit := func(context.Context, string, time.Time, string) error { return nil }
	handleNews(context.Background(), exit, g.Threshold, newsResult{symbol: "AAA", found: fs}, note, func(string, string) {})
	handleNews(context.Background(), exit, g.Threshold, newsResult{symbol: "AAA", err: errors.New("feed down")}, note, func(string, string) {})
	j.Close()
	b, err := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), key) || !strings.Contains(string(b), "HTTP 401") {
		t.Fatalf("journal %s; want the failure without the key", b)
	}
}

// The block a news catastrophe set survives a restart: stopped right after
// the emergency exit and started again within the lookback, the loop neither
// enters the symbol again nor judges the headline again; once the lookback
// is over it may enter again.
func TestNewsBlockSurvivesARestart(t *testing.T) {
	var feedHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		feedHits.Add(1)
		_, _ = io.WriteString(w, `<rss><channel><item><title>AAA exchange hack</title></item></channel></rss>`)
	}))
	defer srv.Close()
	t0 := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for _, h := range []int{0, 1, 2, 3, 25} { // the last one past the 24h lookback
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
	dir := filepath.Join(t.TempDir(), ".scripted")
	// a state from before the news blocks: no news_until, loads as none
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath(dir, "paper"), []byte(`{"running":false,"level":"paper","targets":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tick := &FakeTicker{Ex: f, Candles: cs, Delay: 500 * time.Millisecond}
	tick.OnStep = func(i int) {
		if i == 1 {
			_ = RequestStop(dir, false)
		}
	}
	Run(context.Background(), c, s, f, exchange.Demo, tick, dir)
	st, err := LoadState(dir, "paper")
	if err != nil || !st.NewsUntil["AAA"].After(cs[3].Time) {
		t.Fatalf("state after stop: news_until %v (%v); want the old state loaded, the entry exited and the block saved", st.NewsUntil, err)
	}
	before, hits := len(journalOf(t, dir)), feedHits.Load()
	tick.OnStep = nil
	if r := Run(context.Background(), c, s, f, exchange.Demo, tick, dir); r != "candles exhausted" {
		t.Fatalf("restart ended: %s", r)
	}
	var buys []time.Time
	verdicts := 0
	for _, e := range journalOf(t, dir)[before:] {
		switch {
		case e.Kind == journal.Filled && e.Side == "buy":
			buys = append(buys, e.Time)
		case e.Kind == journal.NewsVerdict && e.Time.Before(cs[4].Time):
			verdicts++
		}
	}
	if len(buys) != 1 || !buys[0].Equal(cs[4].Time) || verdicts != 0 {
		t.Fatalf("after the restart: buys at %v, headlines judged within the lookback %d (feed read %d times before, %d after); want no entry until %v, then one, and no judgment",
			buys, verdicts, hits, feedHits.Load(), cs[4].Time)
	}
}

// A stop while the classifier is answering cancels the call; the failure it
// hands back is no catastrophe: nothing is journaled, no exit is asked.
func TestAStopDuringAClassificationJournalsNoEmergencyExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := news.Finding{Symbol: "AAA", Headline: "AAA exchange hack", Feed: "f", Err: context.Canceled, Catastrophe: true}
	var notes, exits int
	exit := func(context.Context, string, time.Time, string) error { exits++; return nil }
	handleNews(ctx, exit, 0.9, newsResult{symbol: "AAA", found: []news.Finding{f}}, func(journal.Event) { notes++ }, func(string, string) { notes++ })
	if notes != 0 || exits != 0 {
		t.Fatalf("%d journal events, %d exits after the stop; want none", notes, exits)
	}
}

// Past the lookback the same undated headline, still served, is judged
// again and handed over again: the entry taken after the block gets its
// emergency exit too.
func TestAHeadlineJudgedAgainAfterTheLookbackExitsAgain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<rss><channel><item><title>AAA exchange hack</title></item></channel></rss>`)
	}))
	defer srv.Close()
	t0 := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for _, h := range []int{0, 1, 25, 26} {
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
	dir := filepath.Join(t.TempDir(), ".scripted")
	if r := Run(context.Background(), c, s, f, exchange.Demo, &FakeTicker{Ex: f, Candles: cs, Delay: 500 * time.Millisecond}, dir); r != "candles exhausted" {
		t.Fatalf("run ended: %s", r)
	}
	var buys, exits []time.Time
	for _, e := range journalOf(t, dir) {
		switch {
		case e.Kind == journal.Filled && e.Side == "buy":
			buys = append(buys, e.Time)
		case e.Kind == journal.EmergencyExit:
			exits = append(exits, e.Time)
		}
	}
	if len(buys) != 2 || len(exits) != 2 || !exits[1].Equal(cs[2].Time) {
		t.Fatalf("buys at %v, emergency exits at %v; want an entry and its exit at %v and again at %v", buys, exits, cs[0].Time, cs[2].Time)
	}
}

type alwaysNoise struct{}

func (alwaysNoise) Classify(context.Context, string, news.Question) (news.Verdict, error) {
	return news.Verdict{Model: "fake", Yes: 0.1}, nil
}

// The worker hands each judged text over once: the same title with another
// description is another headline, so a later catastrophe told only in the
// description is not lost to the first verdict on that title.
func TestWorkerHandsOverTheSameTitleWithAnotherDescription(t *testing.T) {
	var desc atomic.Value
	desc.Store("AAA exchange hack")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<rss><channel><item><title>Markets update</title><description>"+desc.Load().(string)+"</description></item></channel></rss>")
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := startNewsWorker(ctx, &news.Guard{RSS: &news.RSS{Feeds: []string{srv.URL}, Keywords: []string{"hack"}, Lookback: time.Hour, Client: srv.Client()},
		Classifier: alwaysNoise{}, Threshold: 0.5})
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	await := func() newsResult {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			w.check([]string{"AAA"}, at) // dropped while the worker is busy, so asked until answered
			select {
			case r := <-w.results():
				return r
			case <-deadline:
				t.Fatal("the worker handed nothing over")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	if r := await(); len(r.found) != 1 || r.found[0].Headline != "Markets update" {
		t.Fatalf("first result %+v", r)
	}
	desc.Store("AAA exchange hack, all funds gone")
	if r := await(); len(r.found) != 1 || !strings.Contains(r.found[0].ClassifierText, "funds gone") {
		t.Fatalf("second result %+v", r)
	}
}
