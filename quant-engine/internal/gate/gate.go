// Package gate decides whether a mainnet run may start. The human confirmation
// at start is separate (open decision O6).
package gate

import (
	"errors"
	"fmt"
	"os"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
)

// Validation is the walk-forward verdict file the Statistician signs off:
// status: validated, method: walk-forward, params_hash: <the run's hash>.
type Validation struct {
	Status     string `yaml:"status"`
	Method     string `yaml:"method"`
	ParamsHash string `yaml:"params_hash"`
}

// CheckLive refuses mainnet unless validation_file says validated, walk-forward,
// same hash and, when gate.min_demo_days > 0, the paper (demo) journal under
// the same parameter hash has events on at least that many distinct calendar
// days (in the yml timezone). The typed confirmation is checked apart.
// The example strategy never runs on mainnet. Every unmet condition is named.
func CheckLive(c *config.Config, journalPath string) error {
	if c.StrategyName() == example.Name {
		return fmt.Errorf("mainnet refused: %s is an example strategy, for tests and documentation only", example.Name)
	}
	return errors.Join(checkDemo(c, journalPath), checkValidation(c))
}

func checkDemo(c *config.Config, journalPath string) error {
	if c.Gate.MinDemoDays == 0 {
		return nil
	}
	events, _, err := journal.Read(journalPath)
	if err != nil {
		return fmt.Errorf("mainnet refused: no paper (demo) journal: %w", err)
	}
	hash := c.ParamsHash()
	days := map[string]bool{} // calendar days (yml timezone) with paper activity
	for _, e := range events {
		if e.Mode == "paper" && e.ParamsHash == hash {
			days[e.Time.In(c.Location()).Format(time.DateOnly)] = true
		}
	}
	if len(days) < c.Gate.MinDemoDays {
		return fmt.Errorf("mainnet refused: paper (demo) run with these parameters has activity on %d of the required %d distinct calendar days",
			len(days), c.Gate.MinDemoDays)
	}
	return nil
}

func checkValidation(c *config.Config) error {
	if c.ValidationFile == "" {
		return errors.New("mainnet refused: validation_file is not set")
	}
	b, err := os.ReadFile(c.ValidationFile)
	if err != nil {
		return fmt.Errorf("mainnet refused: validation file: %w", err)
	}
	var v Validation
	if err := yaml.Unmarshal(b, &v); err != nil {
		return errors.New("mainnet refused: validation file is not valid YAML")
	}
	switch {
	case v.Status != "validated":
		return fmt.Errorf("mainnet refused: validation status is %q, not validated", v.Status)
	case v.Method != "walk-forward":
		return fmt.Errorf("mainnet refused: validation method is %q, not walk-forward", v.Method)
	case v.ParamsHash != c.ParamsHash():
		return errors.New("mainnet refused: validation params_hash does not match these parameters")
	}
	return nil
}
