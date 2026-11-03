package news

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type noise struct{ calls atomic.Int32 }

func (n *noise) Classify(context.Context, string, Question) (Verdict, error) {
	n.calls.Add(1)
	return Verdict{Yes: 0.1}, nil
}

// The judged headlines are kept until the end of their lookback, no longer:
// a new one dropped those past it, never one still within it.
func TestSeenKeepsOnlyTheHeadlinesWithinTheLookback(t *testing.T) {
	var titles atomic.Value
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<rss><channel>")
		for _, title := range titles.Load().([]string) {
			_, _ = io.WriteString(w, "<item><title>"+title+"</title></item>")
		}
		_, _ = io.WriteString(w, "</channel></rss>")
	}))
	defer feed.Close()
	c := &noise{}
	g := &Guard{RSS: &RSS{Feeds: []string{feed.URL}, Keywords: []string{"hack"}, Lookback: 24 * time.Hour, Client: http.DefaultClient},
		Classifier: c, Threshold: 0.9}
	t0 := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	for _, step := range []struct {
		h      float64
		titles []string
	}{{0, []string{"BTC hack one"}}, {1, []string{"BTC hack one", "BTC hack two"}}, {24.5, []string{"BTC hack three"}}} {
		titles.Store(step.titles)
		if _, err := g.Check(context.Background(), "BTC", t0.Add(time.Duration(step.h*float64(time.Hour)))); err != nil {
			t.Fatal(err)
		}
	}
	var kept []string
	for k := range g.seen {
		kept = append(kept, k[strings.Index(k, "\n")+1:])
	}
	slices.Sort(kept)
	if !slices.Equal(kept, []string{"BTC hack three", "BTC hack two"}) || c.calls.Load() != 3 {
		t.Fatalf("kept %q after %d calls; want the two within the lookback, each judged once", kept, c.calls.Load())
	}
}
