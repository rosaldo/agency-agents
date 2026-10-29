// Package example is a trivial, neutral strategy for tests and documentation:
// it enters long when the last close crosses above the simple moving average
// of the closes before it. It is not a trading idea, and the live gate
// refuses it by name.
package example

import (
	"errors"
	"fmt"

	"go.yaml.in/yaml/v3"

	"github.com/rosaldo/agency-agents/quant-engine/indicators"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// Name is refused by the live gate.
const Name = "example-sma-cross"

// Params is the yml `strategy:` block.
type Params struct {
	Period    int     `yaml:"period"`     // closes in the moving average, >= 2
	StopPct   float64 `yaml:"stop_pct"`   // stop this far below the entry, in (0, 100)
	TargetPct float64 `yaml:"target_pct"` // exit this far above the entry, 0 = no target
	// TrailPeriod, when >= 2, trails the stop at the average of the last
	// TrailPeriod closes; 0 = no trailing. The engine drops any answer that
	// does not tighten or sits above the price.
	TrailPeriod int `yaml:"trail_period"`
}

type SMACross struct{ p Params }

func New() strategy.Strategy { return &SMACross{} }

func (*SMACross) Name() string     { return Name }
func (*SMACross) Inputs() []string { return nil }
func (s *SMACross) Warmup() int    { return max(s.p.Period, s.p.TrailPeriod) + 1 }

func (s *SMACross) Configure(n yaml.Node) error {
	p := Params{Period: 20, StopPct: 2}
	if n.Kind != 0 {
		if err := n.Decode(&p); err != nil {
			return err
		}
	}
	switch {
	case p.Period < 2:
		return errors.New("period must be >= 2")
	case p.StopPct <= 0 || p.StopPct >= 100:
		return errors.New("stop_pct must be in (0, 100)")
	case p.TargetPct < 0:
		return errors.New("target_pct must be >= 0 (0 = no target)")
	case p.TrailPeriod != 0 && p.TrailPeriod < 2:
		return errors.New("trail_period must be 0 (no trailing) or >= 2")
	}
	s.p = p
	return nil
}

// Decide enters when the previous close sat at or below the average of the
// Period closes before the last one and the last close is above it.
func (s *SMACross) Decide(in strategy.Input) strategy.Decision {
	cs := in.Candles
	if in.Position != nil || len(cs) < s.p.Period+1 {
		return strategy.Decision{}
	}
	closes := make([]float64, s.p.Period)
	for i := range closes {
		closes[i] = cs[len(cs)-1-s.p.Period+i].Close
	}
	avg, ok := indicators.SMA(closes)
	last, prev := cs[len(cs)-1].Close, cs[len(cs)-2].Close
	if !ok || prev > avg || last <= avg {
		return strategy.Decision{}
	}
	d := strategy.Decision{Action: strategy.Enter, Side: strategy.Long, Stop: last * (1 - s.p.StopPct/100),
		Reason: fmt.Sprintf("close %.8g crossed above the %d-close average %.8g", last, s.p.Period, avg)}
	if s.p.TargetPct > 0 {
		d.Target = last * (1 + s.p.TargetPct/100)
	}
	return d
}

// Trail is the moving average of the last TrailPeriod closes.
func (s *SMACross) Trail(in strategy.Input) (float64, bool) {
	n, cs := s.p.TrailPeriod, in.Candles
	if n == 0 || len(cs) < n {
		return 0, false
	}
	closes := make([]float64, n)
	for i := range closes {
		closes[i] = cs[len(cs)-n+i].Close
	}
	return indicators.SMA(closes)
}
