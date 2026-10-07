//go:build !windows

package bg

import (
	"syscall"
)

// pidAlive is the liveness probe used by the registry sweeper.
func pidAlive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

// killChildPgrp signals the session process's process group (resize, kill).
func killChildPgrp(pid int, sig syscall.Signal) {
	_ = syscall.Kill(-pid, sig)
}

var sigWinch = syscall.SIGWINCH

// detachAttr puts the spawned worker in its own session so it survives
// the launcher exiting.
func detachAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// childAttr gives the pty child its own session with the slave as
// controlling terminal (Ctty:0 = stdin — the pty slave).
func childAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
}

// signalHost asks a bg-pty-host worker to end its session.
func signalHost(pid int) {
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
}

// killHost force-kills a wedged bg-pty-host the daemon couldn't TERM.
func killHost(pid int) {
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}
