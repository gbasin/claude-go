// Package repl implements the interactive terminal UI.
package repl

import (
	"os"
	"strings"
)

// editor is a line editor fed by keySource events.
type editor struct {
	history []string
	hIdx    int
	buf     []rune
	pos     int
	prompt  string
	saved   []rune
}

func newEditor() *editor { return &editor{prompt: "> ", hIdx: -1} }

func (e *editor) redraw() {
	os.Stdout.WriteString("\r\x1b[K" + e.prompt + string(e.buf))
	if e.pos < len(e.buf) {
		os.Stdout.WriteString(cursorLeft(len(e.buf) - e.pos))
	}
}

func cursorLeft(n int) string {
	if n <= 0 {
		return ""
	}
	return "\x1b[" + itoa(n) + "D"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// readLine consumes keys until Enter. (line, ok, intr)
func (e *editor) readLine(ks *keySource) (string, bool, bool) {
	os.Stdout.WriteString(e.prompt)
	for k := range ks.ch {
		c := k.b
		switch {
		case k.seq == "[A":
			e.histPrev()
		case k.seq == "[B":
			e.histNext()
		case k.seq == "[C":
			if e.pos < len(e.buf) {
				e.pos++
				e.redraw()
			}
		case k.seq == "[D":
			if e.pos > 0 {
				e.pos--
				e.redraw()
			}
		case k.seq == "[H":
			e.pos = 0
			e.redraw()
		case k.seq == "[F":
			e.pos = len(e.buf)
			e.redraw()
		case k.seq == "[3~":
			if e.pos < len(e.buf) {
				e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
				e.redraw()
			}
		case c == 3:
			if len(e.buf) > 0 {
				e.buf = e.buf[:0]
				e.pos = 0
				e.redraw()
			} else {
				os.Stdout.WriteString("\r\n")
				return "", false, true
			}
		case c == 4:
			if len(e.buf) == 0 {
				os.Stdout.WriteString("\r\n")
				return "", false, false
			}
			if e.pos < len(e.buf) {
				e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
				e.redraw()
			}
		case c == '\r' || c == '\n':
			os.Stdout.WriteString("\r\n")
			l := string(e.buf)
			if strings.TrimSpace(l) != "" {
				e.history = append(e.history, l)
			}
			e.buf = e.buf[:0]
			e.pos = 0
			e.hIdx = -1
			return l, true, false
		case c == 0x7f || c == 8:
			if e.pos > 0 {
				e.buf = append(e.buf[:e.pos-1], e.buf[e.pos:]...)
				e.pos--
				e.redraw()
			}
		case c == 1:
			e.pos = 0
			e.redraw()
		case c == 5:
			e.pos = len(e.buf)
			e.redraw()
		case c == 11:
			e.buf = e.buf[:e.pos]
			e.redraw()
		case c == 21:
			e.buf = e.buf[e.pos:]
			e.pos = 0
			e.redraw()
		case c == 23:
			for e.pos > 0 && e.buf[e.pos-1] == ' ' {
				e.buf = append(e.buf[:e.pos-1], e.buf[e.pos:]...)
				e.pos--
			}
			for e.pos > 0 && e.buf[e.pos-1] != ' ' {
				e.buf = append(e.buf[:e.pos-1], e.buf[e.pos:]...)
				e.pos--
			}
			e.redraw()
		case c == 0x1b:
			// bare esc clears the line
			e.buf = e.buf[:0]
			e.pos = 0
			e.redraw()
		case k.r != 0:
			e.buf = append(e.buf, 0)
			copy(e.buf[e.pos+1:], e.buf[e.pos:])
			e.buf[e.pos] = k.r
			e.pos++
			if e.pos == len(e.buf) {
				os.Stdout.WriteString(string(k.r))
			} else {
				e.redraw()
			}
		case c >= 32 && c < 127:
			e.buf = append(e.buf, 0)
			copy(e.buf[e.pos+1:], e.buf[e.pos:])
			e.buf[e.pos] = rune(c)
			e.pos++
			if e.pos == len(e.buf) {
				os.Stdout.WriteString(string(c))
			} else {
				e.redraw()
			}
		}
	}
	return "", false, false
}

func (e *editor) histPrev() {
	if len(e.history) == 0 {
		return
	}
	if e.hIdx == -1 {
		e.saved = append([]rune(nil), e.buf...)
		e.hIdx = len(e.history) - 1
	} else if e.hIdx > 0 {
		e.hIdx--
	}
	e.buf = []rune(e.history[e.hIdx])
	e.pos = len(e.buf)
	e.redraw()
}

func (e *editor) histNext() {
	if e.hIdx == -1 {
		return
	}
	if e.hIdx < len(e.history)-1 {
		e.hIdx++
		e.buf = []rune(e.history[e.hIdx])
	} else {
		e.hIdx = -1
		e.buf = append([]rune(nil), e.saved...)
	}
	e.pos = len(e.buf)
	e.redraw()
}
