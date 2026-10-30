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

// Commission 0.1% and slippage 0.1% (the half spread of 0.2%): sizing assumes
// the slippage on both legs (buy 105.105, stop fill 100.6992) and commission on both fills,
// floor(100 / (4.4058 + 0.2058042)) = 21; fills 105.105 and 110.889,
// commissions 2.207205 + 2.328669, so P&L = 21 x 5.784 - 4.535874 = 116.928126.
func TestCostsChangeTheResultByTheExpectedAmount(t *testing.T) {
	res, _, _ := runWith(t, "sim: {profiles: {real: {commission_pct: 0.1, slippage_pct: 0.1}}}\n")
	if len(res.Trades) != 1 || res.Trades[0].Qty != 21 || !near(res.Trades[0].PnL, 116.928126) {
		t.Fatalf("trades = %+v, want 21 units, P&L 116.928126", res.Trades)
	}
}

func TestSameInputSameBytes(t *testing.T) {
	_, t1, s1 := runWith(t, "sim: {profiles: {real: {commission_pct: 0.1}}}\n")
	_, t2, s2 := runWith(t, "sim: {profiles: {real: {commission_pct: 0.1}}}\n")
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

// The backtest charges the sim profile; the same backtest
// ends optimistic >= real >= pessimistic, and the summary names the profile.
func TestBacktestProfilesOrderTheResult(t *testing.T) {
	cs, err := market.ReadCSVFile("../testdata/backtest_one_trade.csv")
	if err != nil {
		t.Fatal(err)
	}
	var pnl []float64
	for _, p := range []string{"optimistic", "real", "pessimistic"} {
		y := strings.Replace(cfgYAML, "exchange: fake", "exchange: hyperliquid", 1) + "sim: {profile: " + p + ", liquidity_class: thin}\n"
		res, err := Run(context.Background(), example.New(), []byte(y), Input{Candles: cs, Capital: 10_000, QtyStep: 1, MinQty: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Trades) != 1 || res.Summary.SimProfile != p {
			t.Fatalf("%s: trades %+v, summary profile %q", p, res.Trades, res.Summary.SimProfile)
		}
		pnl = append(pnl, res.Summary.EndEquity-res.Summary.StartEquity)
	}
	if !(pnl[0] > pnl[1] && pnl[1] > pnl[2]) {
		t.Fatalf("P&L optimistic, real, pessimistic = %v; want strictly decreasing", pnl)
	}
}

// The backtest's fills are simulated: sizing counts the active profile as it
// is, without the real-fills floor, so optimistic with a zero fee buys more
// than real (FR-R1).
func TestBacktestSizesWithTheActiveProfile(t *testing.T) {
	cs, err := market.ReadCSVFile("../testdata/backtest_one_trade.csv")
	if err != nil {
		t.Fatal(err)
	}
	qty := map[string]float64{}
	for p, sim := range map[string]string{
		"real":       "sim: {liquidity_class: major}\n",
		"optimistic": "sim: {profile: optimistic, liquidity_class: major, profiles: {optimistic: {commission_pct: 0}}}\n",
	} {
		y := strings.Replace(cfgYAML, "exchange: fake", "exchange: hyperliquid", 1) + sim
		res, err := Run(context.Background(), example.New(), []byte(y), Input{Candles: cs, Capital: 10_000})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Trades) != 1 {
			t.Fatalf("%s: trades %+v", p, res.Trades)
		}
		qty[p] = res.Trades[0].Qty
	}
	if qty["optimistic"] <= qty["real"] {
		t.Fatalf("qty optimistic %v, real %v; want optimistic larger", qty["optimistic"], qty["real"])
	}
}
