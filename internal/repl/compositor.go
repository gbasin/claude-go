package repl

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"golang.org/x/term"
)

// compositor is the region-managed slice of the screen: the terminal is
// split with a DECSTBM scroll region covering rows 1..h-footerRows, so all
// streamed output scrolls normally above a pinned footer zone. The footer
// hosts the spinner during turns and the status line at the prompt.
//
// This is deliberately not a full screen buffer — it mirrors what real CC
// effectively does (Ink's live region at the bottom) using just the scroll
// region primitive, so PTY scrollback still sees real output.

const footerRows = 1 // spinner/status footer line

var (
	compActive atomic.Bool
	compHeight int
)

// compInit pins the footer zone. No-op off-TTY.
func compInit() {
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return
	}
	_, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || h < footerRows+4 {
		return
	}
	compHeight = h
	// scroll region: 1..h-footerRows
	fmt.Fprintf(os.Stdout, "\x1b[1;%dr", h-footerRows)
	compActive.Store(true)
}

// compFooter renders text on the pinned bottom row without moving the
// logical cursor (save/restore around it).
func compFooter(text string) {
	if !compActive.Load() {
		return
	}
	w, _, _ := term.GetSize(int(os.Stdout.Fd()))
	if w <= 0 {
		w = 80
	}
	// truncate to width (visible cols — strip ANSI for measuring)
	vis := stripANSI(text)
	if len(vis) > w {
		for len(stripANSI(text)) > w {
			text = text[:len(text)-1]
		}
	}
	pad := strings.Repeat(" ", w-len(stripANSI(text)))
	fmt.Fprintf(os.Stdout, "\x1b7\x1b[%d;1H\x1b[2K%s%s\x1b8", compHeight, text, pad)
}

// compClearFooter blanks the pinned row.
func compClearFooter() {
	compFooter("")
}

// compClose restores the full scroll region.
func compClose() {
	if !compActive.Swap(false) {
		return
	}
	fmt.Fprint(os.Stdout, "\x1b7\x1b[r\x1b8")
}

// stripANSI removes CSI/OSC sequences for width math.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			j := i + 1
			if j < len(s) && s[j] == '[' {
				for j < len(s) && !((s[j] >= 'a' && s[j] <= 'z') || (s[j] >= 'A' && s[j] <= 'Z')) {
					j++
				}
				i = j
				continue
			}
			if j < len(s) && s[j] == ']' { // OSC ... BEL/ST
				for j < len(s) && s[j] != 0x07 && !(s[j] == 0x1b) {
					j++
				}
				i = j
				if j < len(s) && s[j] == 0x1b {
					i = j + 1 // ST
				}
				continue
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
