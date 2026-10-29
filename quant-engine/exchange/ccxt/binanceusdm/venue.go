// Package binanceusdm is the binanceusdm venue of the CCXT adapter. A strategy's main
// imports it to build the binanceusdm binary; it references only lib.NewBinanceusdm.
package binanceusdm

import (
	lib "github.com/rosaldo/ccxt/v5"

	"github.com/rosaldo/agency-agents/quant-engine/exchange/ccxt"
)

var _ ccxt.APIKeyClient = (*lib.Binanceusdm)(nil) // compile time: the venue satisfies the adapter

// Venue is binanceusdm: basic, not verified per exchange (centralized exchanges get
// no venue-specific code).
var Venue = ccxt.APIKeyVenue("binanceusdm", func() ccxt.APIKeyClient { return lib.NewBinanceusdm(nil) })
