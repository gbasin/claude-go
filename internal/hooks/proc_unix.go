//go:build !windows

package hooks

import "syscall"

func sysProcAttrDetached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(pid int) {
	syscall.Kill(-pid, syscall.SIGKILL)
}
