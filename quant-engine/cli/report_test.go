package cli

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/engine"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/loop"
	"github.com/rosaldo/agency-agents/quant-engine/internal/risk"
	"github.com/rosaldo/agency-agents/quant-engine/internal/scripted"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
)

var update = flag.Bool("update", false, "rewrite the report fixtures written by an engine run")

// tradeRun journals, as a live run does, two trades of a scripted strategy
// on the fake exchange: an entry, an add and an exit by rule; then an entry
// whose target closes it.
func tradeRun(t *testing.T, dir string) {
	t.Helper()
	closes := []float64{100, 100, 97, 95, 99, 100, 96, 100, 100}
	s := &scripted.Strategy{Decider: func(in strategy.Input) strategy.Decision {
		switch in.Now.Hour() {
		case 2:
			return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 92, Fraction: 0.5, Reason: "drop 3.0% >= 3%, volume 2.5x >= 2x"}
		case 3:
			return strategy.Decision{Action: strategy.Add, Side: strategy.Long, Stop: 93, Fraction: 0.5, Reason: "drop 5.0% >= 5%"}
		case 4:
			return strategy.Decision{Action: strategy.Exit, Reason: "reverted 50% of the drop"}
		case 6:
			return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 91, Target: 99, Reason: "drop 4.0% >= 3%, volume 3.0x >= 2x"}
		}
		if in.Position != nil {
			return strategy.Decision{Reason: "holding"}
		}
		return strategy.Decision{Reason: "drop 0.0% < 3%"}
	}}
	c, err := config.Parse([]byte("exchange: fake\nasset_class: crypto\ninterval: 1h\nsymbols: [AAA]\nrisk: {confirmed_by_user: true}\n"), s)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for i, c := range closes {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(i) * time.Hour), Open: c, High: c, Low: c, Close: c, Volume: 10})
	}
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 1, MinQty: 1, Candles: map[string][]market.Candle{"AAA": cs}})
	j, err := journal.Open(dir, "paper", "paper", "feedc0de1234")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	var now time.Time
	e := engine.New(c, s, f, exchange.Demo, j, func() time.Time { return now })
	e.Evaluations = true
	for i, candle := range cs {
		now = candle.Time.Add(time.Hour + time.Second) // one second after the candle closed
		if i > 0 {
			f.Advance()
		}
		if err := e.Step(context.Background()); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
}

// report_trade.jsonl is what the engine journals, byte for byte: the
// report is tested on the format the engine writes, not on a hand-made one.
func TestReportTradeFixtureIsWhatTheEngineJournals(t *testing.T) {
	dir := t.TempDir()
	tradeRun(t, dir)
	got, err := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("..", "testdata", "report_trade.jsonl")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from what the engine journals; run go test -run %s -update\n%s", path, t.Name(), got)
	}
}

// report runs the report subcommand on a fixture journal copied to a fresh
// state directory; st, when set, is saved as the run's state.
func report(t *testing.T, fixture string, st *loop.State, args ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	return reportOn(t, string(b), st, args...)
}

// reportOn runs the report subcommand on journal, the journal's lines.
func reportOn(t *testing.T, journal string, st *loop.State, args ...string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "journal.jsonl"), []byte(journal), 0o600); err != nil {
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

// evaluatedAt is an evaluated event of sym for the candle closed at hh:00.
func evaluatedAt(sym string, hh int) string {
	c := time.Date(2026, 1, 1, hh, 0, 0, 0, time.UTC)
	return fmt.Sprintf(`{"time":%q,"kind":"evaluated","mode":"paper","symbol":%q,"reason":"no signal","action":"none","candle_close":%q}`+"\n",
		c.Add(time.Second).Format(time.RFC3339), sym, c.Format(time.RFC3339))
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

// A period with an entry, an add and an exit, then an entry closed by its
// target: the values behind each tranche, the fills with the stop the engine
// sent after each, each trade's P&L and the total.
func TestReportPeriodWithEntryAddAndExit(t *testing.T) {
	out := report(t, "report_trade.jsonl", nil, "--from", "2026-01-01 00:00", "--to", "2026-01-01 10:00")
	mustHave(t, out,
		"exceptions: none",
		"enter long on the candle closed 2026-01-01 03:00: drop 3.0% >= 3%, volume 2.5x >= 2x",
		"add long on the candle closed 2026-01-01 04:00: drop 5.0% >= 5%",
		"1x exit: reverted 50% of the drop",
		"1x exit: target 99 reached at close 100",
		"AAA              buy  qty 10         price 97           stop 92",
		"AAA              buy  qty 25         price 95           stop 93",
		"AAA              sell qty 35         price 99           closes: P&L +120.00", // 35 x (99 - avg 95.5714)
		"AAA              buy  qty 20         price 96           stop 91",
		"AAA              sell qty 20         price 100          closes: P&L +80.00",
		"P&L of the trades closed in the period: +200.00")
	mustLack(t, out, "feedc0de", "447960362bb6")
}

// The P&L of a trade opened before the period is priced on its real entry.
func TestReportPricesATradeOpenedBeforeThePeriod(t *testing.T) {
	out := report(t, "report_trade.jsonl", nil, "--from", "2026-01-01 05:00", "--to", "2026-01-01 06:00")
	mustHave(t, out, "closes: P&L +120.00", "P&L of the trades closed in the period: +120.00")
	mustLack(t, out, "price 97", "enter long")
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

// SEEN counts candles, not events: a restart that decides the same candle
// again does not count it twice; candles missing between two seen ones (an
// interval inferred from the closes themselves) are an exception.
func TestReportSeenCountsDistinctCandlesAndShowsGaps(t *testing.T) {
	var j strings.Builder
	for _, hh := range []int{1, 2, 2, 3, 7, 8} { // 2 decided twice; 4, 5 and 6 never
		j.WriteString(evaluatedAt("AAA", hh))
	}
	for _, hh := range []int{1, 2, 3, 4, 5, 6, 7, 8} {
		j.WriteString(evaluatedAt("BBB", hh))
	}
	out := reportOn(t, j.String(), nil, "--from", "2026-01-01", "--to", "2026-01-02")
	mustHave(t, out,
		"AAA                  5 candles  first 2026-01-01 01:00  last 2026-01-01 08:00",
		"BBB                  8 candles",
		"GAP AAA: 3 candle(s) missing between 2026-01-01 03:00 and 2026-01-01 07:00")
	mustLack(t, out, "GAP BBB", "exceptions: none")
}

// The period is [from, to): an event at --to belongs to the next period.
func TestReportPeriodEndIsExclusive(t *testing.T) {
	out := report(t, "report_skips.jsonl", nil, "--from", "2026-01-01", "--to", "2026-01-01 04:00") // stop at 04:00
	mustHave(t, out, "OTHER EVENTS: heartbeat 1, rejected 1, start 1\n")
}

// --since and --from/--to are two ways to give the period: both at once is
// an error, not one silently ignored.
func TestReportRefusesSinceWithFromOrTo(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"--since", "1h", "--from", "2026-01-01"}, {"--since", "1h", "--to", "2026-01-01"}} {
		err := (App{Strategy: example.New()}).Run(append([]string{"report", "--dir", dir}, args...), &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "--since or --from") {
			t.Errorf("%v: err %v", args, err)
		}
	}
}

// fill is a filled event of AAA at hh:00.
func fill(hh int, side string, qty, price float64) string {
	return fmt.Sprintf(`{"time":"2026-01-01T%02d:00:00Z","kind":"filled","mode":"paper","symbol":"AAA","side":%q,"qty":%g,"price":%g}`+"\n", hh, side, qty, price)
}

// A fill larger than the position closes it and opens the rest on the other
// side at the fill's price: the next close is priced from there.
func TestReportPricesAPositionThatTurnsSide(t *testing.T) {
	j := fill(1, "buy", 2, 100) + fill(2, "sell", 3, 110) + fill(3, "buy", 1, 100)
	out := reportOn(t, j, nil, "--from", "2026-01-01", "--to", "2026-01-02")
	mustHave(t, out,
		"sell qty 3          price 110          closes: P&L +20.00", // 2 x (110 - 100), short 1 at 110
		"buy  qty 1          price 100          closes: P&L +10.00", // 1 x (110 - 100)
		"P&L of the trades closed in the period: +30.00")
}

// A position its stop closed before the period is gone: the next trade is
// priced from its own entry, not averaged into the closed one.
func TestReportForgetsAPositionClosedOutsideBeforeThePeriod(t *testing.T) {
	j := fill(1, "buy", 2, 100) +
		`{"time":"2026-01-01T02:00:00Z","kind":"closed_outside","mode":"paper","symbol":"AAA"}` + "\n" +
		fill(3, "buy", 1, 90) + fill(4, "sell", 1, 95)
	out := reportOn(t, j, nil, "--from", "2026-01-01 03:00", "--to", "2026-01-02")
	mustHave(t, out, "closes: P&L +5.00", "P&L of the trades closed in the period: +5.00")
}

// A stop filled on the exchange is priced in the report from that fill, and
// the period's total is then what status says the account made: a live run
// on the fake exchange, a trade closed by rule and one whose stop the price
// gaps through (filled at the open, with slippage, not at the stop price).
func TestReportPricesAStopFilledOnTheExchangeAsStatusDoes(t *testing.T) {
	s := &scripted.Strategy{Decider: func(in strategy.Input) strategy.Decision {
		switch h := in.Now.Hour(); {
		case in.Position == nil && (h == 2 || h == 5):
			return strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: 95, Reason: "drop"}
		case in.Position != nil && h == 4:
			return strategy.Decision{Action: strategy.Exit, Reason: "reverted"}
		}
		return strategy.Decision{Reason: "no signal"}
	}}
	c, err := config.Parse([]byte("exchange: fake\nasset_class: crypto\ninterval: 1h\nsymbols: [AAA]\nrisk: {confirmed_by_user: true}\n"), s)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for i, o := range []float64{100, 100, 100, 104, 104, 100, 100, 92, 93, 93} {
		cs = append(cs, market.Candle{Time: t0.Add(time.Duration(i) * time.Hour), Open: o, High: o, Low: o - 1, Close: o, Volume: 10})
	}
	f := fake.New(fake.Options{Paper: true, Cash: 10_000, QtyStep: 0.001, MinQty: 0.001, SlippagePct: 0.1, Candles: map[string][]market.Candle{"AAA": cs}})
	dir := filepath.Join(t.TempDir(), ".scripted")
	loop.Run(context.Background(), c, s, f, exchange.Demo, &loop.FakeTicker{Ex: f, Candles: cs}, dir)

	var out bytes.Buffer
	if err := (App{Strategy: example.New()}).Run([]string{"report", "--dir", dir, "--utc", "--from", "2026-01-01", "--to", "2026-01-02"}, &out); err != nil {
		t.Fatal(err)
	}
	st, err := loop.ReadStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	mustHave(t, out.String(), "(its stop, filled on the exchange)",
		fmt.Sprintf("P&L of the trades closed in the period: %+.2f (prices", st.DayPnL))
	mustLack(t, out.String(), "not in the journal", "INCOMPLETE")
	if st.DayPnL > -1 {
		t.Fatalf("day P&L %.2f: the scenario's stop-out should leave the day at a loss\n%s", st.DayPnL, out.String())
	}
}

// A journal from before stop fills were recorded still reads: the trade
// its stop closed is listed without a P&L, and the total says it is
// incomplete and how many trades it misses.
func TestReportTotalIsIncompleteWhenAClosedOutsideHasNoPrice(t *testing.T) {
	j := fill(1, "buy", 1, 90) + fill(2, "sell", 1, 100) + fill(3, "buy", 2, 100) +
		`{"time":"2026-01-01T04:00:00Z","kind":"closed_outside","mode":"paper","symbol":"AAA"}` + "\n"
	out := reportOn(t, j, nil, "--from", "2026-01-01", "--to", "2026-01-02")
	mustHave(t, out, "P&L not in the journal",
		"P&L of the trades closed in the period: INCOMPLETE, 1 trade(s) closed outside the engine without a known exit price are missing; +10.00 is the others only")
}
