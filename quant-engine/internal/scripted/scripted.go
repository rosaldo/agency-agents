// Package scripted is a fake strategy for tests: Decide answers with a
// function of its Input and logs every call, so two runs can be compared.
package scripted

import (
	"fmt"

	"go.yaml.in/yaml/v3"

	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

type Strategy struct {
	Decider func(strategy.Input) strategy.Decision
	Log     []string // one line per Decide: input and answer
}

func (*Strategy) Name() string              { return "scripted" }
func (*Strategy) Configure(yaml.Node) error { return nil }
func (*Strategy) Warmup() int               { return 2 }
func (*Strategy) Inputs() []string          { return nil }
func (s *Strategy) Decide(in strategy.Input) strategy.Decision {
	d := s.Decider(in)
	var pos strategy.PositionView
	if in.Position != nil {
		pos = *in.Position
	}
	s.Log = append(s.Log, fmt.Sprintf("%s %s %+v %+v %+v", in.Symbol, in.Now.Format("2006-01-02T15"), in.Candles, pos, d))
	return d
}
