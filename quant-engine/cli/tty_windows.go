//go:build windows

package cli

import (
	"errors"
	"io"
	"os"
)

// The console's input and output buffers, not the process's stdin/stdout.
func openTerminal() (io.Reader, io.Writer, func(), error) {
	if os.Getenv("QUANT_ENGINE_NO_TTY") == "1" { // can only remove the terminal, never supply one
		return nil, nil, nil, errors.New("QUANT_ENGINE_NO_TTY=1")
	}
	in, err := os.Open("CONIN$")
	if err != nil {
		return nil, nil, nil, err
	}
	out, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
	if err != nil {
		in.Close()
		return nil, nil, nil, err
	}
	return in, out, func() { in.Close(); out.Close() }, nil
}
