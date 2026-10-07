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

// signalHost asks a bg-pty-host worker to end its session — no pty workers
// exist on windows, but keep the call safe.
func signalHost(pid int) {}

func killHost(pid int) {}
