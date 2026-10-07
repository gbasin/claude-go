//go:build !windows

package repl

import "syscall"

// sigResize is the tty-resize notification signal on unix.
var sigResize = syscall.SIGWINCH
