package ccxt

import (
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exec"
	"github.com/rosaldo/agency-agents/quant-engine/internal/ownbook"
)

// Netting, an add on the engine's long: own_qty is the sum of the tranches,
// each with its own client ID, under one resting stop for all of it; the
// user's quantity on top is still foreign_qty_detected.
func TestNettingAddSumsTheTranchesUnderOneStop(t *testing.T) {
	c, a, e := enteredSwap(t)
	c.positions = account("long", 2)
	p := onePosition(t, a)
	if _, err := e.Add(ctx, perp, now.Add(time.Hour), p, exec.Plan{Side: exchange.Long, Entry: 100, Stop: 90, Qty: 3}); err != nil {
		t.Fatal(err)
	}
	c.positions = account("long", 5)
	p = onePosition(t, a)
	if p.Qty != 5 || p.StopQty != 5 || p.Blocked != "" {
		t.Fatalf("position %+v; want own 5 under a stop for 5, not blocked", p)
	}
	if rs := restingStops(c.stub); len(rs) != 1 || val(rs[0].Amount) != 5 {
		t.Fatalf("resting stops %+v; want one for 5", rs)
	}
	c.positions = account("long", 8)
	if p := onePosition(t, a); p.Qty != 5 || p.AccountQty != 8 || !strings.HasPrefix(p.Blocked, ownbook.ForeignQty) {
		t.Fatalf("position %+v; want own 5 of 8 on the account, blocked foreign_qty_detected", p)
	}
}
