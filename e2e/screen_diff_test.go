//go:build e2e

package e2e

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gbasin/claude-go/internal/mock"
	"github.com/gbasin/claude-go/internal/termemu"
)

// driveInteractive runs bin on a PTY against a fresh mock, sends one turn,
// and returns the rendered screen (ANSI interpreted — the user's actual
// view) plus the raw stream.
func driveInteractive(t *testing.T, bin, send, waitFor string) (screen, raw string) {
	t.Helper()
	cwd, home := newDiffSandbox(t)
	srv, err := mock.Start()
	if err != nil {
		t.Fatalf("mock start: %v", err)
	}
	defer srv.Close()
	master, slave := openPTYSize(t, 120, 40)
	cmd := exec.Command(bin)
	cmd.Dir = cwd
	cmd.Env = diffEnv(home, srv.Addr, true)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = ptyProcAttr()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}
	slave.Close()
	p := &ptyProc{t: t, cmd: cmd, master: master, done: make(chan error, 1),
		emu: termemu.New(120, 40)}
	go func() {
		var tmp [4096]byte
		for {
			n, err := master.Read(tmp[:])
			if n > 0 {
				p.mu.Lock()
				p.buf.Write(tmp[:n])
				p.emu.Feed(tmp[:n])
				p.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	go func() { p.done <- cmd.Wait() }()
	defer p.close()

	// Ink writes cell-diff patches — words arrive positioned, so the raw
	// stream has no contiguous text (e.g. "Mockresponseto:"). Waits must
	// run on the rendered screen for real; screen works for ours too.
	p.waitScreen("❯", 45*time.Second)
	p.send(send)
	p.waitScreen(waitFor, 60*time.Second)
	p.waitScreen("done", 20*time.Second) // turn-end "… done H:MM AM" row

	return p.screen(), p.clean()
}

// TestDiffScreen renders real and ours through the same terminal emulator
// after an identical keystroke script and diffs the visible frames —
// the TUI analog of the wire diff harness.
func TestDiffScreen(t *testing.T) {
	realBin := realClaudeBin(t)
	rScr, _ := driveInteractive(t, realBin, "say hi\r", "Mock response")
	oScr, _ := driveInteractive(t, binPath, "say hi\r", "Mock response")

	// structural anchors both frames must share after one turn
	for _, want := range []string{"❯", "⏺", "done", "effort", "Mock response"} {
		if !strings.Contains(rScr, want) {
			t.Errorf("real frame missing %q\n%s", want, rScr)
		}
		if !strings.Contains(oScr, want) {
			t.Errorf("ours frame missing %q\n%s", want, oScr)
		}
	}
	// report row-level differences for review (not failed on — spinner
	// glyphs, timing and model text legitimately vary)
	rl, ol := strings.Split(rScr, "\n"), strings.Split(oScr, "\n")
	t.Logf("real frame (%d rows):\n%s", len(rl), rScr)
	t.Logf("ours frame (%d rows):\n%s", len(ol), oScr)
}
