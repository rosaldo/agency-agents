//go:build windows

package loop

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Lock creates the lock file exclusively and writes this process's id. A
// lock left by a dead process is taken over: on Windows os.FindProcess fails
// for a process that no longer exists. Not exercised on Windows yet.
func Lock(dir string) (unlock func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "lock")
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.WriteString(strconv.Itoa(os.Getpid()))
			f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		b, rerr := os.ReadFile(path)
		pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
		if rerr == nil && perr == nil {
			if p, ferr := os.FindProcess(pid); ferr == nil {
				p.Release()
				return nil, errors.New("already running: another loop holds " + path)
			}
		}
		_ = os.Remove(path) // stale: its process is gone
	}
	return nil, errors.New("already running: cannot take " + path)
}
