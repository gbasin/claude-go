package repl

import (
	"bytes"
	"fmt"
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
		compSusp, compFits, compZoneH = 0, false, 0
		compDyn, compStatus = "", ""
		compEdit, compCur, compBox, compPainted = false, false, nil, nil
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
	if !strings.HasPrefix(out, compBSU+"\x1b7") || !strings.HasSuffix(out, "\x1b8"+compESU) {
		t.Fatalf("install frame not BSU-wrapped + cursor-saved/restored: %q", out)
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
	if !strings.Contains(out, "new-status") {
		t.Fatalf("status row not repainted: %q", out)
	}
	// differential repaint: the unchanged dynamic row is not re-emitted
	if strings.Contains(out, "spin") {
		t.Fatalf("unchanged dynamic row re-emitted: %q", out)
	}
	// a forced repaint (external erase) brings it back
	f.buf.Reset()
	compRepaint()
	if !strings.Contains(f.buf.String(), "spin") {
		t.Fatalf("dynamic row lost after forced repaint: %q", f.buf.String())
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
	f.h = 4 // below compMinScroll+zone rows — zone can't fit
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
	// CJK runes are double-width (TR11 W/F): 10 cells hold 5
	if got := compClamp(strings.Repeat("界", 12), 10); got != strings.Repeat("界", 5)+"\x1b[0m" {
		t.Fatalf("compClamp utf8: got %q", got)
	}
}

func TestCompClampWcwidth(t *testing.T) {
	// a wide rune that doesn't fit whole ends the clamp early
	if got := compClamp("ab界cd", 3); got != "ab\x1b[0m" {
		t.Fatalf("wide rune split: got %q", got)
	}
	if got := compClamp("ab界cd", 4); got != "ab界\x1b[0m" {
		t.Fatalf("wide rune at exact boundary: got %q", got)
	}
	// combining marks are zero-width and stay attached to their base
	if got := compClamp("e\u0301x", 2); got != "e\u0301x" {
		t.Fatalf("combining fit: got %q", got)
	}
	if got := compClamp("e\u0301x", 1); got != "e\u0301\x1b[0m" {
		t.Fatalf("combining kept with base: got %q", got)
	}
	if w := runeWidth('界'); w != 2 {
		t.Fatalf("CJK width = %d, want 2", w)
	}
	if w := runeWidth('\u0301'); w != 0 {
		t.Fatalf("combining width = %d, want 0", w)
	}
	if w := visWidth("a界\u0301b"); w != 4 {
		t.Fatalf("visWidth = %d, want 4", w)
	}
}

func TestCompGraphemeWidths(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"zwj family", "👨‍👩‍👧", 2},      // one grapheme, 2 cells
		{"zwj rainbow flag", "🏳️‍🌈", 2}, // 🏳 VS16 ZWJ 🌈
		{"vs16 promotes", "☀️", 2},      // narrow base + FE0F → emoji
		{"no vs16 stays text", "☀", 1},  // bare U+2600 is narrow
		{"vs15 stays text", "☀︎", 1},    // FE0E → text presentation
		{"skin tone joins", "👍🏽", 2},    // 👍 + 🏽
		{"keycap", "1️⃣", 2},            // '1' + FE0F + U+20E3
		{"flag pair", "🇫🇷", 2},          // two regional indicators
		{"surrounding text", "a👨‍👩‍👧b", 4},
		{"combining still attaches", "é", 1}, // e + U+0301
		{"plain", "abc", 3},
	}
	for _, c := range cases {
		if got := visWidth(c.in); got != c.want {
			t.Errorf("%s: visWidth(%q) = %d, want %d", c.name, c.in, got, c.want)
		}
	}
}

func TestCompClampGraphemes(t *testing.T) {
	// a ZWJ cluster that doesn't fit whole ends the clamp before it —
	// never split mid-grapheme
	fam := "👨‍👩‍👧"
	if got := compClamp("ab"+fam+"cd", 3); got != "ab\x1b[0m" {
		t.Fatalf("zwj split: got %q", got)
	}
	if got := compClamp("ab"+fam+"cd", 4); got != "ab"+fam+"\x1b[0m" {
		t.Fatalf("zwj boundary: got %q", got)
	}
	// vs16 emoji counts as its 2-cell cluster, not base+selector
	if got := compClamp("x☀️y", 2); got != "x\x1b[0m" {
		t.Fatalf("vs16 split: got %q", got)
	}
	if got := compClamp("x☀️y", 3); got != "x☀️\x1b[0m" {
		t.Fatalf("vs16 boundary: got %q", got)
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

func TestCompDiffRepaint(t *testing.T) {
	f := newFakeTerm(t, 80, 40)
	compInit()
	compSetFooter([]string{"spin", "status"})
	f.buf.Reset()
	compFooter("spin2") // only the dynamic row changed
	out := f.buf.String()
	if !strings.Contains(out, "\x1b[39;1H\x1b[2Kspin2") {
		t.Fatalf("changed row not repainted: %q", out)
	}
	if strings.Contains(out, "\x1b[40;1H") {
		t.Fatalf("unchanged status row re-emitted: %q", out)
	}
	// BSU/ESU wraps every frame
	if !strings.Contains(out, compBSU) || !strings.Contains(out, compESU) {
		t.Fatalf("frame missing synchronized-output wrap: %q", out)
	}
	// save/restore still brackets the frame
	if !strings.Contains(out, "\x1b7") || !strings.Contains(out, "\x1b8") {
		t.Fatalf("frame missing DECSC/DECRC: %q", out)
	}
}

func TestCompEditorZone(t *testing.T) {
	f := newFakeTerm(t, 80, 40)
	compInit()
	t.Setenv("CLAUDE_GO_NO_COMPOSITOR_INPUT", "")
	if !compInputActive() {
		t.Fatal("compInputActive false on a healthy fake term")
	}
	compSetStatus("st")
	f.buf.Reset()
	box := []string{"╭─╮", "│ > hi", "╰─╯"}
	compEditor(box, 1, 5)
	out := f.buf.String()
	// zone grew to 4 rows: DECSTBM covers 1..36
	if !strings.Contains(out, "\x1b[1;36r") {
		t.Fatalf("zone did not grow to 4 rows: %q", out)
	}
	if !strings.Contains(out, "│ > hi") || !strings.Contains(out, "╭─╮") {
		t.Fatalf("box not painted in zone: %q", out)
	}
	if compZoneH != 4 {
		t.Fatalf("compZoneH = %d, want 4", compZoneH)
	}
	// cursor left inside the zone on the input row (row 40-4+1+1 = 38)
	if !strings.Contains(out, "\x1b[38;6H") {
		t.Fatalf("cursor not placed in the box: %q", out)
	}

	// a second frame with identical rows only moves the cursor
	f.buf.Reset()
	compEditor(box, 1, 7)
	out = f.buf.String()
	if strings.Contains(out, "│ > hi") {
		t.Fatalf("unchanged box rows re-emitted: %q", out)
	}
	if !strings.Contains(out, "\x1b[38;8H") {
		t.Fatalf("cursor not updated: %q", out)
	}
}

func TestCompExitEditEchoesBox(t *testing.T) {
	f := newFakeTerm(t, 80, 40)
	compInit()
	compEditor([]string{"╭─╮", "│ > hi", "╰─╯"}, 1, 5)
	f.buf.Reset()
	compExitEdit([]string{"╭─╮", "│ > hi", "╰─╯"})
	out := f.buf.String()
	// turn-mode zone = spinner + pinned box (3) + status = 5 rows
	if !strings.Contains(out, "\x1b[1;35r") {
		t.Fatalf("zone did not shrink to turn layout: %q", out)
	}
	if compZoneH != 5 {
		t.Fatalf("compZoneH = %d, want 5 (spinner+box+status)", compZoneH)
	}
	// the submitted box is echoed into the scroll region
	if !strings.Contains(out, "│ > hi\r\n") || !strings.Contains(out, "╰─╯\r\n") {
		t.Fatalf("box not echoed to scrollback: %q", out)
	}
	// the released box rows were cleared (row 37 = old zone top)
	if !strings.Contains(out, "\x1b[37;1H\x1b[2K") {
		t.Fatalf("released zone rows not cleared: %q", out)
	}
}

func TestCompEditorSuspendKeepsBox(t *testing.T) {
	f := newFakeTerm(t, 80, 40)
	compInit()
	compEditor([]string{"╭─╮", "│ > x", "╰─╯"}, 1, 5)
	f.buf.Reset()
	compSuspend()
	out := f.buf.String()
	// all 4 zone rows cleared (37-40), region released
	for r := 37; r <= 40; r++ {
		seq := fmt.Sprintf("\x1b[%d;1H\x1b[2K", r)
		if !strings.Contains(out, seq) {
			t.Fatalf("suspend did not clear zone row %d: %q", r, out)
		}
	}
	if !strings.Contains(out, "\x1b[r") {
		t.Fatalf("suspend did not release the region: %q", out)
	}
	// edits while suspended are remembered, not emitted
	f.buf.Reset()
	compEditor([]string{"╭─╮", "│ > xy", "╰─╯"}, 1, 6)
	if f.buf.Len() != 0 {
		t.Fatalf("suspended compEditor wrote %q", f.buf.String())
	}
	compResume()
	out = f.buf.String()
	if !strings.Contains(out, "│ > xy") {
		t.Fatalf("resume did not repaint the box: %q", out)
	}
	if !strings.Contains(out, "\x1b[1;36r") {
		t.Fatalf("resume did not re-pin the 4-row zone: %q", out)
	}
	if compZoneH != 4 {
		t.Fatalf("compZoneH = %d after resume, want 4", compZoneH)
	}
}

func TestCompInline(t *testing.T) {
	f := newFakeTerm(t, 80, 40)
	compInit()
	compEditor([]string{"╭─╮", "│ > hi", "╰─╯"}, 1, 5)
	f.buf.Reset()
	compInline("a note\r\n")
	out := f.buf.String()
	// printed at the scroll-region bottom (row 40-4 = 36)
	if !strings.Contains(out, "\x1b[36;1H"+"a note") {
		t.Fatalf("note not printed at region bottom: %q", out)
	}
	// cursor restored into the box
	if !strings.Contains(out, "\x1b[38;6H") {
		t.Fatalf("cursor not restored into the zone: %q", out)
	}
}

func TestCompInputFlag(t *testing.T) {
	newFakeTerm(t, 80, 40)
	compInit()
	t.Setenv("CLAUDE_GO_NO_COMPOSITOR_INPUT", "1")
	if compInputActive() {
		t.Fatal("CLAUDE_GO_NO_COMPOSITOR_INPUT must disable zone input")
	}
}
