package news

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

var at = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir("../../testdata")))
	mux.HandleFunc("/broken", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", 500) })
	mux.HandleFunc("/garbage", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<rss><channel><item>")) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func checker(srv *httptest.Server, feeds ...string) *RSS {
	var urls []string
	for _, f := range feeds {
		urls = append(urls, srv.URL+"/"+f)
	}
	return &RSS{Feeds: urls, Keywords: testKeywords, Aliases: map[string][]string{"BTC": {"bitcoin"}},
		Lookback: 24 * time.Hour, Client: srv.Client()}
}

func TestRSS(t *testing.T) {
	srv := server(t)
	cases := []struct {
		symbol string
		feeds  []string
		clear  bool
	}{
		{"AAA/USDT", []string{"news_rss.xml"}, false},                 // hack, recent
		{"BTC/USDT", []string{"news_rss.xml"}, false},                 // "paused", matched through the alias
		{"CCC/USDT", []string{"news_rss.xml"}, true},                  // mentioned, no alarming keyword
		{"DDD/USDT", []string{"news_rss.xml"}, true},                  // bankruptcy, but a month old
		{"ZZZ/USDT", []string{"news_rss.xml"}, true},                  // not mentioned
		{"EEE-USD", []string{"news_rss.xml", "news_atom.xml"}, false}, // Atom delisting
	}
	for _, tc := range cases {
		ok, reason, err := checker(srv, tc.feeds...).Clear(context.Background(), tc.symbol, at)
		if err != nil {
			t.Fatalf("%s: %v", tc.symbol, err)
		}
		if ok != tc.clear {
			t.Errorf("%s: clear = %v (%s), want %v", tc.symbol, ok, reason, tc.clear)
		}
		if !ok && reason == "" {
			t.Errorf("%s: a block needs the headline as reason", tc.symbol)
		}
	}
}

func TestFeedFailuresAreErrors(t *testing.T) {
	srv := server(t)
	for _, f := range []string{"broken", "garbage", "missing.xml"} {
		if _, _, err := checker(srv, "news_rss.xml", f).Clear(context.Background(), "ZZZ/USDT", at); err == nil {
			t.Errorf("%s: a failing feed must be an error (fail-closed upstream)", f)
		}
	}
}

// testKeywords stand in for a strategy's policy: the engine has no default list.
var testKeywords = []string{"hack", "paused", "bankruptcy", "delisting", "halt"}

func TestKeywordsMatchWholeWords(t *testing.T) {
	if mentions("the shalt of it", []string{"halt"}) {
		t.Error("keyword matched inside another word")
	}
	if !mentions("Trading HALT on AAA", []string{"halt"}) {
		t.Error("keyword missed")
	}
}

// A feed URL's query (where tokens live) never reaches an
// error or a reason, which end up in the journal.
func TestFeedQueryIsRedacted(t *testing.T) {
	srv := server(t)
	for _, f := range []string{"broken?apikey=hunter2", "news_rss.xml?apikey=hunter2"} {
		ok, reason, err := checker(srv, f).Clear(context.Background(), "AAA/USDT", at)
		msg := reason
		if err != nil {
			msg += err.Error()
		}
		if ok || strings.Contains(msg, "hunter2") {
			t.Errorf("%s: clear=%v, message leaks the query: %s", f, ok, msg)
		}
	}
	dead := &RSS{Feeds: []string{"http://127.0.0.1:1/rss?token=hunter2"}, Lookback: time.Hour, Client: srv.Client()}
	if _, _, err := dead.Clear(context.Background(), "AAA/USDT", at); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("connection error leaks the query: %v", err)
	}
}

// oneHeadline serves one recent RSS item.
func oneHeadline(t *testing.T, title string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<rss><channel><item><title>" + title + "</title></item></channel></rss>"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A HIP-3 symbol names a ticker and a dex: a headline naming either, or an
// alias of either, is a hit. The first piece of such a symbol is only the dex.
func TestHIP3SymbolIsSearchedByTickerAndDex(t *testing.T) {
	const symbol = "XYZ-GOOGL/USDC:USDC"
	for _, tc := range []struct {
		headline string
		aliases  map[string][]string
		clear    bool
	}{
		{"GOOGL trading halted", nil, false},
		{"Google trading halted", map[string][]string{"GOOGL": {"google"}}, false},
		{"XYZ trading halted", nil, false},
		{"Google trading halted", map[string][]string{"XYZ": {"googl", "google"}}, false},
		{"Google trading halted", nil, true},
		{"AAPL trading halted", map[string][]string{"GOOGL": {"google"}, "XYZ": {"xyzdex"}}, true},
	} {
		srv := oneHeadline(t, tc.headline)
		r := &RSS{Feeds: []string{srv.URL}, Keywords: []string{"halted"}, Aliases: tc.aliases, Lookback: time.Hour, Client: srv.Client()}
		ok, reason, err := r.Clear(context.Background(), symbol, at)
		if err != nil || ok != tc.clear {
			t.Errorf("%q aliases %v: clear = %v (%s) err %v, want %v", tc.headline, tc.aliases, ok, reason, err, tc.clear)
		}
	}
}

// Any other symbol is searched by its first piece alone, as ever.
func TestOtherSymbolsKeepOneBase(t *testing.T) {
	r := &RSS{Aliases: map[string][]string{"EEE": {"eee coin"}, "USD": {"dollar"}, "USDT": {"tether"}, "KPEPE": {"pepe"}}}
	for symbol, want := range map[string]assetNames{
		"EEE-USD":         {[]string{"EEE"}, []string{"eee coin"}},
		"EEE/USDT":        {[]string{"EEE"}, []string{"eee coin"}},
		"EEE/USDC:USDC":   {[]string{"EEE"}, []string{"eee coin"}},
		"EEE:USDT":        {[]string{"EEE"}, []string{"eee coin"}},
		"ZZZ-USD":         {[]string{"ZZZ"}, nil},
		"kPEPE/USDC:USDC": {nil, []string{"kpepe", "pepe"}},
	} {
		if got := r.names(symbol); !slices.Equal(got.exact, want.exact) || !slices.Equal(got.anyCase, want.anyCase) {
			t.Errorf("%s: names %+v, want %+v", symbol, got, want)
		}
	}
}

// A short name from the symbol matches as written: tickers that are words do
// not match the word. A longer name, and any alias, matches in any case.
func TestShortNamesMatchInExactCase(t *testing.T) {
	for _, tc := range []struct {
		symbol, headline string
		aliases          map[string][]string
		clear            bool
	}{
		{"XYZ-NOW/USDC:USDC", "Bitcoin exchange hack, users wait now", nil, true},
		{"XYZ-NOW/USDC:USDC", "NOW hack disclosed", nil, false},
		{"XYZ-NET/USDC:USDC", "Hack at retailer cuts net income", nil, true},
		{"PARA-AVGO/USDC:USDC", "Fraude para o mercado: hack at bank", nil, true},
		{"PARA-AVGO/USDC:USDC", "AVGO hack", nil, false},
		{"NOT/USDC:USDC", "Exchange says funds were not hack victims", nil, true},
		{"NOT/USDC:USDC", "NOT token hack", nil, false},
		{"S/USDC:USDC", "Bitcoin's hack fears fade", nil, true},
		{"S/USDC:USDC", "U.S. exchange hack", nil, true},
		{"S/USDC:USDC", "S&amp;P 500 hack", nil, true},
		{"S/USDC:USDC", "S network hack", nil, true},
		{"S/USDC:USDC", "sonic hack", map[string][]string{"S": {"sonic"}}, false},
		{"S/USDC:USDC", "U.S. exchange hack", map[string][]string{"S": {"sonic"}}, true},
		{"W/USDC:USDC", "W. Virginia bank hack", nil, true},
		{"XYZ-S/USDC:USDC", "U.S. exchange hack", nil, true},
		{"XYZ-S/USDC:USDC", "XYZ hack", nil, false},
		{"XYZ-S/USDC:USDC", "sonic hack", map[string][]string{"S": {"sonic"}}, false},
		{"X-GOOGL/USDC:USDC", "X hack", nil, true},
		{"X-GOOGL/USDC:USDC", "GOOGL hack", nil, false},
		{"X-GOOGL/USDC:USDC", "xdex hack", map[string][]string{"X": {"xdex"}}, false},
		{"BTC/USDC:USDC", "BTC exchange hack", nil, false},
		{"BTC/USDC:USDC", "btc exchange hack", nil, true},
		{"BTC/USDC:USDC", "btc exchange hack", map[string][]string{"BTC": {"btc"}}, false},
		{"BTC/USDC:USDC", "bitcoin exchange hack", map[string][]string{"BTC": {"Bitcoin"}}, false},
		{"BTC/USDC:USDC", "BITCOIN EXCHANGE HACK", map[string][]string{"BTC": {"bitcoin"}}, false},
		{"XYZ-GOOGL/USDC:USDC", "googl hack", nil, false},
		{"kPEPE/USDC:USDC", "KPEPE hack", nil, false},
	} {
		srv := oneHeadline(t, tc.headline)
		r := &RSS{Feeds: []string{srv.URL}, Keywords: []string{"hack"}, Aliases: tc.aliases, Lookback: time.Hour, Client: srv.Client()}
		ok, reason, err := r.Clear(context.Background(), tc.symbol, at)
		if err != nil || ok != tc.clear {
			t.Errorf("%s %q aliases %v: clear = %v (%s) err %v, want %v", tc.symbol, tc.headline, tc.aliases, ok, reason, err, tc.clear)
		}
	}
}

// The refusal at load and the matcher agree: an alias is called unsearchable
// exactly when a headline carrying it does not hit.
func TestUnsearchableAliasAgreesWithTheMatcher(t *testing.T) {
	for _, alias := range []string{"sonic", "Sonic", "s0nic", "比特币", "sonic labs", "dog-wif-hat", "brk.b", "sonic ", " sonic", "a_b", ""} {
		aliases := map[string][]string{"S": {alias}}
		srv := oneHeadline(t, alias+" hack")
		r := &RSS{Feeds: []string{srv.URL}, Keywords: testKeywords, Aliases: aliases, Lookback: time.Hour, Client: srv.Client()}
		hs, err := r.Hits(context.Background(), "S/USDC:USDC", at)
		if err != nil {
			t.Fatal(err)
		}
		got, bad := UnsearchableAlias("S/USDC:USDC", aliases)
		if bad != (len(hs) == 0) || bad && got != alias {
			t.Errorf("alias %q: unsearchable = %v (%q), hits = %d", alias, bad, got, len(hs))
		}
	}
}
