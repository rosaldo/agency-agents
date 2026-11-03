package news_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/news"
	"github.com/rosaldo/agency-agents/quant-engine/internal/news/typesafe"
)

const testKey = "ts-test-key-0123456789abcdef"

var at = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// feedServer serves one RSS item with title, or HTTP status when status != 0.
func feedServer(t *testing.T, title string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 0 {
			http.Error(w, "down", status)
			return
		}
		_, _ = io.WriteString(w, `<rss><channel><item><title>`+title+`</title><pubDate>Mon, 02 Nov 2026 11:30:00 +0000</pubDate></item></channel></rss>`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// jev is a TypeSafe stand-in: it checks the request (TSDOC shape, Bearer
// key) and answers with answer, or HTTP status, after delay.
type jev struct {
	status int
	answer string
	delay  time.Duration
	calls  atomic.Int32
	bad    atomic.Value // what was wrong with a request
}

func (j *jev) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		j.calls.Add(1)
		var req struct {
			State     string
			Model     string
			Questions map[string]struct {
				Type, Instructions string
				Criteria           map[string]string
			}
		}
		err := json.NewDecoder(r.Body).Decode(&req)
		q, ok := req.Questions["catastrophe"]
		switch {
		case r.Method != http.MethodPost || r.URL.Path != "/v1/systemone":
			j.bad.Store("endpoint " + r.Method + " " + r.URL.Path)
		case r.Header.Get("Authorization") != "Bearer "+testKey || r.Header.Get("Content-Type") != "application/json":
			j.bad.Store("headers")
		case err != nil || !ok || q.Type != "noul" || q.Instructions == "" || q.Criteria["true"] == "" || req.Model != "jev-1.14.0":
			j.bad.Store("body")
		case req.State != "Exchange hack drains BTC reserves":
			j.bad.Store("state " + req.State) // the headline alone
		}
		time.Sleep(j.delay)
		if j.status != 0 {
			http.Error(w, "no", j.status)
			return
		}
		_, _ = io.WriteString(w, j.answer)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func answer(p float64) string {
	b, _ := json.Marshal(map[string]any{"model": "jev-1.14.0", "answers": map[string]any{"catastrophe": map[string]any{"type": "noul", "noul": p}},
		"usage": map[string]int{"input_tokens": 12, "output_tokens": 0}})
	return string(b)
}

func guard(feed string, c news.Classifier) *news.Guard {
	return &news.Guard{RSS: &news.RSS{Feeds: []string{feed}, Keywords: []string{"hack"}, Lookback: 24 * time.Hour, Client: http.DefaultClient},
		Classifier: c, Threshold: 0.9, Timeout: 200 * time.Millisecond,
		Question: news.Question{Type: "noul", Instructions: "Is this a catastrophe for the asset?", Model: "jev-1.14.0",
			Criteria: map[string]string{"true": "the asset is at risk", "false": "noise"}}}
}

func client(url, key string) *typesafe.Client {
	return &typesafe.Client{URL: url + "/v1/systemone", Key: config.Secret(key), KeyVar: "X_TYPESAFE_API_KEY", HTTP: http.DefaultClient}
}

// The failure table: an unreadable feed is an error (the caller keeps the
// position); a matched headline with the classifier unavailable, slow,
// failing, rate limited, overloaded or without its key is a catastrophe;
// noise is not; no classifier makes every match a catastrophe. The key never
// appears in an error or a finding.
func TestFailureTable(t *testing.T) {
	const title = "Exchange hack drains BTC reserves"
	cases := map[string]struct {
		feedStatus  int
		jev         *jev
		key         string
		noClassifer bool
		wantErr     bool
		catastrophe bool
		classified  bool
	}{
		"feed unreadable":       {feedStatus: 500, jev: &jev{answer: answer(0.1)}, key: testKey, wantErr: true},
		"jev says noise":        {jev: &jev{answer: answer(0.2)}, key: testKey, classified: true},
		"jev says catastrophe":  {jev: &jev{answer: answer(0.95)}, key: testKey, catastrophe: true, classified: true},
		"at the threshold":      {jev: &jev{answer: answer(0.9)}, key: testKey, catastrophe: true, classified: true},
		"jev down":              {jev: &jev{status: 503}, key: testKey, catastrophe: true},
		"jev timeout":           {jev: &jev{answer: answer(0.1), delay: time.Second}, key: testKey, catastrophe: true},
		"jev error":             {jev: &jev{status: 400}, key: testKey, catastrophe: true},
		"jev 429":               {jev: &jev{status: 429}, key: testKey, catastrophe: true},
		"jev 529":               {jev: &jev{status: 529}, key: testKey, catastrophe: true},
		"jev malformed answer":  {jev: &jev{answer: `{"model":"jev-1.14.0","answers":{}}`}, key: testKey, catastrophe: true},
		"key missing":           {jev: &jev{answer: answer(0.1)}, catastrophe: true},
		"no classifier (regex)": {noClassifer: true, catastrophe: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var c news.Classifier
			if !tc.noClassifer {
				c = client(tc.jev.server(t).URL, tc.key)
			}
			g := guard(feedServer(t, title, tc.feedStatus).URL, c)
			fs, err := g.Check(context.Background(), "BTC/USDC:USDC", at)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err %v, want error %v", err, tc.wantErr)
			}
			if err != nil {
				if strings.Contains(err.Error(), testKey) {
					t.Fatal("the key is in the error")
				}
				return
			}
			if len(fs) != 1 {
				t.Fatalf("findings %+v; want the one headline", fs)
			}
			f := fs[0]
			if f.Catastrophe != tc.catastrophe || f.Classified != tc.classified || f.Headline != title {
				t.Fatalf("finding %+v (%s); want catastrophe %v classified %v", f, f.Why(g.Threshold), tc.catastrophe, tc.classified)
			}
			if f.Classified && f.Verdict.Model != "jev-1.14.0" {
				t.Fatalf("model %q; want the answer's", f.Verdict.Model)
			}
			if !f.Until.Equal(time.Date(2026, 11, 3, 11, 30, 0, 0, time.UTC)) {
				t.Fatalf("until %v; want the headline's time plus the lookback", f.Until)
			}
			if strings.Contains(f.Why(g.Threshold), testKey) || (f.Err != nil && strings.Contains(f.Err.Error(), testKey)) {
				t.Fatal("the key is in the finding")
			}
			if tc.jev != nil {
				if bad, _ := tc.jev.bad.Load().(string); bad != "" {
					t.Fatalf("request not in the TSDOC shape: %s", bad)
				}
			}
		})
	}
}

// A headline is classified once: the same one in a later check reuses the
// verdict; a headline without the asset or a keyword is never sent.
func TestHeadlineIsClassifiedOnce(t *testing.T) {
	j := &jev{answer: answer(0.2)}
	g := guard(feedServer(t, "Exchange hack drains BTC reserves", 0).URL, client(j.server(t).URL, testKey))
	for range 3 {
		if _, err := g.Check(context.Background(), "BTC/USDC:USDC", at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.Check(context.Background(), "ETH/USDC:USDC", at); err != nil {
		t.Fatal(err)
	}
	if n := j.calls.Load(); n != 1 {
		t.Fatalf("classifier called %d times; want 1", n)
	}
}

// Before an entry (equity), any catastrophe is not clear, with the reason.
func TestClearBeforeAnEntry(t *testing.T) {
	for p, want := range map[float64]bool{0.2: true, 0.95: false} {
		j := &jev{answer: answer(p)}
		g := guard(feedServer(t, "Exchange hack drains BTC reserves", 0).URL, client(j.server(t).URL, testKey))
		ok, why, err := g.Clear(context.Background(), "BTC/USDC:USDC", at)
		if err != nil || ok != want || (!ok && !strings.Contains(why, "catastrophe")) {
			t.Fatalf("yes %v: clear %v %q %v; want %v", p, ok, why, err, want)
		}
	}
}
