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

// The cap counts characters: text under it in characters goes whole, however
// many bytes it takes.
func TestClassifierCapCountsCharacters(t *testing.T) {
	c := &fakeClassifier{yes: 0.1}
	desc := "BTC exchange hacked " + strings.Repeat("é", MaxClassifierText-100)
	items := [][2]string{{"Markets update", desc}}
	if _, err := guardOn(t, c, &items).Check(context.Background(), "BTC/USDC:USDC", at); err != nil {
		t.Fatal(err)
	}
	if want := "Markets update\n" + desc; len(c.states) != 1 || c.states[0] != want {
		t.Errorf("text under the cap was changed: %d characters", utf8.RuneCountInString(c.states[0]))
	}

	// a title over half the cap is kept too while the whole text fits
	c = &fakeClassifier{yes: 0.1}
	title := strings.Repeat("ú", MaxClassifierText*3/4)
	items = [][2]string{{title, "BTC exchange hacked"}}
	if _, err := guardOn(t, c, &items).Check(context.Background(), "BTC/USDC:USDC", at); err != nil {
		t.Fatal(err)
	}
	if want := title + "\nBTC exchange hacked"; len(c.states) != 1 || c.states[0] != want {
		t.Errorf("a long title under the cap was cut to %d characters", utf8.RuneCountInString(c.states[0]))
	}

	// past the cap, a title in many bytes takes no more room than its characters
	c = &fakeClassifier{yes: 0.1}
	items = [][2]string{{strings.Repeat("ú", 500) + " BTC hacked", strings.Repeat("é ", MaxClassifierText)}}
	if _, err := guardOn(t, c, &items).Check(context.Background(), "BTC/USDC:USDC", at); err != nil {
		t.Fatal(err)
	}
	if n := utf8.RuneCountInString(c.states[0]); n < MaxClassifierText-1 || n > MaxClassifierText { // a trailing space is trimmed
		t.Errorf("read %d characters, want the whole %d", n, MaxClassifierText)
	}
}

// Past the cap the classifier reads the title and what surrounds the matched
// words, wherever they are, and it always decides: no verdict comes without a call.
func TestClassifierAlwaysReadsWhatMatched(t *testing.T) {
	pad := strings.Repeat("é ", MaxClassifierText)
	for name, tc := range map[string]struct {
		title, desc string
		want        []string // in what the classifier reads
		wantGap     bool     // two pieces
	}{
		"both past the cap":     {"Markets update", pad + "Unlike BTC, it was never hacked." + pad, []string{"Markets update", "Unlike BTC, it was never hacked."}, false},
		"name far from keyword": {"Report", "Exchange hacked. " + pad + "BTC unaffected.", []string{"Report", "Exchange hacked", "BTC unaffected"}, true},
		"keyword far from name": {"Report", "BTC up. " + pad + "Exchange hacked.", []string{"BTC up", "Exchange hacked"}, true},
		"the closest pair":      {"Report", "BTC up. " + pad + "BTC exchange hacked." + pad, []string{"BTC exchange hacked"}, false},
		"a closer pair later":   {"Report", "BTC up. " + pad + "Funds hacked. " + pad + "BTC exchange hacked." + pad, []string{"BTC exchange hacked"}, false},
		"name in the title":     {"BTC update", pad + "Exchange hacked." + pad, []string{"BTC update", "Exchange hacked"}, false},
		"keyword in the title":  {"Exchange hacked", pad + "Unlike BTC, fine." + pad, []string{"Exchange hacked", "Unlike BTC, fine."}, false},
		"both in the title":     {"BTC hacked", "Long story. " + pad, []string{"BTC hacked", "Long story"}, false},
		"a title over the cap":  {strings.Repeat("ú", 2*MaxClassifierText), pad + "BTC hacked." + pad, []string{"úú", "BTC hacked"}, false},
		"match cut off a title": {strings.Repeat("ú ", MaxClassifierText) + "BTC hacked", pad, []string{"úú"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			c := &fakeClassifier{yes: 0.1}
			items := [][2]string{{tc.title, tc.desc}}
			fs, err := guardOn(t, c, &items).Check(context.Background(), "BTC/USDC:USDC", at)
			if err != nil || len(fs) != 1 || len(c.states) != 1 {
				t.Fatalf("findings %+v, err %v, calls %d", fs, err, len(c.states))
			}
			if fs[0].Catastrophe || !fs[0].Classified {
				t.Errorf("the classifier said noise, the finding says %+v", fs[0])
			}
			got := c.states[0]
			if n := utf8.RuneCountInString(got); n > MaxClassifierText || !utf8.ValidString(got) {
				t.Errorf("%d characters, valid %v", n, utf8.ValidString(got))
			}
			for _, w := range tc.want {
				if !strings.Contains(strings.ReplaceAll(got, "ú ", "ú"), w) {
					t.Errorf("%q is not in what the classifier read", w)
				}
			}
			if gap := strings.Contains(got, excerptGap); gap != tc.wantGap {
				t.Errorf("two pieces: %v, want %v", gap, tc.wantGap)
			}
		})
	}
}

// An excerpt uses the room it has.
func TestExcerptFillsItsSize(t *testing.T) {
	rs := []rune(strings.Repeat("é ", 500) + "BTC hacked" + strings.Repeat(" é", 500))
	isName, isKeyword := func(w string) bool { return w == "BTC" }, func(w string) bool { return w == "hacked" }
	if got := excerpt(rs, 100, isName, isKeyword); utf8.RuneCountInString(got) != 100 || !strings.Contains(got, "BTC hacked") {
		t.Errorf("%d characters: %q", utf8.RuneCountInString(got), got)
	}
	end := []rune(strings.Repeat("é ", 500) + "BTC hacked")
	if got := excerpt(end, 100, isName, isKeyword); utf8.RuneCountInString(got) != 100 || !strings.HasSuffix(got, "BTC hacked") {
		t.Errorf("at the end, %d characters: %q", utf8.RuneCountInString(got), got)
	}
	if got := excerpt(rs, 100, nil, nil); got != string(rs[:100]) {
		t.Errorf("nothing to find: %q", got)
	}
}

// Words only inside a tag, such as a link's address, are not a headline.
func TestWordsInsideATagDoNotMatch(t *testing.T) {
	c := &fakeClassifier{yes: 0.9}
	items := [][2]string{{"Markets update", `<a href="https://x.invalid/btc-hacked">read more</a>`}}
	fs, err := guardOn(t, c, &items).Check(context.Background(), "BTC/USDC:USDC", at)
	if err != nil || len(fs) != 0 || len(c.states) != 0 {
		t.Fatalf("findings %+v, err %v, calls %d", fs, err, len(c.states))
	}
}
