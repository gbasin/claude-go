//go:build !darwin && !linux

package bg

import (
	"errors"
	"os"
)

var errNoPTY = errors.New("bg sessions need a pty — unsupported platform")

func openPTY(cols, rows int) (master, slave *os.File, err error) {
	return nil, nil, errNoPTY
}

func setWinSize(f *os.File, cols, rows int) {}
