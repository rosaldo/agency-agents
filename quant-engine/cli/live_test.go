//go:build unix

package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
)

type fakeTTY struct {
	in  io.Reader
	out bytes.Buffer
}

// useTTY replaces the terminal with input (nil = no terminal at all).
func useTTY(t *testing.T, input io.Reader) *fakeTTY {
	t.Helper()
	tty := &fakeTTY{in: input}
	prev := openTTY
	openTTY = func() (io.Reader, io.Writer, func(), error) {
		if input == nil {
			return nil, nil, nil, errors.New("no terminal")
		}
		return tty.in, &tty.out, func() {}, nil
	}
	t.Cleanup(func() { openTTY = prev })
	return tty
}

// liveSetup writes a live yml (plus extra lines) whose walk-forward
// validation passes. gate.min_demo_days is left at 0: no demo journal.
func liveSetup(t *testing.T, extra ...string) (cfgPath, dir string) {
	t.Helper()
	work, cfgPath := writeConfig(t)
	dir = filepath.Join(work, ".test-strategy")
	b, _ := os.ReadFile(cfgPath)
	b = append(b, []byte("mode: mainnet\nvalidation_file: "+filepath.Join(work, "validation.yml")+"\n"+strings.Join(extra, ""))...)
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(cfgPath, testApp().Strategy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	v := "status: validated\nmethod: walk-forward\nparams_hash: " + c.ParamsHash() + "\n"
	if err := os.WriteFile(filepath.Join(work, "validation.yml"), []byte(v), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, dir
}

func liveArgs(cmd, cfgPath, dir string) []string {
	candles, _ := filepath.Abs("../testdata/loop_two_days.csv")
	return []string{cmd, "--config", cfgPath, "--dir", dir, "--fake-candles", candles, "--fake-tick", "1ms"}
}

// The trading process itself asks at the terminal: a launched start
// without one (the model's case) is refused, and the refusal reaches start.
func TestLiveStartWithoutATerminalIsRefused(t *testing.T) {
	t.Setenv("TEST_STRATEGY_TEST_MAIN", "1")
	t.Setenv("QUANT_ENGINE_NO_TTY", "1")
	cfgPath, dir := liveSetup(t)
	err := testApp().Run(liveArgs("start", cfgPath, dir), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("err = %v", err)
	}
}

func TestLiveWrongAnswerIsRefused(t *testing.T) {
	cfgPath, dir := liveSetup(t)
	tty := useTTY(t, strings.NewReader("y\n"))
	if err := testApp().Run(liveArgs("run", cfgPath, dir), &bytes.Buffer{}); err == nil {
		t.Fatal("live ran without typing yes")
	}
	for _, want := range []string{"1%", "3%", "6%", "demo days required     0", "10000"} {
		if !strings.Contains(tty.out.String(), want) {
			t.Errorf("confirmation prompt lacks %q:\n%s", want, tty.out.String())
		}
	}
}

// The one confirmation also shows the perp limits.
func TestLiveConfirmationShowsThePerpLimits(t *testing.T) {
	cfgPath, dir := liveSetup(t, "market: swap\nperp: {leverage: 2, margin_mode: cross}\n") // risk defaults: max_leverage 3, buffer 0.5
	tty := useTTY(t, strings.NewReader("no\n"))
	if err := testApp().Run(liveArgs("run", cfgPath, dir), &bytes.Buffer{}); err == nil {
		t.Fatal("live ran without typing yes")
	}
	for _, want := range []string{"leverage               2", "max leverage           3", "margin mode            cross", "liquidation buffer     0.5"} {
		if !strings.Contains(tty.out.String(), want) {
			t.Errorf("confirmation prompt lacks %q:\n%s", want, tty.out.String())
		}
	}
}

func TestLiveConfirmationTimesOut(t *testing.T) {
	cfgPath, dir := liveSetup(t)
	r, w := io.Pipe() // never written: the user walked away
	defer w.Close()
	useTTY(t, r)
	prev := confirmTimeout
	confirmTimeout = 100 * time.Millisecond
	defer func() { confirmTimeout = prev }()
	if err := testApp().Run(liveArgs("run", cfgPath, dir), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
}

func TestLiveYesRuns(t *testing.T) {
	cfgPath, dir := liveSetup(t)
	useTTY(t, strings.NewReader("yes\n"))
	var out bytes.Buffer
	if err := testApp().Run(liveArgs("run", cfgPath, dir), &out); err != nil {
		t.Fatalf("live run after yes: %v", err)
	}
	j, _ := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if !strings.Contains(string(j), `"mode":"mainnet"`) {
		t.Fatal("live loop did not run")
	}
}

// With gate.min_demo_days 0 live needs no demo journal, but the
// walk-forward validation is still required.
func TestLiveWithoutDemoDaysStillNeedsTheValidation(t *testing.T) {
	cfgPath, dir := liveSetup(t)
	if err := os.Remove(filepath.Join(filepath.Dir(cfgPath), "validation.yml")); err != nil {
		t.Fatal(err)
	}
	useTTY(t, strings.NewReader("yes\n"))
	err := testApp().Run(liveArgs("run", cfgPath, dir), &bytes.Buffer{})
	j, _ := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if err == nil || !strings.Contains(err.Error(), "validation") || strings.Contains(string(j), `"mode":"mainnet"`) {
		t.Fatalf("live without validation: err=%v", err)
	}
}

// Review B1: forging a hand-off file and variable must not run live without
// the terminal, the demo period or the validation.
func TestForgedHandOffCannotRunLive(t *testing.T) {
	_, cfgPath := writeConfig(t)
	b, _ := os.ReadFile(cfgPath)
	_ = os.WriteFile(cfgPath, append(b, []byte("mode: mainnet\nvalidation_file: /nonexistent.yml\n")...), 0o600)
	c, err := config.Load(cfgPath, testApp().Strategy)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "forged")
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, "live-confirmation"), []byte(c.ParamsHash()+" abc"), 0o600)
	t.Setenv("TEST_STRATEGY_LIVE_NONCE", "abc")
	useTTY(t, nil)
	err = testApp().Run(liveArgs("run", cfgPath, dir), &bytes.Buffer{})
	j, _ := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if err == nil || strings.Contains(string(j), `"mode":"mainnet"`) {
		t.Fatalf("forged hand-off ran live: err=%v", err)
	}
}
