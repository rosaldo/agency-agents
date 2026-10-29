package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestCCXTExchangeChecks(t *testing.T) {
	cfgFor := func(t *testing.T, exch, mode string) *config.Config {
		t.Helper()
		c, err := config.Parse([]byte(strings.Replace(cfg, "exchange: fake", "exchange: "+exch, 1)+mode), testApp().Strategy)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if _, _, err := testApp().newExchange(cfgFor(t, "nosuchexchange", ""), fakeFlags{}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown CCXT id: %v", err)
	}
	t.Setenv("TEST_STRATEGY_KRAKEN_API_KEY", "")
	if _, _, err := testApp().newExchange(cfgFor(t, "kraken", "mode: live\n"), fakeFlags{}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "TEST_STRATEGY_KRAKEN_API_KEY") {
		t.Fatalf("live without credentials: %v", err)
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
}
