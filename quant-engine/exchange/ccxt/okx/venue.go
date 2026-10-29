// Package okx is the okx venue of the CCXT adapter. A strategy's main
// imports it to build the okx binary; it references only lib.NewOkx.
package okx

import (
	lib "github.com/rosaldo/ccxt/v5"

	"github.com/rosaldo/agency-agents/quant-engine/exchange/ccxt"
)

var _ ccxt.APIKeyClient = (*lib.Okx)(nil) // compile time: the venue satisfies the adapter

// Venue is okx: basic, not verified per exchange (centralized exchanges get
// no venue-specific code).
var Venue = ccxt.APIKeyVenue("okx", func() ccxt.APIKeyClient { return lib.NewOkx(nil) })
