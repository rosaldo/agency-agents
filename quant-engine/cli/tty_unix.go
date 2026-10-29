//go:build unix

package cli

import (
	"errors"
	"io"
	"os"
)

func openTerminal() (io.Reader, io.Writer, func(), error) {
	if os.Getenv("QUANT_ENGINE_NO_TTY") == "1" { // can only remove the terminal, never supply one
		return nil, nil, nil, errors.New("QUANT_ENGINE_NO_TTY=1")
	}
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, nil, err
	}
	return f, f, func() { f.Close() }, nil
}
