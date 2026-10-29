package ownbook

import (
	"strings"
	"testing"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
)

// FR-P8 netting rule, one case per line of the spec.
func TestReconcile(t *testing.T) {
	L, S := exchange.Long, exchange.Short
	for _, c := range []struct {
		name         string
		own, account Holding
		want         float64
		why          string // "" none, else a prefix or substring
	}{
		{"equal", Holding{L, 2}, Holding{L, 2}, 2, ""},
		{"equal after a contract conversion", Holding{S, 0.03}, Holding{S, 3 * 0.01}, 0.03, ""},
		{"user added", Holding{L, 2}, Holding{L, 5}, 2, ForeignQty},
		{"user reduced", Holding{L, 2}, Holding{L, 1.5}, 1.5, ""},
		{"closed outside", Holding{S, 2}, Holding{}, 0, ""},
		{"user reversed", Holding{L, 2}, Holding{S, 1}, 0, "against the engine's"},
		{"foreign while flat", Holding{}, Holding{S, 1}, 0, ForeignQty},
		{"both flat", Holding{}, Holding{}, 0, ""},
	} {
		got, why := Reconcile(c.own, c.account)
		if got.Qty != c.want || (c.why == "") != (why == "") || !strings.Contains(why, c.why) {
			t.Errorf("%s: own %+v, %q; want qty %g, reason with %q", c.name, got, why, c.want, c.why)
		}
		if c.want > 0 && got.Side != c.own.Side {
			t.Errorf("%s: side changed to %s", c.name, got.Side)
		}
	}
}
