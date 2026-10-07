//go:build windows

package repl

import "syscall"

// sigResize is zero on Windows — console resize isn't signal-delivered
// there; compWatchResize becomes a no-op watcher.
var sigResize = syscall.Signal(0)
