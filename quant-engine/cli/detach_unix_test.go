//go:build unix

package cli

import (
	"os/exec"
	"testing"
)

// The live loop must stay in start's foreground process group;
// in a group of its own, reading the terminal stops it with SIGTTIN.
// End to end: scripts/live-via-pty.sh in the review probes.
func TestLiveLoopKeepsTheForegroundGroup(t *testing.T) {
	c := exec.Command("true")
	detach(c, true)
	if a := c.SysProcAttr; a != nil && (a.Setpgid || a.Setsid || a.Foreground) {
		t.Fatalf("live loop leaves the foreground group: %+v", a)
	}
}
