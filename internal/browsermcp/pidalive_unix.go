//go:build !windows

package browsermcp

import "syscall"

// syscallKill0 mirrors process.kill(pid, 0) on unix.
func syscallKill0(pid int) error {
	return syscall.Kill(pid, 0)
}
