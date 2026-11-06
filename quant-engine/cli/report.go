package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/loop"
)

// period is the span a report covers, [from, to), shown in loc.
type period struct {
	from, to time.Time
	loc      *time.Location
}

// reportPeriod reads --since or --from/--to; times without a zone are in loc.
func reportPeriod(since time.Duration, from, to string, loc *time.Location, now time.Time) (period, error) {
	p := period{from: now.Add(-since), to: now, loc: loc}
	if from == "" && to != "" {
		return p, errors.New("report: --to needs --from")
	}
	if from == "" {
		if since <= 0 {
			return p, errors.New("report: --since must be positive")
		}
		return p, nil
	}
	var err error
	if p.from, err = parseWhen(from, loc); err != nil {
		return p, err
	}
	if to != "" {
		if p.to, err = parseWhen(to, loc); err != nil {
			return p, err
		}
	}
	if !p.to.After(p.from) {
		return p, errors.New("report: --to must be after --from")
	}
	return p, nil
}

func parseWhen(s string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, l := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(l, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("report: time %q: use YYYY-MM-DD, YYYY-MM-DD HH:MM or RFC 3339", s)
}

func (p period) has(t time.Time) bool   { return !t.Before(p.from) && t.Before(p.to) }
func (p period) fmt(t time.Time) string { return t.In(p.loc).Format("2006-01-02 15:04") }

// numbers is what varies between two evaluations of one rule: reasons are
// grouped by their text with the numbers taken out.
var numbers = regexp.MustCompile(`[-+]?\d+(\.\d+)?([eE][-+]?\d+)?`)

// printReport writes, for one period of dir's journal, what the run saw
// (candles evaluated per symbol), what it analyzed (the reasons) and what it
// traded (fills, P&L of closed trades, open positions from status).
func printReport(dir string, p period, w io.Writer) error {
	// filtered while read: the period's events, and for OPERATED only the
	// order events, the earlier fills that price a trade opened before the
	// period, and a stop sent just after its end
	var in, ops []journal.Event
	counts := map[string]int{}
	warns, err := journal.Scan(filepath.Join(dir, "journal.jsonl"), func(e journal.Event) {
		if p.has(e.Time) {
			in = append(in, e)
			counts[e.Kind]++
		}
		switch e.Kind {
		case journal.Filled, journal.ClosedOutside:
			if e.Time.Before(p.to) {
				ops = append(ops, e)
			}
		case journal.Sent, journal.Resting:
			if !e.Time.Before(p.from) && e.Time.Before(p.to.Add(stopWithin)) {
				ops = append(ops, e)
			}
		}
	})
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("report: no journal in %s (never started?)", dir)
	}
	if err != nil {
		return err
	}
	b := &strings.Builder{}
	pr := func(f string, a ...any) { fmt.Fprintf(b, f+"\n", a...) }
	zone, _ := p.to.In(p.loc).Zone()
	pr("Report %s -> %s (%s)", p.fmt(p.from), p.fmt(p.to), zone)

	runs := map[string]bool{}
	for _, e := range in {
		if e.Level != "" {
			runs[e.Level] = true
		}
	}
	pr("Events in the period: %d%s", len(in), levels(runs))
	for _, w := range warns {
		pr("  warning: %s", w)
	}

	reportSeen(pr, p, in)
	reportAnalyzed(pr, p, in)
	reportOperated(pr, p, ops, dir)
	reportOther(pr, counts)
	_, err = io.WriteString(w, b.String())
	return err
}

func levels(runs map[string]bool) string {
	if len(runs) == 0 {
		return ""
	}
	var ls []string
	for l := range runs {
		ls = append(ls, l)
	}
	slices.Sort(ls)
	return " (level " + strings.Join(ls, ", ") + ")"
}

type printer func(string, ...any)

func symbols(in []journal.Event, keep func(journal.Event) bool) []string {
	var out []string
	for _, e := range in {
		if e.Symbol != "" && keep(e) && !slices.Contains(out, e.Symbol) {
			out = append(out, e.Symbol)
		}
	}
	slices.Sort(out)
	return out
}

func evaluated(e journal.Event) bool { return e.Kind == journal.Evaluated }

// reportSeen: per symbol, the distinct candles evaluated (a restart that
// decides a candle again counts it once), the first and last close, and the
// exceptions: candles missing between two seen ones, and evaluations the
// strategy gave no reason for.
func reportSeen(pr printer, p period, in []journal.Event) {
	pr("\nSEEN (candles evaluated, by candle close)")
	syms := symbols(in, evaluated)
	if len(syms) == 0 {
		pr("  none: no evaluated event in this period (a run that was stopped, or an engine that did not record evaluations)")
		return
	}
	var exc []string
	for _, s := range syms {
		seen, bare := map[time.Time]bool{}, map[time.Time]bool{}
		for _, e := range in {
			if evaluated(e) && e.Symbol == s {
				c := e.CandleClose.UTC() // one key per instant, whatever offset it was written with
				seen[c] = true
				if e.Reason == "" {
					bare[c] = true
				}
			}
		}
		closes := sortedTimes(seen)
		pr("  %-16s %5d candles  first %s  last %s", s, len(closes), p.fmt(closes[0]), p.fmt(closes[len(closes)-1]))
		exc = append(exc, gaps(p, s, closes)...)
		if b := sortedTimes(bare); len(b) > 0 {
			exc = append(exc, fmt.Sprintf("  FAIL %s: %d evaluated without a reason (%s -> %s)", s, len(b), p.fmt(b[0]), p.fmt(b[len(b)-1])))
		}
	}
	if len(exc) == 0 {
		pr("  exceptions: none")
		return
	}
	pr("  exceptions:")
	for _, x := range exc {
		pr("%s", x)
	}
}

func sortedTimes(set map[time.Time]bool) []time.Time {
	ts := make([]time.Time, 0, len(set))
	for t := range set {
		ts = append(ts, t)
	}
	slices.SortFunc(ts, func(a, b time.Time) int { return a.Compare(b) })
	return ts
}

// gaps lists the candles missing between consecutive closes. The interval is
// the smallest step between two closes seen, so the report needs no config.
// shortcut: a period where no two consecutive candles were seen infers a
// multiple of the interval; read the config's interval if that matters.
func gaps(p period, sym string, closes []time.Time) []string {
	var step time.Duration
	for i := 1; i < len(closes); i++ {
		if d := closes[i].Sub(closes[i-1]); step == 0 || d < step {
			step = d
		}
	}
	var out []string
	for i := 1; i < len(closes); i++ {
		if n := int(closes[i].Sub(closes[i-1])/step) - 1; n > 0 {
			out = append(out, fmt.Sprintf("  GAP %s: %d candle(s) missing between %s and %s", sym, n, p.fmt(closes[i-1]), p.fmt(closes[i])))
		}
	}
	return out
}

// reportAnalyzed: the strategy's reasons grouped by rule (numbers taken
// out) with counts and the last example; every entry and add in full; and
// the signals the engine refused, with why.
func reportAnalyzed(pr printer, p period, in []journal.Event) {
	pr("\nANALYZED (the strategy's reasons)")
	syms := symbols(in, func(e journal.Event) bool { return evaluated(e) || isSkip(e) })
	if len(syms) == 0 {
		pr("  nothing analyzed in this period")
		return
	}
	for _, s := range syms {
		pr("  %s", s)
		type group struct {
			n    int
			last string
		}
		groups, order := map[string]*group{}, []string{}
		for _, e := range in {
			if e.Symbol != s {
				continue
			}
			switch {
			case evaluated(e) && (e.Action == "enter" || e.Action == "add"):
				pr("    %s %s %s on the candle closed %s: %s", p.fmt(e.Time), e.Action, e.Side, p.fmt(e.CandleClose), orNone(e.Reason))
			case evaluated(e) && e.Reason != "":
				key := e.Action + ": " + numbers.ReplaceAllString(e.Reason, "#")
				if groups[key] == nil {
					groups[key] = &group{}
					order = append(order, key)
				}
				groups[key].n++
				groups[key].last = e.Reason
			case isSkip(e):
				pr("    %s refused by the engine: %s", p.fmt(e.Time), strings.TrimPrefix(e.Reason, "signal skipped: "))
			}
		}
		for _, k := range order {
			g := groups[k]
			action, _, _ := strings.Cut(k, ": ")
			pr("    %5dx %s: %s", g.n, action, g.last)
		}
	}
}

func isSkip(e journal.Event) bool {
	return e.Kind == journal.Rejected && strings.HasPrefix(e.Reason, "signal skipped: ")
}

func orNone(s string) string {
	if s == "" {
		return "(no reason given)"
	}
	return s
}

// trade is one symbol's position as the journal's fills build it.
type trade struct {
	qty, avg float64 // qty > 0 long, < 0 short
}

// reportOperated: the fills of the period with the stop placed after them,
// the P&L of each trade closed in the period (fills from the whole journal
// build the position, so a trade opened before the period is priced right),
// and the open positions from status. all holds only the order events.
func reportOperated(pr printer, p period, all []journal.Event, dir string) {
	pr("\nOPERATED")
	pos := map[string]*trade{}
	var total float64
	var lines, closed []string
	for i, e := range all {
		if e.Kind == journal.ClosedOutside {
			delete(pos, e.Symbol) // before the period too: the next trade starts flat
			if p.has(e.Time) {
				closed = append(closed, fmt.Sprintf("    %s %s closed outside the engine (its stop, a liquidation or a manual close): P&L not in the journal", p.fmt(e.Time), e.Symbol))
			}
		}
		if e.Kind != journal.Filled || e.Qty <= 0 {
			continue
		}
		t := pos[e.Symbol]
		if t == nil {
			t = &trade{}
			pos[e.Symbol] = t
		}
		signed := e.Qty
		if e.Side == string(exchange.Sell) {
			signed = -signed
		}
		var pnl float64
		closing := t.qty != 0 && (t.qty > 0) != (signed > 0)
		if closing {
			q := min(abs(signed), abs(t.qty))
			pnl = q * (e.Price - t.avg)
			if t.qty < 0 {
				pnl = -pnl
			}
			if abs(signed) > abs(t.qty) { // turns side: the rest opens at this fill's price
				t.avg = e.Price
			}
			t.qty += signed
		} else {
			t.avg = (t.avg*abs(t.qty) + e.Price*abs(signed)) / (abs(t.qty) + abs(signed))
			t.qty += signed
		}
		if abs(t.qty) < 1e-12 {
			delete(pos, e.Symbol)
		}
		if !p.has(e.Time) {
			continue
		}
		line := fmt.Sprintf("    %s %-16s %-4s qty %-10.8g price %-12.8g", p.fmt(e.Time), e.Symbol, e.Side, e.Qty, e.Price)
		if closing {
			total += pnl
			line += fmt.Sprintf(" closes: P&L %+.2f", pnl)
		} else if stop := stopAfter(e, all[i+1:]); stop > 0 {
			line += fmt.Sprintf(" stop %.8g", stop)
		}
		if e.Reason != "" {
			line += "  (" + e.Reason + ")"
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 && len(closed) == 0 {
		pr("  orders: none filled in this period")
	} else {
		pr("  orders filled:")
		for _, l := range append(lines, closed...) {
			pr("%s", l)
		}
		pr("  P&L of the trades closed in the period: %+.2f (prices of the fills; fees and funding not included)", total)
	}
	s, err := loop.ReadStatus(dir)
	if err != nil {
		pr("  open positions: status unreadable: %v", err)
		return
	}
	var open []exchange.Position
	for _, ps := range s.Positions {
		if ps.Qty > 0 {
			open = append(open, ps)
		}
	}
	if len(open) == 0 {
		pr("  open positions now: none")
		return
	}
	pr("  open positions now:")
	for _, ps := range open {
		line := fmt.Sprintf("    %-16s %-5s qty %.8g avg %.8g stop %.8g", ps.Symbol, ps.PSide(), ps.Qty, ps.AvgPrice, ps.StopPrice)
		if t, ok := s.Trades[ps.Symbol]; ok {
			line += fmt.Sprintf("  budget used %.0f%% in %d tranche(s), trade stop %.8g", t.BudgetUsed*100, t.Tranches, t.Stop)
		}
		pr("%s", line)
	}
}

// stopWithin bounds how long after a fill its stop is sent: the engine
// places it in the same step.
const stopWithin = time.Minute

// stopAfter is the price of the first stop resting on sym after fill: the
// engine journals the stop's price on its sent event, and on its open
// (resting) event only the order's state.
func stopAfter(fill journal.Event, rest []journal.Event) float64 {
	sent := map[string]float64{} // client ID -> price sent
	for _, e := range rest {
		if e.Time.After(fill.Time.Add(stopWithin)) {
			return 0
		}
		if e.Symbol != fill.Symbol {
			continue
		}
		switch e.Kind {
		case journal.Sent:
			sent[e.ClientID] = e.Price
		case journal.Resting:
			return sent[e.ClientID]
		case journal.Filled:
			return 0
		}
	}
	return 0
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// reportOther counts the rest of the period's events (start, stop,
// heartbeat, blocks, halts...).
func reportOther(pr printer, counts map[string]int) {
	var ks []string
	for k := range counts {
		switch k {
		case journal.Evaluated, journal.Filled, journal.Proposed, journal.Sent, journal.Resting, journal.Canceled:
		default:
			ks = append(ks, fmt.Sprintf("%s %d", k, counts[k]))
		}
	}
	slices.Sort(ks)
	if len(ks) == 0 {
		ks = []string{"none"}
	}
	pr("\nOTHER EVENTS: %s", strings.Join(ks, ", "))
}
