package repl

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unicode/utf8"

	"golang.org/x/term"
)

// compositor is the region-managed slice of the screen: the terminal is
// split with a DECSTBM scroll region covering rows 1..h-compRows, so all
// streamed output scrolls normally above a pinned live zone at the bottom.
// The zone's top row is dynamic (the spinner during a turn); the bottom row
// is persistent (the cwd · branch · model · mode status line). It mirrors
// what real CC effectively does — Ink redraws a live region at the bottom
// atomically each frame — using just the scroll-region primitive, so PTY
// scrollback still sees real output bytes.
//
// Invariants:
//   - every frame is ONE Write under compMu, wrapped in DECSC/DECRC, so
//     streamed output can't interleave mid-frame and the logical cursor
//     never moves;
//   - while compSusp > 0 (a modal dialog owns the screen) the region is
//     reset to full and zone writes are remembered, not emitted;
//   - when the terminal is too short (compFits=false) the region is reset
//     to full — normal output can never be trapped by a zone that doesn't
//     fit.

const compRows = 2 // live-zone rows: dynamic (spinner) + status

var (
	compActive atomic.Bool // region installed; read lock-free by the spinner
	compMu     sync.Mutex  // serializes every compositor frame
	compW      int         // last measured width
	compH      int         // last measured height
	compFits   bool        // terminal tall enough to host the zone
	compSusp   int         // >0: modal dialog owns the screen (nests)
	compDyn    string      // top-row content; remembered across suspend/resize
	compStatus string      // bottom-row content
	compDone   chan struct{}

	// injectable for tests
	compOut     io.Writer = os.Stdout
	compIsTTY             = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }
	compGetSize           = func() (int, int, error) { return term.GetSize(int(os.Stdout.Fd())) }
)

// compInit pins the live zone and starts resize tracking. No-op off-TTY, on
// TERM=dumb, or under NO_COLOR (mirroring theme.go's FORCE_COLOR escape).
func compInit() {
	if !compIsTTY() || os.Getenv("TERM") == "dumb" ||
		(os.Getenv("NO_COLOR") != "" && os.Getenv("FORCE_COLOR") == "") {
		return
	}
	w, h, err := compGetSize()
	if err != nil || w <= 0 {
		return
	}
	compMu.Lock()
	compW, compH = w, h
	compFits = h >= compRows+4
	compActive.Store(true)
	if compFits {
		compInstallLocked()
	}
	if compDone == nil {
		done := make(chan struct{})
		compDone = done
		go compWatchResize(done)
	}
	compMu.Unlock()
}

// compFooter replaces the dynamic (top) zone row — the spinner's per-tick
// frame. The status row is repainted with it (Ink-style: the live region
// redraws as a unit).
func compFooter(text string) {
	if !compActive.Load() {
		return
	}
	compMu.Lock()
	compDyn = text
	compRepaintLocked()
	compMu.Unlock()
}

// compSetFooter replaces the whole live zone atomically: lines map
// top-down onto the zone rows (1-compRows entries; extra lines dropped,
// missing lines cleared).
func compSetFooter(lines []string) {
	if !compActive.Load() {
		return
	}
	compMu.Lock()
	compDyn, compStatus = "", ""
	if len(lines) > 0 {
		compDyn = lines[0]
	}
	if len(lines) > 1 {
		compStatus = lines[1]
	}
	compRepaintLocked()
	compMu.Unlock()
}

// compSetStatus replaces the persistent status row, keeping the dynamic row.
func compSetStatus(text string) {
	if !compActive.Load() {
		return
	}
	compMu.Lock()
	compStatus = text
	compRepaintLocked()
	compMu.Unlock()
}

// compClearFooter blanks the dynamic row (turn end); the status row stays.
func compClearFooter() {
	compFooter("")
}

// compRepaint re-emits the zone after outside code erased below the cursor
// (the editor redraw's \x1b[J clears to screen bottom, zone included).
// No-op while suspended or when the zone doesn't fit.
func compRepaint() {
	if !compActive.Load() {
		return
	}
	compMu.Lock()
	compRepaintLocked()
	compMu.Unlock()
}

// compSuspend hands the whole screen to a modal dialog (permission prompt,
// pickers): the zone rows are cleared and the scroll region reset to full,
// so the dialog renders and scrolls inline normally. Calls nest; the zone
// content is remembered for compResume.
func compSuspend() {
	if !compActive.Load() {
		return
	}
	compMu.Lock()
	compSusp++
	if compSusp == 1 && compFits {
		var b strings.Builder
		b.WriteString("\x1b7")
		compClearZoneLocked(&b)
		b.WriteString("\x1b[r") // scroll region back to full screen
		b.WriteString("\x1b8")
		io.WriteString(compOut, b.String())
	}
	compMu.Unlock()
}

// compResume re-pins the region and repaints the remembered zone after a
// modal closes. Fresh rows are scrolled in at the bottom first so the
// repaint can't clobber the dialog's last lines, then DECSTBM is re-emitted.
func compResume() {
	if !compActive.Load() {
		return
	}
	compMu.Lock()
	if compSusp == 0 {
		compMu.Unlock()
		return // resume without a matching suspend — nothing to undo
	}
	compSusp--
	if compSusp == 0 {
		if w, h, err := compGetSize(); err == nil && w > 0 {
			compW, compH = w, h // the window may have moved while suspended
		}
		compFits = compH >= compRows+4
		if compFits {
			compInstallLocked()
		}
	}
	compMu.Unlock()
}

// compClose tears the compositor down: zone cleared, full region restored,
// resize watcher stopped.
func compClose() {
	if !compActive.Swap(false) {
		return
	}
	compMu.Lock()
	if compDone != nil {
		close(compDone)
		compDone = nil
	}
	var b strings.Builder
	b.WriteString("\x1b7")
	if compFits {
		compClearZoneLocked(&b)
	}
	b.WriteString("\x1b[r\x1b8")
	io.WriteString(compOut, b.String())
	compSusp, compFits = 0, false
	compMu.Unlock()
}

// compResize recomputes the region and zone position on SIGWINCH.
func compResize() {
	if !compActive.Load() {
		return
	}
	w, h, err := compGetSize()
	if err != nil || w <= 0 {
		return
	}
	compMu.Lock()
	oldH, oldFits := compH, compFits
	compW, compH = w, h
	compFits = h >= compRows+4
	switch {
	case compSusp > 0:
		// suspended: sizes noted, resume re-installs at the new geometry
	case compFits && !oldFits:
		compInstallLocked()
	case compFits && h != oldH:
		compInstallLocked() // zone moved — scroll in fresh rows, re-pin, repaint
	case compFits:
		compRepaintLocked() // width-only change: re-clamp in place
	case !compFits:
		// shrank too short: hand the zone rows back to normal scrolling
		io.WriteString(compOut, "\x1b7\x1b[r\x1b8")
	}
	compMu.Unlock()
}

// compWatchResize relays SIGWINCH to compResize until compClose closes done.
func compWatchResize(done chan struct{}) {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGWINCH)
	defer signal.Stop(ch)
	for {
		select {
		case <-ch:
			compResize()
		case <-done:
			return
		}
	}
}

// --- frame builders (compMu held) ---

// compInstallLocked scrolls compRows blank lines in at the bottom of the
// screen, installs the scroll region above them, and paints the zone.
// Scrolling first guarantees the zone rows are blank — the repaint can
// never overwrite live text.
func compInstallLocked() {
	var b strings.Builder
	b.WriteString("\x1b7")
	b.WriteString("\x1b[r")                       // margins full: SU must cover the new zone rows
	fmt.Fprintf(&b, "\x1b[%d;1H", compH)          // bottom-left
	fmt.Fprintf(&b, "\x1b[%dS", compRows)         // scroll up: zone rows now blank
	fmt.Fprintf(&b, "\x1b[1;%dr", compH-compRows) // DECSTBM: scroll above the zone
	compPaintLocked(&b)
	b.WriteString("\x1b8")
	io.WriteString(compOut, b.String())
}

// compRepaintLocked emits one atomic frame repainting the whole zone.
func compRepaintLocked() {
	if compSusp > 0 || !compFits {
		return
	}
	var b strings.Builder
	b.WriteString("\x1b7")
	compPaintLocked(&b)
	b.WriteString("\x1b8")
	io.WriteString(compOut, b.String())
}

// compPaintLocked appends the zone repaint: each row is cleared then filled
// with its width-clamped content.
func compPaintLocked(b *strings.Builder) {
	for i, text := range []string{compDyn, compStatus} {
		fmt.Fprintf(b, "\x1b[%d;1H\x1b[2K%s", compH-compRows+1+i, compClamp(text, compW))
	}
}

// compClearZoneLocked appends row-clearing sequences for the zone rows.
func compClearZoneLocked(b *strings.Builder) {
	for i := 0; i < compRows; i++ {
		fmt.Fprintf(b, "\x1b[%d;1H\x1b[2K", compH-compRows+1+i)
	}
}

// compClamp truncates s to w visible columns, preserving escape sequences.
// A truncated styled string gets a reset so the dangling color can't bleed.
func compClamp(s string, w int) string {
	vis := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i += ansiLen(s, i)
			continue
		}
		_, sz := utf8.DecodeRuneInString(s[i:])
		if vis >= w {
			return s[:i] + "\x1b[0m"
		}
		vis++
		i += sz
	}
	return s
}

// ansiLen returns the byte length of the escape sequence starting at s[i]
// (s[i] == 0x1b): CSI, OSC (through BEL/ST), or a two-byte sequence.
func ansiLen(s string, i int) int {
	j := i + 1
	if j < len(s) && s[j] == '[' {
		j++
		for j < len(s) && !((s[j] >= 'a' && s[j] <= 'z') || (s[j] >= 'A' && s[j] <= 'Z')) {
			j++
		}
		if j < len(s) {
			return j + 1 - i
		}
		return j - i
	}
	if j < len(s) && s[j] == ']' {
		j++
		for j < len(s) && s[j] != 0x07 && s[j] != 0x1b {
			j++
		}
		if j < len(s) {
			if s[j] == 0x1b {
				j += 2 // ST
			} else {
				j++ // BEL
			}
		}
		return j - i
	}
	if j < len(s) {
		return 2
	}
	return 1
}
