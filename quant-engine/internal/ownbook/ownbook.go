// Package ownbook holds the rule by which the engine keeps managing only what
// it opened on a venue that nets one position per symbol (FR-P8): the
// account's position may mix the engine's own quantity with the user's.
package ownbook

import (
	"fmt"
	"math"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
)

// ForeignQty starts the reason of a symbol whose account position holds
// more than the engine's own: the engine keeps managing only its own and
// opens nothing new there.
const ForeignQty = "foreign_qty_detected"

// Holding is a quantity on a side (Qty 0 = flat).
type Holding struct {
	Side exchange.PositionSide
	Qty  float64
}

// Reconcile applies the netting rule to the engine's own holding and what the
// account holds on the symbol. It returns the own holding to keep, never
// larger than the compatible part of the account, and a non-empty reason when
// the symbol must take no new entry:
//   - account equal to own: unchanged;
//   - more on the same side, or anything while own is flat: own unchanged,
//     ForeignQty;
//   - less on the same side: own drops to the account's;
//   - flat: own drops to 0 (closed outside the engine);
//   - opposite side: own drops to 0 and the rest, not the engine's, is left
//     untouched; the symbol is refused while it lasts.
func Reconcile(own, account Holding) (Holding, string) {
	own.Side, account.Side = side(own.Side), side(account.Side)
	switch {
	case own.Qty <= 0 && account.Qty <= 0:
		return own, ""
	case own.Qty <= 0:
		return own, fmt.Sprintf("%s: the account holds %g %s, the engine none", ForeignQty, account.Qty, account.Side)
	case account.Qty <= 0:
		return Holding{own.Side, 0}, ""
	case account.Side != own.Side:
		return Holding{own.Side, 0}, fmt.Sprintf("the account holds %g %s against the engine's %g %s: not the engine's, left untouched",
			account.Qty, account.Side, own.Qty, own.Side)
	case same(account.Qty, own.Qty):
		return own, ""
	case account.Qty > own.Qty:
		return own, fmt.Sprintf("%s: the account holds %g %s, the engine %g", ForeignQty, account.Qty, account.Side, own.Qty)
	}
	return Holding{own.Side, account.Qty}, ""
}

func side(s exchange.PositionSide) exchange.PositionSide {
	if s == exchange.Short {
		return exchange.Short
	}
	return exchange.Long
}

// same absorbs the float noise of a contract-size conversion.
func same(x, y float64) bool { return math.Abs(x-y) <= 1e-9*math.Max(1, math.Max(x, y)) }
