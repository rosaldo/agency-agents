package config

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
)

const validYAML = `
exchange: fake
asset_class: crypto
interval: 1h
symbols: [AAA-USD]
strategy:
  period: 20
  stop_pct: 4
  target_pct: 5
risk:
  confirmed_by_user: true
`

func TestParseValidAppliesDefaults(t *testing.T) {
	c, err := Parse([]byte(validYAML), example.New())
	if err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	if c.Mode != "paper" {
		t.Errorf("mode default = %q, want paper", c.Mode)
	}
	r := c.Risk
	if r.MaxRiskPerTradePct != 1 || r.MaxDailyDrawdownPct != 3 || r.MaxWeeklyDrawdownPct != 6 || r.MinDemoPeriodDays != 30 {
		t.Errorf("risk defaults = %+v, want 1/3/6/30", r)
	}
	if c.Interval() != time.Hour {
		t.Errorf("interval = %v, want 1h", c.Interval())
	}
	if c.Location().String() != "UTC" {
		t.Errorf("timezone default = %v, want UTC", c.Location())
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	cases := map[string]struct{ old, new, want string }{
		"not confirmed":       {"confirmed_by_user: true", "confirmed_by_user: false", "confirmed_by_user"},
		"confirm missing":     {"  confirmed_by_user: true", "", "confirmed_by_user"},
		"strategy refuses":    {"period: 20", "period: 1", "strategy example-sma-cross: period"},
		"strategy bad type":   {"stop_pct: 4", "stop_pct: [4]", "strategy example-sma-cross"},
		"no symbols":          {"symbols: [AAA-USD]", "symbols: []", "symbols"},
		"blank symbol":        {"symbols: [AAA-USD]", "symbols: [\"\"]", "symbols"},
		"malformed exchange":  {"exchange: fake", "exchange: Not An Id", "exchange"},
		"equity without news": {"asset_class: crypto", "asset_class: equity\nnews_guard: none", "news_guard none"},
		"bad market":          {"exchange: fake", "exchange: fake\nmarket: future", "market"},
		"perp on spot":        {"exchange: fake", "exchange: fake\nperp: {leverage: 2}", "market: swap"},
		"bad margin mode":     {"exchange: fake", "exchange: fake\nmarket: swap\nperp: {margin_mode: portfolio}", "margin_mode"},
		"bad mode":            {"exchange: fake", "exchange: fake\nmode: yolo", "mode"},
		"bad interval":        {"interval: 1h", "interval: soon", "interval"},
		"bad timezone":        {"interval: 1h", "interval: 1h\ntimezone: Mars/Base", "timezone"},
		"negative risk":       {"confirmed_by_user: true", "confirmed_by_user: true\n  max_daily_drawdown_pct: -3", "max_daily_drawdown_pct"},
		"unknown field":       {"interval: 1h", "interval: 1h\nintervall: 2h", "intervall"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := strings.Replace(validYAML, tc.old, tc.new, 1)
			if in == validYAML {
				t.Fatalf("case did not change the input")
			}
			_, err := Parse([]byte(in), example.New())
			if err == nil {
				t.Fatalf("accepted an invalid config")
			}
			if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "\n") {
				t.Errorf("error %q should be one line naming %q", err, tc.want)
			}
		})
	}
}

func TestNoStrategyOrABadName(t *testing.T) {
	if _, err := Parse([]byte(validYAML), nil); err == nil {
		t.Fatal("parsed without a strategy")
	}
	if _, err := Parse([]byte(validYAML), named{example.New(), "Bad Name"}); err == nil || !strings.Contains(err.Error(), "Bad Name") {
		t.Fatalf("bad strategy name: %v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(t.TempDir()+"/none.yml", example.New()); err == nil || !os.IsNotExist(unwrapAll(err)) {
		t.Fatalf("missing file: %v", err)
	}
}

func unwrapAll(err error) error {
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok || u.Unwrap() == nil {
			return err
		}
		err = u.Unwrap()
	}
}

// FR-R3 (O1): perp limits default to max_leverage 3 and buffer 0.5; a
// leverage above max_leverage, or out-of-range values, refuse the yml.
func TestPerpLimits(t *testing.T) {
	c, err := Parse([]byte(validYAML), example.New())
	if err != nil {
		t.Fatal(err)
	}
	if c.Risk.MaxLeverage != 3 || c.Risk.MinLiquidationBuffer != 0.5 || c.Perp.Leverage != 1 {
		t.Fatalf("defaults: max_leverage %v, min_liquidation_buffer %v, leverage %v; want 3, 0.5, 1",
			c.Risk.MaxLeverage, c.Risk.MinLiquidationBuffer, c.Perp.Leverage)
	}
	if _, err := Parse([]byte(validYAML+"market: swap\nperp: {leverage: 3}\n"), example.New()); err != nil {
		t.Fatalf("leverage = max_leverage refused: %v", err)
	}
	for extra, want := range map[string]string{
		"market: swap\nperp: {leverage: 4}\n":                                                   "max_leverage",
		"market: swap\nperp: {leverage: 5}\nrisk: {confirmed_by_user: true, max_leverage: 4}\n": "max_leverage",
		"market: swap\nperp: {leverage: 0}\n":                                                   "leverage",
		"risk: {confirmed_by_user: true, max_leverage: 0}\n":                                    "max_leverage",
		"risk: {confirmed_by_user: true, min_liquidation_buffer: 1}\n":                          "min_liquidation_buffer",
		"risk: {confirmed_by_user: true, min_liquidation_buffer: -0.1}\n":                       "min_liquidation_buffer",
	} {
		y := validYAML + extra
		if strings.HasPrefix(extra, "risk:") || strings.Contains(extra, "\nrisk:") {
			y = strings.Replace(validYAML, "risk:\n  confirmed_by_user: true\n", "", 1) + extra
		}
		if _, err := Parse([]byte(y), example.New()); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want one naming %s", extra, err, want)
		}
	}
}

// D10: equities default to news_guard required, an explicit none is refused,
// and swap defaults to isolated margin.
func TestEquityAndSwapDefaults(t *testing.T) {
	c, err := Parse([]byte(strings.Replace(validYAML, "asset_class: crypto", "asset_class: equity\nmarket: swap", 1)), example.New())
	if err != nil {
		t.Fatal(err)
	}
	if c.NewsGuard != "required" || c.Perp.MarginMode != "isolated" || c.Market != "swap" {
		t.Fatalf("news_guard %q margin_mode %q market %q; want required, isolated, swap", c.NewsGuard, c.Perp.MarginMode, c.Market)
	}
	if c, _ := Parse([]byte(validYAML), example.New()); c.Market != "spot" {
		t.Fatalf("market default %q, want spot", c.Market)
	}
}

// specYAML is the §9 example of the spec, with the four edits a real run
// needs: asset_class (absent from §9, required by D10), the user's
// confirmation, the example strategy's block in place of the comment and
// paper_sim.liquidity_class (the simulator's slippage class).
const specYAML = `
mode: paper
exchange: hyperliquid
market: swap
asset_class: crypto
symbols: [BTC/USDC:USDC]
interval: 1h
timezone: UTC
perp:
  leverage: 2
  margin_mode: isolated
  allow_short: false
risk:
  max_risk_per_trade_pct: 1
  max_daily_drawdown_pct: 3
  max_weekly_drawdown_pct: 6
  min_demo_period_days: 30
  max_leverage: 3
  min_liquidation_buffer: 0.5
  confirmed_by_user: true
costs: { commission_pct: 0.045, spread_pct: 0, slippage_pct: 0.05 }
demo: { source: testnet, candles: mainnet }   # O4
paper_sim:
  profile: real
  liquidity_class: major
news_guard: none
validation_file: validation.json
strategy:
  period: 20
  stop_pct: 4
  target_pct: 5
`

// QA T12 M1: the §9 `demo:` key loads with the O4 value only; any other value
// is refused naming O4, and the key stays optional.
func TestDemoKeyAcceptsOnlyO4(t *testing.T) {
	if _, err := Parse([]byte(specYAML), example.New()); err != nil {
		t.Fatalf("§9 example refused: %v", err)
	}
	if _, err := Parse([]byte(strings.Replace(specYAML, "demo: { source: testnet, candles: mainnet }   # O4\n", "", 1)), example.New()); err != nil {
		t.Fatalf("yml without demo refused: %v", err)
	}
	for _, v := range []string{
		"{ source: testnet, candles: testnet }",
		"{ source: mainnet, candles: mainnet }",
		"{ source: testnet }",
		"{ candles: mainnet }",
	} {
		y := strings.Replace(specYAML, "{ source: testnet, candles: mainnet }", v, 1)
		if _, err := Parse([]byte(y), example.New()); err == nil || !strings.Contains(err.Error(), "the only demo implemented") {
			t.Errorf("demo: %s: err = %v, want a refusal naming the only demo implemented", v, err)
		}
	}
	if _, err := Parse([]byte(strings.Replace(specYAML, "candles: mainnet }", "candles: mainnet, extra: 1 }", 1)), example.New()); err == nil {
		t.Error("unknown field inside demo accepted")
	}
}
