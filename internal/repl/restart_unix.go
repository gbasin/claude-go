//go:build !windows

package repl

import "syscall"

// execSelf replaces this process with the given argv (real /restart's
// "Restart Claude Code and keep this session" flow re-launches the binary).
func execSelf(argv []string) error {
	return syscall.Exec(argv[0], argv, syscall.Environ())
}
