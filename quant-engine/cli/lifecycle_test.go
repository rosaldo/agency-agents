//go:build unix

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The started loop is this test binary re-executed; TestMain routes it to Main.
func TestMain(m *testing.M) {
	if os.Getenv("TEST_STRATEGY_TEST_MAIN") == "1" {
		Main(testApp())
		return
	}
	os.Exit(m.Run())
}

func status(t *testing.T, dir string) map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := testApp().Run([]string{"status", "--dir", dir}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("status must be one line: %q", out.String())
	}
	var s map[string]any
	if err := json.Unmarshal(out.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// processGone reaps the loop: this test process is its parent (start released
// it), so an exited loop lingers as a zombie until waited for. A real start
// exits at once and the loop is reparented.
func processGone(pid int) bool {
	var ws syscall.WaitStatus
	got, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
	return got == pid || err != nil
}

// waitGone waits for the loop to exit (stop only signals it), so it no longer
// writes into dir when t.TempDir's cleanup removes it.
func waitGone(t *testing.T, dir string, pid int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second) // its last reads go through the fake's latency
	for !processGone(pid) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			log, _ := os.ReadFile(filepath.Join(dir, "engine.log"))
			t.Fatalf("loop process still running after stop; engine.log:\n%s", log)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestStartStatusStop(t *testing.T) {
	t.Setenv("TEST_STRATEGY_TEST_MAIN", "1")
	work, cfgPath := writeConfig(t)
	dir := filepath.Join(work, ".test-strategy")
	candles, _ := filepath.Abs("../testdata/loop_two_days.csv")
	start := []string{"start", "--config", cfgPath, "--dir", dir, "--fake-candles", candles, "--fake-tick", "200ms", "--fake-latency", "1s"}

	if err := testApp().Run(start, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for status(t, dir)["alive"] != true {
		if time.Now().After(deadline) {
			t.Fatalf("loop never came alive: %v", status(t, dir))
		}
		time.Sleep(50 * time.Millisecond)
	}
	pid := int(status(t, dir)["pid"].(float64))
	if err := testApp().Run(start, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second start: %v", err)
	}

	t0 := time.Now()
	if err := testApp().Run([]string{"stop", "--dir", dir}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("stop took %v", d)
	}
	waitGone(t, dir, pid)
	s := status(t, dir)
	if s["alive"] != false || !strings.Contains(s["stop_reason"].(string), "stop command") {
		t.Fatalf("status after stop = %v", s)
	}
}

func TestStartRefusesLive(t *testing.T) {
	work, cfgPath := writeConfig(t)
	b, _ := os.ReadFile(cfgPath)
	_ = os.WriteFile(cfgPath, append(b, "mode: mainnet\n"...), 0o600)
	err := testApp().Run([]string{"start", "--config", cfgPath, "--dir", filepath.Join(work, "s")}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "mainnet refused") {
		t.Fatalf("live start: %v", err)
	}
}

// One loop per state directory, held by an exclusive lock.
func TestSecondLoopIsImpossible(t *testing.T) {
	t.Setenv("TEST_STRATEGY_TEST_MAIN", "1")
	work, cfgPath := writeConfig(t)
	dir := filepath.Join(work, ".test-strategy")
	candles, _ := filepath.Abs("../testdata/loop_two_days.csv")
	args := []string{"--config", cfgPath, "--dir", dir, "--fake-candles", candles, "--fake-tick", "200ms"}
	var started bytes.Buffer
	if err := testApp().Run(append([]string{"start"}, args...), &started); err != nil {
		t.Fatal(err)
	}
	var out struct {
		PID         int
		Mode, Level string
	}
	if err := json.Unmarshal(started.Bytes(), &out); err != nil || out.Mode != "paper" || out.Level != "paper" {
		t.Fatalf("started %s (%v); want mode and level paper", started.Bytes(), err)
	}
	defer func() {
		_ = testApp().Run([]string{"stop", "--dir", dir}, &bytes.Buffer{})
		waitGone(t, dir, out.PID)
	}()
	_ = os.Remove(filepath.Join(dir, "state-paper.json")) // even a lost state file must not allow a second loop
	if err := testApp().Run(append([]string{"run"}, args...), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second loop: %v", err)
	}
}

// A start the loop refuses is an error with the reason, not started:true;
// status says the same, also over the state of an older run; the next start
// that works forgets it.
func TestRefusedStartIsAnErrorAndStatusSaysWhy(t *testing.T) {
	t.Setenv("TEST_STRATEGY_TEST_MAIN", "1")
	work, cfgPath := writeConfig(t)
	dir := filepath.Join(work, ".test-strategy")
	candles, _ := filepath.Abs("../testdata/loop_two_days.csv")
	start := []string{"start", "--config", cfgPath, "--dir", dir, "--fake-candles", candles, "--fake-tick", "200ms"}
	good, _ := os.ReadFile(cfgPath)
	refuse := func() {
		t.Helper()
		if err := os.WriteFile(cfgPath, append(slices.Clone(good), "feed: websocket\n"...), 0o600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err := testApp().Run(start, &out)
		if err == nil || !strings.Contains(err.Error(), "candle stream") || strings.Contains(out.String(), "started") {
			t.Fatalf("refused start: err %v, stdout %q", err, out.String())
		}
		if s := status(t, dir); s["alive"] != false || !strings.Contains(s["stop_reason"].(string), "candle stream") {
			t.Fatalf("status after a refused start = %v", s)
		}
	}
	refuse() // no state file yet

	if err := os.WriteFile(cfgPath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := testApp().Run(start, &out); err != nil || !strings.Contains(out.String(), `"started":true`) {
		t.Fatalf("good start: err %v, stdout %q", err, out.String())
	}
	s := status(t, dir)
	if s["alive"] != true { // started means up: no wait
		t.Fatalf("status right after started:true = %v", s)
	}
	pid := int(s["pid"].(float64))
	if err := testApp().Run([]string{"stop", "--dir", dir}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	waitGone(t, dir, pid)
	if s := status(t, dir); !strings.Contains(s["stop_reason"].(string), "stop command") {
		t.Fatalf("status after a good run = %v, the old refusal was not forgotten", s)
	}

	refuse() // over the state of the run above
}

// A failed mainnet gate is a refused start too; a running loop is not.
func TestStatusAfterAGateRefusal(t *testing.T) {
	work, cfgPath := writeConfig(t)
	dir := filepath.Join(work, "s")
	b, _ := os.ReadFile(cfgPath)
	_ = os.WriteFile(cfgPath, append(b, "mode: mainnet\n"...), 0o600)
	if err := testApp().Run([]string{"start", "--config", cfgPath, "--dir", dir}, &bytes.Buffer{}); err == nil {
		t.Fatal("mainnet start passed the gate")
	}
	if s := status(t, dir); !strings.Contains(s["stop_reason"].(string), "mainnet refused") {
		t.Fatalf("status = %v", s)
	}
}
