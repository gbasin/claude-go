// Package repl implements the interactive terminal UI.
package repl

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// editor is a modal (vim-capable) multiline line editor fed by keySource.
type editor struct {
	history   []string
	hIdx      int
	buf       []rune
	pos       int
	prompt    string
	saved     []rune
	vim       bool
	mode      string            // "insert" | "normal"
	pastes    map[string]string // placeholder -> pasted content
	pasteN    int
	lastCtrlC time.Time
	completer func(prefix string) []string // slash command completion
	cycleMode func()                       // shift+tab -> permission mode cycle
}

func newEditor() *editor {
	return &editor{prompt: " > ", hIdx: -1, mode: "insert", pastes: map[string]string{}}
}

// redraw reprints the whole input area (multiline-safe).
func (e *editor) redraw() {
	var b strings.Builder
	b.WriteString("\r\x1b[J") // col0, clear to screen bottom
	b.WriteString("│" + e.prompt + string(e.buf))
	// reposition cursor to e.pos
	before := string(e.buf[:e.pos])
	totalNL := strings.Count(string(e.buf), "\n")
	nlBefore := strings.Count(before, "\n")
	col := len(before) - strings.LastIndex(before, "\n") - 1
	if nlBefore == 0 {
		col += len(e.prompt) + 1 // +1 for left border
	}
	if up := totalNL - nlBefore; up > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", up)
	}
	b.WriteString("\r")
	if col > 0 {
		fmt.Fprintf(&b, "\x1b[%dC", col)
	}
	os.Stdout.WriteString(b.String())
}

func (e *editor) insert(rs ...rune) {
	for _, r := range rs {
		e.buf = append(e.buf, 0)
		copy(e.buf[e.pos+1:], e.buf[e.pos:])
		e.buf[e.pos] = r
		e.pos++
	}
	e.redraw()
}

func (e *editor) del(back bool) {
	if back {
		if e.pos > 0 {
			e.buf = append(e.buf[:e.pos-1], e.buf[e.pos:]...)
			e.pos--
		}
	} else if e.pos < len(e.buf) {
		e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
	}
	e.redraw()
}

// submitLine returns the final text with paste placeholders expanded.
func (e *editor) submitLine() string {
	l := string(e.buf)
	for ph, content := range e.pastes {
		l = strings.ReplaceAll(l, ph, content)
	}
	if strings.TrimSpace(l) != "" {
		e.history = append(e.history, l)
	}
	e.buf = e.buf[:0]
	e.pos = 0
	e.hIdx = -1
	e.pastes = map[string]string{}
	e.pasteN = 0
	return l
}

// expandPaste replaces a multi-line paste with a CC-style placeholder.
func (e *editor) expandPaste(pasted string) string {
	nl := strings.Count(pasted, "\n")
	if nl == 0 && len(pasted) < 200 {
		return pasted
	}
	e.pasteN++
	ph := fmt.Sprintf("[Pasted text #%d +%d lines]", e.pasteN, nl+1)
	// the content the model actually receives, wrapped like CC does
	e.pastes[ph] = fmt.Sprintf("<pasted_content id=\"%d\">\n%s\n</pasted_content>", e.pasteN, pasted)
	return ph
}

type readResult struct {
	line string
	ok   bool // got a line
	intr bool // ctrl-C pressed with empty buffer (double-tap = exit)
	quit bool // double ctrl-C / ctrl-D -> exit repl
}

// readLine consumes keys until submit. Vim-normal mode supported.
func (e *editor) readLine(ks *keySource) readResult {
	os.Stdout.WriteString("╭─╮\n│" + e.prompt)
	startRowDone := false
	_ = startRowDone
	for k := range ks.ch {
		if e.vim && e.mode == "normal" {
			if r := e.vimKey(k); r != nil {
				return *r
			}
			continue
		}
		if r := e.insertKey(k); r != nil {
			return *r
		}
	}
	return readResult{quit: true}
}

func (e *editor) insertKey(k key) *readResult {
	c := k.b
	switch {
	case k.paste != "":
		e.insert([]rune(e.expandPaste(k.paste))...)
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
		e.del(false)
	case c == 3: // ctrl-C
		if len(e.buf) > 0 {
			e.buf = e.buf[:0]
			e.pos = 0
			e.redraw()
		} else if time.Since(e.lastCtrlC) < 2*time.Second {
			os.Stdout.WriteString("\n╰─╯\n")
			return &readResult{quit: true}
		} else {
			e.lastCtrlC = time.Now()
			os.Stdout.WriteString("\r\n  (ctrl-C again to exit)\r\n")
			e.redraw()
		}
	case c == 4: // ctrl-D
		if len(e.buf) == 0 {
			os.Stdout.WriteString("\n╰─╯\n")
			return &readResult{quit: true}
		}
		e.del(false)
	case c == '\r' || c == '\n': // submit (\n submits too; multiline via \-continuation or paste)
		if len(e.buf) > 0 && e.buf[len(e.buf)-1] == '\\' {
			e.buf = e.buf[:len(e.buf)-1]
			e.insert('\n')
			return nil
		}
		os.Stdout.WriteString("\n╰─╯\n")
		return &readResult{line: e.submitLine(), ok: true}
	case c == 0x7f || c == 8:
		e.del(true)
	case c == 1:
		e.pos = e.lineStart()
		e.redraw()
	case c == 5:
		e.pos = e.lineEnd()
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
			e.del(true)
		}
		for e.pos > 0 && e.buf[e.pos-1] != ' ' {
			e.del(true)
		}
	case c == 0x1b && k.seq == "":
		if e.vim {
			e.mode = "normal"
			if e.pos > 0 {
				e.pos--
			}
			e.redraw()
		} else {
			e.buf = e.buf[:0]
			e.pos = 0
			e.redraw()
		}
	case c == '\t':
		e.complete()
	case k.seq == "[Z": // shift+tab: cycle permission mode
		if e.cycleMode != nil {
			e.cycleMode()
		}
	case k.r != 0:
		e.insert(k.r)
	case c >= 32 && c < 127:
		e.insert(rune(c))
	}
	return nil
}

// vimKey handles normal-mode keys.
func (e *editor) vimKey(k key) *readResult {
	c := k.b
	toInsert := func() { e.mode = "insert"; e.redraw() }
	switch {
	case k.paste != "":
		e.insert([]rune(e.expandPaste(k.paste))...)
	case k.seq == "[C":
		if e.pos < len(e.buf)-1 {
			e.pos++
		}
		e.redraw()
	case k.seq == "[D":
		if e.pos > 0 {
			e.pos--
		}
		e.redraw()
	case k.seq == "[A":
		e.histPrev()
	case k.seq == "[B":
		e.histNext()
	case c == '\r':
		os.Stdout.WriteString("\n╰─╯\n")
		return &readResult{line: e.submitLine(), ok: true}
	case c == 'i':
		toInsert()
	case c == 'a':
		e.pos++
		toInsert()
	case c == 'A':
		e.pos = e.lineEnd()
		toInsert()
	case c == 'I':
		e.pos = e.lineStart()
		toInsert()
	case c == 'o':
		e.pos = e.lineEnd()
		e.insert('\n')
		toInsert()
	case c == 'O':
		e.pos = e.lineStart()
		e.insert('\n')
		e.pos--
		toInsert()
	case c == 'h':
		if e.pos > 0 {
			e.pos--
		}
		e.redraw()
	case c == 'l':
		if e.pos < len(e.buf)-1 {
			e.pos++
		}
		e.redraw()
	case c == '0':
		e.pos = e.lineStart()
		e.redraw()
	case c == '$':
		e.pos = max(0, e.lineEnd()-1)
		e.redraw()
	case c == 'w':
		e.pos = e.wordFwd()
		e.redraw()
	case c == 'b':
		e.pos = e.wordBack()
		e.redraw()
	case c == 'x':
		e.del(false)
	case c == 'd': // dd only (dw simplification)
		// second key handled by pending op? keep simple: dd
		// consume next key
		select {
		case k2 := <-pendingKey:
			_ = k2
		default:
		}
	case c == 'D':
		e.buf = e.buf[:e.pos]
		e.redraw()
	case c == 'G':
		e.pos = len(e.buf) - 1
		e.redraw()
	case c == 'j':
		e.pos = e.lineBelow()
		e.redraw()
	case c == 'k':
		e.pos = e.lineAbove()
		e.redraw()
	case c == 'u':
		// undo not implemented
	}
	return nil
}

var pendingKey = make(chan key, 1)

func (e *editor) lineStart() int {
	p := e.pos
	for p > 0 && e.buf[p-1] != '\n' {
		p--
	}
	return p
}

func (e *editor) lineEnd() int {
	p := e.pos
	for p < len(e.buf) && e.buf[p] != '\n' {
		p++
	}
	return p
}

func (e *editor) lineBelow() int {
	end := e.lineEnd()
	if end >= len(e.buf) {
		return e.pos
	}
	col := e.pos - e.lineStart()
	ns := end + 1
	ne := ns
	for ne < len(e.buf) && e.buf[ne] != '\n' {
		ne++
	}
	if ns+col > ne {
		return ne
	}
	return ns + col
}

func (e *editor) lineAbove() int {
	start := e.lineStart()
	if start == 0 {
		return e.pos
	}
	col := e.pos - start
	ps := start - 1
	for ps > 0 && e.buf[ps-1] != '\n' {
		ps--
	}
	if ps+col > start-1 {
		return start - 1
	}
	return ps + col
}

func (e *editor) wordFwd() int {
	p := e.pos
	for p < len(e.buf) && e.buf[p] != ' ' && e.buf[p] != '\n' {
		p++
	}
	for p < len(e.buf) && (e.buf[p] == ' ' || e.buf[p] == '\n') {
		p++
	}
	return p
}

func (e *editor) wordBack() int {
	p := e.pos
	for p > 0 && (e.buf[p-1] == ' ' || e.buf[p-1] == '\n') {
		p--
	}
	for p > 0 && e.buf[p-1] != ' ' && e.buf[p-1] != '\n' {
		p--
	}
	return p
}

// complete runs slash-command completion when buffer starts with /.
func (e *editor) complete() {
	if e.completer == nil || len(e.buf) == 0 || e.buf[0] != '/' {
		return
	}
	prefix := string(e.buf)
	matches := e.completer(prefix[1:])
	if len(matches) == 0 {
		return
	}
	if len(matches) == 1 {
		e.buf = []rune("/" + matches[0] + " ")
		e.pos = len(e.buf)
		e.redraw()
		return
	}
	// longest common prefix
	lcp := matches[0]
	for _, m := range matches[1:] {
		for !strings.HasPrefix(m, lcp) {
			lcp = lcp[:len(lcp)-1]
		}
	}
	if len(lcp) > len(prefix)-1 {
		e.buf = []rune("/" + lcp)
		e.pos = len(e.buf)
	}
	os.Stdout.WriteString("\r\n")
	for _, m := range matches {
		fmt.Printf("  /%s\r\n", m)
	}
	e.redraw()
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

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
