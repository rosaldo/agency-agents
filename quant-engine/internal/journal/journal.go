// Package journal is the append-only trade journal (journal.jsonl): one JSON
// event per line, stamped with the mode and parameter hash of the run.
package journal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event kinds.
const (
	Proposed = "proposed"
	Sent     = "sent"
	Filled   = "filled"
	Resting  = "open" // a stop resting on the exchange
	Canceled = "canceled"
	Rejected = "rejected"
	Limit    = "limit" // a risk limit was reached
	Halt     = "halt"
	// a symbol takes no new entry, with the reason; ForeignQty
	// when the account holds more than the engine's own quantity.
	Blocked    = "blocked"
	ForeignQty = "foreign_qty_detected"
	Unblocked  = "unblocked"
	// ClosedOutside: the engine's position is gone without its own exit
	// (stop, liquidation or a close outside the engine); target cleared.
	ClosedOutside = "closed_outside"
	Funding       = "funding" // a funding payment as the venue reports it (Amount)
	// Derivatives: a gap or recovery of the derivatives snapshots, or the
	// recorder switched off, with the reason.
	Derivatives = "derivatives"
	// CandleNotFinal: with feed: websocket, REST's last closed candle is not
	// the one the stream said ended; the symbol takes no decision that step.
	CandleNotFinal = "candle_not_final"
	// FeedDegraded / FeedRestored: the candle stream dropped (the loop then
	// steps at the boundary plus the grace, as with feed: rest) or came back.
	FeedDegraded = "feed_degraded"
	FeedRestored = "feed_restored"
	// NewsHit: a recent headline names the asset with a keyword (crypto,
	// after the entry); NewsVerdict: how it was judged, with the classifier's
	// model; NewsCheckFailed: a feed was unreadable: the position stays, unless
	// another feed brings a catastrophe;
	// EmergencyExit: a catastrophe closes the position and blocks the symbol.
	NewsHit         = "news_hit"
	NewsVerdict     = "news_verdict"
	NewsCheckFailed = "news_check_failed"
	EmergencyExit   = "emergency_exit"
	// Retarget: a strategy.Retargeter's target set on the position as filled
	// (old and new target, average price, quantity), refused, or put off
	// because the position could not be read after the fill.
	Retarget = "retarget"
	// Evaluated: the strategy decided on a closed candle of the symbol
	// (live levels only): its action and reason, and the candle's close time.
	// An empty reason is the strategy giving none, never filled in here.
	Evaluated = "evaluated"
)

type Event struct {
	Time        time.Time `json:"time"`
	Kind        string    `json:"kind"`
	Mode        string    `json:"mode"`
	Level       string    `json:"level,omitempty"` // paper | testnet | mainnet; absent in journals from before the split
	ParamsHash  string    `json:"params_hash"`
	ClientID    string    `json:"client_id,omitempty"`
	Symbol      string    `json:"symbol,omitempty"`
	Side        string    `json:"side,omitempty"`
	Qty         float64   `json:"qty,omitempty"`
	Price       float64   `json:"price,omitempty"`
	Amount      float64   `json:"amount,omitempty"` // funding, in the settle currency
	Model       string    `json:"model,omitempty"`  // news_verdict: the classifier's model that answered
	Reason      string    `json:"reason,omitempty"`
	Action      string    `json:"action,omitempty"`      // evaluated: none | enter | add | exit
	CandleClose time.Time `json:"candle_close,omitzero"` // evaluated: close time of the decided candle
}

type Journal struct {
	mu                sync.Mutex
	f                 *os.File
	mode, level, hash string
}

// Open opens (creating if needed) dir/journal.jsonl for appending.
func Open(dir, mode, level, paramsHash string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "journal.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	return &Journal{f: f, mode: mode, level: level, hash: paramsHash}, nil
}

// Append writes one event as a single line, stamped with mode, level and hash.
func (j *Journal) Append(e Event) error {
	e.Mode, e.Level, e.ParamsHash = j.mode, j.level, j.hash
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	return nil
}

func (j *Journal) Close() error { return j.f.Close() }

// Read returns every parseable event; an unparseable line (a write cut short
// by a crash) is skipped and reported as a warning instead of failing.
func Read(path string) (events []Event, warnings []string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("journal: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			warnings = append(warnings, fmt.Sprintf("journal line %d skipped: not a complete event", n))
			continue
		}
		events = append(events, e)
	}
	if err := sc.Err(); err != nil {
		return events, warnings, fmt.Errorf("journal: %w", err)
	}
	return events, warnings, nil
}
