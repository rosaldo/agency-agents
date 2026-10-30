package journal

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
)

func TestAppendAndRead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".example-sma-cross")
	j, err := Open(dir, "paper", "paper", "abc")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, k := range []string{Proposed, Sent, Filled} {
		if err := j.Append(Event{Time: t0, Kind: k, ClientID: "id-1", Symbol: "AAA", Qty: 2}); err != nil {
			t.Fatal(err)
		}
	}
	j.Close()
	j, _ = Open(dir, "paper", "paper", "abc") // appends, never truncates
	_ = j.Append(Event{Time: t0, Kind: Limit, Reason: "daily drawdown"})
	j.Close()

	evs, warns, err := Read(filepath.Join(dir, "journal.jsonl"))
	if err != nil || len(warns) != 0 || len(evs) != 4 {
		t.Fatalf("read %d events, warnings %v, err %v", len(evs), warns, err)
	}
	if evs[0].Mode != "paper" || evs[0].ParamsHash != "abc" || evs[2].Kind != Filled || evs[3].Kind != Limit {
		t.Fatalf("events = %+v", evs)
	}
}

func TestCorruptLastLineIsSkippedWithAWarning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	ok := `{"time":"2026-01-01T00:00:00Z","kind":"filled","mode":"paper","params_hash":"abc"}`
	if err := os.WriteFile(path, []byte(ok+"\n"+`{"time":"2026-01-01T0`), 0o600); err != nil {
		t.Fatal(err)
	}
	evs, warns, err := Read(path)
	if err != nil || len(evs) != 1 || len(warns) != 1 {
		t.Fatalf("events %d, warnings %v, err %v", len(evs), warns, err)
	}
}

func TestNoSecretReachesTheJournal(t *testing.T) {
	env := map[string]string{"TEST_FAKE_API_KEY": "key-hunter2", "TEST_FAKE_API_SECRET": "secret-hunter2"}
	cred, err := config.LoadCredentials("test", "fake", false, []string{"API_KEY", "API_SECRET"}, nil, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	j, _ := Open(dir, "paper", "paper", "abc")
	_ = j.Append(Event{Kind: Rejected, Reason: fmt.Sprintf("auth failed for %v / %s", cred, cred["API_SECRET"])})
	j.Close()
	b, _ := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if strings.Contains(string(b), "hunter2") {
		t.Fatalf("journal leaks a secret: %s", b)
	}
}
