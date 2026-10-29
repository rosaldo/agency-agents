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

// confirmLive shows the four limits and the capital on the terminal and
// accepts only a typed "yes" within confirmTimeout.
func confirmLive(cfg *config.Config, ex exchange.Exchange) error {
	in, out, closeTTY, err := openTTY()
	if err != nil {
		return fmt.Errorf("live refused: the confirmation must be typed at a terminal, and none is attached (%v)", err)
	}
	defer closeTTY()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bal, err := ex.Balance(ctx, exchange.Real)
	if err != nil {
		return fmt.Errorf("live refused: cannot read the real balance: %w", err)
	}
	r := cfg.Risk
	fmt.Fprintf(out, `
LIVE trading with real money on %s: %s
  capital (equity)       %.2f
  max risk per trade     %g%% (about %.2f)
  max daily drawdown     %g%%
  max weekly drawdown    %g%%
  demo period required   %d days (met)
From here on the engine enforces these limits by itself, with no approval per order.
Type yes to go live: `, cfg.Exchange, strings.Join(cfg.Symbols, ", "), bal.Equity,
		r.MaxRiskPerTradePct, bal.Equity*r.MaxRiskPerTradePct/100, r.MaxDailyDrawdownPct, r.MaxWeeklyDrawdownPct, r.MinDemoPeriodDays)

	answer := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(in).ReadString('\n')
		answer <- strings.TrimSpace(line)
	}()
	select {
	case a := <-answer:
		if a != "yes" {
			return errors.New("live refused: confirmation not given")
		}
		return nil
	case <-time.After(confirmTimeout):
		return errors.New("live refused: confirmation timed out")
	}
}
