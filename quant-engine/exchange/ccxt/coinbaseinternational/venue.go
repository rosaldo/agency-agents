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

// Venue is coinbaseinternational: basic, not verified per exchange, with two
// exceptions (owner's decision 29/10, perp). The fork has no setMarginMode
// here, so margin mode goes on setLeverage (setMargin). And on its Coinbase
// gateway route the fork converts no amount to contracts (createOrder sends
// the amount as is, parseOrder and parsePosition read amount and size as
// is; only the native Deribit route converts), so amounts are the
// exchange's own, the base asset: NativeAmounts. The route is pinned to
// the gateway, so Deribit-shaped keys cannot switch the unit underneath.
var Venue = func() ccxt.Venue {
	v := ccxt.APIKeyVenue("coinbaseinternational", func() ccxt.APIKeyClient { return lib.NewCoinbaseinternational(nil) })
	v.SetMargin = setMargin
	v.NativeAmounts = true
	dial := v.Dial
	v.Dial = func(cred map[string]string, sandbox bool, symbols []string) (ccxt.Client, error) {
		c, err := dial(cred, sandbox, symbols)
		if err == nil {
			c.(*lib.Coinbaseinternational).Options.Store("route", "coinbase")
		}
		return c, err
	}
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
