package backtest

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
)

const cfgYAML = `
exchange: fake
asset_class: crypto
interval: 1h
symbols: [AAA]
strategy: {period: 3, stop_pct: 4, target_pct: 5}
risk: {confirmed_by_user: true}
`

func runWith(t *testing.T, costs string) (Result, string, string) {
	t.Helper()
	cs, err := market.ReadCSVFile("../testdata/backtest_one_trade.csv")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), example.New(), []byte(cfgYAML+costs), Input{Candles: cs, Capital: 10_000, QtyStep: 1, MinQty: 1})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := res.Write(dir); err != nil {
		t.Fatal(err)
	}
	trades, _ := os.ReadFile(filepath.Join(dir, "trades.csv"))
	summary, _ := os.ReadFile(filepath.Join(dir, "summary.json"))
	return res, string(trades), string(summary)
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// Entry at 105 (the close crosses above the 100 average): stop 100.8,
// target 110.25, size floor(100 / 4.2) = 23. The next close, 111, reaches the
// target.
func TestOneKnownTrade(t *testing.T) {
	res, trades, summary := runWith(t, "")
	if len(res.Trades) != 1 {
		t.Fatalf("trades = %+v, want 1", res.Trades)
	}
	tr := res.Trades[0]
	if tr.Qty != 23 || tr.EntryPrice != 105 || tr.ExitPrice != 111 || tr.ExitReason != "target" || !near(tr.PnL, 138) {
		t.Fatalf("trade = %+v, want 23 @ 105 -> 111 by target, P&L 138", tr)
	}
	if !near(tr.R, 138/(23*4.2)) {
		t.Fatalf("R = %v", tr.R)
	}
	if !strings.Contains(trades, "target") || !strings.Contains(summary, res.Summary.ParamsHash) || res.Summary.Status != "not validated" {
		t.Fatalf("outputs:\n%s\n%s", trades, summary)
	}
}

// Commission 0.1% and spread 0.2%: fills 105.105 and 110.889, commissions
// 2.417415 + 2.550447, so P&L = 23 x 5.784 - 4.967862 = 128.064138.
func TestCostsChangeTheResultByTheExpectedAmount(t *testing.T) {
	res, _, _ := runWith(t, "costs: {commission_pct: 0.1, spread_pct: 0.2}\n")
	if len(res.Trades) != 1 || !near(res.Trades[0].PnL, 128.064138) {
		t.Fatalf("trades = %+v, want P&L 128.064138", res.Trades)
	}
}

func TestSameInputSameBytes(t *testing.T) {
	_, t1, s1 := runWith(t, "costs: {commission_pct: 0.1}\n")
	_, t2, s2 := runWith(t, "costs: {commission_pct: 0.1}\n")
	if !bytes.Equal([]byte(t1+s1), []byte(t2+s2)) {
		t.Fatal("backtest output is not deterministic")
	}
}

func TestStopExit(t *testing.T) {
	cs, _ := market.ReadCSVFile("../testdata/backtest_one_trade.csv")
	cs[4].Low, cs[4].Close, cs[4].High = 99, 100, 105 // next candle crosses the 100.8 stop
	cs[5].Low, cs[5].Close, cs[5].High = 99, 100, 100 // and no new cross after it
	res, err := Run(context.Background(), example.New(), []byte(cfgYAML), Input{Candles: cs, Capital: 10_000, QtyStep: 1, MinQty: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Trades) != 1 || res.Trades[0].ExitReason != "stop" || !near(res.Trades[0].ExitPrice, 100.8) || !near(res.Trades[0].R, -1) {
		t.Fatalf("trades = %+v, want one stop exit at 100.8 (-1R)", res.Trades)
	}
}

// A strategy that reads another symbol gets it from Input.Aux, aligned.
func TestAuxCandlesMustBeAligned(t *testing.T) {
	cs, _ := market.ReadCSVFile("../testdata/backtest_one_trade.csv")
	_, err := Run(context.Background(), withAux{example.New()}, []byte(cfgYAML), Input{Candles: cs, Capital: 10_000})
	if err == nil || !strings.Contains(err.Error(), "BBB") {
		t.Fatalf("missing aux candles: %v", err)
	}
	res, err := Run(context.Background(), withAux{example.New()}, []byte(cfgYAML),
		Input{Candles: cs, Aux: map[string][]market.Candle{"BBB": cs}, Capital: 10_000, QtyStep: 1, MinQty: 1})
	if err != nil || len(res.Trades) != 1 {
		t.Fatalf("aligned aux candles: %+v %v", res.Trades, err)
	}
}

type withAux struct{ strategy.Strategy }

func (withAux) Inputs() []string { return []string{"BBB"} }

// A bad strategy: block fails before any candle is replayed.
func TestBadStrategyBlockFailsEarly(t *testing.T) {
	cs, _ := market.ReadCSVFile("../testdata/backtest_one_trade.csv")
	_, err := Run(context.Background(), example.New(), []byte(strings.Replace(cfgYAML, "period: 3", "period: 1", 1)), Input{Candles: cs, Capital: 10_000})
	if err == nil || !strings.Contains(err.Error(), "period") {
		t.Fatalf("bad block: %v", err)
	}
}
