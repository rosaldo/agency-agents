//go:build unix

package loop

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Lock takes the state directory's exclusive lock for the life of a loop. The
// kernel drops it when the process dies, so a crash never leaves it stale.
func Lock(dir string) (unlock func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("already running: another loop holds " + f.Name())
	}
	return func() { f.Close() }, nil
}
