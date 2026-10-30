package config

import (
	"os"
	"strings"
	"testing"

	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
)

// The README's yml example must load as written: a reader copies it.
func TestReadmeExampleLoads(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(b), "```yaml\n")
	y, _, closed := strings.Cut(rest, "```")
	if !ok || !closed {
		t.Fatal("README has no yaml example")
	}
	c, err := Parse([]byte(y), example.New())
	if err != nil {
		t.Fatalf("README yml example refused: %v", err)
	}
	if c.Exchange != "hyperliquid" || c.Market != "swap" || c.Perp.Leverage != 2 {
		t.Fatalf("README example parsed to %+v", c)
	}
}
