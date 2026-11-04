package config

import (
	"strings"
	"testing"

	"github.com/rosaldo/agency-agents/quant-engine/strategy"
	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
	"go.yaml.in/yaml/v3"
)

// named renames a strategy, as a second strategy with the same block would be.
type named struct {
	strategy.Strategy
	name string
}

func (n named) Name() string { return n.name }

const reordered = `
risk:
  confirmed_by_user: true
strategy:
  target_pct: 5
  stop_pct: 4
  period: 20
symbols: [AAA-USD]
interval: 1h
exchange: fake
asset_class: crypto
`

func hashOf(t *testing.T, y string) string {
	t.Helper()
	return hashFor(t, y, example.New())
}

func hashFor(t *testing.T, y string, s strategy.Strategy) string {
	t.Helper()
	c, err := Parse([]byte(y), s)
	if err != nil {
		t.Fatal(err)
	}
	return c.ParamsHash()
}

func TestParamsHash(t *testing.T) {
	base := hashOf(t, validYAML)
	if len(base) != 64 {
		t.Fatalf("hash %q is not a sha256 hex", base)
	}
	if hashOf(t, reordered) != base {
		t.Error("key order changed the hash")
	}
	if hashOf(t, validYAML+"mode: mainnet\nvalidation_file: v.yml\n") != base {
		t.Error("mode or validation_file changed the hash")
	}
	if hashOf(t, strings.Replace(validYAML, "stop_pct: 4", "stop_pct: 3", 1)) == base {
		t.Error("a strategy parameter change kept the hash")
	}
	if hashFor(t, validYAML, named{example.New(), "other-strategy"}) == base {
		t.Error("another strategy name kept the hash")
	}
	if hashOf(t, strings.Replace(validYAML, "confirmed_by_user: true", "confirmed_by_user: true\n  max_daily_drawdown_pct: 2", 1)) == base {
		t.Error("a risk limit change kept the hash")
	}
	if hashOf(t, validYAML+"market: swap\nperp: {allow_short: true}\n") == base {
		t.Error("allowing shorts kept the hash")
	}
}

// acceptsAnyBlock stands for a strategy whose Configure takes these blocks
// (yaml turns the key true into the string "true" for a map[string]string).
type acceptsAnyBlock struct{ strategy.Strategy }

func (acceptsAnyBlock) Configure(yaml.Node) error { return nil }

// A yml the hash could not take is refused at load, with what to do, so no
// command gets as far as hashing it. A quoted key is fine.
func TestParseRefusesWhatCannotBeHashed(t *testing.T) {
	const block = "exchange: fake\nasset_class: crypto\ninterval: 1h\nsymbols: [AAA-USD]\nrisk: {confirmed_by_user: true}\n"
	s := acceptsAnyBlock{example.New()}
	for _, y := range []string{
		"strategy: {criteria: {true: a, false: b}}\n",
		"strategy: {1.5: a}\n",
		"strategy: {[1, 2]: a}\n",
		"strategy: {list: [{~: a}]}\n",
		"strategy: {x: .inf}\n",
		"strategy: {x: .nan}\n",
	} {
		if _, err := Parse([]byte(block+y), s); err == nil || !strings.Contains(err.Error(), "quote") {
			t.Errorf("%s: err = %v, want a refusal saying what to quote", y, err)
		}
	}
	if _, err := Parse([]byte(block+"strategy: {criteria: {\"true\": a, \"false\": b}}\n"), s); err != nil {
		t.Errorf("quoted keys: %v", err)
	}
	if _, err := Parse([]byte(strings.Replace(validYAML, "confirmed_by_user: true", "confirmed_by_user: true\n  max_daily_drawdown_pct: .inf", 1)), example.New()); err == nil || !strings.Contains(err.Error(), ".inf") {
		t.Errorf("an infinite risk limit: %v", err)
	}
	// a repeated key that only the hash's own decode of the block would meet
	if _, err := Parse([]byte(block+"strategy:\n  a: 1\n  a: 2\n"), s); err == nil || !strings.Contains(err.Error(), "config:") {
		t.Errorf("a repeated key: %v", err)
	}
}
