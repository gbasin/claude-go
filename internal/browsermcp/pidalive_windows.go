//go:build windows

package browsermcp

import "os"

// syscallKill0 on Windows: FindProcess succeeds for live pids; a dead pid
// still resolves, matching node's conservative probe (treat as alive).
func syscallKill0(pid int) error {
	_, err := os.FindProcess(pid)
	return err
}
