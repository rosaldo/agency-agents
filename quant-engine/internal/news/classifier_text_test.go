package news

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeClassifier records what it is asked and answers yes.
type fakeClassifier struct {
	mu     sync.Mutex
	states []string
	yes    float64
}

func (c *fakeClassifier) Classify(_ context.Context, state string, _ Question) (Verdict, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.states = append(c.states, state)
	return Verdict{Model: "fake", Yes: c.yes}, nil
}

// guardOn serves *items, read at each fetch: each is {title, description}.
func guardOn(t *testing.T, c Classifier, items *[][2]string) *Guard {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<rss><channel>"))
		for _, it := range *items {
			var title, desc strings.Builder
			_ = xml.EscapeText(&title, []byte(it[0]))
			_ = xml.EscapeText(&desc, []byte(it[1]))
			_, _ = w.Write([]byte("<item><title>" + title.String() + "</title><description>" + desc.String() + "</description></item>"))
		}
		_, _ = w.Write([]byte("</channel></rss>"))
	}))
	t.Cleanup(srv.Close)
	return &Guard{RSS: &RSS{Feeds: []string{srv.URL}, Keywords: []string{"hacked"}, Lookback: time.Hour, Client: srv.Client()},
		Classifier: c, Threshold: 0.5}
}

// A catastrophe told only in the description reaches the classifier, as
// plain text; the journal keeps the title.
func TestClassifierReadsTheDescription(t *testing.T) {
	c := &fakeClassifier{yes: 0.9}
	items := [][2]string{{"Markets update", `<p>BTC exchange <b>hacked</b> &amp; drained</p>`}}
	fs, err := guardOn(t, c, &items).Check(context.Background(), "BTC/USDC:USDC", at)
	if err != nil || len(fs) != 1 {
		t.Fatalf("findings %v, err %v", fs, err)
	}
	if want := "Markets update\nBTC exchange hacked & drained"; len(c.states) != 1 || c.states[0] != want {
		t.Errorf("classifier read %q, want %q", c.states, want)
	}
	if fs[0].Headline != "Markets update" || !fs[0].Catastrophe {
		t.Errorf("finding %+v", fs[0])
	}
}

// A feed that gives no title journals the start of the text, never an empty line.
func TestHeadlineWithoutTitleStaysReadable(t *testing.T) {
	items := [][2]string{{"", "BTC exchange hacked " + strings.Repeat("and more ", 100)}}
	fs, err := guardOn(t, &fakeClassifier{}, &items).Check(context.Background(), "BTC/USDC:USDC", at)
	if err != nil || len(fs) != 1 {
		t.Fatalf("findings %v, err %v", fs, err)
	}
	if h := fs[0].Headline; !strings.HasPrefix(h, "BTC exchange hacked") || utf8.RuneCountInString(h) > 200 {
		t.Errorf("headline %q", h)
	}
}

// The verdict is kept per feed and text: the same title with another
// description is another headline, the same text is not judged twice.
func TestVerdictIsCachedByTheTextJudged(t *testing.T) {
	c := &fakeClassifier{yes: 0.1}
	items := [][2]string{{"Markets update", "BTC exchange hacked"}}
	g := guardOn(t, c, &items)
	for range 2 {
		if _, err := g.Check(context.Background(), "BTC/USDC:USDC", at); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.states) != 1 {
		t.Fatalf("same text judged %d times: %q", len(c.states), c.states)
	}
	items[0][1] = "BTC exchange hacked again, funds gone"
	if _, err := g.Check(context.Background(), "BTC/USDC:USDC", at); err != nil {
		t.Fatal(err)
	}
	if len(c.states) != 2 {
		t.Fatalf("a new description served the old verdict: %q", c.states)
	}
}

// The text is capped in characters. Matched words within the cap are judged;
// past it the classifier could not read them, so the hit is a catastrophe
// with no call.
func TestClassifierTextIsCapped(t *testing.T) {
	pad := strings.Repeat("é", MaxClassifierText)

	c := &fakeClassifier{yes: 0.1}
	items := [][2]string{{"Markets update", "BTC exchange hacked " + pad}}
	fs, err := guardOn(t, c, &items).Check(context.Background(), "BTC/USDC:USDC", at)
	if err != nil || len(fs) != 1 || fs[0].Catastrophe || len(c.states) != 1 {
		t.Fatalf("within the cap: findings %+v, err %v, calls %d", fs, err, len(c.states))
	}
	if n := utf8.RuneCountInString(c.states[0]); n != MaxClassifierText || !utf8.ValidString(c.states[0]) {
		t.Errorf("classifier read %d characters, want %d, valid", n, MaxClassifierText)
	}

	c = &fakeClassifier{yes: 0.1}
	items = [][2]string{{"Markets update", pad + " BTC exchange hacked"}}
	fs, err = guardOn(t, c, &items).Check(context.Background(), "BTC/USDC:USDC", at)
	if err != nil || len(fs) != 1 || !fs[0].Catastrophe || len(c.states) != 0 {
		t.Fatalf("past the cap: findings %+v, err %v, calls %d", fs, err, len(c.states))
	}
	if why := fs[0].Why(0.5); !strings.Contains(why, "past the first") {
		t.Errorf("why: %s", why)
	}
}
