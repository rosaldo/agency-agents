package loop

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
)

// status shows risk.max_open_positions in limits and the slots left, and
// leaves its JSON as it was when the limit is off.
func TestStatusShowsTheOpenPositionsLimitAndFreeSlots(t *testing.T) {
	positions := []exchange.Position{{Symbol: "AAA", Qty: 1}, {Symbol: "BBB", Qty: 2}, {Symbol: "CCC"}}
	for _, tc := range []struct {
		limit int
		want  string // in the status JSON
		not   string
	}{
		{3, `"max_open_positions":3`, ""},
		{3, `"free_slots":1`, ""},
		{2, `"free_slots":0`, ""},
		{0, `"min_demo_days":0}`, "free_slots"},
		{0, `"limits"`, "max_open_positions"},
	} {
		dir := t.TempDir()
		if err := SaveState(dir, State{Mode: "paper", Level: "paper", Limits: Limits{MaxOpenPositions: tc.limit}, Positions: positions}); err != nil {
			t.Fatal(err)
		}
		s, err := ReadStatus(dir)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(s)
		if !strings.Contains(string(b), tc.want) || tc.not != "" && strings.Contains(string(b), tc.not) {
			t.Fatalf("limit %d: status %s, want %s and no %q", tc.limit, b, tc.want, tc.not)
		}
	}
}
