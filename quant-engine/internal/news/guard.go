package news

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Question is what the classifier is asked about one headline (the state):
// a yes/no question whose yes means a catastrophe for the asset.
type Question struct {
	Type         string            // "noul"
	Instructions string            // the question
	Criteria     map[string]string // optional: what a yes ("true") and a no ("false") mean
	Model        string            // the classifier's model id, e.g. a pinned version
}

// Verdict is the classifier's answer: Yes is the probability the answer is
// yes (0 to 1); Model is the model that answered.
type Verdict struct {
	Model string
	Yes   float64
}

// Classifier classifies a headline. It is a port: the engine wires the one
// the strategy's news policy names.
type Classifier interface {
	Classify(ctx context.Context, state string, q Question) (Verdict, error)
}

// Finding is one headline the keyword pre-filter matched for a symbol.
type Finding struct {
	Symbol, Headline, Feed string
	At                     time.Time // the headline's time; the check's when undated
	Until                  time.Time // At + lookback: while a catastrophe blocks entries
	Classified             bool      // the classifier answered
	Verdict                Verdict
	Err                    error // the classifier failed
	Catastrophe            bool  // above the threshold, the classifier failed, or no classifier
}

// Why says in one line how the finding was judged.
func (f Finding) Why(threshold float64) string {
	switch {
	case f.Err != nil:
		return "classifier failed, treated as a catastrophe: " + f.Err.Error()
	case !f.Classified:
		return "no classifier: a keyword match is a catastrophe"
	case f.Catastrophe:
		return fmt.Sprintf("catastrophe: yes %.4g >= threshold %.4g", f.Verdict.Yes, threshold)
	}
	return fmt.Sprintf("noise: yes %.4g < threshold %.4g", f.Verdict.Yes, threshold)
}

// Guard is the news check: the RSS keyword pre-filter, then the classifier
// on each headline it matched, once per headline (feed and text).
type Guard struct {
	RSS        *RSS
	Classifier Classifier // nil: every keyword match is a catastrophe
	Question   Question
	Threshold  float64       // a yes at or above it is a catastrophe
	Timeout    time.Duration // per classification; 0 = 10s

	mu   sync.Mutex
	seen map[string]Finding // feed + text -> judged once, until its Until (then dropped: an undated one still served is judged again)
}

// Check runs the pre-filter for symbol at at and judges every headline it
// matched; an unreadable feed is an error and judges nothing.
func (g *Guard) Check(ctx context.Context, symbol string, at time.Time) ([]Finding, error) {
	hs, err := g.RSS.Hits(ctx, symbol, at)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, h := range hs {
		f := g.judge(ctx, h, at)
		f.Symbol = symbol
		out = append(out, f)
	}
	return out, nil
}

func (g *Guard) judge(ctx context.Context, h Hit, at time.Time) Finding {
	key := h.Feed + "\n" + h.Text
	g.mu.Lock()
	f, ok := g.seen[key]
	g.mu.Unlock()
	if ok && f.Until.After(at) {
		return f
	}
	f = Finding{Headline: h.Text, Feed: h.Feed, At: at, Catastrophe: true}
	if !h.When.IsZero() {
		f.At = h.When
	}
	f.Until = f.At.Add(g.RSS.Lookback)
	if g.Classifier != nil {
		timeout := g.Timeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		f.Verdict, f.Err = g.Classifier.Classify(cctx, h.Text, g.Question)
		cancel()
		f.Classified = f.Err == nil
		f.Catastrophe = f.Err != nil || f.Verdict.Yes >= g.Threshold
		if f.Err != nil && ctx.Err() != nil {
			return f // the caller's deadline or stop cut it short, not the classifier: judged again at the next check
		}
	}
	g.mu.Lock()
	if g.seen == nil {
		g.seen = map[string]Finding{}
	}
	for k, old := range g.seen {
		if !old.Until.After(at) {
			delete(g.seen, k)
		}
	}
	g.seen[key] = f
	g.mu.Unlock()
	return f
}

// Clear is the check before an entry: not clear on any catastrophe.
func (g *Guard) Clear(ctx context.Context, symbol string, at time.Time) (bool, string, error) {
	fs, err := g.Check(ctx, symbol, at)
	if err != nil {
		return false, "", err
	}
	for _, f := range fs {
		if f.Catastrophe {
			return false, fmt.Sprintf("%q (%s): %s", f.Headline, f.Feed, f.Why(g.Threshold)), nil
		}
	}
	return true, "", nil
}
