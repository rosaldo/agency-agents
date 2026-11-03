package config

import (
	"strings"
	"testing"

	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
)

// feed is rest by default, websocket on request, anything else refused; it
// changes latency, not the rule, so the parameter hash ignores it.
func TestFeedDefaultsToRestAndStaysOutOfTheHash(t *testing.T) {
	rest, err := Parse([]byte(specYAML), example.New())
	if err != nil {
		t.Fatal(err)
	}
	ws, err := Parse([]byte(specYAML+"feed: websocket\n"), example.New())
	if err != nil {
		t.Fatal(err)
	}
	if rest.Feed != "rest" || ws.Feed != "websocket" {
		t.Fatalf("feed %q / %q; want rest by default, websocket when asked", rest.Feed, ws.Feed)
	}
	if rest.ParamsHash() != ws.ParamsHash() {
		t.Fatal("feed changed the parameter hash")
	}
	if _, err := Parse([]byte(specYAML+"feed: grpc\n"), example.New()); err == nil || !strings.Contains(err.Error(), "feed must be rest or websocket") {
		t.Fatalf("feed: grpc gave %v", err)
	}
}
