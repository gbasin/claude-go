//go:build windows

package hooks

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// Windows has no Setsid/process-group attr — the hook process runs
// unattached; killProcessGroup falls back to taskkill for the tree.
func sysProcAttrDetached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killProcessGroup kills the hook process and its children — syscall.Kill
// doesn't exist on Windows; taskkill /T walks the process tree.
func killProcessGroup(pid int) {
	if err := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run(); err == nil {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill() // taskkill missing (stripped env) — kill the direct child
	}
}
