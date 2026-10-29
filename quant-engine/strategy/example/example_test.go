package example

import (
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

func configured(t *testing.T, block string) strategy.Strategy {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(block), &n); err != nil {
		t.Fatal(err)
	}
	s := New()
	if err := s.Configure(*n.Content[0]); err != nil {
		t.Fatal(err)
	}
	return s
}

func closes(xs ...float64) []market.Candle {
	cs := make([]market.Candle, len(xs))
	for i, x := range xs {
		cs[i] = market.Candle{Time: time.Unix(int64(i)*3600, 0), Close: x}
	}
	return cs
}

func TestEntersOnTheCrossOnly(t *testing.T) {
	s := configured(t, "{period: 3, stop_pct: 4, target_pct: 5}")
	if s.Warmup() != 4 {
		t.Fatalf("warmup = %d, want 4", s.Warmup())
	}
	d := s.Decide(strategy.Input{Candles: closes(100, 100, 100, 105)})
	if d.Action != strategy.Enter || d.Side != strategy.Long || d.Stop != 100.8 || d.Target != 110.25 {
		t.Fatalf("cross: %+v, want long, stop 100.8, target 110.25", d)
	}
	for name, in := range map[string]strategy.Input{
		"already above": {Candles: closes(100, 100, 106, 107)},
		"below":         {Candles: closes(100, 100, 100, 99)},
		"too few":       {Candles: closes(100, 100, 105)},
		"holding":       {Candles: closes(100, 100, 100, 105), Position: &strategy.PositionView{Qty: 1}},
	} {
		if d := s.Decide(in); d.Action != strategy.None {
			t.Errorf("%s: %+v, want no action", name, d)
		}
	}
}

func TestConfigureDefaultsAndRefusals(t *testing.T) {
	s := New()
	if err := s.Configure(yaml.Node{}); err != nil || s.Warmup() != 21 {
		t.Fatalf("defaults: %v, warmup %d", err, s.Warmup())
	}
	if d := configured(t, "{period: 3, stop_pct: 4}").Decide(strategy.Input{Candles: closes(100, 100, 100, 105)}); d.Target != 0 {
		t.Fatalf("no target_pct must mean no target, got %+v", d)
	}
	for _, bad := range []string{"{period: 1}", "{stop_pct: 0}", "{stop_pct: 100}", "{target_pct: -1}"} {
		var n yaml.Node
		_ = yaml.Unmarshal([]byte(bad), &n)
		if err := New().Configure(*n.Content[0]); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// D11: the neutral trailing rule is the average of the last trail_period
// closes; 0 disables it; the warmup covers the longer window.
func TestTrailIsTheMovingAverage(t *testing.T) {
	s := configured(t, "{period: 3, stop_pct: 4, trail_period: 4}")
	if s.Warmup() != 5 {
		t.Fatalf("warmup = %d, want 5", s.Warmup())
	}
	tr := s.(strategy.Trailer)
	if got, ok := tr.Trail(strategy.Input{Candles: closes(1, 100, 102, 104, 106)}); !ok || got != 103 {
		t.Fatalf("trail = %v %v, want 103", got, ok)
	}
	if _, ok := tr.Trail(strategy.Input{Candles: closes(100, 102, 104)}); ok {
		t.Fatal("trail with too few candles must answer no")
	}
	if _, ok := configured(t, "{period: 3, stop_pct: 4}").(strategy.Trailer).Trail(strategy.Input{Candles: closes(1, 2, 3, 4)}); ok {
		t.Fatal("trail_period 0 must not trail")
	}
	var n yaml.Node
	_ = yaml.Unmarshal([]byte("{trail_period: 1}"), &n)
	if err := New().Configure(*n.Content[0]); err == nil {
		t.Fatal("trail_period 1 accepted")
	}
}
