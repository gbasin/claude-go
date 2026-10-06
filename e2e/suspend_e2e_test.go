//go:build e2e

// suspend_e2e_test.go — ctrl+z job-control suspend (real CC v1.0.44+):
//   - the raw 0x1a byte suspends the process (state T); SIGCONT resumes it
//     and the session answers normally
//   - kitty/CSI-u's \x1b[122;5u suspends too (real's v2.1.9 fix)
package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// waitStopped polls ps until the process shows stopped (state T).
func waitStopped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("ps", "-o", "stat=", "-p",
			fmt.Sprint(pid)).Output()
		if err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "T") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("pid %d never stopped", pid)
}

func TestSuspendResume(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)

	p.send("\x1a")
	waitStopped(t, p.cmd.Process.Pid)
	syscall.Kill(p.cmd.Process.Pid, syscall.SIGCONT)

	// resumed session answers normally
	p.sendUntilEchoed(t, "after-suspend\r", "❯ after-suspend", 8)
	p.waitFor("Mock response to: after-suspend", 30*time.Second)
	p.exit()
}

func TestSuspendResumeKitty(t *testing.T) {
	s := newSession(t)
	// TERM=xterm-kitty makes startKeys push kitty flags; ctrl+z then arrives
	// as CSI-u \x1b[122;5u instead of the raw byte
	p := s.runPTY(t, []string{"TERM=xterm-kitty"}, "--mock")
	p.waitFor("❯", 30*time.Second)

	p.send("\x1b[122;5u")
	waitStopped(t, p.cmd.Process.Pid)
	syscall.Kill(p.cmd.Process.Pid, syscall.SIGCONT)

	p.sendUntilEchoed(t, "kitty-resumed\r", "❯ kitty-resumed", 8)
	p.waitFor("Mock response to: kitty-resumed", 30*time.Second)
	p.exit()
}
