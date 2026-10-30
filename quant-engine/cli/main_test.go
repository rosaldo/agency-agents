package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ccxtx "github.com/rosaldo/agency-agents/quant-engine/exchange/ccxt"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
)

const cfg = `
exchange: fake
asset_class: crypto
interval: 1h
symbols: [AAA]
strategy: {period: 3, stop_pct: 4, target_pct: 5}
risk: {confirmed_by_user: true}
sim: {liquidity_class: major}
`

// renamed is the example under another name: the example never goes live.
type renamed struct{ strategy.Strategy }

func (renamed) Name() string { return "test-strategy" }

func testApp() App { return App{Strategy: renamed{example.New()}, Version: "test"} }

func writeConfig(t *testing.T) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "test-strategy.yml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func TestBacktestCommand(t *testing.T) {
	dir, path := writeConfig(t)
	out := filepath.Join(dir, "bt")
	var stdout bytes.Buffer
	err := testApp().Run([]string{"backtest", "--config", path, "--candles", "../testdata/backtest_one_trade.csv", "--capital", "10000", "--out", out}, &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"trades":1`) || strings.Count(stdout.String(), "\n") != 1 {
		t.Fatalf("stdout = %q, want one JSON line with 1 trade", stdout.String())
	}
	for _, f := range []string{"trades.csv", "summary.json"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBacktestNeedsCandlesAndCapital(t *testing.T) {
	_, path := writeConfig(t)
	for _, args := range [][]string{
		{"backtest", "--config", path, "--capital", "10000"},
		{"backtest", "--config", path, "--candles", "../testdata/backtest_one_trade.csv"},
	} {
		if err := testApp().Run(args, &bytes.Buffer{}); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

// With no loop alive, stop --flatten closes positions itself through the adapter.
func TestStopFlattenWithoutALoop(t *testing.T) {
	work, path := writeConfig(t)
	var out bytes.Buffer
	err := testApp().Run([]string{"stop", "--flatten", "--config", path, "--dir", filepath.Join(work, "s"),
		"--fake-candles", "../testdata/loop_two_days.csv"}, &out)
	if err != nil || !strings.Contains(out.String(), `"alive":false`) {
		t.Fatalf("stop --flatten: %v %q", err, out.String())
	}
}

// dialed records how the venue was dialed and stops there.
type dialed struct {
	cred    map[string]string
	sandbox bool
	symbols []string
}

func testVenue(id string, equity bool, got *dialed) ccxtx.Venue {
	return ccxtx.Venue{ID: id, Credentials: []string{"WALLET_ADDRESS", "PRIVATE_KEY"}, Equity: equity,
		Dial: func(cred map[string]string, sandbox bool, symbols []string) (ccxtx.Client, error) {
			*got = dialed{cred, sandbox, symbols}
			return nil, errors.New("dialed")
		}}
}

func writeYML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test-strategy.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// D9: the yml's exchange must be the venue compiled into the binary; D10: an
// equity needs a venue that lists equities.
func TestTheYmlMustNameTheBinarysVenue(t *testing.T) {
	var got dialed
	app := testApp()
	app.Venue = testVenue("hyperliquid", false, &got)
	for body, want := range map[string]string{
		strings.Replace(cfg, "exchange: fake", "exchange: okx", 1):                                                   "built for \"hyperliquid\"",
		strings.Replace(cfg, "exchange: fake\nasset_class: crypto", "exchange: hyperliquid\nasset_class: equity", 1): "equity",
	} {
		if _, err := app.load(writeYML(t, body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err %v, want it naming %s", err, want)
		}
	}
	if _, err := testApp().load(writeYML(t, strings.Replace(cfg, "exchange: fake", "exchange: hyperliquid", 1))); err == nil {
		t.Error("a binary without a venue accepted an exchange")
	}
	app.Venue.Equity = true
	c, err := app.load(writeYML(t, strings.Replace(cfg, "exchange: fake\nasset_class: crypto", "exchange: hyperliquid\nasset_class: equity", 1)))
	if err != nil || c.NewsGuard != "required" {
		t.Fatalf("equity on an equity venue: %v, news_guard %q", err, c.NewsGuard)
	}
	if _, err := app.load(writeYML(t, cfg)); err != nil {
		t.Fatalf("fake refused: %v", err)
	}
}

// §9: live reads only mainnet credentials, paper only testnet ones (and
// trades the sandbox with them); paper without them uses public data.
func TestCredentialsFollowTheMode(t *testing.T) {
	var got dialed
	app := testApp()
	app.Venue = testVenue("hyperliquid", false, &got)
	load := func(mode string) *config.Config {
		c, err := app.load(writeYML(t, strings.Replace(cfg, "exchange: fake", "exchange: hyperliquid", 1)+mode))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	t.Setenv("TEST_STRATEGY_HYPERLIQUID_WALLET_ADDRESS", "0xmain")
	t.Setenv("TEST_STRATEGY_HYPERLIQUID_PRIVATE_KEY", "main-hunter2")
	if _, _, err := app.newExchange(load("mode: paper\n"), fakeFlags{}, t.TempDir()); err == nil || got.cred != nil || got.sandbox {
		t.Fatalf("paper with mainnet credentials only: cred %v sandbox %v (%v); want public data", got.cred, got.sandbox, err)
	}
	t.Setenv("TEST_STRATEGY_HYPERLIQUID_TESTNET_WALLET_ADDRESS", "0xtest")
	t.Setenv("TEST_STRATEGY_HYPERLIQUID_TESTNET_PRIVATE_KEY", "test-hunter2")
	if _, _, err := app.newExchange(load("mode: paper\n"), fakeFlags{}, t.TempDir()); err == nil || got.cred["PRIVATE_KEY"] != "test-hunter2" || !got.sandbox {
		t.Fatalf("paper with testnet credentials: cred %v sandbox %v", got.cred != nil, got.sandbox)
	}
	if _, _, err := app.newExchange(load("mode: live\n"), fakeFlags{}, t.TempDir()); err == nil || got.cred["PRIVATE_KEY"] != "main-hunter2" || got.sandbox {
		t.Fatalf("live: mainnet credential not used, sandbox %v", got.sandbox)
	}
	t.Setenv("TEST_STRATEGY_HYPERLIQUID_PRIVATE_KEY", "")
	if _, _, err := app.newExchange(load("mode: live\n"), fakeFlags{}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "TEST_STRATEGY_HYPERLIQUID_PRIVATE_KEY") {
		t.Fatalf("live without its key: %v", err)
	}
	t.Setenv("TEST_STRATEGY_HYPERLIQUID_PRIVATE_KEY", "test-hunter2") // one key on both networks: accepted
	if _, _, err := app.newExchange(load("mode: live\n"), fakeFlags{}, t.TempDir()); err == nil || got.cred["PRIVATE_KEY"] != "test-hunter2" || got.sandbox {
		t.Fatalf("live with the testnet's key: cred %v sandbox %v", got.cred != nil, got.sandbox)
	}
}

// FR-P10: the simulator gets the yml's profile and its costs.
func TestTheSimulatorChargesTheYmlProfile(t *testing.T) {
	app := testApp()
	app.Venue = testVenue("hyperliquid", false, &dialed{})
	c, err := app.load(writeYML(t, strings.Replace(cfg, "exchange: fake", "exchange: hyperliquid", 1)+"mode: paper\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := simProfile(c)
	if p.Name != "real" || p.Costs["AAA"] != c.SimCosts()["AAA"] || p.Costs["AAA"].CommissionPct != 0.045 {
		t.Fatalf("profile %+v; want real with the yml's costs", p)
	}
}

// Names derive from Strategy.Name(); a bad strategy: block fails check early.
func TestNamesAndConfigureComeFromTheStrategy(t *testing.T) {
	var out bytes.Buffer
	if err := testApp().Run([]string{"version"}, &out); err != nil || out.String() != "test\n" {
		t.Fatalf("version: %q %v", out.String(), err)
	}
	if err := testApp().Run(nil, &out); err == nil || !strings.Contains(err.Error(), "test-strategy <command> [--config test-strategy.yml] [--dir .test-strategy]") {
		t.Fatalf("usage: %v", err)
	}
	if testApp().readyEnv() != "TEST_STRATEGY_READY_FD" {
		t.Fatalf("ready env = %q", testApp().readyEnv())
	}
	_, path := writeConfig(t)
	b, _ := os.ReadFile(path)
	_ = os.WriteFile(path, []byte(strings.Replace(string(b), "period: 3", "period: 1", 1)), 0o600)
	if err := testApp().Run([]string{"check", "--config", path}, &out); err == nil || !strings.Contains(err.Error(), "period") {
		t.Fatalf("check with a bad strategy block: %v", err)
	}
	dir := filepath.Join(t.TempDir(), ".test-strategy")
	if err := testApp().Run([]string{"start", "--config", path, "--dir", dir}, &out); err == nil || !strings.Contains(err.Error(), "period") {
		t.Fatalf("start with a bad strategy block: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("start with a bad strategy block touched the state dir: %v", err)
	}
}

// T12/O4: paper with testnet credentials trades the sandbox and dials
// mainnet's public data (no credentials) for the signal's candles.
func TestTestnetDemoDialsMainnetForCandles(t *testing.T) {
	var calls []dialed
	app := testApp()
	app.Venue = ccxtx.Venue{ID: "hyperliquid", Credentials: []string{"WALLET_ADDRESS", "PRIVATE_KEY"},
		Dial: func(cred map[string]string, sandbox bool, symbols []string) (ccxtx.Client, error) {
			calls = append(calls, dialed{cred, sandbox, symbols})
			if sandbox {
				return nil, nil
			}
			return nil, errors.New("dialed")
		}}
	c, err := app.load(writeYML(t, strings.Replace(cfg, "exchange: fake", "exchange: hyperliquid", 1)+"mode: paper\n"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_STRATEGY_HYPERLIQUID_TESTNET_WALLET_ADDRESS", "0xtest")
	t.Setenv("TEST_STRATEGY_HYPERLIQUID_TESTNET_PRIVATE_KEY", "test-hunter2")
	_, _, err = app.newExchange(c, fakeFlags{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "mainnet candles") || len(calls) != 2 ||
		calls[0].cred["PRIVATE_KEY"] != "test-hunter2" || !calls[0].sandbox || calls[1].cred != nil || calls[1].sandbox {
		t.Fatalf("err %v, dials %+v; want the sandbox with the testnet key, then mainnet public data", err, calls)
	}
}
