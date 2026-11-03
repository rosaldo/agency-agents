package engine

import (
	"context"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
)

// The stream ended an older candle than REST's last closed one: REST's
// newer candle is not the one expected either, so no decision is taken on
// it (candle_not_final). The same expectation met decides as usual.
func TestRESTCandleNewerThanTheStreamsIsNotDecidedOn(t *testing.T) {
	for name, back := range map[string]time.Duration{"newer": time.Hour, "control": 0} {
		t.Run(name, func(t *testing.T) {
			e, f, _ := trancheEngine(t, "", []float64{105}, enter(1, 100))
			cs, err := f.Candles(context.Background(), e.Env, "AAA", time.Hour, 1)
			if err != nil || len(cs) != 1 {
				t.Fatal(cs, err)
			}
			want := cs[0].Time.Add(-back)
			e.ClosedCandle = func() time.Time { return want }
			steps(t, e, f, 1)
			nf := 0
			for _, ev := range *journalOf(e) {
				if ev.Kind == journal.CandleNotFinal {
					nf++
				}
			}
			if decided := f.Calls("PlaceOrder") > 0; decided != (back == 0) || (nf == 1) != (back > 0) {
				t.Fatalf("decided %v, candle_not_final %d; REST is %s, the stream ended %s", decided, nf, cs[0].Time, want)
			}
		})
	}
}
