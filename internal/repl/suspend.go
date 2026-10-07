package repl

// Ctrl+Z job-control suspend (real CC since v1.0.44; the CSI-u decode here
// covers the kitty-protocol terminals real fixed in v2.1.9). Real's
// handleSuspend (mod_0444): write show-cursor + suspend modes, unwind raw
// mode, then process.kill(0, "SIGTSTP") — and the resume work runs inside a
// SIGCONT listener, NOT after kill() returns, because the stop lands
// asynchronously. Same shape here: release the tty, post the stop, block on
// SIGCONT, then re-grab.

// isSuspendKey reports whether the decoded key is ctrl+z — the raw control
// byte from a legacy tty, or kitty/CSI-u's 'z'+ctrl event.
func isSuspendKey(k key) bool {
	if k.paste != "" {
		return false
	}
	if k.b == 26 {
		return true
	}
	return k.mod == modCtrl && (k.b == 'z' || k.r == 'z')
}
