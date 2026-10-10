//go:build !darwin && !linux

package extract

import (
	"fmt"
	"os"
	"syscall"
)

func ptyAlloc(cols, rows int) (*os.File, *os.File, error) {
	return nil, nil, fmt.Errorf("pty unavailable on this platform")
}

func ptyProcAttr() *syscall.SysProcAttr { return nil }
