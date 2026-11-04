// Package news is a generic RSS/Atom news check for the engine's news guard:
// an entry is not clear when a recent headline names the asset together with
// an alarming keyword. No feed and no keyword is built in: the strategy's
// news policy supplies them.
package news

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
	"unicode"

	"github.com/rosaldo/agency-agents/quant-engine/market"
)

type RSS struct {
	Feeds    []string
	Keywords []string            // lowercase single words
	Aliases  map[string][]string // base asset (on HIP-3: ticker or dex) -> extra names, e.g. BTC: [bitcoin]
	Lookback time.Duration
	Client   *http.Client
}

type feed struct {
	Items []struct {
		Title       string `xml:"title"`
		Description string `xml:"description"`
		PubDate     string `xml:"pubDate"`
	} `xml:"channel>item"`
	Entries []struct {
		Title     string `xml:"title"`
		Summary   string `xml:"summary"`
		Updated   string `xml:"updated"`
		Published string `xml:"published"`
	} `xml:"entry"`
}

type headline struct {
	text string
	when string
}

// Hit is a recent headline naming the asset with a keyword.
type Hit struct {
	Text string    // the headline (its title)
	Feed string    // the feed URL without its query and fragment
	When time.Time // zero when undated
}

// Clear reports whether no recent headline in any feed names symbol's base
// asset with a keyword. Any feed that cannot be read or parsed is an error,
// which the engine treats as not clear.
func (r *RSS) Clear(ctx context.Context, symbol string, at time.Time) (bool, string, error) {
	hs, err := r.Hits(ctx, symbol, at)
	if err != nil || len(hs) == 0 {
		return err == nil, "", err
	}
	return false, fmt.Sprintf("%q (%s)", hs[0].Text, hs[0].Feed), nil
}

// Hits lists the recent headlines of every feed that name symbol's base
// asset with a keyword. Any feed that cannot be read or parsed is an error.
func (r *RSS) Hits(ctx context.Context, symbol string, at time.Time) ([]Hit, error) {
	names := r.names(symbol)
	var out []Hit
	for _, url := range r.Feeds {
		hs, err := r.fetch(ctx, url)
		if err != nil {
			return nil, err
		}
		for _, h := range hs {
			t, dated := parseTime(h.when)
			if dated && t.Before(at.Add(-r.Lookback)) {
				continue // undated headlines count as recent: fail closed
			}
			if mentions(h.text, names) && mentions(h.text, r.Keywords) {
				out = append(out, Hit{Text: firstLine(h.text), Feed: redact(url), When: t})
			}
		}
	}
	return out, nil
}

// names lists what a headline can call symbol's asset: its base and that
// base's aliases; on a HIP-3 symbol, the ticker and the dex, with the aliases
// of each (news of the dex matters to its markets too).
func (r *RSS) names(symbol string) []string {
	bases := []string{strings.FieldsFunc(symbol, func(c rune) bool { return c == '/' || c == '-' || c == ':' })[0]}
	if dex, ticker, ok := market.HIP3(symbol); ok {
		bases = []string{ticker, dex}
	}
	var names []string
	for _, b := range bases {
		names = append(names, strings.ToLower(b))
		for _, a := range r.Aliases[strings.ToUpper(b)] {
			names = append(names, strings.ToLower(a))
		}
	}
	return names
}

// redact drops a feed URL's query and fragment, where access tokens live.
func redact(raw string) string {
	u, err := neturl.Parse(raw)
	if err != nil {
		return "(unparseable feed URL)"
	}
	u.RawQuery, u.Fragment, u.User = "", "", nil
	return u.String()
}

func (r *RSS) fetch(ctx context.Context, rawURL string) ([]headline, error) {
	url := redact(rawURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if ue := (*neturl.Error)(nil); errors.As(err, &ue) {
		ue.URL = url
	}
	if err != nil {
		return nil, fmt.Errorf("news feed %s: %w", url, err)
	}
	resp, err := r.Client.Do(req)
	if ue := (*neturl.Error)(nil); errors.As(err, &ue) {
		ue.URL = url // the client's error repeats the full URL
	}
	if err != nil {
		return nil, fmt.Errorf("news feed %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("news feed %s: HTTP %d", url, resp.StatusCode)
	}
	var f feed
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 5<<20)).Decode(&f); err != nil {
		return nil, fmt.Errorf("news feed %s: not RSS or Atom: %w", url, err)
	}
	var hs []headline
	for _, i := range f.Items {
		hs = append(hs, headline{i.Title + "\n" + i.Description, i.PubDate})
	}
	for _, e := range f.Entries {
		when := e.Published
		if when == "" {
			when = e.Updated
		}
		hs = append(hs, headline{e.Title + "\n" + e.Summary, when})
	}
	return hs, nil
}

func parseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC1123Z, time.RFC1123, time.RFC3339, "Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// mentions reports whether text contains any of the words as a whole word.
func mentions(text string, words []string) bool {
	set := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(c rune) bool { return !unicode.IsLetter(c) && !unicode.IsDigit(c) }) {
		set[w] = true
	}
	for _, w := range words {
		if set[w] {
			return true
		}
	}
	return false
}

func firstLine(s string) string { return strings.SplitN(s, "\n", 2)[0] }
