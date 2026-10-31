package ccxt

import (
	"context"
	"strings"
	"testing"
	"time"

	lib "github.com/rosaldo/ccxt/v5"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
)

// derivStub is a swap client that reads open interest and funding rates.
type derivStub struct {
	*swapStub
	ois   lib.OpenInterests
	rates lib.FundingRates
	asked [][]string
}

func (c *derivStub) FetchOpenInterests(o ...lib.FetchOpenInterestsOptions) (lib.OpenInterests, error) {
	var opts lib.FetchOpenInterestsOptionsStruct
	for _, fn := range o {
		fn(&opts)
	}
	if opts.Symbols != nil {
		c.asked = append(c.asked, *opts.Symbols)
	}
	return c.ois, nil
}

func (c *derivStub) FetchFundingRates(o ...lib.FetchFundingRatesOptions) (lib.FundingRates, error) {
	var opts lib.FetchFundingRatesOptionsStruct
	for _, fn := range o {
		fn(&opts)
	}
	if opts.Symbols != nil {
		c.asked = append(c.asked, *opts.Symbols)
	}
	return c.rates, nil
}

// Snapshot reads open interest (in base units: contracts times the contract
// size), the funding rate and the mark price over REST, stamped with the
// time it was read; a market the venue does not report is an error.
func TestSnapshotReadsOpenInterestAndFunding(t *testing.T) {
	c := &derivStub{swapStub: newSwapStub()}
	c.ois = lib.OpenInterests{OpenInterests: map[string]lib.OpenInterest{"AAA/USDC:USDC": {OpenInterestAmount: f(2500)}}}
	c.rates = lib.FundingRates{FundingRates: map[string]lib.FundingRate{"AAA/USDC:USDC": {FundingRate: f(-0.0000125), MarkPrice: f(101.5)}}}
	a := swapAdapter(t, c)
	d, err := a.Snapshot(context.Background(), exchange.Real, "AAA/USDC:USDC")
	if err != nil {
		t.Fatal(err)
	}
	if !d.HasOpenInterest || d.OpenInterest != 2500 || d.FundingRate != -0.0000125 || d.MarkPrice != 101.5 || !d.Time.Equal(now) {
		t.Fatalf("snapshot = %+v", d)
	}
	for _, syms := range c.asked {
		if len(syms) != 1 || syms[0] != "AAA/USDC:USDC" {
			t.Fatalf("asked for %v, want the one symbol", c.asked)
		}
	}
	if len(c.asked) != 2 {
		t.Fatalf("calls = %v, want fetchOpenInterests and fetchFundingRates", c.asked)
	}

	c.ois = lib.OpenInterests{OpenInterests: map[string]lib.OpenInterest{"BIG/USDC:USDC": {OpenInterestAmount: f(300)}}}
	c.rates = lib.FundingRates{FundingRates: map[string]lib.FundingRate{"BIG/USDC:USDC": {FundingRate: f(0.0001)}}}
	big, err := New(c, Options{Symbols: []string{"BIG/USDC:USDC"}, Market: "swap", Env: exchange.Real, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if d, err := big.Snapshot(context.Background(), exchange.Real, "BIG/USDC:USDC"); err != nil || !d.HasOpenInterest || d.OpenInterest != 3 || d.MarkPrice != 0 {
		t.Fatalf("contract size 0.01: snapshot = %+v, %v; want 3 base units, mark 0 (not reported)", d, err)
	}
	if _, err := a.Snapshot(context.Background(), exchange.Real, "AAA/USDC:USDC"); err == nil {
		t.Fatal("a market missing from the venue's answer gave a snapshot")
	}
}

func TestSnapshotRefusesSpotAndAClientWithoutTheCalls(t *testing.T) {
	if _, err := swapAdapter(t, newSwapStub()).Snapshot(context.Background(), exchange.Real, "AAA/USDC:USDC"); err == nil ||
		!strings.Contains(err.Error(), "open interest") {
		t.Fatalf("client without the calls: %v", err)
	}
	spot, err := New(&derivStub{swapStub: newSwapStub()}, Options{Symbols: []string{"AAA/USDC"}, Market: "spot", Env: exchange.Real})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spot.Snapshot(context.Background(), exchange.Real, "AAA/USDC"); err == nil || !strings.Contains(err.Error(), "swap") {
		t.Fatalf("spot: %v", err)
	}
	if _, err := spot.Snapshot(context.Background(), exchange.Demo, "AAA/USDC"); err == nil {
		t.Fatal("the adapter answered for another environment")
	}
}
