//go:build windows

package cli

import (
	"os/exec"
	"syscall"
)

const detachedProcess = 0x00000008 // DETACHED_PROCESS, Win32 process creation flag

// detach starts the loop in its own process group; a live loop keeps the
// console because it asks for the confirmation there itself.
func detach(c *exec.Cmd, keepTerminal bool) {
	flags := uint32(syscall.CREATE_NEW_PROCESS_GROUP)
	if !keepTerminal {
		flags |= detachedProcess
	}
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
}

func ignoreHangup() {}
