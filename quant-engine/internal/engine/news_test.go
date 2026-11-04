package engine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/news"
	"github.com/rosaldo/agency-agents/quant-engine/internal/news/typesafe"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
)

const newsKey = "ts-engine-test-key-9d1c"

// newsGuard reads one feed whose headline names AAA with "hack", and asks a
// TypeSafe stand-in answering status (200: yes = p).
func newsGuard(t *testing.T, status int, p string) *news.Guard {
	t.Helper()
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<rss><channel><item><title>AAA exchange hack</title></item></channel></rss>`)
	}))
	t.Cleanup(feed.Close)
	jev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			http.Error(w, "no", status)
			return
		}
		_, _ = io.WriteString(w, `{"model":"jev-1.14.0","answers":{"catastrophe":{"type":"noul","noul":`+p+`}}}`)
	}))
	t.Cleanup(jev.Close)
	return &news.Guard{RSS: &news.RSS{Feeds: []string{feed.URL}, Keywords: []string{"hack"}, Lookback: time.Hour, Client: http.DefaultClient},
		Classifier: &typesafe.Client{URL: jev.URL, Key: config.Secret(newsKey), KeyVar: "K", HTTP: http.DefaultClient},
		Question:   news.Question{Type: "noul", Instructions: "Catastrophe?", Model: "jev-1.14.0"}, Threshold: 0.9}
}

// An equity checks the news before its entry, now with the classifier: noise
// enters; a catastrophe or an unavailable classifier stays out, and the
// rejection carries no key. A crypto enters at once (checked afterwards).
func TestNewsBeforeTheEntryOnEquityAfterItOnCrypto(t *testing.T) {
	cases := []struct {
		name, class string
		status      int
		p           string
		want        bool
	}{
		{"equity, noise", "equity", 200, "0.2", true},
		{"equity, catastrophe", "equity", 200, "0.95", false},
		{"equity, classifier down", "equity", 503, "", false},
		{"equity, classifier refuses the key", "equity", 401, "", false},
		{"crypto, catastrophe: enters, checked after", "crypto\nnews_guard: required", 200, "0.95", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, s := parse(t, strings.Replace(cfgYAML, "CLASS", tc.class, 1))
			var j events
			got := stepWith(t, c, s, func(e *Engine) { e.News, e.Exec.Journal = newsGuard(t, tc.status, tc.p), &j })
			if got != tc.want {
				t.Fatalf("entered %v, want %v; rejections %q", got, tc.want, j.rejected())
			}
			if strings.Contains(j.rejected(), newsKey) {
				t.Fatal("the key is in the journal")
			}
		})
	}
}

// After an emergency exit the symbol takes no entry (nor add) until the
// block ends; a failed exit is retried at the next step.
func TestEmergencyExitBlocksTheSymbol(t *testing.T) {
	e, f, _ := trancheEngine(t, "", []float64{105, 105, 105, 105}, enter(1, 100), enter(1, 100), enter(1, 100))
	steps(t, e, f, 1)
	if f.Calls("PlaceOrder") == 0 {
		t.Fatal("no entry")
	}
	if err := e.EmergencyExit(context.Background(), "AAA", e.Exec.Now().Add(2*time.Hour), "test catastrophe"); err != nil {
		t.Fatal(err)
	}
	if ps, _ := f.Positions(context.Background(), exchange.Demo); len(ps) != 0 && ps[0].Qty > 0 {
		t.Fatalf("still holding %+v after the emergency exit", ps)
	}
	orders := f.Calls("PlaceOrder")
	f.Advance()
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	ev := journalOf(e)
	if f.Calls("PlaceOrder") != orders || !strings.Contains(ev.rejected(), "blocked by a news catastrophe") {
		t.Fatalf("orders %d -> %d, rejections %q; want the re-entry refused", orders, f.Calls("PlaceOrder"), ev.rejected())
	}
	n := 0
	for _, x := range *ev {
		if x.Kind == journal.EmergencyExit {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("emergency_exit events %d; want 1", n)
	}
}

// A policy with a classifier wires TypeSafe with the strategy's key variable.
func TestClassifierPolicyWiresTypeSafe(t *testing.T) {
	s := withClassifier{example.New()}
	c, err := config.Parse([]byte(strings.Replace(cfgYAML, "CLASS", "equity", 1)), s)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("EXAMPLE_SMA_CROSS_TYPESAFE_API_KEY", newsKey)
	g := New(c, s, nil, exchange.Demo, nil, time.Now).News.(*news.Guard)
	ts, ok := g.Classifier.(*typesafe.Client)
	if !ok || ts.URL != typesafe.Endpoint || ts.Key.Reveal() != newsKey || ts.KeyVar != "EXAMPLE_SMA_CROSS_TYPESAFE_API_KEY" ||
		g.Threshold != 0.85 || g.Question.Model != "jev-latest" || g.Question.Type != "noul" {
		t.Fatalf("guard %+v, classifier %+v", g, g.Classifier)
	}
}

type withClassifier struct{ strategy.Strategy }

func (withClassifier) NewsPolicy() strategy.NewsPolicy {
	return strategy.NewsPolicy{Feeds: []string{"https://example.invalid/rss"}, Keywords: []string{"hack"},
		Classifier: &strategy.NewsClassifier{Model: "jev-latest", Type: "noul", Question: "Catastrophe?", Threshold: 0.85}}
}

// An emergency exit that fails is retried at the next step, before any
// decision.
func TestFailedEmergencyExitIsRetriedAtTheNextStep(t *testing.T) {
	e, f, _ := trancheEngine(t, "", []float64{105, 105, 105}, enter(1, 100))
	steps(t, e, f, 1)
	f.FailNext("Positions", errors.New("venue down"), false)
	if err := e.EmergencyExit(context.Background(), "AAA", e.Exec.Now().Add(2*time.Hour), "test catastrophe"); err == nil {
		t.Fatal("the failed exit reported no error")
	}
	if ps, _ := f.Positions(context.Background(), exchange.Demo); len(ps) == 0 || ps[0].Qty <= 0 {
		t.Fatal("the position is gone although the exit failed")
	}
	f.Advance()
	if err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ps, _ := f.Positions(context.Background(), exchange.Demo); len(ps) != 0 && ps[0].Qty > 0 {
		t.Fatalf("still holding %+v after the next step", ps)
	}
}

// An equity with one feed down: a headline from another decides as if all had
// answered, and the failure is journaled; with no headline, it stays out.
func TestEquityWithAFailingFeed(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", 500) }))
	t.Cleanup(down.Close)
	quiet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<rss><channel><item><title>Nothing</title></item></channel></rss>`)
	}))
	t.Cleanup(quiet.Close)
	for _, tc := range []struct {
		name, p, rejection string
		quiet, want        bool
		failure            bool // journaled as news_check_failed
	}{
		{name: "catastrophe", p: "0.95", rejection: "news check not clear", failure: true},
		{name: "noise", p: "0.2", want: true, failure: true},
		{name: "no headline", p: "0.95", quiet: true, rejection: "news check failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, s := parse(t, strings.Replace(cfgYAML, "CLASS", "equity", 1))
			var j events
			got := stepWith(t, c, s, func(e *Engine) {
				g := newsGuard(t, 200, tc.p)
				if tc.quiet {
					g.RSS.Feeds[0] = quiet.URL
				}
				g.RSS.Feeds = append(g.RSS.Feeds, down.URL)
				e.News, e.Exec.Journal = g, &j
			})
			failed := false
			for _, x := range j {
				failed = failed || x.Kind == journal.NewsCheckFailed
			}
			if got != tc.want || failed != tc.failure || !strings.Contains(j.rejected(), tc.rejection) {
				t.Fatalf("entered %v, news_check_failed %v, rejections %q; want %v, %v, %q", got, failed, j.rejected(), tc.want, tc.failure, tc.rejection)
			}
		})
	}
}
