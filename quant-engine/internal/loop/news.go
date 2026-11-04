package loop

import (
	"context"
	"fmt"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/news"
)

// newsResult is one symbol's news check after its entry: the findings not
// handed over before, or why the feeds could not be read.
type newsResult struct {
	symbol string
	found  []news.Finding
	err    error
}

type newsAsk struct {
	symbols []string
	at      time.Time
}

// newsWorker checks, off the loop's goroutine, the symbols the engine holds
// (crypto with news_guard required): it only reads the feeds and asks the
// classifier; the loop journals and acts on what it hands back.
type newsWorker struct {
	guard *news.Guard
	ask   chan newsAsk
	out   chan newsResult
}

func startNewsWorker(ctx context.Context, g *news.Guard) *newsWorker {
	w := &newsWorker{guard: g, ask: make(chan newsAsk, 1), out: make(chan newsResult, 16)}
	go func() {
		handed := map[string]time.Time{} // symbol + feed + headline -> the Until of the finding handed over
		for {
			var a newsAsk
			select {
			case a = <-w.ask:
			case <-ctx.Done():
				return
			}
			for k, until := range handed {
				if !until.After(a.at) {
					delete(handed, k) // past its lookback: a headline judged again is handed over again
				}
			}
			for _, sym := range a.symbols {
				fs, err := g.Check(ctx, sym, a.at)
				r := newsResult{symbol: sym, err: err}
				for _, f := range fs {
					k := sym + "\n" + f.Feed + "\n" + f.ClassifierText
					if _, ok := handed[k]; !ok {
						handed[k] = f.Until
						r.found = append(r.found, f)
					}
				}
				if err == nil && len(r.found) == 0 {
					continue
				}
				select {
				case w.out <- r:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return w
}

// check asks for the symbols held after a step; while the worker is still
// on the previous ask, this one is dropped: the next step asks again.
func (w *newsWorker) check(symbols []string, at time.Time) {
	if w == nil || len(symbols) == 0 {
		return
	}
	select {
	case w.ask <- newsAsk{symbols, at}:
	default:
	}
}

// results is nil without a worker: a nil channel never delivers.
func (w *newsWorker) results() <-chan newsResult {
	if w == nil {
		return nil
	}
	return w.out
}

// emergencyExit is engine.Engine.EmergencyExit.
type emergencyExit func(ctx context.Context, sym string, until time.Time, reason string) error

// handleNews journals a result and, on a catastrophe, exits at once and
// blocks the symbol; on the loop's goroutine. A result that comes during a
// stop is dropped: a classification the stop cut short is no verdict, and
// the next run checks the headline again.
func handleNews(ctx context.Context, exit emergencyExit, threshold float64, r newsResult, note func(journal.Event), record func(kind, reason string)) {
	if ctx.Err() != nil {
		return
	}
	if r.err != nil {
		note(journal.Event{Kind: journal.NewsCheckFailed, Symbol: r.symbol,
			Reason: "the position stays, its stop on the exchange; checked again at the next step: " + r.err.Error()})
		return
	}
	for _, f := range r.found {
		note(journal.Event{Kind: journal.NewsHit, Symbol: r.symbol, Reason: fmt.Sprintf("%q (%s)", f.Headline, f.Feed)})
		note(journal.Event{Kind: journal.NewsVerdict, Symbol: r.symbol, Model: f.Verdict.Model, Reason: fmt.Sprintf("%q: %s", f.Headline, f.Why(threshold))})
		if !f.Catastrophe {
			continue
		}
		if err := exit(ctx, r.symbol, f.Until, fmt.Sprintf("%q: %s", f.Headline, f.Why(threshold))); err != nil && ctx.Err() == nil {
			record(Error, "emergency exit "+r.symbol+": "+err.Error()+"; retried at the next step")
		}
	}
}

// next waits for the next candle; meanwhile it hands each news result over
// as it comes, so an emergency exit happens in the same cycle as its verdict.
func next(ctx context.Context, tick Ticker, results <-chan newsResult, handle func(newsResult)) error {
	if results == nil {
		return tick.Next(ctx)
	}
	done := make(chan error, 1)
	go func() { done <- tick.Next(ctx) }()
	for {
		select {
		case err := <-done:
			return err
		case r := <-results:
			handle(r)
		}
	}
}
