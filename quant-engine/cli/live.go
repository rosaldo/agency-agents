package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
)

// openTTY opens the controlling terminal itself, never stdin, so a plain tool
// call or pipe cannot answer. It is not proof of a human: a process of the
// same user can open a pseudo-terminal and type.
// The trading process calls it, so no file or variable handed over by another
// process stands in for the answer. Tests replace it.
var openTTY = openTerminal

var confirmTimeout = 2 * time.Minute

// confirmLive shows the risk and perp limits and the capital on the terminal and
// accepts only a typed "yes" within confirmTimeout.
func confirmLive(cfg *config.Config, ex exchange.Exchange) error {
	in, out, closeTTY, err := openTTY()
	if err != nil {
		return fmt.Errorf("mainnet refused: the confirmation must be typed at a terminal, and none is attached (%v)", err)
	}
	defer closeTTY()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bal, err := ex.Balance(ctx, exchange.Real)
	if err != nil {
		return fmt.Errorf("mainnet refused: cannot read the mainnet balance: %w", err)
	}
	r := cfg.Risk
	margin := cfg.Perp.MarginMode
	if margin == "" {
		margin = "none (spot)"
	}
	fmt.Fprintf(out, `
MAINNET trading with real money on %s: %s
  capital (equity)       %.2f
  max risk per trade     %g%% (about %.2f)
  max daily drawdown     %g%%
  max weekly drawdown    %g%%
  demo days required     %d (met; gate.min_demo_days)
  leverage               %d
  max leverage           %d
  margin mode            %s
  liquidation buffer     %g (min share of the distance to liquidation the stop leaves free)
From here on the engine enforces these limits by itself, with no approval per order.
Type yes to start on mainnet: `, cfg.Exchange, strings.Join(cfg.Symbols, ", "), bal.Equity,
		r.MaxRiskPerTradePct, bal.Equity*r.MaxRiskPerTradePct/100, r.MaxDailyDrawdownPct, r.MaxWeeklyDrawdownPct, cfg.Gate.MinDemoDays,
		cfg.Perp.Leverage, r.MaxLeverage, margin, r.MinLiquidationBuffer)

	answer := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(in).ReadString('\n')
		answer <- strings.TrimSpace(line)
	}()
	select {
	case a := <-answer:
		if a != "yes" {
			return errors.New("mainnet refused: confirmation not given")
		}
		return nil
	case <-time.After(confirmTimeout):
		return errors.New("mainnet refused: confirmation timed out")
	}
}
