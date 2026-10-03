package repl

import (
	"bytes"
	"strings"
	"testing"
)

// fakeTerm captures compositor output and stubs the terminal size.
type fakeTerm struct {
	buf  *bytes.Buffer
	w, h int
}

// newFakeTerm points the compositor at a buffer-backed fake TTY of w×h.
// Cleanup tears the compositor down and restores the real I/O hooks.
func newFakeTerm(t *testing.T, w, h int) *fakeTerm {
	t.Helper()
	f := &fakeTerm{buf: &bytes.Buffer{}, w: w, h: h}
	oldOut, oldTTY, oldSize := compOut, compIsTTY, compGetSize
	compOut = f.buf
	compIsTTY = func() bool { return true }
	compGetSize = func() (int, int, error) { return f.w, f.h, nil }
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
	t.Setenv("FORCE_COLOR", "")
	t.Cleanup(func() {
		compClose() // stops the SIGWINCH watcher, resets state
		compOut, compIsTTY, compGetSize = oldOut, oldTTY, oldSize
		compSusp, compFits = 0, false
		compDyn, compStatus = "", ""
	})
	return f
}

func TestCompInitPinsRegion(t *testing.T) {
	f := newFakeTerm(t, 80, 40)
	compInit()
	out := f.buf.String()
	if !compActive.Load() {
		t.Fatal("compInit did not activate")
	}
	if !strings.Contains(out, "\x1b[1;38r") {
		t.Fatalf("expected DECSTBM 1;38r, got %q", out)
	}
	if !strings.HasPrefix(out, "\x1b7") || !strings.HasSuffix(out, "\x1b8") {
		t.Fatalf("install frame not cursor-saved/restored: %q", out)
	}
}

func TestCompInitNotTTY(t *testing.T) {
	buf := &bytes.Buffer{}
	oldOut, oldTTY := compOut, compIsTTY
	compOut = buf
	compIsTTY = func() bool { return false }
	t.Cleanup(func() { compOut, compIsTTY = oldOut, oldTTY })

	compInit()
	if buf.Len() != 0 || compActive.Load() {
		t.Fatalf("compInit off-TTY must no-op, wrote %q", buf.String())
	}
	compFooter("x")
	compSetStatus("y")
	compSuspend()
	compResume()
	compResize()
	if buf.Len() != 0 {
		t.Fatalf("inactive compositor must not write, got %q", buf.String())
	}
}

func TestCompInitDumbTerm(t *testing.T) {
	f := newFakeTerm(t, 80, 40)
	t.Setenv("TERM", "dumb")
	compInit()
	if f.buf.Len() != 0 || compActive.Load() {
		t.Fatalf("TERM=dumb must no-op, wrote %q", f.buf.String())
	}
}

func TestCompInitNoColor(t *testing.T) {
	f := newFakeTerm(t, 80, 40)
	t.Setenv("NO_COLOR", "1")
	compInit()
	if f.buf.Len() != 0 || compActive.Load() {
		t.Fatalf("NO_COLOR must no-op, wrote %q", f.buf.String())
	}
	// FORCE_COLOR overrides NO_COLOR, like theme.go
	t.Setenv("FORCE_COLOR", "1")
	compInit()
	if !compActive.Load() {
		t.Fatal("NO_COLOR+FORCE_COLOR should still activate")
	}
}

func TestCompFooterClampsToWidth(t *testing.T) {
	f := newFakeTerm(t, 20, 10)
	compInit()
	f.buf.Reset()
	compFooter(strings.Repeat("x", 50))
	out := f.buf.String()
	if strings.Contains(out, strings.Repeat("x", 21)) {
		t.Fatalf("footer not clamped to width 20: %q", out)
	}
	if !strings.Contains(out, strings.Repeat("x", 20)) {
		t.Fatalf("expected 20 visible cols, got %q", out)
	}
	// clamped styled text must not bleed color into later output
	if !strings.Contains(out, strings.Repeat("x", 20)+"\x1b[0m") {
		t.Fatalf("truncated frame missing reset: %q", out)
	}
}

func TestCompFooterPaintsBothRows(t *testing.T) {
	f := newFakeTerm(t, 80, 40)
	compInit()
	f.buf.Reset()
	compSetFooter([]string{"spin-line", "status-line"})
	out := f.buf.String()
	if !strings.Contains(out, "\x1b[39;1H\x1b[2Kspin-line") {
		t.Fatalf("dynamic row not painted at row 39: %q", out)
	}
	if !strings.Contains(out, "\x1b[40;1H\x1b[2Kstatus-line") {
		t.Fatalf("status row not painted at row 40: %q", out)
	}
}

func TestCompSetStatusKeepsDynamic(t *testing.T) {
	f := newFakeTerm(t, 80, 40)
	compInit()
	compSetFooter([]string{"spin", "old-status"})
	f.buf.Reset()
	compSetStatus("new-status")
	out := f.buf.String()
	if !strings.Contains(out, "spin") || !strings.Contains(out, "new-status") {
		t.Fatalf("status update must preserve the dynamic row: %q", out)
	}
}

func TestCompSuspendResume(t *testing.T) {
	f := newFakeTerm(t, 80, 40)
	compInit()
	compSetFooter([]string{"spin", "status"})

	f.buf.Reset()
	compSuspend()
	out := f.buf.String()
	if !strings.Contains(out, "\x1b[r") {
		t.Fatalf("suspend did not reset the scroll region: %q", out)
	}
	if !strings.Contains(out, "\x1b[40;1H\x1b[2K") {
		t.Fatalf("suspend did not clear the zone: %q", out)
	}

	// while suspended, footer writes are remembered, not emitted
	f.buf.Reset()
	compFooter("spin2")
	if f.buf.Len() != 0 {
		t.Fatalf("suspended compositor wrote %q", f.buf.String())
	}

	f.buf.Reset()
	compResume()
	out = f.buf.String()
	if !strings.Contains(out, "\x1b[1;38r") {
		t.Fatalf("resume did not re-emit DECSTBM: %q", out)
	}
	if !strings.Contains(out, "spin2") || !strings.Contains(out, "status") {
		t.Fatalf("resume did not repaint remembered content: %q", out)
	}
}

func TestCompSuspendNests(t *testing.T) {
	f := newFakeTerm(t, 80, 40)
	compInit()
	compSuspend()
	f.buf.Reset()
	compSuspend() // inner modal — no second teardown
	compResume()  // still suspended — no reinstall
	if f.buf.Len() != 0 {
		t.Fatalf("nested suspend/resume wrote %q", f.buf.String())
	}
	compResume() // outermost resume re-installs
	if !strings.Contains(f.buf.String(), "\x1b[1;38r") {
		t.Fatalf("outer resume did not reinstall: %q", f.buf.String())
	}
}

func TestCompResizeReemitsRegion(t *testing.T) {
	f := newFakeTerm(t, 80, 40)
	compInit()
	f.buf.Reset()
	f.w, f.h = 100, 50 // grow
	compResize()
	if out := f.buf.String(); !strings.Contains(out, "\x1b[1;48r") {
		t.Fatalf("resize did not re-emit DECSTBM for h=50: %q", out)
	}
	if compW != 100 || compH != 50 {
		t.Fatalf("stored geometry not updated: %dx%d", compW, compH)
	}
}

func TestCompResizeTooSmall(t *testing.T) {
	f := newFakeTerm(t, 80, 40)
	compInit()
	f.buf.Reset()
	f.h = 4 // below compRows+4 — zone can't fit
	compResize()
	if out := f.buf.String(); !strings.Contains(out, "\x1b[r") {
		t.Fatalf("shrink did not release the region: %q", out)
	}
	f.buf.Reset()
	compFooter("spin")
	if f.buf.Len() != 0 {
		t.Fatalf("footer wrote into a zone that doesn't fit: %q", f.buf.String())
	}
	// growing back re-installs the region and repaints
	f.h = 40
	compResize()
	if out := f.buf.String(); !strings.Contains(out, "\x1b[1;38r") || !strings.Contains(out, "spin") {
		t.Fatalf("regrow did not reinstall + repaint: %q", out)
	}
}

func TestCompClampANSI(t *testing.T) {
	in := "\x1b[31m" + strings.Repeat("a", 30) + "\x1b[0m"
	got := compClamp(in, 10)
	want := "\x1b[31m" + strings.Repeat("a", 10) + "\x1b[0m"
	if got != want {
		t.Fatalf("compClamp styled: got %q want %q", got, want)
	}
	if got := compClamp("short", 10); got != "short" {
		t.Fatalf("compClamp passthrough: got %q", got)
	}
	// wide runes count as one column each (approximation, noted vs wcwidth)
	if got := compClamp(strings.Repeat("界", 12), 10); got != strings.Repeat("界", 10)+"\x1b[0m" {
		t.Fatalf("compClamp utf8: got %q", got)
	}
}

func TestCompCloseRestores(t *testing.T) {
	f := newFakeTerm(t, 80, 40)
	compInit()
	f.buf.Reset()
	compClose()
	out := f.buf.String()
	if !strings.Contains(out, "\x1b[r") {
		t.Fatalf("close did not restore the full region: %q", out)
	}
	if compActive.Load() {
		t.Fatal("close left compositor active")
	}
	f.buf.Reset()
	compFooter("x")
	if f.buf.Len() != 0 {
		t.Fatalf("writes after close: %q", f.buf.String())
	}
	// idempotent
	compClose()
}
