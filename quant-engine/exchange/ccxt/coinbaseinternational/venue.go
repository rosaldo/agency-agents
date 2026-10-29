// Package coinbaseinternational is the coinbaseinternational venue of the CCXT adapter. A strategy's main
// imports it to build the coinbaseinternational binary; it references only lib.NewCoinbaseinternational.
package coinbaseinternational

import (
	lib "github.com/rosaldo/ccxt/v5"

	"github.com/rosaldo/agency-agents/quant-engine/exchange/ccxt"
)

var _ ccxt.APIKeyClient = (*lib.Coinbaseinternational)(nil) // compile time: the venue satisfies the adapter

// Venue is coinbaseinternational: basic, not verified per exchange (centralized exchanges get
// no venue-specific code).
var Venue = ccxt.APIKeyVenue("coinbaseinternational", func() ccxt.APIKeyClient { return lib.NewCoinbaseinternational(nil) })
