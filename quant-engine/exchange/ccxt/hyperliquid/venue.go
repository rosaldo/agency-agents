// Package hyperliquid is the Hyperliquid venue of the CCXT adapter, through
// the fork github.com/rosaldo/ccxt/v5 (no builder fee, no referral). A
// strategy's main imports it to build the hyperliquid binary; it references
// only lib.NewHyperliquid. It trades the main dex and HIP-3 perp dexes
// (third-party markets such as equities, symbol DEX-TICKER/USDC:USDC); which
// dex and symbols is the strategy's yml, never the engine's.
package hyperliquid

import (
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	lib "github.com/rosaldo/ccxt/v5"

	"github.com/rosaldo/agency-agents/quant-engine/exchange/ccxt"
)

var _ ccxt.Client = (*client)(nil) // compile time: the venue satisfies the adapter

// Venue is Hyperliquid: the master wallet address and an agent (API) wallet
// key without withdrawal rights (V3); client order IDs are 128-bit hex with
// 0x (V9); HIP-3 dexes list equities.
var Venue = ccxt.Venue{
	ID:          "hyperliquid",
	Credentials: []string{"WALLET_ADDRESS", "PRIVATE_KEY"},
	Dial:        dial,
	ClientID:    func(d [16]byte) string { return "0x" + hex.EncodeToString(d[:]) },
	Equity:      true,
	Check:       check,
	// liquidationPrice comes from the clearinghouse's liquidationPx (V2)
	ReportsLiquidation: true,
	MaintenanceMargin:  maintenanceMargin,
}

// maintenanceMargin: "The maintenance margin is half of the initial margin
// at max leverage" (hyperliquid docs, trading/liquidations), so the rate is
// 1/(2*maxLeverage) of the market (HIP-3 markets carry their own, e.g.
// xyz:GOOGL 20). The fork maps the meta's maxLeverage to limits.leverage.max
// and reads no margin table (marginTableId): if a large position sits in a
// tier with lower leverage it liquidates nearer, which the post-fill check
// against the exchange's liquidation price catches.
func maintenanceMargin(m lib.MarketInterface) (float64, bool) {
	if m.Limits.Leverage.Max == nil || *m.Limits.Leverage.Max <= 0 {
		return 0, false
	}
	return 1 / (2 * *m.Limits.Leverage.Max), true
}

// client is the fork's client bound to one dex: the balance of a HIP-3 dex
// lives in that dex's clearinghouse, so fetchBalance carries it.
type client struct {
	*lib.Hyperliquid
	dex string // "" = main dex
}

func (c *client) FetchBalance(params ...any) (lib.Balances, error) {
	if c.dex == "" {
		return c.Hyperliquid.FetchBalance(params...)
	}
	p := map[string]any{}
	if len(params) > 0 {
		if m, ok := params[0].(map[string]any); ok {
			for k, v := range m {
				p[k] = v
			}
		}
	}
	p["dex"] = c.dex
	return c.Hyperliquid.FetchBalance(p)
}

// hip3Base is the base of a HIP-3 symbol as the fork builds it: dex name
// upper-cased, a dash, the ticker (xyz:GOOGL -> XYZ-GOOGL/USDC:USDC).
var hip3Base = regexp.MustCompile(`^([A-Z0-9]+)-[A-Z0-9]+/`)

// dexOf is the HIP-3 dex a symbol names, "" for the main dex.
func dexOf(symbol string) string {
	if m := hip3Base.FindStringSubmatch(symbol); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

func dial(cred map[string]string, sandbox bool, symbols []string) (ccxt.Client, error) {
	x := lib.NewHyperliquid(nil)
	var dexes []any
	for _, s := range symbols {
		if d := dexOf(s); d != "" && !slices.Contains(dexes, any(d)) {
			dexes = append(dexes, d)
		}
	}
	// before LoadMarkets: the main dex's spot and swap markets, plus exactly
	// the HIP-3 dexes the symbols name (the fork would otherwise load the
	// first 10 dexes of perpDexs and silently miss the others)
	fm := map[string]any{"types": []any{"spot", "swap"}}
	if len(dexes) > 0 {
		fm = map[string]any{"types": []any{"spot", "swap", "hip3"}, "hip3": map[string]any{"limit": len(dexes), "dexes": dexes}}
	}
	x.Options.Store("fetchMarkets", fm)
	if sandbox {
		if err := ccxt.EnableSandbox("hyperliquid", x); err != nil {
			return nil, err
		}
	}
	if cred != nil {
		x.SetWalletAddress(cred["WALLET_ADDRESS"])
		x.SetPrivateKey(cred["PRIVATE_KEY"])
	}
	c := &client{Hyperliquid: x}
	if len(dexes) == 1 {
		c.dex = dexes[0].(string)
	}
	return c, nil
}

// check refuses, before trading (t9-hip3.md §6): swap symbols of more than
// one dex (positions and margin are per dex clearinghouse, and the fork
// reads positions of one dex at a time); a HIP-3 market whose dex differs
// from the one its symbol names; a HIP-3 dex whose collateral is not USDC
// (the fork reports every perp balance as USDC, so margin in USDH, USDT0 or
// USDE would be misread; how that margin is debited is unverified, T17).
func check(markets map[string]lib.MarketInterface) error {
	dexes := map[string]bool{}
	var collateral []error
	for sym, m := range markets {
		if m.Swap == nil || !*m.Swap {
			continue
		}
		d := ""
		if bn := deref(m.BaseName); strings.Contains(bn, ":") {
			d = strings.SplitN(bn, ":", 2)[0]
		}
		if d != dexOf(sym) {
			return fmt.Errorf("%s belongs to dex %q, not to the dex its symbol names", sym, d)
		}
		if settle := deref(m.Settle); d != "" && settle != "USDC" {
			collateral = append(collateral, fmt.Errorf("%s: HIP-3 dex %s settles in %s; only USDC-collateral dexes are traded", sym, d, settle))
		}
		dexes[d] = true
	}
	if len(dexes) > 1 {
		return errors.New("swap symbols span more than one dex; one dex per config (main or one HIP-3 dex)")
	}
	return errors.Join(collateral...)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
