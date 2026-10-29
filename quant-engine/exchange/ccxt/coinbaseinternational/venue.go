// Package coinbaseinternational is the coinbaseinternational venue of the CCXT adapter. A strategy's main
// imports it to build the coinbaseinternational binary; it references only lib.NewCoinbaseinternational.
package coinbaseinternational

import (
	"fmt"

	lib "github.com/rosaldo/ccxt/v5"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/exchange/ccxt"
)

var _ ccxt.APIKeyClient = (*lib.Coinbaseinternational)(nil) // compile time: the venue satisfies the adapter

// Venue is coinbaseinternational: basic, not verified per exchange, with one
// exception (owner's decision 29/10, perp): the fork has no setMarginMode
// here, so margin mode goes on setLeverage (setMargin).
var Venue = func() ccxt.Venue {
	v := ccxt.APIKeyVenue("coinbaseinternational", func() ccxt.APIKeyClient { return lib.NewCoinbaseinternational(nil) })
	v.SetMargin = setMargin
	return v
}()

// setMargin sends params.isolated on setLeverage, the fork's only margin
// mode switch for this exchange ("set to true for isolated margin"). Cross
// is refused: the fork documents no way to ask for it.
func setMargin(c ccxt.Client, symbol string, want exchange.MarginSettings) error {
	if want.MarginMode != "isolated" {
		return fmt.Errorf("margin mode %q is refused: the fork sets only isolated here (params.isolated on setLeverage)", want.MarginMode)
	}
	_, err := c.SetLeverage(int64(want.Leverage), lib.WithSetLeverageSymbol(symbol),
		lib.WithSetLeverageParams(map[string]any{"isolated": true}))
	return err
}
