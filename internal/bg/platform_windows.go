//go:build windows

package bg

import (
	"syscall"
)

func pidAlive(pid int) bool { return false } // no kill(0) — bg registry treats all jobs dead

// killChildPgrp is a no-op without process groups.
func killChildPgrp(pid int, sig syscall.Signal) {}

var sigWinch = syscall.Signal(0)

func detachAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }

func childAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }
