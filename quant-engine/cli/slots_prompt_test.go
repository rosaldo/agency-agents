//go:build unix

package cli

import (
	"strings"
	"testing"

	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
)

// The mainnet confirmation shows risk.max_open_positions when it is set,
// right before the closing lines, and has no such line without it.
func TestLiveConfirmationShowsTheOpenPositionsLimit(t *testing.T) {
	for _, tc := range []struct {
		risk string
		want bool
	}{{", max_open_positions: 3", true}, {"", false}} {
		c, err := config.Parse([]byte("exchange: fake\nasset_class: crypto\ninterval: 1h\nsymbols: [AAA]\nrisk: {confirmed_by_user: true"+tc.risk+"}\n"), testApp().Strategy)
		if err != nil {
			t.Fatal(err)
		}
		tty := useTTY(t, strings.NewReader("no\n"))
		if err := confirmLive(c, fake.New(fake.Options{Cash: 10_000})); err == nil {
			t.Fatal("confirmed without typing yes")
		}
		out := tty.out.String()
		line := "\n  max open positions     3\nFrom here on the engine enforces"
		if got := strings.Contains(out, line); got != tc.want || !tc.want && strings.Contains(out, "max open positions") {
			t.Fatalf("risk %q: prompt has the limit line %v, want %v:\n%s", tc.risk, got, tc.want, out)
		}
	}
}
