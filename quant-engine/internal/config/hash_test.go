package config

import (
	"strings"
	"testing"

	"github.com/rosaldo/agency-agents/quant-engine/strategy"
	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
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
