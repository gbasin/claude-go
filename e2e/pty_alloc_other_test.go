//go:build e2e && !darwin && !linux

package e2e

import (
	"fmt"
	"os"
	"syscall"
)

// ptyAlloc: no pty on this platform — tests skip.
func ptyAlloc(cols, rows int) (*os.File, *os.File, error) {
	return nil, nil, fmt.Errorf("pty unavailable on this platform")
}

func ptyProcAttr() *syscall.SysProcAttr { return nil }
