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
)

type Event struct {
	Time       time.Time `json:"time"`
	Kind       string    `json:"kind"`
	Mode       string    `json:"mode"`
	ParamsHash string    `json:"params_hash"`
	ClientID   string    `json:"client_id,omitempty"`
	Symbol     string    `json:"symbol,omitempty"`
	Side       string    `json:"side,omitempty"`
	Qty        float64   `json:"qty,omitempty"`
	Price      float64   `json:"price,omitempty"`
	Reason     string    `json:"reason,omitempty"`
}

type Journal struct {
	mu         sync.Mutex
	f          *os.File
	mode, hash string
}

// Open opens (creating if needed) dir/journal.jsonl for appending.
func Open(dir, mode, paramsHash string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "journal.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	return &Journal{f: f, mode: mode, hash: paramsHash}, nil
}

// Append writes one event as a single line, stamped with mode and hash.
func (j *Journal) Append(e Event) error {
	e.Mode, e.ParamsHash = j.mode, j.hash
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
