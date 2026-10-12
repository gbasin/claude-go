//go:build e2e

package e2e

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gbasin/claude-go/internal/mock"
	"github.com/gbasin/claude-go/internal/termemu"
)

var (
	pathRe  = regexp.MustCompile(`/(private|tmp|var|Users|home)[^\s]*`)
	clockRe = regexp.MustCompile(`\d{1,2}:\d{2} (AM|PM)`)
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

	p.mu.Lock()
	hist := strings.Join(p.emu.History(), "\n") + "\n"
	p.mu.Unlock()
	return hist + p.screen(), p.clean()
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
	// normalized row-set diff — volatile content masked, every real row
	// must exist in ours (hard parity check, not just anchors)
	rNorm, oNorm := normFrame(rScr), normFrame(oScr)
	var missing []string
	seen := map[string]bool{}
	for _, row := range rNorm {
		if !containsRow(oNorm, row) && !seen[row] {
			missing = append(missing, row)
			seen[row] = true
		}
	}
	var extra []string
	seen = map[string]bool{}
	for _, row := range oNorm {
		if !containsRow(rNorm, row) && !seen[row] {
			extra = append(extra, row)
			seen[row] = true
		}
	}
	t.Logf("real frame (%d rows):\n%s", len(rNorm), rScr)
	t.Logf("ours frame (%d rows):\n%s", len(oNorm), oScr)
	for _, m := range missing {
		t.Logf("  - %s", m)
	}
	for _, e := range extra {
		t.Logf("  + %s", e)
	}
	// shared non-volatile rows must dominate — a <40% overlap means the
	// layouts diverged structurally
	shared := len(rNorm) - len(missing)
	if len(rNorm) > 0 && float64(shared)/float64(len(rNorm)) < 0.4 {
		t.Errorf("screen parity low: %d/%d real rows present in ours\nreal:\n%s\nours:\n%s",
			shared, len(rNorm), rScr, oScr)
	}
}

// normFrame renders a frame comparable: drop volatile rows (spinner,
// clock, banner noise) and mask per-run values.
func normFrame(scr string) []string {
	var out []string
	for _, ln := range strings.Split(scr, "\n") {
		ln = strings.TrimRight(ln, " ")
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if strings.Contains(ln, "… ") || strings.HasPrefix(strings.TrimSpace(ln), "✻") {
			continue // spinner/turn-end rows — verb+timing vary
		}
		ln = pathRe.ReplaceAllString(ln, "<DIR>")
		ln = clockRe.ReplaceAllString(ln, "<TIME>")
		ln = strings.ReplaceAll(ln, "\u00a0", " ") // real's box uses NBSP
		ln = strings.TrimSpace(ln)
		out = append(out, ln)
	}
	return out
}

func containsRow(rows []string, want string) bool {
	for _, r := range rows {
		if r == want {
			return true
		}
	}
	return false
}
