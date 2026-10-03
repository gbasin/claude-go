//go:build !windows

package voice

import "syscall"

var sigTerm = syscall.SIGTERM
