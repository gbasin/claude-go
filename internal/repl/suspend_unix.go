//go:build !windows

package repl

import (
	"os"
	"os/signal"
	"syscall"
	"time"
)

const suspendSupported = true

// suspendToShell is real's handleSuspend: drop the tty modes the app owns
// (compositor zone, kitty flags, bracketed paste, raw mode, hidden cursor),
// stop the process, and let a SIGCONT edge drive the re-grab — post-stop
// code must not run until the shell hands the terminal back, or the
// kitty/paste modes would leak into the user's shell while we're stopped.
// repaint redraws the caller's UI (editor draft) after the zone repaints.
func suspendToShell(ks *keySource, repaint func()) {
	os.Stdout.WriteString("\x1b[?25h") // real's XI: show cursor
	compSuspend()
	ks.release()
	cont := make(chan os.Signal, 1)
	signal.Notify(cont, syscall.SIGCONT)
	defer signal.Stop(cont)
	// real: process.kill(0,"SIGTSTP") — the process group, so an in-pipeline
	// invocation stops as one job
	_ = syscall.Kill(0, syscall.SIGTSTP)
	select {
	case <-cont:
	case <-time.After(75 * time.Millisecond):
		// SIGTSTP was ignored — inherited SIG_IGN from a launch context
		// without job control (`bash -c`, go test, supervisors; SIG_IGN
		// survives exec and Go can't reset it portably). SIGSTOP can't be
		// caught or ignored. A stray SIGCONT racing the timeout means the
		// stop fell through harmlessly — skip the hard stop then.
		select {
		case <-cont:
		default:
			_ = syscall.Kill(syscall.Getpid(), syscall.SIGSTOP)
			<-cont
		}
	}
	ks.grab()
	compResume()
	if repaint != nil {
		repaint()
	}
}
