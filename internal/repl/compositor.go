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
// split with a DECSTBM scroll region covering rows 1..h-zoneH, so all
// streamed output scrolls normally above a pinned live zone at the bottom.
// It mirrors what real CC effectively does — Ink redraws a live region at
// the bottom atomically each frame — using just the scroll-region
// primitive, so PTY scrollback still sees real output bytes.
//
// Two layouts:
//   - turn mode (2 rows): dynamic row (spinner) + persistent status row;
//   - edit mode (3+n rows): the input box (╭─╮ / │ > … / ╰─╯) + status.
//     readLine hosts the box in the zone; while the agent runs the zone
//     shrinks back to spinner+status and the submitted box is echoed into
//     the scroll region, exactly where the inline box used to live.
//
// Invariants:
//   - every frame is ONE Write under compMu, wrapped in BSU/ESU
//     (synchronized-output mode 2026 — terminals that support it render
//     atomically, others ignore it harmlessly);
//   - repaints are differential: compPainted caches the last emitted
//     (post-clamp) row contents and only changed rows get cursor moves;
//   - while editing, the hardware cursor rests inside the zone at
//     compCurRow/compCurCol instead of being restored — that IS the input
//     cursor; non-edit frames keep the DECSC/DECRC save/restore;
//   - while compSusp > 0 (a modal dialog owns the screen) the region is
//     reset to full and zone writes are remembered, not emitted;
//   - when the zone doesn't fit (compFits=false) the region is released —
//     normal output can never be trapped by a zone that isn't installed.

const (
	compMinScroll = 4 // transcript rows guaranteed above the zone
	compMinEditH  = 8 // smallest screen hosting a minimal box zone

	// synchronized output (mode 2026): begin/end update
	compBSU = "\x1b[?2026h"
	compESU = "\x1b[?2026l"
)

var (
	compActive  atomic.Bool // region installed; read lock-free by the spinner
	compMu      sync.Mutex  // serializes every compositor frame
	compW       int         // last measured width
	compH       int         // last measured height
	compFits    bool        // terminal tall enough to host the current zone
	compSusp    int         // >0: modal dialog owns the screen (nests)
	compZoneH   int         // zone height actually installed (0 = released)
	compEdit    bool        // zone hosts the input box
	compBox     []string    // edit-mode rows: box chrome + input lines
	compDyn     string      // turn-mode top row (spinner)
	compStatus  string      // persistent bottom row (all modes)
	compPainted []string    // last emitted row contents (post-clamp); nil = stale
	compCur     bool        // cursor rests inside the zone (editing)
	compCurRow  int         // 0-based row within the zone
	compCurCol  int         // 0-based column
	compDone    chan struct{}

	// injectable for tests
	compOut     io.Writer = os.Stdout
	compIsTTY             = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }
	compGetSize           = func() (int, int, error) { return term.GetSize(int(os.Stdout.Fd())) }
)

// compRowsLocked composes the desired zone rows top-down (compMu held).
func compRowsLocked() []string {
	if compEdit {
		rows := make([]string, 0, len(compBox)+1)
		rows = append(rows, compBox...)
		return append(rows, compStatus)
	}
	// turn mode: spinner + pinned input box (real keeps the box live —
	// queue-typing is visible) + status
	rows := []string{compDyn}
	rows = append(rows, compBox...)
	return append(rows, compStatus)
}

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
	compActive.Store(true)
	compFrameLocked(nil)
	if compDone == nil {
		done := make(chan struct{})
		compDone = done
		go compWatchResize(done)
	}
	compMu.Unlock()
}

// compFooter replaces the dynamic (top) zone row — the spinner's per-tick
// frame. Remembered while suspended.
func compFooter(text string) {
	if !compActive.Load() {
		return
	}
	compMu.Lock()
	compDyn = text
	compFrameLocked(nil)
	compMu.Unlock()
}

// compSetFooter replaces the base-mode zone rows atomically: lines map
// top-down onto [dynamic, status].
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
	compFrameLocked(nil)
	compMu.Unlock()
}

// compSetStatus replaces the persistent status row (bottom row in every
// layout — it sits under the input box while editing).
func compSetStatus(text string) {
	if !compActive.Load() {
		return
	}
	compMu.Lock()
	compStatus = text
	compFrameLocked(nil)
	compMu.Unlock()
}

// compClearFooter blanks the dynamic row (turn end); the status row stays.
func compClearFooter() {
	compFooter("")
}

// compRepaint re-emits the zone after outside code erased below the cursor
// (the legacy editor redraw's \x1b[J clears to screen bottom, zone
// included). Invalidates the diff cache so every row is re-emitted.
func compRepaint() {
	if !compActive.Load() {
		return
	}
	compMu.Lock()
	compPainted = nil
	compFrameLocked(nil)
	compMu.Unlock()
}

// --- input-box hosting (edit mode) ---

// compInputActive reports whether readLine may draw the input box inside
// the live zone. CLAUDE_GO_NO_COMPOSITOR_INPUT=1 forces the legacy inline
// box (escape hatch for PTY/terminal quirks).
func compInputActive() bool {
	if os.Getenv("CLAUDE_GO_NO_COMPOSITOR_INPUT") != "" {
		return false
	}
	if !compActive.Load() {
		return false
	}
	compMu.Lock()
	defer compMu.Unlock()
	return compSusp == 0 && compFits && compH >= compMinEditH
}

// compAvailRows is the maximum number of input lines the zone can host at
// the current geometry (box borders + status take 3 more rows).
func compAvailRows() int {
	compMu.Lock()
	defer compMu.Unlock()
	n := compH - compMinScroll - 3
	if n < 1 {
		n = 1
	}
	return n
}

// compEditor publishes the input box into the zone: box rows are laid out
// top-down above the status row, the zone grows/shrinks to fit, and the
// hardware cursor is left at (curRow, curCol) inside the zone — that's the
// visible input cursor. Called on every editor redraw while editing.
func compEditor(box []string, curRow, curCol int) {
	if !compActive.Load() {
		return
	}
	compMu.Lock()
	compEdit, compBox = true, box
	compCur, compCurRow, compCurCol = true, curRow, curCol
	compFrameLocked(nil)
	compMu.Unlock()
}

// compExitEdit collapses the zone back to turn mode after readLine
// returns: echo (the submitted box as rendered) is written into the scroll
// region just above the shrunken zone, so scrollback keeps the same
// ╭─╮ │ > text ╰─╯ record the inline box produced.
func compExitEdit(echo []string) {
	if !compActive.Load() {
		return
	}
	compMu.Lock()
	compEdit = false
	compBox = queueBoxRows("") // box stays pinned during turns (real parity)
	compCur = false
	if compZoneH > 0 {
		compFrameLocked(echo)
	} else if len(echo) > 0 {
		// zone not installed (too small / suspended): plain inline echo
		io.WriteString(compOut, "\r\n"+strings.Join(echo, "\r\n")+"\r\n")
	}
	compMu.Unlock()
}

// compInline prints s at the bottom of the scroll region (the row just
// above the zone) without disturbing the zone or the editing cursor — for
// mid-edit notes like "(ctrl-C again to exit)", completion lists and
// toggle banners. Falls back to a plain write whenever the editor isn't
// living in the zone.
func compInline(s string) {
	if s == "" {
		return
	}
	if !compActive.Load() {
		io.WriteString(compOut, s)
		return
	}
	compMu.Lock()
	if compSusp > 0 || compZoneH == 0 || !compCur {
		compMu.Unlock()
		io.WriteString(compOut, s)
		return
	}
	var b strings.Builder
	b.WriteString(compBSU)
	fmt.Fprintf(&b, "\x1b[%d;1H", compH-compZoneH) // last scroll-region row
	b.WriteString(s)
	compPaintRowsLocked(&b, compRowsLocked()) // heal any clobbered zone row
	fmt.Fprintf(&b, "\x1b[%d;%dH", compH-compZoneH+1+compCurRow, compCursorColLocked())
	b.WriteString(compESU)
	io.WriteString(compOut, b.String())
	compMu.Unlock()
}

// compSuspend hands the whole screen to a modal dialog (permission prompt,
// pickers, $EDITOR, transcript view): the zone rows are cleared and the
// scroll region reset to full, so the dialog renders and scrolls inline
// normally. Calls nest; the zone content is remembered for compResume.
func compSuspend() {
	if !compActive.Load() {
		return
	}
	compMu.Lock()
	compSusp++
	if compSusp == 1 && compZoneH > 0 {
		var b strings.Builder
		b.WriteString(compBSU)
		b.WriteString("\x1b7")
		compClearZoneLocked(&b)
		b.WriteString("\x1b[r") // scroll region back to full screen
		b.WriteString("\x1b8")
		b.WriteString(compESU)
		io.WriteString(compOut, b.String())
		compZoneH, compPainted = 0, nil
	}
	compMu.Unlock()
}

// compResume re-pins the region and repaints the remembered zone after a
// modal closes. Fresh rows are scrolled in at the bottom first so the
// repaint can't clobber the dialog's last lines, then DECSTBM is
// re-emitted — at whichever size the zone currently wants (box included).
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
		compFrameLocked(nil) // compZoneH==0 -> reinstall at current size
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
	b.WriteString(compBSU)
	b.WriteString("\x1b7")
	if compZoneH > 0 {
		compClearZoneLocked(&b)
	}
	b.WriteString("\x1b[r\x1b8")
	b.WriteString(compESU)
	io.WriteString(compOut, b.String())
	compSusp, compFits, compZoneH = 0, false, 0
	compEdit, compCur, compBox, compPainted = false, false, nil, nil
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
	oldH := compH
	compW, compH = w, h
	if compSusp == 0 && compZoneH > 0 && h != oldH &&
		compH >= len(compRowsLocked())+compMinScroll {
		// zone moved with the screen height: force a reinstall at the new
		// geometry (scroll fresh rows in, re-pin, repaint everything)
		compZoneH, compPainted = 0, nil
	}
	compFrameLocked(nil)
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

// compFrameLocked is the single entry point for zone changes: it
// reconciles the installed zone (compZoneH) with the desired rows —
// releasing, installing, growing, shrinking or diff-repainting as needed —
// and leaves the cursor where the mode dictates (inside the zone while
// editing, restored to the scroll region otherwise). echo is written into
// the scroll region when the zone shrinks (the submitted input box).
func compFrameLocked(echo []string) {
	rows := compRowsLocked()
	n := len(rows)
	compFits = compH >= n+compMinScroll
	if compSusp > 0 {
		return
	}
	if !compFits {
		if compZoneH > 0 {
			io.WriteString(compOut, compBSU+"\x1b7\x1b[r\x1b8"+compESU)
			compZoneH, compPainted = 0, nil
		}
		return
	}
	var b strings.Builder
	b.WriteString(compBSU)
	if !compCur {
		b.WriteString("\x1b7")
	}
	echoed := false
	switch {
	case compZoneH == 0:
		// install: scroll n fresh rows in at the bottom so the repaint can
		// never overwrite live text, then pin the region above them
		b.WriteString("\x1b[r")
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[%dS", compH, n)
		fmt.Fprintf(&b, "\x1b[1;%dr", compH-n)
		compZoneH, compPainted = n, nil
		compPaintRowsLocked(&b, rows)
	case compZoneH != n:
		echoed = compResizeRowsLocked(&b, rows, echo)
	default:
		compPaintRowsLocked(&b, rows)
	}
	switch {
	case compCur:
		row := compCurRow
		if row < 0 {
			row = 0
		}
		if row > compZoneH-1 {
			row = compZoneH - 1
		}
		fmt.Fprintf(&b, "\x1b[%d;%dH", compH-compZoneH+1+row, compCursorColLocked())
	case echoed:
		fmt.Fprintf(&b, "\x1b[%d;1H", compH-compZoneH) // bottom of scroll region
	default:
		b.WriteString("\x1b8")
	}
	b.WriteString(compESU)
	io.WriteString(compOut, b.String())
}

// compResizeRowsLocked reconciles an installed zone of compZoneH rows to
// len(rows) rows: grow scrolls fresh rows in at the bottom; shrink clears
// the rows being handed back to the transcript (and writes the echo
// there). Returns whether the echo was emitted.
func compResizeRowsLocked(b *strings.Builder, rows []string, echo []string) bool {
	n := len(rows)
	if n > compZoneH {
		// grow: push the transcript up so the new bottom rows are blank
		b.WriteString("\x1b[r")
		fmt.Fprintf(b, "\x1b[%d;1H\x1b[%dS", compH, n-compZoneH)
		fmt.Fprintf(b, "\x1b[1;%dr", compH-n)
		compZoneH, compPainted = n, nil
		// a zone GROW can still carry an echo (edit→turn keeps the box
		// pinned but the submitted box still belongs in scrollback)
		if len(echo) > 0 {
			fmt.Fprintf(b, "\x1b[%d;1H", compH-n) // last scroll-region row
			for _, ln := range echo {
				b.WriteString(ln)
				b.WriteString("\r\n")
			}
		}
		compPaintRowsLocked(b, rows)
		return len(echo) > 0
	}
	// shrink: the released rows (old zone top) go back to the transcript —
	// clear them, re-pin, then print the echo at the region bottom
	for r := compH - compZoneH + 1; r <= compH-n; r++ {
		fmt.Fprintf(b, "\x1b[%d;1H\x1b[2K", r)
	}
	fmt.Fprintf(b, "\x1b[1;%dr", compH-n)
	compZoneH, compPainted = n, nil
	echoed := false
	if len(echo) > 0 {
		fmt.Fprintf(b, "\x1b[%d;1H", compH-n)
		for _, ln := range echo {
			b.WriteString(ln)
			b.WriteString("\r\n")
		}
		echoed = true
	}
	compPaintRowsLocked(b, rows)
	return echoed
}

// compPaintRowsLocked appends the differential zone repaint: only rows
// whose clamped content changed since the last frame are cleared and
// filled — Ink-style minimal diffing. compZoneH must already be the new
// height (callers set it before painting).
func compPaintRowsLocked(b *strings.Builder, rows []string) {
	if len(compPainted) != len(rows) {
		compPainted = nil // row count changed — repaint everything
	}
	painted := make([]string, len(rows))
	for i, text := range rows {
		want := compClamp(text, compW)
		if compPainted != nil && compPainted[i] == want {
			painted[i] = want
			continue
		}
		fmt.Fprintf(b, "\x1b[%d;1H\x1b[2K%s", compH-compZoneH+1+i, want)
		painted[i] = want
	}
	compPainted = painted
}

// compClearZoneLocked appends row-clearing sequences for the installed zone.
func compClearZoneLocked(b *strings.Builder) {
	for i := 0; i < compZoneH; i++ {
		fmt.Fprintf(b, "\x1b[%d;1H\x1b[2K", compH-compZoneH+1+i)
	}
}

// compCursorColLocked clamps the editing cursor column into the screen.
func compCursorColLocked() int {
	c := compCurCol + 1 // 0-based -> 1-based
	if c < 1 {
		c = 1
	}
	if c > compW {
		c = compW
	}
	return c
}

// --- cell width (wcwidth stand-in; hand-rolled TR11 tables, no deps) ---

// doubleWidth: East Asian Wide/Fullwidth ranges per Unicode TR11 (plus the
// emoji blocks terminals render at two cells).
var doubleWidth = [][2]rune{
	{0x1100, 0x115F},                                     // Hangul Jamo
	{0x231A, 0x231B},                                     // hourglass emoji
	{0x2329, 0x232A},                                     // angle brackets
	{0x23E9, 0x23EC}, {0x23F0, 0x23F0}, {0x23F3, 0x23F3}, // media/sand emoji
	{0x25FD, 0x25FE}, {0x2614, 0x2615}, {0x2648, 0x2653}, {0x267F, 0x267F},
	{0x2693, 0x2693}, {0x26A1, 0x26A1}, {0x26AA, 0x26AB}, {0x26BD, 0x26BE},
	{0x26C4, 0x26C5}, {0x26CE, 0x26CE}, {0x26D4, 0x26D4}, {0x26EA, 0x26EA},
	{0x26F2, 0x26F3}, {0x26F5, 0x26F5}, {0x26FA, 0x26FA}, {0x26FD, 0x26FD},
	{0x2705, 0x2705}, {0x270A, 0x270B}, {0x2728, 0x2728}, {0x274C, 0x274C},
	{0x274E, 0x274E}, {0x2753, 0x2755}, {0x2757, 0x2757}, {0x2795, 0x2797},
	{0x27B0, 0x27B0}, {0x27BF, 0x27BF}, {0x2B1B, 0x2B1C}, {0x2B50, 0x2B50},
	{0x2B55, 0x2B55},
	{0x2E80, 0x303E}, // CJK Radicals .. Ideographic Description
	{0x3041, 0x33FF}, // Hiragana .. CJK Compat (kana, bopomofo, hangul jamo, squares)
	{0x3400, 0x4DBF}, // CJK Extension A
	{0x4E00, 0x9FFF}, // CJK Unified Ideographs
	{0xA000, 0xA4CF}, // Yi
	{0xA960, 0xA97F}, // Hangul Jamo Extended-B
	{0xAC00, 0xD7A3}, // Hangul Syllables
	{0xF900, 0xFAFF}, // CJK Compatibility Ideographs
	{0xFE10, 0xFE19}, {0xFE30, 0xFE52}, {0xFE54, 0xFE66}, {0xFE68, 0xFE6B},
	{0xFF00, 0xFF60}, // Fullwidth Forms
	{0xFFE0, 0xFFE6}, // Fullwidth signs
	{0x16FE0, 0x16FE4}, {0x17000, 0x18AFF}, {0x1B000, 0x1B152},
	{0x1B164, 0x1B167}, {0x1B170, 0x1B2FB},
	{0x1F004, 0x1F004}, {0x1F0CF, 0x1F0CF}, {0x1F18E, 0x1F18E},
	{0x1F191, 0x1F19A}, {0x1F200, 0x1F202}, {0x1F210, 0x1F23B},
	{0x1F240, 0x1F248}, {0x1F250, 0x1F251},
	{0x1F300, 0x1F64F}, // emoji (commonly double-width — approximation)
	{0x1F900, 0x1F9FF}, {0x1FA70, 0x1FAFF},
	{0x20000, 0x2FFFD}, {0x30000, 0x3FFFD}, // CJK Ext B+
}

// zeroWidth: combining marks, format controls and variation selectors —
// zero display cells.
var zeroWidth = [][2]rune{
	{0x0300, 0x036F}, // combining diacritical marks
	{0x0483, 0x0489}, {0x0591, 0x05BD}, {0x05BF, 0x05BF}, {0x05C1, 0x05C2},
	{0x05C4, 0x05C5}, {0x05C7, 0x05C7}, {0x0610, 0x061A}, {0x064B, 0x065F},
	{0x0670, 0x0670}, {0x06D6, 0x06DC}, {0x06DF, 0x06E4}, {0x06E7, 0x06E8},
	{0x06EA, 0x06ED}, {0x0E31, 0x0E31}, {0x0E34, 0x0E3A}, {0x0E47, 0x0E4E},
	{0x1AB0, 0x1AFF}, {0x1DC0, 0x1DFF},
	{0x200B, 0x200F}, {0x202A, 0x202E}, {0x2060, 0x2064}, // ZWSP .. Cf
	{0x20D0, 0x20F0},   // combining marks for symbols
	{0xFE00, 0xFE0F},   // variation selectors
	{0xFE20, 0xFE2F},   // combining half marks
	{0xE0100, 0xE01EF}, // variation selectors supplement
}

func inRanges(r rune, t [][2]rune) bool {
	lo, hi := 0, len(t)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		switch {
		case r < t[mid][0]:
			hi = mid - 1
		case r > t[mid][1]:
			lo = mid + 1
		default:
			return true
		}
	}
	return false
}

// runeWidth returns the terminal cell width of a printable rune: 0 for
// controls/marks/format, 2 for East Asian W/F, 1 otherwise.
func runeWidth(r rune) int {
	switch {
	case r == 0, r < 0x20, r >= 0x7f && r < 0xa0:
		return 0
	case inRanges(r, zeroWidth):
		return 0
	case inRanges(r, doubleWidth):
		return 2
	}
	return 1
}

// visWidth is the display-cell width of s, skipping ANSI escape sequences.
func visWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i += ansiLen(s, i)
			continue
		}
		r, sz := utf8.DecodeRuneInString(s[i:])
		w += runeWidth(r)
		i += sz
	}
	return w
}

// compClamp truncates s to w visible cells, preserving escape sequences.
// Wide runes count as 2 cells; a rune that doesn't fit whole ends the
// string (the row is already blanked by \x1b[2K). A truncated styled
// string gets a reset so the dangling color can't bleed.
func compClamp(s string, w int) string {
	vis := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i += ansiLen(s, i)
			continue
		}
		r, sz := utf8.DecodeRuneInString(s[i:])
		if vis+runeWidth(r) > w {
			return s[:i] + "\x1b[0m"
		}
		vis += runeWidth(r)
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

// queueBoxRows renders the small prompt box for the pinned turn-mode zone.
func queueBoxRows(draft string) []string {
	w := compW
	if w <= 0 {
		w = 80
	}
	mid := "│ > " + draft
	if visWidth(mid) > w-1 {
		mid = "│ > …" + draft[max(0, len(draft)-(w-8)):]
	}
	return []string{"╭─╮", mid, "╰─╯"}
}

// compQueueDraft renders queued input into the pinned box while the agent
// runs — real shows queued text accumulating in the input box.
func compQueueDraft(draft string) {
	if !compActive.Load() {
		return
	}
	compMu.Lock()
	if !compEdit { // only while in turn mode
		compBox = queueBoxRows(draft)
		compFrameLocked(nil)
	}
	compMu.Unlock()
}
