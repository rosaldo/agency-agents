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
// confirmation flag and anything credential-related, so paper and mainnet of the
// same parameters share it and any threshold change breaks it. The sim
// profile and its resolved costs are in it too.
func (c *Config) ParamsHash() string {
	h, err := c.paramsHash()
	if err != nil {
		panic(err) // Parse refuses a yml that cannot be hashed
	}
	return h
}

func (c *Config) paramsHash() (string, error) {
	r := c.Risk
	r.ConfirmedByUser = false
	symbols := slices.Clone(c.Symbols)
	slices.Sort(symbols)
	var block any // a zero Node (no block) hashes as null
	if c.StrategyParams.Kind != 0 {
		if err := c.StrategyParams.Decode(&block); err != nil {
			return "", err
		}
	}
	b, err := json.Marshal(struct {
		Exchange, Market, AssetClass string
		NewsGuard                    string
		Interval, Timezone           string
		Symbols                      []string
		Perp                         Perp
		Risk                         Risk
		Strategy                     string
		Params                       any
		News                         *strategy.NewsPolicy
		SimProfile                   string
		Sim                          map[string]SimCosts
	}{c.Exchange, c.Market, c.AssetClass, c.NewsGuard, c.IntervalSpec, c.Timezone, symbols, c.Perp, r, c.name, block, c.news,
		c.Sim.Profile, c.sim})
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
