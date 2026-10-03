// Package repl implements the interactive terminal UI.
package repl

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"encoding/json"

	"golang.org/x/term"
)

// editor is a modal (vim-capable) multiline line editor fed by keySource.
type editor struct {
	history        []string
	hIdx           int
	buf            []rune
	pos            int
	prompt         string
	saved          []rune
	vim            bool
	mode           string            // "insert" | "normal"
	pastes         map[string]string // placeholder -> pasted content
	pasteN         int
	lastCtrlC      time.Time
	lastEsc        int64
	completer      func(prefix string) []string // slash command completion
	cycleMode      func()                       // shift+tab -> permission mode cycle
	keys           *keySource
	pendingX       bool
	chordAt        time.Time
	actions        map[byte]func(*editor) *readResult
	histSearch     bool
	searchBuf      []rune
	onToggleExpand func()
	onToggleTodos  func()
	onKillAgents   func()
	insertRemaps   []string // vimInsertModeRemaps: 2-char sequences -> <Esc>
	lastIns        rune
	vimOp          rune
	vimFind        rune
	vimPendingG    rune
	vimCount       int
	vimCountRaw    int
	vimVisual      int
	vimReplace     bool
	lastFindCh     rune
	lastFindDir    rune
	lastDel        string
	lastChange     rune
	undoStack      [][]rune
	undoPos        []int
	kb             map[string]string // canonical chord -> action (keybindings.go)
	kbPrefix       map[string]bool   // canonical chord prefixes awaiting more keys
	kbUnbound      map[string]bool   // chords explicitly unbound via keybindings.json
	pendingChord   []string          // matched chord-prefix strokes
	imgDir         string            // scratchpad dir for pasted images
	imgN           int               // pasted-image counter
}

func newEditor() *editor {
	e := &editor{prompt: " > ", hIdx: -1, mode: "insert", pastes: map[string]string{}}
	e.history = loadHistoryFile()
	e.loadKeybindings()
	return e
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
		appendHistoryFile(l)
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
	line   string
	ok     bool // got a line
	intr   bool // ctrl-C pressed with empty buffer (double-tap = exit)
	quit   bool // double ctrl-C / ctrl-D -> exit repl
	rewind bool // esc-esc: open the rewind/message selector
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
	if k.mod&modCtrl != 0 && k.r >= 'a' && k.r <= 'z' {
		c = byte(k.r) - 'a' + 1 // CSI-u ctrl+letter -> control byte
	}
	if e.histSearch {
		return e.searchKey(k)
	}
	// keybinding layer (defaults + ~/.claude/keybindings.json), incl. chords
	if res, done := e.bindingKey(k); done {
		return res
	}
	// ctrl+x chord prefix (3s window like real CC)
	if e.pendingX {
		e.pendingX = false
		if c == 'k' && time.Since(e.chordAt) < 3*time.Second && !e.kbUnbound["ctrl+x ctrl+k"] {
			if e.actions != nil {
				if a := e.actions[24]; a != nil { // ctrl+x ctrl+k -> killAgents
					return a(e)
				}
			}
		}
	}
	if c == 24 { // ctrl+x
		e.pendingX = true
		e.chordAt = time.Now()
		return nil
	}
	// global action bindings (ctrl+o/r/t/g/l)
	if e.actions != nil && !e.vim {
		if a, ok := e.actions[c]; ok {
			if res := a(e); res != nil {
				return res
			}
		}
	}
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
		return e.interruptKey()
	case c == 4: // ctrl-D
		return e.exitKey()
	case (c == '\r' || c == '\n') && k.r != '\n' && k.mod == 0: // submit
		return e.submitKey()
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
	case c == 0x1b && k.seq == "" && k.mod == 0:
		return e.cancelKey()
	case c == '\t':
		e.complete()
	case k.seq == "[Z": // shift+tab: cycle permission mode
		if e.cycleMode != nil {
			e.cycleMode()
		}
	case k.r != 0 && k.mod&^modShift == 0: // shift alone still inserts text
		// vimInsertModeRemaps: 2-char sequence mapped to <Esc>
		if e.vim && len(e.insertRemaps) > 0 && e.lastIns != 0 {
			pair := string([]rune{e.lastIns, k.r})
			for _, seq := range e.insertRemaps {
				if seq == pair {
					// remove the first char of the pair
					if e.pos > 0 {
						e.buf = append(e.buf[:e.pos-1], e.buf[e.pos:]...)
						e.pos--
					}
					e.lastIns = 0
					e.mode = "normal"
					if e.pos > 0 {
						e.pos--
					}
					e.redraw()
					return nil
				}
			}
		}
		e.lastIns = k.r
		e.insert(k.r)
	case c >= 32 && c < 127 && k.mod&^modShift == 0:
		e.insert(rune(c))
	}
	return nil
}

// vimKey handles normal-mode keys — full state machine: counts, operators
// (d/c/y/>/<) × motions, linewise ops, f/F/t/T finds, registers, undo,
// dot-repeat.
func (e *editor) vimKey(k key) *readResult {
	c := k.b
	toInsert := func() { e.mode = "insert"; e.redraw() }
	if k.mod&(modCtrl|modAlt|modCmd) != 0 && k.r != 0 {
		return nil // CSI-u modified keys aren't normal-mode commands
	}
	if k.paste != "" {
		e.pushUndo()
		e.insert([]rune(e.expandPaste(k.paste))...)
		return nil
	}
	switch k.seq {
	case "[C":
		e.pos = min(len(e.buf), e.pos+1)
		e.redraw()
		return nil
	case "[D":
		if e.pos > 0 {
			e.pos--
		}
		e.redraw()
		return nil
	case "[A":
		e.histPrev()
		return nil
	case "[B":
		e.histNext()
		return nil
	}
	if c == '\r' {
		os.Stdout.WriteString("\n╰─╯\n")
		return &readResult{line: e.submitLine(), ok: true}
	}
	if e.mode == "visual" {
		return e.visualKey(k)
	}

	// find-char states (f/F/t/T awaiting target)
	if e.vimFind != 0 {
		fc := e.vimFind
		e.vimFind = 0
		if c >= 32 {
			e.lastFindCh = rune(c)
			e.lastFindDir = fc
			np := e.findChar(e.lastFindCh, fc)
			if np >= 0 {
				e.vimApplyOrMove(np, fc == 't' || fc == 'T')
			}
		}
		return nil
	}
	// operator-pending: next key resolves the motion
	if e.vimOp != 0 {
		op := e.vimOp
		e.vimOp = 0
		// linewise ops: dd cc yy >><<
		if rune(c) == op {
			e.linewiseOp(op)
			e.vimCount = 0
			return nil
		}
		if c >= '1' && c <= '9' {
			e.vimOp = op
			e.vimCount = int(c - '0')
			return nil
		}
		if c == '0' && e.vimCount > 0 {
			e.vimCount *= 10
			e.vimOp = op
			return nil
		}
		switch c {
		case 'w', 'b', 'e', 'W', 'B', 'E', '0', '^', '$', 'h', 'l', ' ':
			e.vimCount = max(1, e.vimCount)
			e.applyOp(op, e.motionEnd(c))
		case 'f', 'F', 't', 'T':
			e.vimOp = op
			e.vimFind = rune(c)
			return nil
		case 'g':
			// operator g — fall through to gg
			e.vimPendingG = op
			return nil
		case 'j', 'k':
			// linewise
			lines := e.vimCount
			if lines == 0 {
				lines = 1
			}
			start, end := e.linelRange(c, lines)
			e.applyOpRange(op, start, end, true)
		case 0x1b:
			// cancelled
		}
		e.vimCount = 0
		return nil
	}
	// g-pending (gg, g_ motions)
	if e.vimPendingG != 0 {
		op := e.vimPendingG
		e.vimPendingG = 0
		if c == 'g' {
			if op == 'm' {
				e.pos = 0
				e.redraw()
			} else {
				// operator to top of buffer
				e.applyOpRange(op, 0, e.lineEndAt(0)+1, true)
			}
		}
		return nil
	}
	// count prefix
	if c >= '1' && c <= '9' || (c == '0' && e.vimCount > 0) {
		if e.vimCount > 10000 {
			e.vimCount = 10000
		} else {
			e.vimCount = e.vimCount*10 + int(c-'0')
		}
		return nil
	}
	cnt := e.vimCount
	e.vimCount = 0
	if cnt == 0 {
		cnt = 1
	}
	repeat := func() { e.lastChange = rune(c) }
	switch c {
	case 'i':
		toInsert()
	case 'a':
		e.pos = min(len(e.buf), e.pos+1)
		toInsert()
	case 'A':
		e.pos = e.lineEnd()
		toInsert()
	case 'I':
		e.pos = e.lineStart()
		toInsert()
	case 'o':
		e.pos = e.lineEnd()
		e.pushUndo()
		e.insert('\n')
		toInsert()
		repeat()
	case 'O':
		e.pos = e.lineStart()
		e.pushUndo()
		e.insert('\n')
		e.pos--
		toInsert()
		repeat()
	case 'h', 0x7f:
		e.pos = max(0, e.pos-cnt)
		e.redraw()
	case 'l', ' ':
		e.pos = min(len(e.buf)-1, e.pos+cnt)
		e.redraw()
	case 'j':
		for i := 0; i < cnt; i++ {
			e.pos = e.lineBelow()
		}
		e.redraw()
	case 'k':
		for i := 0; i < cnt; i++ {
			e.pos = e.lineAbove()
		}
		e.redraw()
	case 'w', 'W':
		for i := 0; i < cnt; i++ {
			e.pos = e.wordFwd()
		}
		e.redraw()
	case 'b', 'B':
		for i := 0; i < cnt; i++ {
			e.pos = e.wordBack()
		}
		e.redraw()
	case 'e', 'E':
		for i := 0; i < cnt; i++ {
			e.pos = e.wordEnd()
		}
		e.redraw()
	case '0':
		e.pos = e.lineStart()
		e.redraw()
	case '^':
		p := e.lineStart()
		for p < len(e.buf) && (e.buf[p] == ' ' || e.buf[p] == '\t') {
			p++
		}
		e.pos = p
		e.redraw()
	case '$':
		e.pos = max(e.lineStart(), e.lineEnd()-1)
		e.redraw()
	case 'G':
		if e.vimCountRaw > 0 {
			e.pos = e.lineStartOf(e.vimCountRaw - 1)
		} else {
			e.pos = e.lineStartOf(e.lineCount() - 1)
		}
		e.redraw()
	case 'g':
		e.vimPendingG = 'm' // gg marker
	case 'f', 'F', 't', 'T':
		e.vimFind = rune(c)
	case ';':
		e.pos = e.findChar(e.lastFindCh, e.lastFindDir)
		e.redraw()
	case ',':
		e.pos = e.findChar(e.lastFindCh, flipDir(e.lastFindDir))
		e.redraw()
	case 'x':
		e.pushUndo()
		for i := 0; i < cnt && e.pos < len(e.buf); i++ {
			e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
		}
		e.recordReg(e.lastDel, 0)
		e.redraw()
		repeat()
	case 'X':
		e.pushUndo()
		for i := 0; i < cnt && e.pos > 0; i++ {
			e.buf = append(e.buf[:e.pos-1], e.buf[e.pos:]...)
			e.pos--
		}
		e.redraw()
		repeat()
	case 'd', 'c', 'y', '>', '<':
		e.vimOp = rune(c)
		if cnt > 0 {
			e.vimCount = cnt
		}
	case 'D':
		e.pushUndo()
		e.lastDel = string(e.buf[e.pos:e.lineEnd()])
		e.buf = e.buf[:e.pos]
		e.redraw()
		repeat()
	case 'C':
		e.pushUndo()
		e.lastDel = string(e.buf[e.pos:e.lineEnd()])
		e.buf = e.buf[:e.pos]
		toInsert()
		repeat()
	case 'Y':
		s, en := e.linelRange('j', 0)
		e.lastDel = string(e.buf[s:en])
		e.redraw()
	case 's':
		e.pushUndo()
		for i := 0; i < cnt && e.pos < len(e.buf); i++ {
			e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
		}
		toInsert()
		repeat()
	case 'S':
		e.pushUndo()
		s, en := e.linelRange('j', 0)
		e.lastDel = string(e.buf[s:en])
		e.buf = append(e.buf[:s], e.buf[en:]...)
		e.pos = s
		toInsert()
		repeat()
	case 'r':
		e.vimReplace = true
	case 'p':
		if e.lastDel != "" {
			e.pushUndo()
			at := e.pos + 1
			if strings.Contains(e.lastDel, "\n") {
				at = e.lineEnd() + 1
			}
			for i := 0; i < cnt; i++ {
				e.buf = append(e.buf[:at], append([]rune(e.lastDel), e.buf[at:]...)...)
			}
			e.redraw()
		}
	case 'u':
		e.undo()
	case '.':
		e.dotRepeat()
	case '~':
		if e.pos < len(e.buf) {
			e.pushUndo()
			r := e.buf[e.pos]
			if r >= 'a' && r <= 'z' {
				e.buf[e.pos] = r - 32
			} else if r >= 'A' && r <= 'Z' {
				e.buf[e.pos] = r + 32
			}
			e.pos++
			e.redraw()
			repeat()
		}
	case 'J':
		e.pushUndo()
		le := e.lineEnd()
		if le < len(e.buf) {
			e.buf[le] = ' '
			e.buf = append(e.buf[:le+1], append([]rune{}, e.buf[le+1:]...)...)
		}
		e.redraw()
		repeat()
	case 'v':
		e.vimVisual = e.pos // visual anchor
		e.mode = "visual"
		e.redraw()
	case '?':
		// help hint — real shows keybindings; noop visually
	case '/':
		e.histSearch = true
		e.searchBuf = e.searchBuf[:0]
		e.redrawSearch()
	case 0x1b:
		// already normal
	}
	return nil
}

func flipDir(d rune) rune {
	switch d {
	case 'f':
		return 'F'
	case 'F':
		return 'f'
	case 't':
		return 'T'
	case 'T':
		return 't'
	}
	return d
}
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

// registerDefaultActions wires the standard global keybindings.
func (e *editor) registerDefaultActions() {
	if e.actions == nil {
		e.actions = map[byte]func(*editor) *readResult{}
	}
	e.actions[12] = func(e *editor) *readResult { // ctrl+l clear input
		e.clearInput()
		return nil
	}
	e.actions[7] = func(e *editor) *readResult { // ctrl+g external editor
		return e.externalEditor()
	}
	e.actions[18] = func(e *editor) *readResult { // ctrl+r history search
		e.startSearch()
		return nil
	}
	e.actions[15] = func(e *editor) *readResult { // ctrl+o toggle expand
		e.toggleExpand()
		return nil
	}
	e.actions[20] = func(e *editor) *readResult { // ctrl+t toggle todos
		e.toggleTodos()
		return nil
	}
	e.actions[24] = func(e *editor) *readResult { // ctrl+x ctrl+k kill agents
		e.killAgents()
		return nil
	}
}

// ---- shared key actions (invoked by the switch and by keybindings) ----

// interruptKey is ctrl-C / app:interrupt: clear buffer; double-tap exits.
func (e *editor) interruptKey() *readResult {
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
	return nil
}

// exitKey is ctrl-D / app:exit: quit on empty buffer, else delete forward.
func (e *editor) exitKey() *readResult {
	if len(e.buf) == 0 {
		os.Stdout.WriteString("\n╰─╯\n")
		return &readResult{quit: true}
	}
	e.del(false)
	return nil
}

// cancelKey is esc / chat:cancel: vim insert->normal; else clear buffer with
// double-esc opening the rewind selector.
func (e *editor) cancelKey() *readResult {
	if e.vim {
		e.mode = "normal"
		if e.pos > 0 {
			e.pos--
		}
		e.redraw()
		return nil
	}
	if e.lastEsc > 0 && time.Since(time.Unix(0, e.lastEsc)) < 400*time.Millisecond {
		e.lastEsc = 0
		return &readResult{rewind: true}
	}
	e.lastEsc = time.Now().UnixNano()
	e.buf = e.buf[:0]
	e.pos = 0
	e.redraw()
	return nil
}

// submitKey is enter / chat:submit (with \-continuation -> newline).
func (e *editor) submitKey() *readResult {
	if len(e.buf) > 0 && e.buf[len(e.buf)-1] == '\\' {
		e.buf = e.buf[:len(e.buf)-1]
		e.insert('\n')
		return nil
	}
	os.Stdout.WriteString("\n╰─╯\n")
	return &readResult{line: e.submitLine(), ok: true}
}

// clearInput is ctrl-l / chat:clearInput.
func (e *editor) clearInput() {
	e.buf = e.buf[:0]
	e.pos = 0
	e.redraw()
}

// startSearch is ctrl-r / history:search.
func (e *editor) startSearch() {
	e.histSearch = true
	e.searchBuf = e.searchBuf[:0]
	e.redrawSearch()
}

func (e *editor) toggleExpand() {
	if e.onToggleExpand != nil {
		e.onToggleExpand()
	}
}

func (e *editor) toggleTodos() {
	if e.onToggleTodos != nil {
		e.onToggleTodos()
	}
}

func (e *editor) cyclePermMode() {
	if e.cycleMode != nil {
		e.cycleMode()
	}
}

func (e *editor) killAgents() {
	if e.onKillAgents != nil {
		e.onKillAgents()
	}
}

// imagePaste is ctrl-v / chat:imagePaste: pull an image off the clipboard
// (macOS: pngpaste, osascript fallback), save it to a scratchpad dir and
// insert a "[Image #N]" placeholder. On submit the placeholder expands into
// the \x00IMAGE\x00<path>\x00<size> marker the agent loop turns into an image
// block. Falls back to a text paste (pbpaste) when no image is present.
func (e *editor) imagePaste() *readResult {
	if e.imgDir == "" {
		d, err := os.MkdirTemp("", "claude-go-images-")
		if err != nil {
			return nil
		}
		e.imgDir = d
	}
	e.imgN++
	path := filepath.Join(e.imgDir, fmt.Sprintf("image-%d.png", e.imgN))
	if !clipboardImageTo(path) {
		e.imgN--
		if out, err := exec.Command("pbpaste").Output(); err == nil && len(out) > 0 {
			e.insert([]rune(e.expandPaste(string(out)))...)
		}
		return nil
	}
	b, _ := os.ReadFile(path)
	ph := fmt.Sprintf("[Image #%d]", e.imgN)
	e.pastes[ph] = fmt.Sprintf("\x00IMAGE\x00%s\x00%d", path, len(b))
	e.insert([]rune(ph)...)
	return nil
}

// clipboardImageTo writes the clipboard's image to path as PNG. Returns false
// when the clipboard holds no image or no helper tool is available.
func clipboardImageTo(path string) bool {
	if _, err := exec.LookPath("pngpaste"); err == nil {
		if exec.Command("pngpaste", path).Run() == nil && validPNG(path) {
			return true
		}
		os.Remove(path)
	}
	// osascript fallback: write the clipboard as PNGf to the file.
	script := `set f to open for access (POSIX file "` + path + `") with write permission
try
  write (the clipboard as «class PNGf») to f
end try
close access f`
	if exec.Command("osascript", "-e", script).Run() == nil && validPNG(path) {
		return true
	}
	os.Remove(path)
	return false
}

func validPNG(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var hdr [8]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return false
	}
	return bytes.Equal(hdr[:], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
}

// loadHistoryFile seeds the editor history from ~/.claude/history.jsonl so
// ctrl-r search covers past sessions. Dedupes, newest last.
func loadHistoryFile() []string {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".claude", "history.jsonl"))
	if err != nil {
		return nil
	}
	var disp []string
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var rec struct {
			Display string `json:"display"`
		}
		if json.Unmarshal([]byte(ln), &rec) != nil || rec.Display == "" {
			continue
		}
		disp = append(disp, rec.Display)
	}
	// dedupe keeping the newest occurrence, restore chronological order
	seen := map[string]bool{}
	out := make([]string, 0, len(disp))
	for i := len(disp) - 1; i >= 0; i-- {
		if seen[disp[i]] {
			continue
		}
		seen[disp[i]] = true
		out = append(out, disp[i])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if len(out) > 10000 {
		out = out[len(out)-10000:]
	}
	return out
}

// externalEditor opens $VISUAL/$EDITOR with the buffer and imports the result.
func (e *editor) externalEditor() *readResult {
	ed := os.Getenv("VISUAL")
	if ed == "" {
		ed = os.Getenv("EDITOR")
	}
	if ed == "" {
		ed = "vi"
	}
	dir, _ := os.MkdirTemp("", "claude-go-edit-")
	path := filepath.Join(dir, "prompt.md")
	os.WriteFile(path, []byte(string(e.buf)), 0o600)
	os.Stdout.WriteString("\r\x1b[2K  ($EDITOR: " + ed + ")\r\n")
	if e.keys != nil {
		term.Restore(int(os.Stdin.Fd()), e.keys.old)
	}
	cmd := exec.Command("sh", "-c", ed+" "+path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
	if e.keys != nil {
		if st, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
			e.keys.old = st
		}
	}
	if b, err := os.ReadFile(path); err == nil {
		e.buf = []rune(strings.TrimRight(string(b), "\n"))
		e.pos = len(e.buf)
	}
	e.redraw()
	return nil
}

// searchKey handles ctrl+r history search mode.
func (e *editor) searchKey(k key) *readResult {
	c := k.b
	switch {
	case c == 3 || (c == 0x1b && k.seq == ""): // esc/ctrl-c exits search
		e.histSearch = false
		e.searchBuf = e.searchBuf[:0]
		os.Stdout.WriteString("\r\x1b[2K" + e.prompt)
		e.redraw()
	case c == '\r':
		e.histSearch = false
		if m := e.searchMatch(); m != "" {
			e.buf = []rune(m)
			e.pos = len(e.buf)
		}
		e.searchBuf = e.searchBuf[:0]
		e.redraw()
	case c == 0x7f || c == 8:
		if len(e.searchBuf) > 0 {
			e.searchBuf = e.searchBuf[:len(e.searchBuf)-1]
		}
		e.redrawSearch()
	case c == 18: // ctrl+r again cycles matches — naive: keep searching
		e.redrawSearch()
	case c >= 32 && c < 127:
		e.searchBuf = append(e.searchBuf, rune(c))
		e.redrawSearch()
	}
	return nil
}

func (e *editor) searchMatch() string {
	q := string(e.searchBuf)
	// subsequence match against history, newest first
	for i := len(e.history) - 1; i >= 0; i-- {
		h := e.history[i]
		if subseq(q, h) {
			return h
		}
	}
	return ""
}

func subseq(q, s string) bool {
	qi := 0
	for _, ch := range s {
		if qi < len(q) && rune(q[qi]) == ch {
			qi++
		}
	}
	return qi == len(q)
}

func (e *editor) redrawSearch() {
	m := e.searchMatch()
	os.Stdout.WriteString("\r\x1b[2K  " + dim + "(search prompts `" + string(e.searchBuf) + "`): " + reset + m)
}

// ---- vim helpers ----

func (e *editor) pushUndo() {
	e.undoStack = append(e.undoStack, append([]rune{}, e.buf...))
	e.undoPos = append(e.undoPos, e.pos)
	if len(e.undoStack) > 50 {
		e.undoStack = e.undoStack[1:]
		e.undoPos = e.undoPos[1:]
	}
}

func (e *editor) undo() {
	if len(e.undoStack) == 0 {
		return
	}
	e.buf = e.undoStack[len(e.undoStack)-1]
	e.pos = e.undoPos[len(e.undoPos)-1]
	e.undoStack = e.undoStack[:len(e.undoStack)-1]
	e.undoPos = e.undoPos[:len(e.undoPos)-1]
	e.redraw()
}

func (e *editor) dotRepeat() {
	if e.lastChange == 0 {
		return
	}
	e.vimKey(key{b: byte(e.lastChange)})
}

func (e *editor) recordReg(s string, _ int) { e.lastDel = s }

func (e *editor) wordEnd() int {
	p := e.pos
	n := len(e.buf)
	p++
	for p < n && isWordSep(e.buf[p]) {
		p++
	}
	for p < n && !isWordSep(e.buf[p]) {
		p++
	}
	if p > 0 {
		p--
	}
	return min(p, n-1)
}

func isWordSep(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }

// findChar searches for ch in the current line; dir 'f'/'t' forward, 'F'/'T' back.
func (e *editor) findChar(ch rune, dir rune) int {
	if ch == 0 {
		return e.pos
	}
	c := ch
	switch dir {
	case 'f', 't':
		p := e.pos + 1
		end := e.lineEnd()
		for p < end {
			if e.buf[p] == c {
				if dir == 't' && p > e.pos+1 {
					return p - 1
				}
				return p
			}
			p++
		}
	case 'F', 'T':
		p := e.pos - 1
		st := e.lineStart()
		for p >= st {
			if e.buf[p] == c {
				if dir == 'T' && p < e.pos-1 {
					return p + 1
				}
				return p
			}
			p--
		}
	}
	return e.pos
}

// vimApplyOrMove resolves a completed find-motion under a pending operator.
func (e *editor) vimApplyOrMove(np int, _ bool) {
	if e.vimOp != 0 {
		op := e.vimOp
		e.vimOp = 0
		e.applyOp(op, np)
		return
	}
	e.pos = np
	e.redraw()
}

// motionEnd returns the target position for a single-char motion.
func (e *editor) motionEnd(c byte) int {
	switch c {
	case 'w', 'W':
		return e.wordFwd()
	case 'b', 'B':
		return e.wordBack()
	case 'e', 'E':
		return e.wordEnd()
	case '0', '^':
		return e.lineStart()
	case '$':
		return e.lineEnd()
	case 'h':
		return max(0, e.pos-1)
	case 'l', ' ':
		return min(len(e.buf)-1, e.pos+1)
	}
	return e.pos
}

// applyOp applies operator op over [pos, target).
func (e *editor) applyOp(op rune, target int) {
	s, en := e.pos, target
	if s > en {
		s, en = en, s
	}
	if s == en && en < len(e.buf) {
		en++
	}
	e.applyOpRange(op, s, en, false)
}

func (e *editor) applyOpRange(op rune, s, en int, linewise bool) {
	if en > len(e.buf) {
		en = len(e.buf)
	}
	if s < 0 {
		s = 0
	}
	if en <= s {
		return
	}
	switch op {
	case 'd', 'c', 'y':
		e.lastDel = string(e.buf[s:en])
		if op == 'y' {
			e.redraw()
			return
		}
		e.pushUndo()
		e.buf = append(e.buf[:s], e.buf[en:]...)
		e.pos = s
		if op == 'c' {
			e.mode = "insert"
		}
		e.lastChange = op
	case '>':
		e.pushUndo()
		for p := s; p < en && p < len(e.buf); p++ {
			if e.buf[p] == '\n' || p == s {
				// indent at line start
				e.buf = append(e.buf[:p], append([]rune("  "), e.buf[p:]...)...)
				en += 2
				p++
			}
		}
		e.pos = s
	case '<':
		e.pushUndo()
		var nb []rune
		for p := 0; p < len(e.buf); p++ {
			lineStart := p == 0 || e.buf[p-1] == '\n'
			if lineStart && p >= s && p < en && p+1 < len(e.buf) && e.buf[p] == ' ' && e.buf[p+1] == ' ' {
				p++ // skip two spaces
				continue
			}
			nb = append(nb, e.buf[p])
		}
		e.buf = nb
		e.pos = min(s, len(e.buf))
	}
	e.redraw()
}

// linewiseOp handles dd/cc/yy/>>/<<.
func (e *editor) linewiseOp(op rune) {
	s, en := e.linelRange('j', 0)
	cnt := e.vimCount
	if cnt > 1 {
		// extend down cnt-1 lines
		end := en
		for i := 1; i < cnt && end < len(e.buf); i++ {
			ne := end
			for ne < len(e.buf) && e.buf[ne] != '\n' {
				ne++
			}
			end = ne + 1
		}
		en = end
	}
	if op == 'y' {
		e.lastDel = string(e.buf[s:en])
		e.redraw()
		return
	}
	e.applyOpRange(op, s, en, true)
}

// linelRange returns [start,end) covering the current line plus n extra
// lines in direction dir ('j' or 'k'). dir 'j' with n=0 covers just this line.
func (e *editor) linelRange(dir byte, n int) (int, int) {
	s := e.lineStart()
	en := e.lineEnd()
	if en < len(e.buf) {
		en++ // include the newline
	}
	if dir == 'k' && n > 0 {
		for i := 0; i < n && s > 0; i++ {
			s--
			for s > 0 && e.buf[s-1] != '\n' {
				s--
			}
		}
	} else {
		for i := 0; i < n && en <= len(e.buf); i++ {
			ne := en
			for ne < len(e.buf) && e.buf[ne] != '\n' {
				ne++
			}
			if ne >= len(e.buf) {
				break
			}
			en = ne + 1
		}
	}
	return s, en
}

func (e *editor) lineStartOf(ln int) int {
	if ln < 0 {
		ln = 0
	}
	cur, p := 0, 0
	for p < len(e.buf) {
		if cur == ln {
			return p
		}
		if e.buf[p] == '\n' {
			cur++
		}
		p++
	}
	return len(e.buf)
}

func (e *editor) lineEndAt(p int) int {
	for p < len(e.buf) && e.buf[p] != '\n' {
		p++
	}
	return p
}

func (e *editor) lineCount() int {
	n := 1
	for _, r := range e.buf {
		if r == '\n' {
			n++
		}
	}
	return n
}

// visualKey handles VISUAL mode: motions extend the anchor selection,
// operators apply to the selection.
func (e *editor) visualKey(k key) *readResult {
	c := k.b
	exit := func() { e.mode = "normal"; e.vimVisual = -1; e.redraw() }
	switch c {
	case 0x1b, 'v':
		exit()
		return nil
	case 'h', 0x7f:
		if e.pos > 0 {
			e.pos--
		}
		e.redraw()
	case 'l', ' ':
		if e.pos < len(e.buf)-1 {
			e.pos++
		}
		e.redraw()
	case 'w', 'W':
		e.pos = e.wordFwd()
		e.redraw()
	case 'b', 'B':
		e.pos = e.wordBack()
		e.redraw()
	case 'e', 'E':
		e.pos = e.wordEnd()
		e.redraw()
	case '0':
		e.pos = e.lineStart()
		e.redraw()
	case '$':
		e.pos = max(e.lineStart(), e.lineEnd()-1)
		e.redraw()
	case 'o':
		e.pos, e.vimVisual = e.vimVisual, e.pos // swap anchor/cursor
		e.redraw()
	case 'd', 'x', 'y', 'c':
		s, en := min(e.pos, e.vimVisual), max(e.pos, e.vimVisual)+1
		op := rune(c)
		if c == 'x' {
			op = 'd'
		}
		e.mode = "normal"
		e.vimVisual = -1
		e.applyOpRange(op, s, en, false)
		return nil
	}
	return nil
}

// appendHistoryFile persists prompts to ~/.claude/history.jsonl (real file).
func appendHistoryFile(line string) {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".claude")
	os.MkdirAll(dir, 0o755)
	f, err := os.OpenFile(filepath.Join(dir, "history.jsonl"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(map[string]any{
		"display": line, "timestamp": time.Now().UnixMilli(),
	})
	f.Write(append(b, '\n'))
}

// vimInsertRemaps reads vimInsertModeRemaps from settings (["jj","jk"]…).
func vimInsertRemaps() []string {
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(home, ".claude.json"),
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var st struct {
			VimInsertModeRemaps []string `json:"vimInsertModeRemaps"`
		}
		if json.Unmarshal(b, &st) == nil && len(st.VimInsertModeRemaps) > 0 {
			return st.VimInsertModeRemaps
		}
	}
	return nil
}
