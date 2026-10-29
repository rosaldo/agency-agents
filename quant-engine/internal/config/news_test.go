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
	c, err := Parse([]byte(strings.Replace(validYAML, "asset_class: crypto", "asset_class: equity\nnews_guard: none", 1)), example.New())
	if err != nil || c.NewsGuard != "none" {
		t.Fatalf("explicit news_guard: %+v %v", c, err)
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
