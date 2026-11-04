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
	"html"
	"io"
	"net/http"
	neturl "net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
	title, description string // description as plain text: a link's address is not the news
	when               string
}

// MaxClassifierText caps the text of one headline sent to the classifier, in
// characters: a whole title and summary fit; of a full article pasted in a
// feed it reads the title and what surrounds the matched words.
const MaxClassifierText = 2000

// Hit is a recent headline naming the asset with a keyword. The keywords match
// the title and the description, so the classifier reads both.
type Hit struct {
	Text           string    // the headline for people: its title
	ClassifierText string    // what the classifier reads: the asset, then title and description without HTML, capped
	Feed           string    // the feed URL without its query and fragment
	When           time.Time // zero when undated
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
			if full := h.title + "\n" + h.description; names.in(full) && mentions(full, r.Keywords) {
				asset := assetLine(symbol) + "\n"
				text := classifierText(h, names, r.Keywords, MaxClassifierText-utf8.RuneCountInString(asset))
				out = append(out, Hit{Text: label(h, text), ClassifierText: asset + text, Feed: redact(url), When: t})
			}
		}
	}
	return out, nil
}

// assetNames is what a headline can call an asset. A short name from the
// symbol is matched as written (NOT, S, NET and NOW are tickers, not, s, net
// and now are words); a longer one and every alias in any case. An alias of
// the same word brings any case back: {BTC: [btc]}. A name of one character
// never matches by itself.
type assetNames struct{ exact, anyCase []string }

const maxExactName = 4 // characters

func (n assetNames) in(text string) bool {
	return mentionsExactly(text, n.exact) || mentions(text, n.anyCase)
}

// is reports whether one word is one of the names.
func (n assetNames) is(word string) bool { return n.in(word) }

// names lists symbol's base and that base's aliases; on a HIP-3 symbol, the
// ticker and the dex, with the aliases of each (news of the dex matters to its
// markets too).
func (r *RSS) names(symbol string) assetNames {
	bases := []string{baseOf(symbol)}
	if dex, ticker, ok := market.HIP3(symbol); ok {
		bases = []string{ticker, dex}
	}
	var n assetNames
	for _, b := range bases {
		switch size := utf8.RuneCountInString(b); {
		case size == 1: // the S of "U.S." and "S&P": only an alias can name such an asset
		case size <= maxExactName:
			n.exact = append(n.exact, b)
		default:
			n.anyCase = append(n.anyCase, strings.ToLower(b))
		}
		for _, a := range r.Aliases[strings.ToUpper(b)] {
			n.anyCase = append(n.anyCase, strings.ToLower(a))
		}
	}
	return n
}

func baseOf(symbol string) string {
	return strings.FieldsFunc(symbol, func(c rune) bool { return c == '/' || c == '-' || c == ':' })[0]
}

// assetLine tells the classifier whose news it is judging.
func assetLine(symbol string) string {
	if dex, ticker, ok := market.HIP3(symbol); ok {
		return "Asset: " + ticker + " (dex " + strings.ToLower(dex) + ")"
	}
	return "Asset: " + baseOf(symbol)
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
		hs = append(hs, headline{i.Title, plainText(i.Description), i.PubDate})
	}
	for _, e := range f.Entries {
		when := e.Published
		if when == "" {
			when = e.Updated
		}
		hs = append(hs, headline{e.Title, plainText(e.Summary), when})
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

// mentions reports whether text contains any of the lowercase words as a
// whole word, in any case.
func mentions(text string, words []string) bool { return mentionsExactly(strings.ToLower(text), words) }

// mentionsExactly is mentions in the words' own case.
func mentionsExactly(text string, words []string) bool {
	return slices.ContainsFunc(strings.FieldsFunc(text, func(c rune) bool { return !unicode.IsLetter(c) && !unicode.IsDigit(c) }),
		func(w string) bool { return slices.Contains(words, w) })
}

var htmlTag = regexp.MustCompile(`</?[a-zA-Z!][^>]*>`) // a tag, not a loose < as in "fees <1%"

func plainText(s string) string {
	return strings.Join(strings.Fields(html.UnescapeString(htmlTag.ReplaceAllString(s, " "))), " ")
}

// classifierText is the headline's title and description in at most limit
// characters; when they do not fit, the title and what surrounds the matched
// words, so the classifier always reads what matched.
func classifierText(h headline, names assetNames, keywords []string, limit int) string {
	title := strings.TrimSpace(h.title)
	if text := strings.TrimSpace(title + "\n" + h.description); utf8.RuneCountInString(text) <= limit {
		return text
	}
	if t := []rune(title); len(t) > limit/2 {
		title = string(t[:limit/2])
	}
	isName, isKeyword := names.is, func(w string) bool { return slices.Contains(keywords, strings.ToLower(w)) }
	if names.in(title) {
		isName = nil
	}
	if mentions(title, keywords) {
		isKeyword = nil
	}
	return strings.TrimSpace(title + "\n" + excerpt([]rune(h.description), limit-utf8.RuneCountInString(title)-1, isName, isKeyword))
}

const excerptGap = " ... "

// excerpt is at most size characters of rs around the closest pair of a word
// isName takes and one isKeyword takes (nil: not looked for); two pieces when
// the pair does not fit in one; the start of rs when there is nothing to find.
func excerpt(rs []rune, size int, isName, isKeyword func(string) bool) string {
	type span struct{ from, to int }
	var name, keyword, bestName, bestKeyword *span
	best := len(rs) + 1
	union := func(n, k *span) (int, int) {
		switch {
		case n == nil:
			return k.from, k.to
		case k == nil:
			return n.from, n.to
		}
		return min(n.from, k.from), max(n.to, k.to)
	}
	for i := 0; i < len(rs); i++ {
		if !unicode.IsLetter(rs[i]) && !unicode.IsDigit(rs[i]) {
			continue
		}
		j := i
		for j < len(rs) && (unicode.IsLetter(rs[j]) || unicode.IsDigit(rs[j])) {
			j++
		}
		w, at := string(rs[i:j]), &span{i, j}
		if isName != nil && isName(w) {
			name = at
		}
		if isKeyword != nil && isKeyword(w) {
			keyword = at
		}
		if (name != nil || isName == nil) && (keyword != nil || isKeyword == nil) && (name != nil || keyword != nil) {
			if from, to := union(name, keyword); to-from < best {
				best, bestName, bestKeyword = to-from, name, keyword
			}
		}
		i = j
	}
	piece := func(from, to, size int) string {
		start := max(0, from-(size-(to-from))/2)
		end := min(len(rs), start+size)
		return string(rs[max(0, end-size):end])
	}
	switch {
	case best > len(rs): // nothing to find here: what matched is in the part of the title kept
		return piece(0, 0, size)
	case best <= size || bestName == nil || bestKeyword == nil:
		from, to := union(bestName, bestKeyword)
		return piece(from, to, size)
	}
	half := (size - len(excerptGap)) / 2
	first, second := bestName, bestKeyword
	if second.from < first.from {
		first, second = second, first
	}
	return piece(first.from, first.to, half) + excerptGap + piece(second.from, second.to, half)
}

// label is the one line journaled for a headline: its title, or the start of
// its text when the feed gives none.
func label(h headline, text string) string {
	if t := strings.TrimSpace(h.title); t != "" {
		return strings.SplitN(t, "\n", 2)[0]
	}
	if r := []rune(text); len(r) > 200 {
		return string(r[:200])
	}
	return text
}
