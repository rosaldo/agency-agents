package loop

import (
	"os"
	"testing"

	"github.com/rosaldo/agency-agents/quant-engine/internal/risk"
)

// A state file written before graduated entries has no "trades": it loads
// as no tranche in progress, everything else intact; a new one carries the
// budget and the fraction used across a restart.
func TestStateWithoutTradesLoadsAsNoTrancheInProgress(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"running":false,"pid":0,"mode":"paper","level":"paper","params_hash":"abc","heartbeat":"2026-10-30T10:00:00Z",` +
		`"equity":10000,"breaker":{"day":"2026-10-30T00:00:00Z","week":"2026-10-27T00:00:00Z","day_open":10000,"week_open":10000,"last_equity":10000,"mode":"paper"},` +
		`"limits":{"max_risk_per_trade_pct":1,"max_daily_drawdown_pct":3,"max_weekly_drawdown_pct":6,"min_demo_days":0},` +
		`"positions":[{"Symbol":"AAA","Side":"long","Qty":10,"AvgPrice":105,"StopClientID":"s1","StopPrice":100}],"targets":{"AAA":110},"last_event":"","stop_reason":"stopped"}`
	if err := os.WriteFile(statePath(dir, "paper"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(dir, "paper")
	if err != nil {
		t.Fatal(err)
	}
	if st.Trades != nil || st.Targets["AAA"] != 110 || len(st.Positions) != 1 || st.Equity != 10000 {
		t.Fatalf("legacy state %+v, want no trades and the rest intact", st)
	}
	st.Trades = map[string]risk.Trade{"AAA": {Budget: 100, Used: 0.5}}
	if err := SaveState(dir, st); err != nil {
		t.Fatal(err)
	}
	back, err := LoadState(dir, "paper")
	if err != nil || back.Trades["AAA"] != (risk.Trade{Budget: 100, Used: 0.5}) {
		t.Fatalf("round trip %+v (%v), want budget 100 used 0.5", back.Trades, err)
	}
}
