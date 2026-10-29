package config

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
)

const validYAML = `
exchange: fake
asset_class: crypto
interval: 1h
symbols: [AAA-USD]
strategy:
  period: 20
  stop_pct: 4
  target_pct: 5
risk:
  confirmed_by_user: true
`

func TestParseValidAppliesDefaults(t *testing.T) {
	c, err := Parse([]byte(validYAML), example.New())
	if err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	if c.Mode != "paper" {
		t.Errorf("mode default = %q, want paper", c.Mode)
	}
	r := c.Risk
	if r.MaxRiskPerTradePct != 1 || r.MaxDailyDrawdownPct != 3 || r.MaxWeeklyDrawdownPct != 6 || r.MinDemoPeriodDays != 30 {
		t.Errorf("risk defaults = %+v, want 1/3/6/30", r)
	}
	if c.Interval() != time.Hour {
		t.Errorf("interval = %v, want 1h", c.Interval())
	}
	if c.Location().String() != "UTC" {
		t.Errorf("timezone default = %v, want UTC", c.Location())
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	cases := map[string]struct{ old, new, want string }{
		"not confirmed":      {"confirmed_by_user: true", "confirmed_by_user: false", "confirmed_by_user"},
		"confirm missing":    {"  confirmed_by_user: true", "", "confirmed_by_user"},
		"strategy refuses":   {"period: 20", "period: 1", "strategy example-sma-cross: period"},
		"strategy bad type":  {"stop_pct: 4", "stop_pct: [4]", "strategy example-sma-cross"},
		"no symbols":         {"symbols: [AAA-USD]", "symbols: []", "symbols"},
		"blank symbol":       {"symbols: [AAA-USD]", "symbols: [\"\"]", "symbols"},
		"malformed exchange": {"exchange: fake", "exchange: Not An Id", "exchange"},
		"equity off CCXT":    {"exchange: fake\nasset_class: crypto", "exchange: kraken\nasset_class: equity", "crypto"},
		"bad mode":           {"exchange: fake", "exchange: fake\nmode: yolo", "mode"},
		"bad interval":       {"interval: 1h", "interval: soon", "interval"},
		"bad timezone":       {"interval: 1h", "interval: 1h\ntimezone: Mars/Base", "timezone"},
		"negative risk":      {"confirmed_by_user: true", "confirmed_by_user: true\n  max_daily_drawdown_pct: -3", "max_daily_drawdown_pct"},
		"unknown field":      {"interval: 1h", "interval: 1h\nintervall: 2h", "intervall"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := strings.Replace(validYAML, tc.old, tc.new, 1)
			if in == validYAML {
				t.Fatalf("case did not change the input")
			}
			_, err := Parse([]byte(in), example.New())
			if err == nil {
				t.Fatalf("accepted an invalid config")
			}
			if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "\n") {
				t.Errorf("error %q should be one line naming %q", err, tc.want)
			}
		})
	}
}

func TestNoStrategyOrABadName(t *testing.T) {
	if _, err := Parse([]byte(validYAML), nil); err == nil {
		t.Fatal("parsed without a strategy")
	}
	if _, err := Parse([]byte(validYAML), named{example.New(), "Bad Name"}); err == nil || !strings.Contains(err.Error(), "Bad Name") {
		t.Fatalf("bad strategy name: %v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(t.TempDir()+"/none.yml", example.New()); err == nil || !os.IsNotExist(unwrapAll(err)) {
		t.Fatalf("missing file: %v", err)
	}
}

func unwrapAll(err error) error {
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok || u.Unwrap() == nil {
			return err
		}
		err = u.Unwrap()
	}
}
