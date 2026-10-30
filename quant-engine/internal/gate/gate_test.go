package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
)

const liveYAML = `
mode: mainnet
exchange: fake
asset_class: crypto
interval: 1h
symbols: [AAA-USD]
validation_file: validation.yml
strategy: {period: 20, stop_pct: 4}
risk: {confirmed_by_user: true}
`

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// testStrategy is the example under another name: the example itself never
// passes the live gate.
type renamed struct{ strategy.Strategy }

func (renamed) Name() string { return "test-strategy" }

func testStrategy() strategy.Strategy { return renamed{example.New()} }

func TestTheExampleNeverGoesLive(t *testing.T) {
	c, err := config.Parse([]byte(liveYAML), example.New())
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckLive(c, filepath.Join(t.TempDir(), "journal.jsonl")); err == nil || !strings.Contains(err.Error(), "example") {
		t.Fatalf("example strategy live: %v", err)
	}
}

// setup writes a paper journal with one event on each of `days` distinct
// calendar days for hash, and a validation file; gate.min_demo_days is 30.
func setup(t *testing.T, days int, hash, validation string) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	c, err := config.Parse([]byte(liveYAML), testStrategy())
	if err != nil {
		t.Fatal(err)
	}
	c.ValidationFile = filepath.Join(dir, "validation.yml")
	c.Gate.MinDemoDays = 30
	if hash == "" {
		hash = c.ParamsHash()
	}
	j, _ := journal.Open(dir, "paper", "paper", hash)
	for d := 0; d < days; d++ {
		_ = j.Append(journal.Event{Time: t0.AddDate(0, 0, d), Kind: journal.Filled})
	}
	j.Close()
	live, _ := journal.Open(dir, "mainnet", "mainnet", c.ParamsHash()) // mainnet events never count as demo time
	_ = live.Append(journal.Event{Time: t0.AddDate(0, 0, 90), Kind: journal.Filled})
	live.Close()
	if validation != "" {
		validation = strings.ReplaceAll(validation, "HASH", c.ParamsHash())
		if err := os.WriteFile(c.ValidationFile, []byte(validation), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return c, filepath.Join(dir, "journal.jsonl")
}

const validated = "status: validated\nmethod: walk-forward\nparams_hash: HASH\n"

func TestLiveGate(t *testing.T) {
	cases := []struct {
		name, hash, validation string
		days                   int
		want                   string // "" = allowed; else a word the refusal must contain
	}{
		{"30 demo days and validation pass", "", validated, 30, ""},
		{"29 demo days refused", "", validated, 29, "demo"},
		{"demo under other parameters refused", "other", validated, 60, "demo"},
		{"validation for other parameters refused", "", strings.Replace(validated, "HASH", "other", 1), 30, "params_hash"},
		{"not validated refused", "", strings.Replace(validated, "validated\n", "not validated\n", 1), 30, "status"},
		{"other method refused", "", strings.Replace(validated, "walk-forward", "in-sample", 1), 30, "walk-forward"},
		{"missing validation file refused", "", "", 30, "validation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, jpath := setup(t, tc.days, tc.hash, tc.validation)
			err := CheckLive(c, jpath)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

func TestNoJournalNoLive(t *testing.T) {
	c, _ := setup(t, 30, "", validated)
	if err := CheckLive(c, filepath.Join(t.TempDir(), "none.jsonl")); err == nil || !strings.Contains(err.Error(), "demo") {
		t.Fatalf("err = %v", err)
	}
}

func TestEmptyValidationPathRefused(t *testing.T) {
	c, jpath := setup(t, 30, "", validated)
	c.ValidationFile = ""
	if err := CheckLive(c, jpath); err == nil || !strings.Contains(err.Error(), "validation_file") {
		t.Fatalf("err = %v", err)
	}
}

// One hour on day 1 and one on day 31 is two days of activity, not thirty.
func TestDemoCountsDistinctDaysNotSpan(t *testing.T) {
	c, jpath := setup(t, 0, "", validated)
	j, _ := journal.Open(filepath.Dir(jpath), "paper", "paper", c.ParamsHash())
	_ = j.Append(journal.Event{Time: t0, Kind: journal.Filled})
	_ = j.Append(journal.Event{Time: t0.AddDate(0, 0, 30), Kind: journal.Filled})
	j.Close()
	if err := CheckLive(c, jpath); err == nil || !strings.Contains(err.Error(), "demo") {
		t.Fatalf("err = %v, want a demo refusal", err)
	}
}

// Day boundaries follow the yml timezone: 22:00 and 02:00 UTC are two UTC days
// but one day in Asia/Tokyo (UTC+9).
func TestDemoDaysUseConfiguredTimezone(t *testing.T) {
	for tz, wantDays := range map[string]bool{"UTC": true, "Asia/Tokyo": false} {
		dir := t.TempDir()
		c, err := config.Parse([]byte(liveYAML+"timezone: "+tz+"\n"), testStrategy())
		if err != nil {
			t.Fatal(err)
		}
		c.Gate.MinDemoDays = 2
		j, _ := journal.Open(dir, "paper", "paper", c.ParamsHash())
		_ = j.Append(journal.Event{Time: t0.Add(22 * time.Hour), Kind: journal.Filled})
		_ = j.Append(journal.Event{Time: t0.Add(26 * time.Hour), Kind: journal.Filled})
		j.Close()
		err = checkDemo(c, filepath.Join(dir, "journal.jsonl"))
		if wantDays != (err == nil) {
			t.Errorf("%s: err = %v, want two days = %v", tz, err, wantDays)
		}
	}
}

// gate.min_demo_days 0 (the default) lets live start with no demo
// journal at all, but the walk-forward validation is still required.
func TestNoDemoDaysByDefault(t *testing.T) {
	c, _ := setup(t, 0, "", validated)
	c.Gate.MinDemoDays = 0
	none := filepath.Join(t.TempDir(), "none.jsonl")
	if err := CheckLive(c, none); err != nil {
		t.Fatalf("no demo journal, min_demo_days 0: %v", err)
	}
	c.ValidationFile = filepath.Join(t.TempDir(), "missing.yml")
	if err := CheckLive(c, none); err == nil || !strings.Contains(err.Error(), "validation") {
		t.Fatalf("no validation, min_demo_days 0: %v", err)
	}
	d, _ := config.Parse([]byte(liveYAML), testStrategy())
	if d.Gate.MinDemoDays != 0 {
		t.Fatalf("default min_demo_days %d, want 0", d.Gate.MinDemoDays)
	}
}
