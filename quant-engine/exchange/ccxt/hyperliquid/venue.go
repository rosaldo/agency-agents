// Package hyperliquid is the Hyperliquid venue of the CCXT adapter, through
// the fork github.com/rosaldo/ccxt/v5 (no builder fee, no referral). A
// strategy's main imports it to build the hyperliquid binary; it references
// only lib.NewHyperliquid.
package hyperliquid

import (
	"encoding/hex"

	lib "github.com/rosaldo/ccxt/v5"

	"github.com/rosaldo/agency-agents/quant-engine/exchange/ccxt"
)

var _ ccxt.Client = (*lib.Hyperliquid)(nil) // compile time: the venue satisfies the adapter

// Venue is Hyperliquid: the master wallet address and an agent (API) wallet
// key without withdrawal rights (V3); client order IDs are 128-bit hex with
// 0x (V9).
var Venue = ccxt.Venue{
	ID:          "hyperliquid",
	Credentials: []string{"WALLET_ADDRESS", "PRIVATE_KEY"},
	Dial:        dial,
	ClientID:    func(d [16]byte) string { return "0x" + hex.EncodeToString(d[:]) },
}

func dial(cred map[string]string, sandbox bool, symbols []string) (ccxt.Client, error) {
	x := lib.NewHyperliquid(nil)
	// before LoadMarkets: only the main dex's spot and swap markets
	x.Options.Store("fetchMarkets", map[string]any{"types": []any{"spot", "swap"}})
	if sandbox {
		x.SetSandboxMode(true)
	}
	if cred != nil {
		x.SetWalletAddress(cred["WALLET_ADDRESS"])
		x.SetPrivateKey(cred["PRIVATE_KEY"])
	}
	return x, nil
}
