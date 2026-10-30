//go:build unix

package cli

import (
	"os/exec"
	"os/signal"
	"syscall"
)

// detach lets the loop outlive the launching shell. A mainnet loop stays in the
// foreground process group of start, because it reads the confirmation from
// the terminal itself (a background group reading it gets SIGTTIN and stops);
// once answered it ignores the terminal's hangup.
func detach(c *exec.Cmd, keepTerminal bool) {
	if !keepTerminal {
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
}

func ignoreHangup() { signal.Ignore(syscall.SIGHUP) }
