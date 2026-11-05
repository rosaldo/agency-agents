package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/loop"
	"github.com/rosaldo/agency-agents/quant-engine/internal/risk"
	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
)

// report runs the report subcommand on a fixture journal copied to a fresh
// state directory; st, when set, is saved as the run's state.
func report(t *testing.T, fixture string, st *loop.State, args ...string) string {
	t.Helper()
	dir := t.TempDir()
	b, err := os.ReadFile(filepath.Join("..", "testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "journal.jsonl"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if st != nil {
		if err := loop.SaveState(dir, *st); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := (App{Strategy: example.New()}).Run(append([]string{"report", "--dir", dir, "--utc"}, args...), &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func mustHave(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("report lacks %q\n%s", w, out)
		}
	}
}

func mustLack(t *testing.T, out string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(out, b) {
			t.Errorf("report shows %q\n%s", b, out)
		}
	}
}

// A period without fills: what each symbol saw, the reasons grouped by
// rule, an entry the engine refused, and an evaluation without a reason as a FAIL.
func TestReportPeriodWithSkipsOnly(t *testing.T) {
	out := report(t, "report_skips.jsonl", nil, "--from", "2026-01-01", "--to", "2026-01-02")
	mustHave(t, out,
		"Report 2026-01-01 00:00 -> 2026-01-02 00:00 (UTC)",
		"AAA                  3 candles  first 2026-01-01 01:00  last 2026-01-01 03:00",
		"BBB                  3 candles  first 2026-01-01 01:00  last 2026-01-01 03:00",
		"FAIL BBB: 1 evaluated without a reason (2026-01-01 02:00 -> 2026-01-01 02:00)",
		"2x none: drop 2.9% < 3%", // two drops, one rule: counted, the last shown
		"enter long on the candle closed 2026-01-01 03:00: drop 3.2% >= 3%, volume 2.1x >= 2x",
		"refused by the engine: margin 50.00 for qty 1 at leverage 1 is above the free margin 10.00",
		"orders: none filled in this period",
		"open positions now: none",
		"OTHER EVENTS: heartbeat 1, rejected 1, start 1, stop 1")
	mustLack(t, out, "FAIL AAA", "feedc0de")
}

// A period with an entry, an add and an exit: the values behind each
// tranche, the fills with their stops, the trade's P&L and the total.
func TestReportPeriodWithEntryAddAndExit(t *testing.T) {
	out := report(t, "report_trade.jsonl", nil, "--from", "2026-01-01 00:00", "--to", "2026-01-01 05:00")
	mustHave(t, out,
		"exceptions: none",
		"enter long on the candle closed 2026-01-01 01:00: drop 3.4% >= 3%, volume 2.5x >= 2x",
		"add long on the candle closed 2026-01-01 02:00: drop 5.1% >= 5%",
		"1x exit: reverted 50% of the drop",
		"AAA              buy  qty 2          price 100          stop 95",
		"AAA              buy  qty 1          price 97           stop 96",
		"AAA              sell qty 3          price 103          closes: P&L +12.00", // 3 x (103 - avg 99)
		"P&L of the trades closed in the period: +12.00")
	mustLack(t, out, "secretish", "feedc0de")
}

// The P&L of a trade opened before the period is priced on its real entry.
func TestReportPricesATradeOpenedBeforeThePeriod(t *testing.T) {
	out := report(t, "report_trade.jsonl", nil, "--from", "2026-01-01 03:00", "--to", "2026-01-01 05:00")
	mustHave(t, out, "closes: P&L +12.00", "P&L of the trades closed in the period: +12.00")
	mustLack(t, out, "price 100", "enter long")
}

// An empty period says so in each part instead of printing nothing.
func TestReportEmptyPeriod(t *testing.T) {
	out := report(t, "report_skips.jsonl", nil, "--from", "2026-02-01", "--to", "2026-02-02")
	mustHave(t, out,
		"Events in the period: 0",
		"none: no evaluated event in this period",
		"nothing analyzed in this period",
		"orders: none filled in this period",
		"OTHER EVENTS: none")
}

// The open position comes from status, with its budget and tranches.
func TestReportShowsTheOpenPosition(t *testing.T) {
	st := &loop.State{Level: "paper", Mode: "paper", Heartbeat: time.Now(),
		Positions: []exchange.Position{{Symbol: "AAA", Side: exchange.Long, Qty: 3, AvgPrice: 99, StopPrice: 96}},
		Trades:    map[string]risk.Trade{"AAA": {Budget: 100, Used: 0.5, Tranches: 2, Stop: 96}}}
	out := report(t, "report_skips.jsonl", st, "--from", "2026-01-01", "--to", "2026-01-02")
	mustHave(t, out, "AAA              long  qty 3 avg 99 stop 96  budget used 50% in 2 tranche(s), trade stop 96")
}

func TestReportPeriodFlags(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		since    time.Duration
		from, to string
		bad      bool
	}{
		{since: 12 * time.Hour},
		{since: 0, bad: true},
		{to: "2026-01-01", bad: true},
		{from: "2026-01-02", to: "2026-01-01", bad: true},
		{from: "yesterday", bad: true},
		{from: "2026-01-01T10:00:00-03:00"},
	} {
		p, err := reportPeriod(c.since, c.from, c.to, time.UTC, now)
		if (err != nil) != c.bad {
			t.Errorf("%+v: err %v", c, err)
		}
		if c.since > 0 && !c.bad && !p.from.Equal(now.Add(-c.since)) {
			t.Errorf("--since: from %v", p.from)
		}
	}
}
