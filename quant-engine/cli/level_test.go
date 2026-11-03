package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	lib "github.com/rosaldo/ccxt/v5"

	ccxtx "github.com/rosaldo/agency-agents/quant-engine/exchange/ccxt"
)

// marketsOnly lists AAA as a spot market and nothing else: enough for the
// adapter to be built and read its book.
type marketsOnly struct{ ccxtx.Client }

func (marketsOnly) LoadMarkets(...any) (map[string]lib.MarketInterface, error) {
	spot, yes := "spot", true
	return map[string]lib.MarketInterface{"AAA": {Type: &spot, Spot: &yes}}, nil
}

// Testnet credentials pick the sandbox, and with it the
// testnet level and book, never the simulator's (mode paper) ones.
func TestCredentialPickedTestnetUsesItsOwnLevelAndBook(t *testing.T) {
	app := testApp()
	app.Venue = ccxtx.Venue{ID: "hyperliquid", Credentials: []string{"WALLET_ADDRESS", "PRIVATE_KEY"},
		Dial: func(map[string]string, bool, []string) (ccxtx.Client, error) { return marketsOnly{}, nil }}
	c, err := app.load(writeYML(t, strings.Replace(cfg, "exchange: fake", "exchange: hyperliquid", 1)))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_STRATEGY_HYPERLIQUID_TESTNET_WALLET_ADDRESS", "0xtest")
	t.Setenv("TEST_STRATEGY_HYPERLIQUID_TESTNET_PRIVATE_KEY", "test-hunter2")
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "book-testnet.json"), []byte("not json"), 0o600)
	_, _, err = app.newExchange(c, fakeFlags{}, dir)
	if c.Level() != "testnet" || err == nil || !strings.Contains(err.Error(), "book-testnet.json") {
		t.Fatalf("level %q, err %v; want testnet reading book-testnet.json", c.Level(), err)
	}
}

// stop --flatten checks only its own level's files.
// Another level's file never blocks closing; its own refuses with what keeps
// the positions safe meanwhile and how to close them now.
func TestFlattenChecksOnlyItsLevel(t *testing.T) {
	flatten := func(mode, file, body string) (string, error) {
		work, path := writeConfig(t)
		b, _ := os.ReadFile(path)
		_ = os.WriteFile(path, append(b, "mode: "+mode+"\n"...), 0o600)
		dir := filepath.Join(work, "s")
		_ = os.MkdirAll(dir, 0o700)
		_ = os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600)
		err := testApp().Run([]string{"stop", "--flatten", "--config", path, "--dir", dir,
			"--fake-candles", "../testdata/loop_two_days.csv"}, &bytes.Buffer{})
		return dir, err
	}
	for _, tc := range []struct{ mode, file, body string }{
		{"mainnet", "state-paper.json", `{"mode":"paper"}`},
		{"mainnet", "book-demo.json", "{}"},
		{"mainnet", "state.json", `{"mode":"paper"}`},
		{"paper", "state-live.json", "{}"},
		{"paper", "state-mainnet.json", `{"mode":"mainnet"}`},
	} {
		if _, err := flatten(tc.mode, tc.file, tc.body); err != nil && !strings.Contains(err.Error(), "closed the positions") {
			t.Errorf("%s with %s: %v; another level's file must not block it", tc.mode, tc.file, err)
		}
	}
	for _, tc := range []struct{ mode, file, body, want string }{
		{"mainnet", "state-live.json", "{}", "state-mainnet.json"},
		{"mainnet", "book-real.json", "{}", "book-mainnet.json"},
		{"mainnet", "state-mainnet.json", `{"mode":"mainnet"}`, `"level": "mainnet"`},
		{"mainnet", "state-paper.json", `{"mode":"mainnet","level":"mainnet"}`, "move it to state-mainnet.json"},
		{"paper", "state-paper.json", `{"mode":"paper"}`, `"level": "paper"`},
	} {
		dir, err := flatten(tc.mode, tc.file, tc.body)
		if _, jerr := os.Stat(filepath.Join(dir, "journal.jsonl")); !errors.Is(jerr, os.ErrNotExist) {
			t.Errorf("%s with %s: flatten ran (journal written); want it refused before", tc.mode, tc.file)
		}
		if err == nil || strings.Contains(err.Error(), "closed the positions") || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "keep their exchange stops") ||
			!strings.Contains(err.Error(), "close them on the exchange") {
			t.Errorf("%s with %s: %v; want a refusal naming %s and how to close now", tc.mode, tc.file, err, tc.want)
		}
	}
}
