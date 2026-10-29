package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"

	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// ParamsHash identifies the engine, strategy and risk parameters a run trades
// with: the engine's fields, the strategy's Name(), its `strategy:` block and
// its news policy. It ignores key order, mode, validation_file, the
// confirmation flag and anything credential-related, so paper and live of the
// same parameters share it and any threshold change breaks it.
func (c *Config) ParamsHash() string {
	r := c.Risk
	r.ConfirmedByUser = false
	symbols := slices.Clone(c.Symbols)
	slices.Sort(symbols)
	var block any // a zero Node (no block) hashes as null
	if c.StrategyParams.Kind != 0 {
		if err := c.StrategyParams.Decode(&block); err != nil {
			panic(err) // the same node already decoded in Parse
		}
	}
	b, err := json.Marshal(struct {
		Exchange, AssetClass, NewsGuard string
		Interval, Timezone              string
		Symbols                         []string
		Risk                            Risk
		Costs                           Costs
		Strategy                        string
		Params                          any
		News                            *strategy.NewsPolicy
	}{c.Exchange, c.AssetClass, c.NewsGuard, c.IntervalSpec, c.Timezone, symbols, r, c.Costs, c.name, block, c.news})
	if err != nil {
		panic(err) // plain numbers, strings and decoded yml always marshal
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
