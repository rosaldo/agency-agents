package config

import (
	"strings"
	"testing"

	"github.com/rosaldo/agency-agents/quant-engine/strategy"
	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
)

func TestNewsGuardDefaultsByAssetClass(t *testing.T) {
	for class, want := range map[string]string{"crypto": "none", "equity": "required"} {
		c, err := Parse([]byte(strings.Replace(validYAML, "asset_class: crypto", "asset_class: "+class, 1)), example.New())
		if err != nil {
			t.Fatal(err)
		}
		if c.NewsGuard != want {
			t.Errorf("%s: news_guard = %q, want %q", class, c.NewsGuard, want)
		}
	}
	c, err := Parse([]byte(strings.Replace(validYAML, "asset_class: crypto", "asset_class: crypto\nnews_guard: required", 1)), example.New())
	if err != nil || c.NewsGuard != "required" {
		t.Fatalf("explicit news_guard: %+v %v", c, err)
	}
	// an equity cannot opt out of the news check
	if _, err := Parse([]byte(strings.Replace(validYAML, "asset_class: crypto", "asset_class: equity\nnews_guard: none", 1)), example.New()); err == nil {
		t.Fatal("equity with news_guard none accepted")
	}
}

func TestAssetClassAndNewsGuardValidated(t *testing.T) {
	for name, y := range map[string]string{
		"missing class": strings.Replace(validYAML, "asset_class: crypto\n", "", 1),
		"unknown class": strings.Replace(validYAML, "asset_class: crypto", "asset_class: bonds", 1),
		"unknown guard": strings.Replace(validYAML, "asset_class: crypto", "asset_class: crypto\nnews_guard: maybe", 1),
	} {
		if _, err := Parse([]byte(y), example.New()); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestNewsPolicyChangesTheHash(t *testing.T) {
	if hashOf(t, validYAML) == hashOf(t, strings.Replace(validYAML, "asset_class: crypto", "asset_class: crypto\nnews_guard: required", 1)) {
		t.Fatal("news_guard did not change the parameter hash")
	}
}

// The news policy is the strategy's: it enters the hash, and the yml has no
// news block of its own.
func TestStrategyNewsPolicy(t *testing.T) {
	policy := func(feed string) strategy.Strategy {
		return withNews{example.New(), strategy.NewsPolicy{Feeds: []string{feed}, Keywords: []string{"halt"}}}
	}
	c, err := Parse([]byte(validYAML), policy("https://example.invalid/rss"))
	if err != nil || c.NewsPolicy() == nil || len(c.NewsPolicy().Feeds) != 1 {
		t.Fatalf("news policy: %+v %v", c, err)
	}
	if hashFor(t, validYAML, policy("https://example.invalid/rss")) == hashFor(t, validYAML, policy("https://example.invalid/other")) {
		t.Error("feeds did not change the parameter hash")
	}
	if _, err := Parse([]byte(validYAML+"news:\n  feeds: [https://example.invalid/rss]\n"), example.New()); err == nil {
		t.Error("a news block in the yml was accepted")
	}
}

type withNews struct {
	strategy.Strategy
	p strategy.NewsPolicy
}

func (w withNews) NewsPolicy() strategy.NewsPolicy { return w.p }

// Feeds without keywords would match no headline and leave the guard always
// clear: fail open. Parse refuses such a policy.
func TestNewsPolicyFeedsWithoutKeywordsRefused(t *testing.T) {
	s := withNews{example.New(), strategy.NewsPolicy{Feeds: []string{"https://example.invalid/rss"}}}
	if _, err := Parse([]byte(validYAML), s); err == nil || !strings.Contains(err.Error(), "keywords") {
		t.Fatalf("feeds without keywords: %v", err)
	}
}

func classified(c *strategy.NewsClassifier) strategy.Strategy {
	return withNews{example.New(), strategy.NewsPolicy{Feeds: []string{"https://example.invalid/rss"}, Keywords: []string{"hack"}, Classifier: c}}
}

// The classifier's question, criteria, type, threshold and model enter the
// hash; one the engine cannot ask is refused at load.
func TestNewsClassifierIsHashedAndValidated(t *testing.T) {
	guarded := strings.Replace(validYAML, "asset_class: crypto", "asset_class: crypto\nnews_guard: required", 1)
	base := strategy.NewsClassifier{Model: "jev-latest", Type: "noul", Question: "Catastrophe?", Threshold: 0.9,
		Criteria: map[string]string{"true": "at risk", "false": "noise"}}
	h := hashFor(t, guarded, classified(&base))
	if hashFor(t, guarded, classified(nil)) == h {
		t.Error("the classifier did not change the hash")
	}
	for name, change := range map[string]func(*strategy.NewsClassifier){
		"model":     func(c *strategy.NewsClassifier) { c.Model = "jev-1.14.0" },
		"question":  func(c *strategy.NewsClassifier) { c.Question = "Other?" },
		"threshold": func(c *strategy.NewsClassifier) { c.Threshold = 0.85 },
		"criteria":  func(c *strategy.NewsClassifier) { c.Criteria = map[string]string{"true": "other"} },
	} {
		c := base
		change(&c)
		if hashFor(t, guarded, classified(&c)) == h {
			t.Errorf("%s did not change the hash", name)
		}
	}
	for name, change := range map[string]func(*strategy.NewsClassifier){
		"choice type":      func(c *strategy.NewsClassifier) { c.Type = "choice" },
		"no question":      func(c *strategy.NewsClassifier) { c.Question = " " },
		"no model":         func(c *strategy.NewsClassifier) { c.Model = "" },
		"threshold 0":      func(c *strategy.NewsClassifier) { c.Threshold = 0 },
		"threshold > 1":    func(c *strategy.NewsClassifier) { c.Threshold = 1.5 },
		"unknown criteria": func(c *strategy.NewsClassifier) { c.Criteria = map[string]string{"maybe": "x"} },
	} {
		c := base
		change(&c)
		if _, err := Parse([]byte(guarded), classified(&c)); err == nil || !strings.Contains(err.Error(), "news classifier") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A classifier under news_guard none would never be asked: refused, saying so.
// Without a classifier, news_guard none loads as ever.
func TestClassifierWithoutTheGuardIsRefused(t *testing.T) {
	c := strategy.NewsClassifier{Model: "jev-latest", Type: "noul", Question: "Catastrophe?", Threshold: 0.9}
	for _, y := range []string{validYAML, strings.Replace(validYAML, "asset_class: crypto", "asset_class: crypto\nnews_guard: none", 1)} {
		if _, err := Parse([]byte(y), classified(&c)); err == nil || !strings.Contains(err.Error(), "news_guard is none") {
			t.Errorf("classifier with no guard: %v", err)
		}
		if _, err := Parse([]byte(y), classified(nil)); err != nil {
			t.Errorf("no classifier: %v", err)
		}
	}
}

// An equity's news guard searches the ticker of a HIP-3 symbol: any other
// symbol is refused rather than left with a guard that finds nothing.
func TestEquitySymbolMustBeHIP3(t *testing.T) {
	y := strings.NewReplacer("exchange: fake", "exchange: hyperliquid", "asset_class: crypto", "asset_class: equity").Replace(validYAML) + "sim: {liquidity_class: major}\n"
	for symbol, ok := range map[string]bool{
		"XYZ-GOOGL/USDC:USDC": true,
		"XYZ-BRK.B/USDC:USDC": false,
		"XYZ-BRK-B/USDC:USDC": false,
		"GOOGL/USDC:USDC":     false,
		"XYZ-GOOGL":           false,
	} {
		_, err := Parse([]byte(strings.Replace(y, "[AAA-USD]", "[\""+symbol+"\"]", 1)), example.New())
		if ok && err != nil || !ok && (err == nil || !strings.Contains(err.Error(), "XYZ-GOOGL/USDC:USDC")) {
			t.Errorf("%s: err = %v", symbol, err)
		}
	}
	// a crypto on the same venue keeps any symbol
	if _, err := Parse([]byte(strings.Replace(y, "asset_class: equity", "asset_class: crypto", 1)), example.New()); err != nil {
		t.Errorf("crypto: %v", err)
	}
}

// With the guard on, an asset whose name is one character needs an alias: it
// could not be searched otherwise. Refused, saying which alias to add.
func TestOneCharacterNameNeedsAnAliasUnderTheGuard(t *testing.T) {
	on := strings.Replace(validYAML, "asset_class: crypto", "asset_class: crypto\nnews_guard: required", 1)
	aliases := func(key string) map[string][]string { return map[string][]string{key: {"full name"}} }
	feeds := []string{"https://example.invalid/rss"}
	for _, tc := range []struct {
		yml, symbol string
		feeds       []string
		aliases     map[string][]string
		want        string // "" = loads
	}{
		{on, "S/USDC:USDC", feeds, nil, "{S: [full name]}"},
		{on, "S/USDC:USDC", feeds, map[string][]string{"S": {}}, "{S: [full name]}"},
		{on, "S/USDC:USDC", feeds, aliases("W"), "{S: [full name]}"},
		{on, "S/USDC:USDC", feeds, aliases("S"), ""},
		{on, "BTC/USDC:USDC", feeds, nil, ""},
		{on, "XYZ-S/USDC:USDC", feeds, nil, "{S: [full name]}"},
		{on, "XYZ-S/USDC:USDC", feeds, aliases("S"), ""},
		{on, "XYZ-S/USDC:USDC", feeds, aliases("XYZ"), ""},
		{on, "X-GOOGL/USDC:USDC", feeds, nil, ""},
		{on, "X-S/USDC:USDC", feeds, nil, "{S: [full name]}"},
		{on, "X-S/USDC:USDC", feeds, aliases("X"), ""},
		{on, "S/USDC:USDC", nil, nil, ""}, // no feeds: no entry at all, nothing inert
		{validYAML, "S/USDC:USDC", feeds, nil, ""},
	} {
		y := strings.Replace(tc.yml, "[AAA-USD]", "[\""+tc.symbol+"\"]", 1)
		kw := []string{"hack"}
		_, err := Parse([]byte(y), withNews{example.New(), strategy.NewsPolicy{Feeds: tc.feeds, Keywords: kw, Aliases: tc.aliases}})
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.symbol)) {
			t.Errorf("%s feeds %v aliases %v: err = %v, want %q", tc.symbol, tc.feeds, tc.aliases, err, tc.want)
		}
	}
}
