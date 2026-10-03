//go:build windows

package hooks

import "syscall"

func sysProcAttrDetached() *syscall.SysProcAttr { return nil }

func killProcessGroup(pid int) {
	syscall.Kill(pid, syscall.SIGKILL)
}
