// Package risk holds the hard limits: position sizing by the per-trade risk
// budget and the drawdown circuit breaker (once the daily or weekly loss
// reaches its limit, no entry is allowed until the next day or week).
package risk

import (
	"errors"
	"fmt"
	"math"
	"time"
)

var ErrHalted = errors.New("risk: halted")

// Breaker measures drawdown against the equity first seen in the current day
// and week (weeks start Monday) of Loc. A daily halt lasts until the next
// day, a weekly halt until the next week; Snapshot/Restore carry both bases
// and the halt across restarts, so stopping and starting again cannot reset them.
type Breaker struct {
	DailyPct, WeeklyPct float64
	Loc                 *time.Location

	day, week         time.Time
	dayOpen, weekOpen float64
	reason            string
	weekly            bool
	lastEquity        float64
}

// Snapshot is the breaker state persisted in state.json.
type Snapshot struct {
	Day        time.Time `json:"day"`
	Week       time.Time `json:"week"`
	DayOpen    float64   `json:"day_open"`
	WeekOpen   float64   `json:"week_open"`
	Halt       string    `json:"halt,omitempty"`
	HaltWeekly bool      `json:"halt_weekly,omitempty"`
	LastEquity float64   `json:"last_equity"`
	Mode       string    `json:"mode"` // set by the loop: bases belong to one account
}

func (b *Breaker) Snapshot() Snapshot {
	return Snapshot{b.day, b.week, b.dayOpen, b.weekOpen, b.reason, b.weekly, b.lastEquity, ""}
}

func (b *Breaker) Restore(s Snapshot) {
	b.day, b.week, b.dayOpen, b.weekOpen, b.reason, b.weekly = s.Day, s.Week, s.DayOpen, s.WeekOpen, s.Halt, s.HaltWeekly
	b.lastEquity = s.LastEquity
}

// RestoreFor restores s only for the same mode (paper and live are different
// accounts). A halt is always kept. Without a halt, an equity more than the
// daily limit ABOVE the last one seen (capital added outside the engine)
// starts fresh bases; a lower equity keeps them, so a loss taken while
// stopped still counts. The returned note explains any fresh start.
func (b *Breaker) RestoreFor(s Snapshot, mode string, equity float64) string {
	if s.Mode != mode {
		*b = Breaker{DailyPct: b.DailyPct, WeeklyPct: b.WeeklyPct, Loc: b.Loc}
		if s.Mode == "" && s.Day.IsZero() {
			return ""
		}
		return fmt.Sprintf("drawdown bases start fresh: the last run was %q, this one is %q", s.Mode, mode)
	}
	b.Restore(s)
	if b.reason == "" && s.LastEquity > 0 && (equity-s.LastEquity)/s.LastEquity*100 > b.DailyPct {
		b.day, b.week = time.Time{}, time.Time{}
		return fmt.Sprintf("drawdown bases start fresh: equity %.2f is above the last seen %.2f", equity, s.LastEquity)
	}
	return ""
}

// Trip halts until the next day for a reason outside drawdown, such as a
// stop the exchange would not take.
func (b *Breaker) Trip(reason string) { b.reason, b.weekly = reason, false }

// Allow updates the breaker with the current equity and refuses a new entry
// while a limit is reached.
func (b *Breaker) Allow(now time.Time, equity float64) error {
	b.update(now, equity)
	if b.reason != "" {
		return fmt.Errorf("%w: %s", ErrHalted, b.reason)
	}
	return nil
}

// Opens returns the equity bases of the current day and week.
func (b *Breaker) Opens() (day, week float64) { return b.dayOpen, b.weekOpen }

// Halted is the reason the breaker tripped, or "" while trading is allowed.
func (b *Breaker) Halted() string { return b.reason }

func (b *Breaker) update(now time.Time, equity float64) {
	t := now.In(b.Loc)
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, b.Loc)
	week := day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
	if !day.Equal(b.day.In(b.Loc)) {
		b.day, b.dayOpen = day, equity
		if !b.weekly {
			b.reason = ""
		}
	}
	if !week.Equal(b.week.In(b.Loc)) {
		b.week, b.weekOpen = week, equity
		b.reason, b.weekly = "", false
	}
	b.lastEquity = equity
	if b.reason != "" {
		return
	}
	if dd := drawdown(b.dayOpen, equity); dd >= b.DailyPct {
		b.reason = fmt.Sprintf("daily drawdown %.2f%% reached the %.2f%% limit", dd, b.DailyPct)
	} else if dd := drawdown(b.weekOpen, equity); dd >= b.WeeklyPct {
		b.reason, b.weekly = fmt.Sprintf("weekly drawdown %.2f%% reached the %.2f%% limit", dd, b.WeeklyPct), true
	}
}

func drawdown(open, equity float64) float64 {
	if open <= 0 {
		return 0
	}
	return (open - equity) / open * 100
}

// SizeInput is what a position size depends on.
type SizeInput struct {
	Equity      float64 // account equity in quote currency
	RiskPct     float64 // risk.max_risk_per_trade_pct
	Entry, Stop float64 // long: 0 < Stop < Entry
	SpreadPct   float64 // costs.spread_pct: half of it is paid on each fill
	SlippagePct float64 // costs.slippage_pct: paid on each fill, entry and stop
	// CommissionPct is the taker fee per fill (costs.commission_pct), paid on
	// the entry and on the stop notional. No leverage here: the size comes
	// from the risk budget, leverage only sets the margin locked (FR-R1).
	CommissionPct float64
	QtyStep       float64 // market's quantity increment (0 = fractional)
	MinQty        float64 // market's minimum quantity
}

// Size is the quantity whose loss at the stop stays within the per-trade risk
// budget, with both fills at their worst: half the spread plus the slippage
// against each leg, and the commission on each fill's notional. qty 0 means do not enter; why says why.
func Size(in SizeInput) (qty float64, why string) {
	if in.Stop <= 0 || in.Stop >= in.Entry {
		return 0, fmt.Sprintf("stop %.8g is not between 0 and the entry %.8g", in.Stop, in.Entry)
	}
	adj := (in.SpreadPct/2 + in.SlippagePct) / 100
	buy, sell := in.Entry*(1+adj), in.Stop*(1-adj)
	perUnit := buy - sell + (buy+sell)*in.CommissionPct/100
	raw := in.Equity * in.RiskPct / 100 / perUnit
	if in.QtyStep > 0 {
		raw = math.Floor(raw/in.QtyStep) * in.QtyStep
	}
	if raw <= 0 || raw < in.MinQty {
		return 0, fmt.Sprintf("size %.8g is below the minimum quantity %.8g", raw, in.MinQty)
	}
	return raw, fmt.Sprintf("entry %.8g stop %.8g qty %.8g", in.Entry, in.Stop, raw)
}
