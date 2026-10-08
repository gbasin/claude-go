// Package repl implements the interactive terminal UI.
package repl

import (
	"bytes"
	"claude-go/internal/auth"
	"claude-go/internal/config"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"encoding/base64"
	"encoding/json"

	"claude-go/internal/voice"
)

// editor is a modal (vim-capable) multiline line editor fed by keySource.
type editor struct {
	history         []string
	hIdx            int
	buf             []rune
	pos             int
	stashed         []rune // ctrl+s draft stash — real's is single-slot
	busyChord       []string
	busyChordAt     time.Time
	prompt          string
	saved           []rune
	vim             bool
	mode            string            // "insert" | "normal"
	pastes          map[string]string // placeholder -> pasted content
	pasteN          int
	lastEsc         int64
	escArmAt        time.Time // "Esc again to clear" window (non-empty draft)
	exitArm         bool      // "Press <key> again to exit" armed (jz shared gate)
	exitArmKey      string    // first arming key's display name
	exitArmAt       time.Time
	cleared         []rune   // held cleared draft — Up restores (real holdCleared)
	clearedHist     bool     // held draft enters history if never restored
	killRing        []string // emacs kill-ring (ring[0] = newest)
	killLast        bool     // last key was a kill — consecutive kills coalesce
	wasKill         bool     // killLast captured at insertKey entry
	yankPos         int      // yanked span start (-1 = no live yank)
	yankLen         int
	yankIdx         int
	changeKeys      []key  // vim: recorded keystrokes of the last change (.)
	pendingKeys     []key  // vim: accumulating the in-flight op sequence
	recIns          []rune // vim: text inserted during the current change
	recordingIns    bool
	lastInsText     []rune                       // vim: inserted text of the last change (.)
	searchIdx       int                          // ctrl+r: index of current match in e.history
	completer       func(prefix string) []string // slash command completion
	acOpen          bool                         // autocomplete menu visible
	acDesc          func(name string) string     // per-item description
	acItems         []string                     // filtered matches
	acSel           int                          // selected index
	leftArm         bool                         // ← pressed once on empty input
	acAt            string                       // @-mention token being completed
	cycleMode       func()                       // shift+tab -> permission mode cycle
	keys            *keySource
	pendingX        bool
	chordAt         time.Time
	actions         map[byte]func(*editor) *readResult
	histSearch      bool
	searchBuf       []rune
	onToggleExpand  func()
	onToggleTodos   func()
	onKillAgents    func()
	onFastMode      func()
	runningAgents   func() int // ← agents-pane gate (real needs running tasks)
	insertRemaps    []string   // vimInsertModeRemaps: 2-char sequences -> <Esc>
	lastIns         rune
	insGroup        bool // mid insert-group — push undo once per burst
	vimOp           rune
	vimFind         rune
	vimPendingG     rune
	vimObj          byte // i/a text-object prefix awaiting the object char
	repCnt          int  // r<count> pending replace count
	vimCount        int
	vimCountRaw     int
	vimVisual       int
	vimReplace      bool
	lastFindCh      rune
	lastFindDir     rune
	lastDel         string
	undoStack       [][]rune
	undoPos         []int
	kb              map[string]string // canonical chord -> action (keybindings.go)
	kbResolve       map[string]string // ctx+action -> display chord for hints
	kbPrefix        map[string]bool   // canonical chord prefixes awaiting more keys
	kbUnbound       map[string]bool   // chords explicitly unbound via keybindings.json
	pendingChord    []string          // matched chord-prefix strokes
	imgDir          string            // scratchpad dir for pasted images
	imgN            int               // pasted-image counter
	zoneEdit        bool              // input box lives in the compositor live zone
	boxAll          []string          // last-rendered full box (for the scrollback echo)
	voice           *voiceUI          // voice session glue (internal/voice)
	voiceBase       string            // api.BaseURL for the voice_stream ws
	voiceCreds      *auth.Credentials // OAuth creds for Bearer auth
	voiceCWD        string            // cwd for settings + keyterms
	deferredActions []string          // bound actions resolved mid-turn — fire at idle
	footer          string            // status row rendered under the box
}

func newEditor() *editor {
	// real's prompt glyph is ❯ followed by a non-breaking space
	e := &editor{prompt: " ❯\u00a0", hIdx: -1, mode: "insert", pastes: map[string]string{}}
	e.history = loadHistoryFile()
	e.loadKeybindings()
	// --prefill <text> / --prefill-b64: real seeds the input buffer
	// without submitting (deep-link launch paths)
	if pf := func() string {
		if p := os.Getenv("CLAUDE_GO_PREFILL"); p != "" {
			return p
		}
		if b64 := os.Getenv("CLAUDE_GO_PREFILL_B64"); b64 != "" {
			if dec, err := base64.RawURLEncoding.DecodeString(b64); err == nil {
				return string(dec)
			}
		}
		return ""
	}(); pf != "" {
		e.buf = []rune(pf)
		e.pos = len(e.buf)
	}
	return e
}

// boxFrame renders the prompt box (╭─╮ / │ > … / ╰─╯) as compositor zone
// rows plus the cursor's position within them (row 0 = top border). The
// full unwindowed box is kept in e.boxAll for the scrollback echo.
func (e *editor) boxFrame() (box []string, curRow, curCol int) {
	if e.histSearch {
		ln := "  " + dim + "(search prompts `" + string(e.searchBuf) + "`): " + reset + e.searchMatch()
		rule := "\x1b[38;5;244m" + strings.Repeat("─", compW) + "\x1b[39m"
		e.boxAll = []string{rule, ln, rule}
		return e.boxAll, 1, visWidth(ln)
	}
	lines := strings.Split(e.renderedBuf(), "\n")
	pw := visWidth(e.prompt)
	// usable text width per box row: screen minus the left border and the
	// prompt indent — long input soft-wraps onto extra rows (real CC
	// behavior) instead of being clamped off the right edge, which hid
	// everything past the border at narrow widths
	w := compW
	if w <= 0 {
		w = 80
	}
	textW := w - pw
	if textW < 4 {
		textW = 4
	}
	// which buffer line holds the cursor, and its cell offset in it
	before := string(e.buf[:e.pos])
	curLine := strings.Count(before, "\n")
	curCells := visWidth(before[strings.LastIndex(before, "\n")+1:])
	// lay out: top border, soft-wrapped input rows, voice hint, bottom
	// border — tracking which box row each logical line starts on
	rule := "\x1b[38;5;244m" + strings.Repeat("─", w) + "\x1b[39m"
	all := make([]string, 0, len(lines)+5)
	// real renders the effort pill right-aligned on the row above the
	// top rule: "◐ medium · /effort"
	if eff := effortLabel(); eff != "" {
		pad := w - visWidth(eff)
		if pad < 0 {
			pad = 0
		}
		all = append(all, strings.Repeat(" ", pad)+eff)
	}
	all = append(all, rule)
	lineRow := make([]int, len(lines))
	for i, ln := range lines {
		lineRow[i] = len(all) - 1 // 0-based row among box content rows
		for j, sg := range softWrap(ln, textW) {
			if i == 0 && j == 0 {
				all = append(all, e.prompt+sg)
			} else {
				all = append(all, strings.Repeat(" ", pw)+sg)
			}
		}
	}
	if hint := e.voiceHint(); hint != "" {
		// voice indicator row (real renders it inside the composer area)
		all = append(all, strings.Repeat(" ", pw)+hint)
	}
	all = append(all, e.acRows(w)...)
	all = append(all, rule)
	if f := e.footerText(); f != "" {
		all = append(all, f) // footer row below the bottom rule
	}
	e.boxAll = all
	content := all[1 : len(all)-1]
	// cursor's box row among content rows, and its column
	curRow = lineRow[curLine] + curCells/textW
	curCol = pw + curCells%textW
	if e.pos == len(e.buf) && curCells > 0 && curCells%textW == 0 {
		// cursor sits at a wrap boundary with no following row — the
		// terminal's deferred-wrap position is the last cell of the
		// full row, not the (nonexistent) next row
		curRow--
		curCol = w
	}
	// window into at most compAvailRows rows, keeping curRow visible
	start := 0
	if avail := compAvailRows(); len(content) > avail {
		start = curRow - avail + 1
		if start < 0 {
			start = 0
		}
		if start > len(content)-avail {
			start = len(content) - avail
		}
	}
	box = append([]string{all[0]}, append(content[start:], all[len(all)-1])...)
	return box, 1 + curRow - start, curCol
}

// redraw reprints the whole input area — into the compositor live zone
// while editing there (the Ink model), else inline (multiline-safe).
func (e *editor) redraw() {
	if e.zoneEdit {
		box, r, c := e.boxFrame()
		compEditor(box, r, c)
		return
	}
	var b strings.Builder
	b.WriteString("\r\x1b[J") // col0, clear to screen bottom
	hint := e.voiceHint()
	b.WriteString(e.prompt + e.renderedBuf())
	extra := 0 // rows drawn below the input line
	if hint != "" {
		b.WriteString("\r\n  " + hint)
		extra++
	}
	// autocomplete dropdown — real renders the menu inside the box
	w := compW
	if w <= 0 {
		w = 80
	}
	for _, row := range e.acRows(w) {
		b.WriteString("\r\n" + row)
		extra++
	}
	// bottom rule + footer always render (the box stays complete while
	// typing — real keeps the full chrome live during composition)
	b.WriteString("\r\n" + boxRule())
	extra++
	if f := e.footerText(); f != "" {
		b.WriteString("\r\n" + f)
		extra++
	}
	// reposition cursor to e.pos
	before := string(e.buf[:e.pos])
	totalNL := strings.Count(string(e.buf), "\n")
	nlBefore := strings.Count(before, "\n")
	col := len(before) - strings.LastIndex(before, "\n") - 1
	if nlBefore == 0 {
		col += len(e.prompt) + 1 // +1 for left border
	}
	if up := totalNL - nlBefore + extra; up > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", up)
	}
	b.WriteString("\r")
	if col > 0 {
		fmt.Fprintf(&b, "\x1b[%dC", col)
	}
	os.Stdout.WriteString(b.String())
	compRepaint() // \x1b[J erased to screen bottom — restore the pinned zone
}

func (e *editor) insert(rs ...rune) {
	e.yankPos = -1
	e.escArmAt = time.Time{} // typing disarms "Esc again to clear"
	e.flushCleared()         // typing after a clear commits the held draft to history
	for _, r := range rs {
		e.buf = append(e.buf, 0)
		copy(e.buf[e.pos+1:], e.buf[e.pos:])
		e.buf[e.pos] = r
		e.pos++
	}
	e.acRefresh()
	e.redraw()
}

func (e *editor) del(back bool) {
	e.yankPos = -1
	e.escArmAt = time.Time{}
	if back {
		if e.pos > 0 {
			e.buf = append(e.buf[:e.pos-1], e.buf[e.pos:]...)
			e.pos--
		}
	} else if e.pos < len(e.buf) {
		e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
	}
	e.acRefresh()
	e.redraw()
}

// submitLine returns the final text with paste placeholders expanded.
func (e *editor) submitLine() string {
	e.flushCleared() // a held cleared draft precedes this line in history
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

// expandPaste replaces a big paste with a CC-style placeholder — real
// wraps when len > 800 OR newlines > min(rows-10,2) (~2); "+N lines" is
// the NEWLINE count and is omitted for a single-line paste (mod_0304 zee).
func (e *editor) expandPaste(pasted string) string {
	pasted = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(pasted)
	pasted = strings.ReplaceAll(pasted, "\t", "    ")
	nl := strings.Count(pasted, "\n")
	if len(pasted) <= 800 && nl <= 2 {
		return pasted
	}
	e.pasteN++
	ph := fmt.Sprintf("[Pasted text #%d]", e.pasteN)
	if nl > 0 {
		ph = fmt.Sprintf("[Pasted text #%d +%d lines]", e.pasteN, nl)
	}
	// the content the model actually receives, wrapped like CC does
	e.pastes[ph] = fmt.Sprintf("<pasted_content id=\"%d\">\n%s\n</pasted_content>", e.pasteN, pasted)
	return ph
}

type readResult struct {
	line        string
	ok          bool // got a line
	intr        bool // ctrl-C pressed with empty buffer (double-tap = exit)
	quit        bool // double ctrl-C / ctrl-D -> exit repl
	rewind      bool // esc-esc: open the rewind/message selector
	sendNow     bool // ctrl+enter / ctrl+x ctrl+s — deliver even while busy
	queued      bool // ctrl+x enter — force queue
	modelPicker bool // meta+p — open the /model picker
	agentsPane  bool // ← ← on empty input — open the agents view
}

// readLine consumes keys until submit. Vim-normal mode supported.
func (e *editor) readLine(ks *keySource) readResult {
	if len(e.deferredActions) > 0 {
		// a bound command pressed while the turn ran — fire it now
		act := e.deferredActions[0]
		e.deferredActions = e.deferredActions[1:]
		if h, ok := kbActions[act]; ok {
			if r := h(e); r != nil {
				return *r
			}
		}
	}
	if e.zoneEdit = compInputActive(); e.zoneEdit {
		// Ink model: the box lives in the compositor live zone. On return
		// (any readResult) the zone shrinks back to spinner+status and the
		// box is echoed into the scroll region.
		e.redraw()
		defer func() {
			e.zoneEdit = false
			compExitEdit(e.boxAll)
		}()
	} else {
		if eff := effortLabel(); eff != "" {
			w := compW
			if w <= 0 {
				w = 80
			}
			os.Stdout.WriteString("\r\x1b[K" + strings.Repeat(" ", max(0, w-visWidth(eff))) + eff + "\n")
		}
		os.Stdout.WriteString(boxRule() + "\n" + e.prompt + "\n" + boxRule())
		if f := e.footerText(); f != "" {
			os.Stdout.WriteString("\n" + f)
		}
		// cursor back to the input row
		back := 1
		if e.footer != "" {
			back = 2
		}
		os.Stdout.WriteString(fmt.Sprintf("\x1b[%dA\r\x1b[%dC", back, visWidth(e.prompt)))
	}
	for {
		var k key
		var ev *voiceEvent
		var ve voiceEvent
		select {
		case kk := <-ks.replay: // post-turn drained keys replay first
			k = kk
		case kk, ok := <-ks.ch:
			if !ok {
				return readResult{quit: true}
			}
			k = kk
		case v2 := <-e.voiceChan():
			ve = v2
			ev = &ve
		}
		if ev != nil {
			// voice controller event (interim/final/error/state) — may
			// submit the line on autoSubmit
			if r := e.voiceApply(*ev); r != nil {
				return *r
			}
			continue
		}
		if suspendSupported && isSuspendKey(k) {
			// ctrl+z — global like real: suspends from insert, vim, and
			// search modes alike
			suspendToShell(ks, e.redraw)
			continue
		}
		if e.vim && e.mode != "insert" {
			if r := e.vimKey(k); r != nil {
				return *r
			}
			continue
		}
		if r := e.insertKey(k); r != nil {
			return *r
		}
	}
}

func (e *editor) insertKey(k key) *readResult {
	c := k.b
	wasInsert := e.insGroup
	e.insGroup = false // any non-insert key closes the previous group
	e.wasKill = e.killLast
	e.killLast = false // any non-kill key breaks the kill chain
	if e.exitArm && k.b != 4 && k.b != 3 {
		e.exitArm = false // an unrelated key press disarms the exit gate
		e.exitArmKey = ""
	}
	if os.Getenv("CG_DBGKEY") != "" {
		fmt.Fprintf(os.Stderr, "[dbgkey b=%d seq=%q] ", c, k.seq)
	}
	if k.mod&modCtrl != 0 && k.r >= 'a' && k.r <= 'z' {
		c = byte(k.r) - 'a' + 1 // CSI-u ctrl+letter -> control byte
	}
	if e.histSearch {
		return e.searchKey(k)
	}
	// voice session takes esc/ctrl+c and its bound push-to-talk key first
	// (WMe handler in mod_0747 runs before composer keys)
	if e.voice != nil && e.voice.ctl != nil {
		st := e.voice.ctl.CurrentState()
		if st != voice.StateIdle {
			// escape or ctrl+c (no shift/meta) — real's WMe handler
			if (k.b == 0x1b && k.seq == "" && k.mod == 0) ||
				(c == 3 && k.mod&(modShift|modAlt|modCmd) == 0) {
				e.voiceCancel()
				return nil
			}
		}
	}
	// voice:pushToTalk bound to a printable char (default: space) — real
	// fires only with input empty (tap) or while a session is live
	if k.b == ' ' && k.mod == 0 && k.seq == "" &&
		e.kb["space"] == "voice:pushToTalk" {
		st := voice.StateIdle
		if e.voice != nil && e.voice.ctl != nil {
			st = e.voice.ctl.CurrentState()
		}
		switch {
		case st == voice.StateRecording:
			e.voiceToggle() // tap again → finish
			return nil
		case st == voice.StateIdle && len(e.buf) == 0 && e.voiceEnabled():
			e.voiceToggle()
			return nil
		}
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
		// real's mt(): cursor up one wrapped row first; history only
		// when already on the draft's top row
		if !e.cursorUp() {
			e.histPrev()
		}
	case k.seq == "[B":
		if !e.cursorDown() {
			e.histNext()
		}
	case c == 16: // ctrl+p — same as Up (real's mt + w at top row)
		if !e.cursorUp() {
			e.histPrev()
		}
	case c == 14: // ctrl+n — same as Down (real's ht + v at bottom row)
		if !e.cursorDown() {
			e.histNext()
		}
	case c == 2: // ctrl+b — char left
		if e.pos > 0 {
			e.pos--
			e.redraw()
		}
	case c == 6: // ctrl+f — char right
		if e.pos < len(e.buf) {
			e.pos++
			e.redraw()
		}
	case k.seq == "[C":
		if k.mod&modCmd != 0 { // super+right -> logical line end (real)
			e.pos = e.lineEnd()
			e.redraw()
		} else if e.pos < len(e.buf) {
			e.pos++
			e.redraw()
		}
	case k.seq == "[D":
		if len(e.buf) == 0 && e.mode == "insert" {
			// real (captured): ← opens the agents view/badge, ← again
			// enters prompt history ("History N/100")
			if e.leftArm {
				e.leftArm = false
				if len(e.history) > 0 {
					e.histPrev()
					return nil
				}
				return &readResult{agentsPane: true}
			}
			e.leftArm = true
			time.AfterFunc(1500*time.Millisecond, func() { e.leftArm = false })
			if e.runningAgents != nil && e.runningAgents() > 0 {
				return &readResult{agentsPane: true}
			}
			if len(e.history) > 0 {
				e.histPrev()
			} else {
				os.Stdout.WriteString("\r\x1b[K  " + dim + "Press ← again to open agents" + reset)
			}
			return nil
		}
		if k.mod&modCmd != 0 { // super+left -> logical line start (real)
			e.pos = e.lineStart()
			e.redraw()
		} else if e.pos > 0 {
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
		if k.mod&(modAlt|modCmd) != 0 {
			e.killToLineEnd() // meta|super+delete -> kill-to-end (real)
		} else {
			e.del(false)
		}
	case c == 3: // ctrl-C
		return e.interruptKey()
	case c == 4: // ctrl-D
		return e.exitKey()
	case (c == '\r' || c == '\n') && k.r != '\n' && k.mod == 0: // submit
		// menu open + input is still a strict prefix → Enter accepts the
		// completion; an exact match already typed submits (real behavior)
		if e.acOpen && len(e.acItems) > 0 &&
			string(e.buf) != "/"+e.acItems[e.acSel] &&
			!(e.acAt != "" && e.acAt == "@"+e.acItems[e.acSel]) {
			e.acAccept()
			e.redraw()
			return nil
		}
		return e.submitKey()
	case c == 0x7f && k.mod&(modAlt|modCmd|modCtrl) != 0:
		e.backwardKillWord() // meta|super|ctrl+backspace (real's Vt table)
	case c == 0x7f || c == 8:
		e.del(true)
	case k.mod&modAlt != 0 && k.r == 'b':
		e.pos = e.wordBack()
		e.redraw()
	case k.mod&modAlt != 0 && k.r == 'f':
		e.pos = e.wordFwd()
		e.redraw()
	case k.mod&modAlt != 0 && k.r == 'd':
		e.killWordFwd()
	case k.mod&modAlt != 0 && k.r == 'y':
		e.yankPop()
	case c == 1:
		e.pos = e.lineStart()
		e.redraw()
	case c == 5:
		e.pos = e.lineEnd()
		e.redraw()
	case c == 11:
		e.killToLineEnd() // real: kill to line end -> kill-ring
	case c == 21:
		e.killToLineStart() // real: kill to line start -> kill-ring
	case c == 23:
		e.backwardKillWord() // deleteWORDBefore -> kill-ring prepend
	case c == 25:
		e.yankKill() // editor-layer ctrl+y (real's Kt["y"]) — kill-ring yank
	case k.seq == "[A" && e.acOpen:
		if e.acSel > 0 {
			e.acSel--
		}
		e.redraw()
	case k.seq == "[B" && e.acOpen:
		if e.acSel < len(e.acItems)-1 {
			e.acSel++
		}
		e.redraw()
	case c == 0x1b && k.seq == "" && k.mod == 0:
		if e.acOpen {
			e.acOpen = false
			e.acItems = nil
			e.redraw()
			return nil
		}
		return e.cancelKey()
	case c == '\t':
		if e.acOpen {
			e.acAccept()
			e.redraw()
			return nil
		}
		e.complete()
	case k.seq == "[Z": // shift+tab: cycle permission mode
		if e.cycleMode != nil {
			e.cycleMode()
		}
	case (k.r == '?' || c == '?') && len(e.buf) == 0 && k.seq == "":
		e.showShortcuts() // real: '?' on empty input opens the shortcuts overlay
		return nil
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
		if !wasInsert {
			e.pushUndo()
		}
		e.insGroup = true
		if e.recordingIns {
			e.recIns = append(e.recIns, k.r)
		}
		e.insert(k.r)
	case c >= 32 && c < 127 && k.mod&^modShift == 0:
		if !wasInsert {
			e.pushUndo()
		}
		e.insGroup = true
		if e.recordingIns {
			e.recIns = append(e.recIns, rune(c))
		}
		e.insert(rune(c))
	}
	return nil
}

// vimKey handles normal-mode keys — full state machine: counts, operators
// (d/c/y/>/<) × motions, linewise ops, f/F/t/T finds, registers, undo,
// dot-repeat.
func (e *editor) vimKey(k key) *readResult {
	if len(e.buf) == 0 {
		e.pos = 0
	} else if e.pos < 0 {
		e.pos = 0
	} else if e.pos >= len(e.buf) {
		e.pos = len(e.buf) - 1
	}
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
		e.pos = e.lineAbove()
		e.redraw()
		return nil
	case "[B":
		e.pos = e.lineBelow()
		e.redraw()
		return nil
	}
	if c == '\r' {
		e.closeBox()
		return &readResult{line: e.submitLine(), ok: true}
	}
	// Chat/Global bindings stay live inside vim modes (real's keymap is
	// active under the vim state machine — ctrl+c interrupt, ctrl+d exit,
	// meta+p picker…). Plain letters and 'space' are vim commands, never
	// bindings.
	if cs := keystroke(k); cs != "" && cs != "space" {
		for _, ctx := range [2]string{"Chat", "Global"} {
			if r, handled := e.dispatchKb(k, ctx, cs); handled {
				return r
			}
		}
	}
	if e.mode == "visual" || e.mode == "vline" {
		return e.visualKey(k)
	}
	// every normal-mode key extends the candidate change sequence
	e.pendingKeys = append(e.pendingKeys, k)
	// r<char> pending — consume the replacement char
	if e.vimReplace {
		e.vimReplace = false
		if c >= 32 && c != 0x1b {
			e.pushUndo()
			for i := 0; i < e.repCnt && e.pos+i < len(e.buf); i++ {
				if e.buf[e.pos+i] != '\n' {
					e.buf[e.pos+i] = rune(c)
				}
			}
			e.noteChange()
			e.redraw()
		} else {
			e.pendingKeys = nil
		}
		return nil
	}
	// i/a text-object pending under an operator (diw, ci", ya(…)
	if e.vimObj != 0 {
		kind := e.vimObj
		e.vimObj = 0
		if s2, en2, ok := e.textObjRange(kind, rune(c)); ok {
			op := e.vimOp
			e.vimOp = 0
			e.applyOpRange(op, s2, en2, false)
		} else {
			e.vimOp = 0
			e.pendingKeys = nil
		}
		e.vimCount = 0
		return nil
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
				if e.vimOp != 0 {
					op := e.vimOp
					e.vimOp = 0
					e.applyOp(op, np)
				} else {
					e.pos = np
					e.pendingKeys = nil
					e.redraw()
				}
			} else {
				e.pendingKeys = nil
			}
		} else {
			e.vimOp = 0
			e.pendingKeys = nil
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
		case 'i', 'a':
			e.vimOp = op
			e.vimObj = byte(c)
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
			e.pendingKeys = nil // operator cancelled
		default:
			e.pendingKeys = nil
		}
		e.vimCount = 0
		return nil
	}
	// g-pending (gg, gj, gk motions)
	if e.vimPendingG != 0 {
		op := e.vimPendingG
		e.vimPendingG = 0
		e.pendingKeys = nil // motions are neutral for '.'
		switch {
		case c == 'j' && op == 'm':
			e.cursorDown() // gj — display-row down (real)
		case c == 'k' && op == 'm':
			e.cursorUp() // gk — display-row up (real)
		case c == 'g':
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
	changeIns := func() { // change that enters INSERT — record ins text for '.'
		e.noteChange()
		e.startInsRec()
	}
	switch c {
	case 'i':
		changeIns()
		toInsert()
	case 'a':
		e.pos = min(len(e.buf), e.pos+1)
		changeIns()
		toInsert()
	case 'A':
		e.pos = e.lineEnd()
		changeIns()
		toInsert()
	case 'I':
		e.pos = e.lineStart()
		changeIns()
		toInsert()
	case 'o':
		e.pos = e.lineEnd()
		changeIns()
		e.pushUndo()
		e.insert('\n')
		toInsert()
	case 'O':
		e.pos = e.lineStart()
		changeIns()
		e.pushUndo()
		e.insert('\n')
		e.pos--
		toInsert()
	case 'h', 0x7f:
		e.pendingKeys = nil
		e.pos = max(0, e.pos-cnt)
		e.redraw()
	case 'l', ' ':
		e.pendingKeys = nil
		if len(e.buf) > 0 {
			e.pos = min(len(e.buf)-1, e.pos+cnt)
		}
		e.redraw()
	case 'j':
		e.pendingKeys = nil
		for i := 0; i < cnt; i++ {
			e.pos = e.lineBelow()
		}
		e.redraw()
	case 'k':
		e.pendingKeys = nil
		for i := 0; i < cnt; i++ {
			e.pos = e.lineAbove()
		}
		e.redraw()
	case 'w', 'W':
		e.pendingKeys = nil
		for i := 0; i < cnt; i++ {
			e.pos = e.wordFwd()
		}
		e.redraw()
	case 'b', 'B':
		e.pendingKeys = nil
		for i := 0; i < cnt; i++ {
			e.pos = e.wordBack()
		}
		e.redraw()
	case 'e', 'E':
		e.pendingKeys = nil
		for i := 0; i < cnt; i++ {
			e.pos = e.wordEnd()
		}
		e.redraw()
	case '0':
		e.pendingKeys = nil
		e.pos = e.lineStart()
		e.redraw()
	case '^':
		e.pendingKeys = nil
		p := e.lineStart()
		for p < len(e.buf) && (e.buf[p] == ' ' || e.buf[p] == '\t') {
			p++
		}
		e.pos = p
		e.redraw()
	case '$':
		e.pendingKeys = nil
		e.pos = max(e.lineStart(), e.lineEnd()-1)
		e.redraw()
	case 'G':
		e.pendingKeys = nil
		if e.vimCountRaw > 0 {
			e.pos = e.lineStartOf(e.vimCountRaw - 1)
		} else {
			e.pos = e.lineStartOf(e.lineCount() - 1)
		}
		e.redraw()
	case 'g':
		e.vimPendingG = 'm' // gg/gj/gk marker — resolution clears pending
	case 'f', 'F', 't', 'T':
		e.vimFind = rune(c)
	case ';':
		e.pendingKeys = nil
		e.pos = e.findChar(e.lastFindCh, e.lastFindDir)
		e.redraw()
	case ',':
		e.pendingKeys = nil
		e.pos = e.findChar(e.lastFindCh, flipDir(e.lastFindDir))
		e.redraw()
	case 'x':
		e.pushUndo()
		for i := 0; i < cnt && e.pos < len(e.buf); i++ {
			e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
		}
		e.recordReg(e.lastDel, 0)
		e.noteChange()
		e.redraw()
	case 'X':
		e.pushUndo()
		for i := 0; i < cnt && e.pos > 0; i++ {
			e.buf = append(e.buf[:e.pos-1], e.buf[e.pos:]...)
			e.pos--
		}
		e.noteChange()
		e.redraw()
	case 'd', 'c', 'y', '>', '<':
		e.vimOp = rune(c)
		if cnt > 0 {
			e.vimCount = cnt
		}
	case 'D':
		e.pushUndo()
		e.lastDel = string(e.buf[e.pos:e.lineEnd()])
		e.buf = e.buf[:e.pos]
		e.noteChange()
		e.redraw()
	case 'C':
		e.pushUndo()
		e.lastDel = string(e.buf[e.pos:e.lineEnd()])
		e.buf = e.buf[:e.pos]
		changeIns()
		toInsert()
	case 'Y':
		e.pendingKeys = nil // yank isn't a '.'-change
		s, en := e.linelRange('j', 0)
		e.lastDel = string(e.buf[s:en])
		e.redraw()
	case 's':
		e.pushUndo()
		for i := 0; i < cnt && e.pos < len(e.buf); i++ {
			e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
		}
		changeIns()
		toInsert()
	case 'S':
		e.pushUndo()
		s, en := e.linelRange('j', 0)
		e.lastDel = string(e.buf[s:en])
		e.buf = append(e.buf[:s], e.buf[en:]...)
		e.pos = s
		changeIns()
		toInsert()
	case 'r':
		e.vimReplace = true
		e.repCnt = cnt
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
			e.pos = at
			e.noteChange()
			e.redraw()
		}
	case 'P':
		if e.lastDel != "" {
			e.pushUndo()
			at := e.pos
			if strings.Contains(e.lastDel, "\n") {
				at = e.lineStart()
			}
			for i := 0; i < cnt; i++ {
				e.buf = append(e.buf[:at], append([]rune(e.lastDel), e.buf[at:]...)...)
			}
			e.pos = at
			e.noteChange()
			e.redraw()
		}
	case 'u':
		e.pendingKeys = nil
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
			e.noteChange()
			e.redraw()
		}
	case 'J':
		e.pushUndo()
		le := e.lineEnd()
		if le < len(e.buf) {
			e.buf[le] = ' '
			e.buf = append(e.buf[:le+1], append([]rune{}, e.buf[le+1:]...)...)
		}
		e.noteChange()
		e.redraw()
	case 'v':
		e.vimVisual = e.pos // visual anchor
		e.mode = "visual"
		e.redraw()
	case 'V':
		e.vimVisual = e.pos
		e.mode = "vline" // VISUAL-LINE (real)
		e.redraw()
	case '?':
		e.pendingKeys = nil
		// help hint — real shows keybindings; noop visually
	case '/':
		e.pendingKeys = nil
		e.histSearch = true
		e.searchBuf = e.searchBuf[:0]
		e.redrawSearch()
	case 0x1b:
		e.pendingKeys = nil
		// already normal
	default:
		e.pendingKeys = nil
	}
	return nil
}

// noteChange snapshots the accumulated keystroke sequence as the last
// recorded change (real's recordChange — '.' replays it).
func (e *editor) noteChange() {
	e.changeKeys = append([]key(nil), e.pendingKeys...)
	e.pendingKeys = nil
}

// startInsRec begins recording insert-mode text for '.'-replay (real's
// recordedChange captures op keystrokes + the typed text).
func (e *editor) startInsRec() {
	e.recordingIns = true
	e.recIns = e.recIns[:0]
}

// textObjRange resolves an i/a text object to a buffer range (real's Zn).
func (e *editor) textObjRange(kind byte, obj rune) (int, int, bool) {
	inner := kind == 'i'
	switch obj {
	case 'w', 'W':
		s, en := e.pos, e.pos
		for s > 0 && !isWordSep(e.buf[s-1]) {
			s--
		}
		for en < len(e.buf) && !isWordSep(e.buf[en]) {
			en++
		}
		if !inner {
			if en < len(e.buf) {
				for en < len(e.buf) && isWordSep(e.buf[en]) {
					en++
				}
			} else {
				for s > 0 && isWordSep(e.buf[s-1]) {
					s--
				}
			}
		}
		if s == en {
			return 0, 0, false
		}
		return s, en, true
	case '"', '\'', '`':
		q := obj
		st, en := -1, -1
		for i := e.pos - 1; i >= 0 && e.buf[i] != '\n'; i-- {
			if e.buf[i] == q {
				st = i
				break
			}
		}
		for i := e.pos; i < len(e.buf) && e.buf[i] != '\n'; i++ {
			if e.buf[i] == q {
				en = i
				break
			}
		}
		if st < 0 || en <= st {
			return 0, 0, false
		}
		if inner {
			return st + 1, en, true
		}
		return st, en + 1, true
	case '(', ')', 'b':
		return e.pairRange('(', ')', inner)
	case '[', ']':
		return e.pairRange('[', ']', inner)
	case '{', '}', 'B':
		return e.pairRange('{', '}', inner)
	case '<', '>':
		return e.pairRange('<', '>', inner)
	case 'p': // paragraph: blank-line-delimited block
		st := e.lineStart()
		for st > 0 {
			// find start of the previous logical line
			ps := st - 1
			for ps > 0 && e.buf[ps-1] != '\n' {
				ps--
			}
			if ps == st-1 { // previous line is blank — block starts at st
				break
			}
			st = ps
		}
		en := e.lineEnd()
		for en < len(e.buf) {
			ns := en + 1
			ne := ns
			for ne < len(e.buf) && e.buf[ne] != '\n' {
				ne++
			}
			if ne == ns { // next line is blank — block ends here
				break
			}
			en = ne
		}
		if !inner && en < len(e.buf) {
			en++ // around: include the trailing newline
		}
		return st, en, true
	}
	return 0, 0, false
}

// pairRange finds the innermost balanced open/close pair around pos.
func (e *editor) pairRange(open, close rune, inner bool) (int, int, bool) {
	depth, st := 0, -1
	for i := e.pos; i >= 0; i-- {
		if e.buf[i] == close {
			depth++
		}
		if e.buf[i] == open {
			if depth == 0 {
				st = i
				break
			}
			depth--
		}
	}
	if st < 0 {
		return 0, 0, false
	}
	depth, en := 0, -1
	for i := st + 1; i < len(e.buf); i++ {
		if e.buf[i] == open {
			depth++
		}
		if e.buf[i] == close {
			if depth == 0 {
				en = i
				break
			}
			depth--
		}
	}
	if en < 0 {
		return 0, 0, false
	}
	if inner {
		return st + 1, en, true
	}
	return st, en + 1, true
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

// closeBox draws the box's bottom border inline — legacy path only. In
// zone mode the readLine defer echoes the whole box into the scroll
// region when the zone collapses.
func (e *editor) closeBox() {
	if !e.zoneEdit {
		os.Stdout.WriteString("\n" + boxRule() + "\n")
	}
}

// acRefresh updates the autocomplete dropdown — "/" filters commands,
// "@<prefix>" lists file paths (real's file_suggestions at-mention).
func (e *editor) acRefresh() {
	e.acOpen, e.acItems, e.acAt = false, nil, ""
	s := string(e.buf)
	switch {
	case e.completer != nil && len(s) > 0 && s[0] == '/' && !strings.ContainsRune(s, ' '):
		e.acItems = e.completer(s[1:])
	case len(s) > 0:
		// @-mention: token under the cursor starts with @
		before := s[:byteIdxOfRunePos(s, e.pos)]
		at := strings.LastIndex(before, "@")
		if at >= 0 && (at == 0 || before[at-1] == ' ') && !strings.ContainsAny(before[at:], " \t") {
			e.acAt = before[at:]
			e.acItems = fileSuggestions(e.voiceCWD, before[at+1:])
		}
	}
	e.acOpen = len(e.acItems) > 0
	if e.acSel >= len(e.acItems) {
		e.acSel = 0
	}
}

func byteIdxOfRunePos(s string, pos int) int {
	i := 0
	for n := 0; n < pos && i < len(s); n++ {
		_, sz := utf8.DecodeRuneInString(s[i:])
		i += sz
	}
	return i
}

// fileSuggestions fuzzy-prefix-matches cwd files — real caps the menu.
func fileSuggestions(cwd, prefix string) []string {
	var out []string
	filepath.WalkDir(cwd, func(p string, d os.DirEntry, err error) error {
		if err != nil || len(out) >= 40 {
			return err
		}
		rel, err := filepath.Rel(cwd, p)
		if err != nil || rel == "." {
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(rel, ".git") || rel == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(rel, prefix) || strings.HasPrefix(filepath.Base(rel), prefix) {
			out = append(out, rel)
		}
		return nil
	})
	return out
}

// acAccept commits the selected completion into the buffer.
func (e *editor) acAccept() {
	if !e.acOpen || len(e.acItems) == 0 {
		return
	}
	if e.acAt != "" {
		// replace the partial @token with the completed path
		s := string(e.buf)
		before := s[:byteIdxOfRunePos(s, e.pos)]
		at := strings.LastIndex(before, "@")
		rest := s[byteIdxOfRunePos(s, e.pos):]
		e.buf = []rune(s[:at] + "@" + e.acItems[e.acSel] + " " + rest)
		e.pos = len([]rune(s[:at] + "@" + e.acItems[e.acSel] + " "))
	} else {
		e.buf = []rune("/" + e.acItems[e.acSel] + " ")
		e.pos = len(e.buf)
	}
	e.acOpen = false
	e.acItems = nil
	e.acAt = ""
}

// acRows renders the dropdown — real draws it between the input line and
// the bottom rule, highlighted row in suggestion color.
func (e *editor) acRows(w int) []string {
	if !e.acOpen || len(e.acItems) == 0 {
		return nil
	}
	show := e.acItems
	sel := e.acSel
	if len(show) > 8 {
		// window around the selection
		start := sel - 4
		if start < 0 {
			start = 0
		}
		if start+8 > len(show) {
			start = len(show) - 8
		}
		show = show[start : start+8]
		sel -= start
	}
	rows := make([]string, 0, len(show))
	for i, m := range show {
		desc := ""
		if e.acAt == "" && e.acDesc != nil {
			desc = " · " + dim + e.acDesc(m) + reset
		}
		disp := "/" + m
		if e.acAt != "" {
			disp = "@" + m
		}
		line := "  " + disp + desc
		if i == sel {
			line = "\x1b[38;5;153m❯ " + disp + "\x1b[39m" + desc
		}
		rows = append(rows, compClamp(line, w))
	}
	return rows
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
			r := []rune(lcp)
			lcp = string(r[:len(r)-1])
		}
	}
	if len(lcp) > len(prefix)-1 {
		e.buf = []rune("/" + lcp)
		e.pos = len(e.buf)
	}
	var out strings.Builder
	out.WriteString("\r\n")
	for _, m := range matches {
		fmt.Fprintf(&out, "  /%s\r\n", m)
	}
	compInline(out.String()) // into the scroll region above the zone box
	e.redraw()
}

func (e *editor) histPrev() {
	// real's Up handler: a held cleared draft restores before history
	// pages — "a cleared prompt stays reachable with Up" (2.1.x changelog)
	if e.restoreCleared() {
		return
	}
	e.flushCleared()
	if len(e.history) == 0 {
		return
	}
	e.histSearch = true // badge state — History i/total while browsing
	if e.hIdx == -1 {
		e.saved = append([]rune(nil), e.buf...)
		e.hIdx = len(e.history) - 1
	} else if e.hIdx > 0 {
		e.hIdx--
	}
	e.buf = []rune(e.history[e.hIdx])
	e.pos = len(e.buf)
	e.redraw()
	// real shows a dim "History i/total" badge above the composer
	if e.histSearch {
		compInline(fmt.Sprintf("\r\x1b[K  %sHistory %d/%d%s\r\n", dim, e.hIdx+1, len(e.history), reset))
	}
}

func (e *editor) histNext() {
	e.flushCleared()
	if e.hIdx == -1 {
		return
	}
	if e.hIdx < len(e.history)-1 {
		e.hIdx++
		e.buf = []rune(e.history[e.hIdx])
	} else {
		e.hIdx = -1
		e.buf = append([]rune(nil), e.saved...)
		e.histSearch = false
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

// exitArmWindow is real's jz double-press timeout (800ms).
const exitArmWindow = 800 * time.Millisecond

// armExit is the shared ctrl+c/ctrl+d exit gate (real's Mt/jz + ut): the
// first key press arms "Press <key> again to exit" — the name sticks to
// whichever key armed it — and any second press inside the window exits.
func (e *editor) armExit(name string) *readResult {
	if e.exitArm && time.Since(e.exitArmAt) >= exitArmWindow {
		e.exitArm = false // stale arm — next press re-arms
		e.exitArmKey = ""
	}
	if e.exitArm {
		e.exitArm = false
		e.closeBox()
		return &readResult{quit: true}
	}
	if e.exitArmKey == "" {
		e.exitArmKey = name
	}
	e.exitArm = true
	e.exitArmAt = time.Now()
	compInline("\r\x1b[K  " + dim + "Press " + e.exitArmKey + " again to exit" + reset)
	e.redraw()
	return nil
}

// interruptKey is ctrl-C / app:interrupt: holds the cleared draft (it
// re-enters history / Up restores it — real's holdCleared), clears, and
// arms the double-press exit gate.
func (e *editor) interruptKey() *readResult {
	e.holdCleared(true)
	return e.armExit("Ctrl-C")
}

// exitKey is ctrl-D / app:exit: non-empty buffer -> delete forward (real
// `It()`); empty buffer -> arm "Press Ctrl-D again to exit", 2nd exits.
func (e *editor) exitKey() *readResult {
	if len(e.buf) > 0 {
		e.del(false)
		return nil
	}
	return e.armExit("Ctrl-D")
}

// cancelKey is esc / chat:cancel: vim insert->normal; on a non-empty draft
// the first esc shows "Esc again to clear" and the second (within 800ms)
// saves the draft to history then clears (real's Wt jz gate); on empty
// input esc-esc opens the rewind/message selector (real's AA jz).
func (e *editor) cancelKey() *readResult {
	if e.vim && e.mode == "insert" {
		e.mode = "normal"
		if e.pos > 0 {
			e.pos--
		}
		if e.recordingIns {
			e.recordingIns = false
			e.lastInsText = append([]rune(nil), e.recIns...)
		}
		e.redraw()
		return nil
	}
	if len(e.buf) == 0 {
		if e.lastEsc > 0 && time.Since(time.Unix(0, e.lastEsc)) < exitArmWindow {
			e.lastEsc = 0
			return &readResult{rewind: true}
		}
		e.lastEsc = time.Now().UnixNano()
		return nil
	}
	if !e.escArmAt.IsZero() && time.Since(e.escArmAt) < exitArmWindow {
		e.escArmAt = time.Time{}
		// real: the esc-esc clear saves the draft to history directly
		l := string(e.buf)
		if strings.TrimSpace(l) != "" {
			e.pushUndo()
			e.history = append(e.history, l)
			appendHistoryFile(l)
		}
		e.buf = e.buf[:0]
		e.pos = 0
		e.redraw()
		return nil
	}
	e.escArmAt = time.Now()
	compInline("\r\x1b[K  " + dim + "Esc again to clear" + reset)
	e.redraw()
	return nil
}

// holdCleared stashes the current draft as "cleared" — real's holdCleared:
// Up restores it (restoreCleared); if the user moves on without restoring,
// it enters prompt history (flushCleared).
func (e *editor) holdCleared(intoHistory bool) {
	if strings.TrimSpace(string(e.buf)) == "" {
		return
	}
	e.flushCleared() // an earlier unrestored draft commits to history now
	e.pushUndo()
	e.cleared = append([]rune(nil), e.buf...)
	e.clearedHist = intoHistory
	e.buf = e.buf[:0]
	e.pos = 0
}

// restoreCleared pops a held cleared draft back into the buffer — real's
// Up handler calls this before paging history.
func (e *editor) restoreCleared() bool {
	if e.cleared == nil || len(e.buf) > 0 {
		return false
	}
	e.buf = e.cleared
	e.pos = len(e.buf)
	e.cleared = nil
	e.redraw()
	return true
}

// flushCleared commits a held cleared draft to history (real's #v — runs
// when the draft was never restored before the next edit/history event).
func (e *editor) flushCleared() {
	if e.cleared == nil {
		return
	}
	if e.clearedHist {
		l := string(e.cleared)
		if strings.TrimSpace(l) != "" {
			e.history = append(e.history, l)
			appendHistoryFile(l)
		}
	}
	e.cleared = nil
}

// submitKey is enter / chat:submit (with \-continuation -> newline).
func (e *editor) submitKey() *readResult {
	if len(e.buf) > 0 && e.buf[len(e.buf)-1] == '\\' {
		e.buf = e.buf[:len(e.buf)-1]
		if e.pos > len(e.buf) {
			// the stripped backslash was under the cursor — keep pos in
			// bounds or insert() slices out of range (crash on \+enter)
			e.pos = len(e.buf)
		}
		e.insert('\n')
		return nil
	}
	e.closeBox()
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
	e.searchIdx = -1
	if e.hIdx == -1 {
		e.saved = append([]rune(nil), e.buf...) // ctrl+c restores this draft
	}
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
	b, err := os.ReadFile(filepath.Join(config.ClaudeDir(), "history.jsonl"))
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
	compSuspend() // the spawned editor owns the whole screen
	defer compResume()
	os.Stdout.WriteString("\r\x1b[2K  ($EDITOR: " + ed + ")\r\n")
	if e.keys != nil {
		e.keys.release() // cooked mode + kitty flags off for the editor
	}
	cmd := exec.Command("sh", "-c", ed+" "+path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
	if e.keys != nil {
		e.keys.grab()
	}
	if b, err := os.ReadFile(path); err == nil {
		e.buf = []rune(strings.TrimRight(string(b), "\n"))
		e.pos = len(e.buf)
	}
	e.redraw()
	return nil
}

// searchKey handles ctrl+r history search — real's HistorySearch context:
// enter executes the match, esc/tab accept it into the buffer, ctrl+c
// cancels back to the pre-search draft, ctrl+r cycles to the next older
// match, ctrl+s cycles scope.
func (e *editor) searchKey(k key) *readResult {
	c := k.b
	accept := func() {
		e.histSearch = false
		if m := e.searchMatch(); m != "" {
			e.buf = []rune(m)
			e.pos = len(e.buf)
		}
		e.searchBuf = e.searchBuf[:0]
		if !e.zoneEdit {
			os.Stdout.WriteString("\r\x1b[2K" + e.prompt)
		}
		e.redraw()
	}
	switch {
	case c == 3: // ctrl+c — historySearch:cancel (restore pre-search draft)
		e.histSearch = false
		e.buf = append([]rune(nil), e.saved...)
		e.pos = len(e.buf)
		e.searchBuf = e.searchBuf[:0]
		if !e.zoneEdit {
			os.Stdout.WriteString("\r\x1b[2K" + e.prompt)
		}
		e.redraw()
	case (c == 0x1b && k.seq == "") || c == '\t': // esc/tab — historySearch:accept
		accept()
	case c == '\r': // enter — historySearch:execute (accept + submit)
		e.histSearch = false
		if m := e.searchMatch(); m != "" {
			e.buf = []rune(m)
			e.pos = len(e.buf)
		}
		e.searchBuf = e.searchBuf[:0]
		return e.submitKey()
	case c == 0x7f || c == 8:
		if len(e.searchBuf) > 0 {
			e.searchBuf = e.searchBuf[:len(e.searchBuf)-1]
			e.searchIdx = -1 // re-match from newest
		}
		e.redrawSearch()
	case c == 18: // ctrl+r — historySearch:next (cycle to next older match)
		e.searchNext()
	case c == 19: // ctrl+s — historySearch:cycleScope (single scope: inert)
	case c >= 32 && c < 127:
		e.searchBuf = append(e.searchBuf, rune(c))
		e.searchIdx = -1 // query changed — re-match from newest
		e.redrawSearch()
	}
	return nil
}

// searchMatch returns the current match — searchIdx pins the cycled
// position; -1 means "search from newest" (subsequence match, newest first).
func (e *editor) searchMatch() string {
	if e.searchIdx >= 0 && e.searchIdx < len(e.history) {
		return e.history[e.searchIdx]
	}
	q := string(e.searchBuf)
	for i := len(e.history) - 1; i >= 0; i-- {
		h := e.history[i]
		if subseq(q, h) {
			e.searchIdx = i
			return h
		}
	}
	return ""
}

// searchNext walks to the next older match, wrapping to newest — real's
// ctrl+r cycling.
func (e *editor) searchNext() {
	q := string(e.searchBuf)
	start := e.searchIdx - 1
	for i := start; i >= 0; i-- {
		if subseq(q, e.history[i]) {
			e.searchIdx = i
			e.redrawSearch()
			return
		}
	}
	for i := len(e.history) - 1; i > start; i-- {
		if subseq(q, e.history[i]) {
			e.searchIdx = i
			e.redrawSearch()
			return
		}
	}
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
	if e.zoneEdit {
		e.redraw() // the search prompt renders inside the zone box
		return
	}
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

// dotRepeat replays the last recorded change — real's '.' replays the
// whole command (op keystrokes + the inserted text of an insert burst).
func (e *editor) dotRepeat() {
	if len(e.changeKeys) == 0 {
		return
	}
	keys := append([]key(nil), e.changeKeys...)
	ins := append([]rune(nil), e.lastInsText...)
	e.pendingKeys = nil
	for _, k := range keys {
		if e.mode == "visual" || e.mode == "vline" {
			e.visualKey(k)
		} else {
			e.vimKey(k)
		}
	}
	if e.mode == "insert" {
		e.recordingIns = false // replayed text isn't a new recording
		if len(ins) > 0 {
			e.insert(ins...)
		}
		e.mode = "normal"
		if e.pos > 0 {
			e.pos--
		}
	}
	e.changeKeys = keys
	e.lastInsText = ins
	e.redraw()
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
			e.pendingKeys = nil // yanks aren't '.'-changes
			e.redraw()
			return
		}
		e.pushUndo()
		e.buf = append(e.buf[:s], e.buf[en:]...)
		e.pos = s
		if op == 'c' {
			e.mode = "insert"
			e.startInsRec()
		}
		e.noteChange()
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
		e.noteChange()
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
		e.noteChange()
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
		e.pendingKeys = nil
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

// selRange returns the active visual selection as [start,end); in
// visual-line mode it extends to whole lines (newline-inclusive).
func (e *editor) selRange() (int, int) {
	s, en := min(e.vimVisual, e.pos), max(e.vimVisual, e.pos)+1
	if en > len(e.buf) {
		en = len(e.buf)
	}
	if e.mode == "vline" {
		for s > 0 && e.buf[s-1] != '\n' {
			s--
		}
		for en < len(e.buf) && e.buf[en-1] != '\n' {
			en++
		}
	}
	return s, en
}

// visualKey handles VISUAL and VISUAL-LINE modes: motions extend the
// anchor selection, operators apply to it (real's `No` dispatch).
func (e *editor) visualKey(k key) *readResult {
	c := k.b
	vline := e.mode == "vline"
	exit := func() { e.mode = "normal"; e.vimVisual = -1; e.redraw() }
	// r<char> replaces every selected char with the target (real visual r)
	if e.vimReplace {
		e.vimReplace = false
		e.pendingKeys = append(e.pendingKeys, k)
		if c >= 32 && c != 0x1b {
			s, en := e.selRange()
			e.pushUndo()
			for i := s; i < en && i < len(e.buf); i++ {
				if e.buf[i] != '\n' {
					e.buf[i] = rune(c)
				}
			}
			e.noteChange()
			e.mode = "normal"
			e.vimVisual = -1
			e.pos = s
			e.redraw()
			return nil
		}
		e.pendingKeys = nil
		exit()
		return nil
	}
	// i/a text object pending — select the object's range
	if e.vimObj != 0 {
		kind := e.vimObj
		e.vimObj = 0
		e.pendingKeys = nil // object selection isn't itself a change
		if s2, en2, ok := e.textObjRange(kind, rune(c)); ok {
			e.vimVisual = s2
			e.pos = max(s2, en2-1)
			e.redraw()
		}
		return nil
	}
	// find-char pending — the target extends the selection
	if e.vimFind != 0 {
		fc := e.vimFind
		e.vimFind = 0
		e.pendingKeys = nil
		if c >= 32 {
			e.lastFindCh = rune(c)
			e.lastFindDir = fc
			e.pos = e.findChar(e.lastFindCh, fc)
			e.redraw()
		}
		return nil
	}
	if e.vimPendingG == 'm' {
		e.vimPendingG = 0
		e.pendingKeys = nil
		if c == 'g' {
			e.pos = 0
			e.redraw()
		}
		return nil
	}
	// count prefix (real visual motions take counts)
	if c >= '1' && c <= '9' || (c == '0' && e.vimCount > 0) {
		e.vimCount = min(10000, e.vimCount*10+int(c-'0'))
		return nil
	}
	e.pendingKeys = append(e.pendingKeys, k)
	cnt := e.vimCount
	e.vimCount = 0
	if cnt == 0 {
		cnt = 1
	}
	neutral := func() { e.pendingKeys = nil }
	change := func(op rune) {
		s, en := e.selRange()
		e.mode = "normal"
		e.vimVisual = -1
		e.applyOpRange(op, s, en, vline)
		e.noteChange()
		if op == 'c' {
			e.mode = "insert"
			e.startInsRec()
		}
		e.pos = s
	}
	switch c {
	case 0x1b:
		e.pendingKeys = nil
		exit()
		return nil
	case 'v':
		e.pendingKeys = nil
		if vline {
			e.mode = "visual" // V in vline switches to charwise (real)
			e.redraw()
		} else {
			exit()
		}
		return nil
	case 'V':
		e.pendingKeys = nil
		e.mode = "vline"
		e.redraw()
		return nil
	case 'h', 0x7f:
		e.pos = max(0, e.pos-cnt)
		e.redraw()
	case 'l', ' ':
		if len(e.buf) > 0 {
			e.pos = min(len(e.buf)-1, e.pos+cnt)
		}
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
	case 'g':
		e.vimPendingG = 'm'
	case 'G':
		if cnt > 1 {
			e.pos = e.lineStartOf(cnt - 1)
		} else {
			e.pos = e.lineStartOf(e.lineCount() - 1)
		}
		e.redraw()
	case 'f', 'F', 't', 'T':
		e.vimFind = rune(c)
	case ';':
		e.pos = e.findChar(e.lastFindCh, e.lastFindDir)
		e.redraw()
	case ',':
		e.pos = e.findChar(e.lastFindCh, flipDir(e.lastFindDir))
		e.redraw()
	case 'i', 'a':
		e.vimObj = byte(c)
	case 'o':
		e.pos, e.vimVisual = e.vimVisual, e.pos // swap anchor/cursor
		e.redraw()
	case 'd', 'x':
		op := rune(c)
		if c == 'x' {
			op = 'd'
		}
		change(op)
	case 'y':
		change('y')
	case 'c':
		change('c')
	case '>':
		change('>')
	case '<':
		change('<')
	case '~':
		s, en := e.selRange()
		e.pushUndo()
		for i := s; i < en && i < len(e.buf); i++ {
			r := e.buf[i]
			if r >= 'a' && r <= 'z' {
				e.buf[i] = r - 32
			} else if r >= 'A' && r <= 'Z' {
				e.buf[i] = r + 32
			}
		}
		e.noteChange()
		e.mode = "normal"
		e.vimVisual = -1
		e.pos = s
		e.redraw()
	case 'J':
		// join the selected lines with single spaces
		s, en := e.selRange()
		e.pushUndo()
		var nb []rune
		first := true
		for i := s; i < en && i < len(e.buf); i++ {
			if e.buf[i] == '\n' && i+1 < en {
				nb = append(nb, ' ')
				first = false
				continue
			}
			nb = append(nb, e.buf[i])
			first = false
		}
		_ = first
		e.buf = append(e.buf[:s], append(nb, e.buf[en:]...)...)
		e.noteChange()
		e.mode = "normal"
		e.vimVisual = -1
		e.pos = s
		e.redraw()
	case 'p', 'P':
		if e.lastDel != "" {
			s, en := e.selRange()
			e.pushUndo()
			e.buf = append(e.buf[:s], e.buf[en:]...)
			at := s
			if c == 'p' && strings.Contains(e.lastDel, "\n") {
				at = min(len(e.buf), e.lineEndAt(s)+1)
			}
			for i := 0; i < cnt; i++ {
				e.buf = append(e.buf[:at], append([]rune(e.lastDel), e.buf[at:]...)...)
			}
			e.pos = at
			e.noteChange()
		}
		e.mode = "normal"
		e.vimVisual = -1
		e.redraw()
	case 'r':
		e.vimReplace = true
	default:
		neutral()
	}
	return nil
}

var historySessionID string // stamped on history records (real field)

// appendHistoryFile persists prompts to ~/.claude/history.jsonl (real file).
func appendHistoryFile(line string) {
	dir := filepath.Join(config.ClaudeDir())
	os.MkdirAll(dir, 0o755)
	f, err := os.OpenFile(filepath.Join(dir, "history.jsonl"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	cwd, _ := os.Getwd()
	rec := map[string]any{
		"display": line, "timestamp": time.Now().UnixMilli(),
		"project": cwd, "pastedContents": map[string]any{}, // real schema
	}
	if historySessionID != "" {
		rec["sessionId"] = historySessionID
	}
	b, _ := json.Marshal(rec)
	f.Write(append(b, '\n'))
}

// vimInsertRemaps reads vimInsertModeRemaps from settings (["jj","jk"]…).
func vimInsertRemaps() []string {
	for _, p := range []string{
		filepath.Join(config.ClaudeDir(), "settings.json"),
		filepath.Join(config.Home(), ".claude.json"),
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

var fastModeFlag atomic.Bool   // meta+o toggle
var thinkingHidden atomic.Bool // meta+t toggle

// sendNow submits immediately even while the agent runs (flushes the queue
// ahead of queued lines) — real chat:sendNow.
func (e *editor) sendNow() *readResult {
	line := e.submitLine()
	// ok:true — at idle this is a normal submit; the sendNow flag only
	// matters to the mid-turn busy path (which reads the chord itself).
	return &readResult{line: line, sendNow: true, ok: true}
}

// queueSubmitKey explicitly queues the line when the agent is running.
func (e *editor) queueSubmitKey() *readResult {
	line := e.submitLine()
	return &readResult{line: line, queued: true, ok: true}
}

// modelPickerKey opens /model from the keymap (meta+p in real).
func (e *editor) modelPickerKey() *readResult {
	return &readResult{line: "/model", modelPicker: true}
}

// toggleFastMode is chat:fastMode (meta+o) — flips the loop's fast-mode
// lane via the onFastMode callback (the loop owns the wire field).
func (e *editor) toggleFastMode() {
	fastModeFlag.Store(!fastModeFlag.Load())
	if e.onFastMode != nil {
		e.onFastMode()
	}
}

func (e *editor) toggleThinking() { thinkingHidden.Store(!thinkingHidden.Load()) }

// stashOrPop is ctrl+s (real chat:stash): a non-empty draft is stashed and
// the input clears (single-slot stashedPrompt, not a stack); ctrl+s again
// on an empty input pops the stashed draft back (real's popStash).
func (e *editor) stashOrPop() {
	if len(e.buf) > 0 {
		e.flushCleared()
		e.pushUndo()
		e.stashed = append([]rune(nil), e.buf...)
		e.buf = nil
		e.pos = 0
		e.redraw()
		return
	}
	if e.stashed != nil {
		e.pushUndo()
		e.buf = e.stashed
		e.stashed = nil
		e.pos = len(e.buf)
		e.redraw()
	}
}

// ---- emacs kill-ring (real's `se` store: Kt/Gt maps in mod_0263) ----

// killAdd pushes killed text on the ring — consecutive kills coalesce
// (prepend for backward kills, append for forward kills).
func (e *editor) killAdd(text string, prepend bool) {
	if text == "" {
		return
	}
	if e.wasKill && len(e.killRing) > 0 {
		if prepend {
			e.killRing[0] = text + e.killRing[0]
		} else {
			e.killRing[0] += text
		}
	} else {
		e.killRing = append([]string{text}, e.killRing...)
		if len(e.killRing) > 30 {
			e.killRing = e.killRing[:30]
		}
	}
	e.killLast = true
}

// killToLineEnd is ctrl+k — kill to end of logical line (or the newline
// itself when at EOL).
func (e *editor) killToLineEnd() {
	en := e.lineEnd()
	if en == e.pos && en < len(e.buf) {
		en++ // at EOL the kill takes the newline (real deleteToLineEnd)
	}
	e.killAdd(string(e.buf[e.pos:en]), false)
	e.pushUndo()
	e.buf = append(e.buf[:e.pos], e.buf[en:]...)
	e.redraw()
}

// killToLineStart is ctrl+u — kill back to start of logical line.
func (e *editor) killToLineStart() {
	st := e.lineStart()
	e.killAdd(string(e.buf[st:e.pos]), true)
	e.pushUndo()
	e.buf = append(e.buf[:st], e.buf[e.pos:]...)
	e.pos = st
	e.redraw()
}

// killWordFwd is meta+d — kill the next word forward.
func (e *editor) killWordFwd() {
	p := e.pos
	for p < len(e.buf) && isWordSep(e.buf[p]) {
		p++
	}
	for p < len(e.buf) && !isWordSep(e.buf[p]) {
		p++
	}
	e.killAdd(string(e.buf[e.pos:p]), false)
	e.pushUndo()
	e.buf = append(e.buf[:e.pos], e.buf[p:]...)
	e.redraw()
}

// backwardKillWord is meta/ctrl+backspace and ctrl+w — kill the word
// behind the cursor.
func (e *editor) backwardKillWord() {
	p := e.pos
	for p > 0 && isWordSep(e.buf[p-1]) {
		p--
	}
	for p > 0 && !isWordSep(e.buf[p-1]) {
		p--
	}
	e.killAdd(string(e.buf[p:e.pos]), true)
	e.pushUndo()
	e.buf = append(e.buf[:p], e.buf[e.pos:]...)
	e.pos = p
	e.redraw()
}

// yankKill is the editor-layer ctrl+y — insert the newest kill-ring entry.
func (e *editor) yankKill() {
	if len(e.killRing) == 0 {
		return
	}
	t := e.killRing[0]
	st := e.pos
	e.insert([]rune(t)...)
	e.yankPos = st
	e.yankLen = len([]rune(t))
	e.yankIdx = 0
}

// yankPop is meta+y — swap the last yank for the next ring entry.
func (e *editor) yankPop() {
	if e.yankPos < 0 || len(e.killRing) == 0 || e.pos != e.yankPos+e.yankLen {
		return
	}
	e.yankIdx = (e.yankIdx + 1) % len(e.killRing)
	t := []rune(e.killRing[e.yankIdx])
	nb := append([]rune(nil), e.buf[:e.yankPos]...)
	nb = append(nb, t...)
	nb = append(nb, e.buf[e.yankPos+e.yankLen:]...)
	e.buf = nb
	e.pos = e.yankPos + len(t)
	e.yankLen = len(t)
	e.redraw()
}

// ---- wrapped-row cursor movement (real's mt/ht: up/down move between
// visual lines of the draft before reaching history) ----

// boxTextW is the usable text width of one composer row — mirrors what
// boxFrame uses so wrapped-row math matches the rendered layout.
func (e *editor) boxTextW() int {
	w := compW
	if w <= 0 {
		w = 80
	}
	t := w - visWidth(e.prompt)
	if t < 4 {
		t = 4
	}
	return t
}

func visRunes(rs []rune) int { return visWidth(string(rs)) }

// wrapRows is how many screen rows a logical line of `cells` columns
// occupies at width w — a line of exactly w cells still needs a second
// row only when followed by more text (deferred wrap), so it counts 1.
func wrapRows(cells, w int) int {
	if cells <= 0 {
		return 1
	}
	return (cells + w - 1) / w
}

// visualPos returns the wrapped row/column of e.pos plus the draft's
// total wrapped-row count.
func (e *editor) visualPos() (row, col, rows int) {
	w := e.boxTextW()
	ls := 0
	seen := false
	for i := 0; i <= len(e.buf); i++ {
		if i != len(e.buf) && e.buf[i] != '\n' {
			continue
		}
		cells := visRunes(e.buf[ls:i])
		lr := wrapRows(cells, w)
		if !seen && e.pos <= i {
			seen = true
			cc := visRunes(e.buf[ls:e.pos])
			row = rows + cc/w
			col = cc % w
			if cc > 0 && cc%w == 0 && e.pos == len(e.buf) {
				// deferred-wrap cursor stays on the filled row
				row--
				col = w
			}
		}
		rows += lr
		ls = i + 1
	}
	return
}

// posAtRowCol resolves a wrapped row+column back to a buffer offset.
func (e *editor) posAtRowCol(row, col int) int {
	w := e.boxTextW()
	ls, cur := 0, 0
	for i := 0; i <= len(e.buf); i++ {
		if i != len(e.buf) && e.buf[i] != '\n' {
			continue
		}
		cells := visRunes(e.buf[ls:i])
		lr := wrapRows(cells, w)
		if row < cur+lr {
			want := (row-cur)*w + col
			if want > cells {
				want = cells
			}
			pos, cw := ls, 0
			for pos < i && cw < want {
				rw := visRunes(e.buf[pos : pos+1])
				if cw+rw > want {
					break
				}
				cw += rw
				pos++
			}
			return pos
		}
		cur += lr
		ls = i + 1
	}
	return len(e.buf)
}

// cursorUp moves the cursor one wrapped row up — false on the top row.
func (e *editor) cursorUp() bool {
	row, col, _ := e.visualPos()
	if row == 0 {
		return false
	}
	e.pos = e.posAtRowCol(row-1, col)
	e.redraw()
	return true
}

// cursorDown moves the cursor one wrapped row down — false on the last row.
func (e *editor) cursorDown() bool {
	row, col, rows := e.visualPos()
	if row >= rows-1 {
		return false
	}
	e.pos = e.posAtRowCol(row+1, col)
	e.redraw()
	return true
}

// showShortcuts prints the keyboard-shortcut overlay ('?' on empty input).
func (e *editor) showShortcuts() {
	var b strings.Builder
	b.WriteString("\r\n\x1b[2m")
	rows := [][2]string{
		{"enter", "submit"}, {"shift+enter", "newline"},
		{"shift+tab", "cycle permission mode"},
		{"ctrl+c", "interrupt · twice to exit"}, {"ctrl+d", "exit"},
		{"ctrl+l", "clear input"}, {"ctrl+r", "search history"},
		{"ctrl+o", "transcript view"}, {"ctrl+t", "toggle todos"},
		{"ctrl+g / ctrl+x ctrl+e", "external editor"},
		{"ctrl+_ / ctrl+-", "undo"}, {"ctrl+s", "stash/pop draft"},
		{"ctrl+k / ctrl+u / ctrl+w", "kill to end/start/word"},
		{"ctrl+y / meta+y", "yank / cycle kill-ring"},
		{"meta+b / meta+f / meta+d", "word back/fwd/kill"},
		{"meta+p", "model picker"}, {"meta+o", "fast mode"},
		{"meta+t", "toggle thinking"}, {"esc esc", "rewind"},
		{"<any> while running", "queue the message"},
	}
	for _, r := range rows {
		fmt.Fprintf(&b, "  %-22s %s\r\n", r[0], r[1])
	}
	b.WriteString("\x1b[0m")
	compInline(b.String()) // scroll-region print — works in both box modes
	e.redraw()
}

// Alt-screen lifecycle — real runs the interactive UI in the alternate
// screen buffer with full mouse tracking and a "✳ Claude Code" window
// title; teardown restores the primary screen and modes.

const altScreenEnter = "]0;â³ Claude Code" +
	"[?1049h[2J[H" +
	"[?1000h[?1002h[?1003h[?1006h" + // mouse: click/drag/motion/SGR
	"[?2031h[?1004h" // colormode report + focus events
const altScreenExit = "[?1004l[?2031l" +
	"[?2004l" +
	"[?1006l[?1003l[?1002l[?1000l" +
	"[?1049l" + "]0;"

var altScreenOn atomic.Bool

func enterAltScreen() {
	if altScreenOn.Swap(true) {
		return
	}
	os.Stdout.WriteString(altScreenEnter)
}

func exitAltScreen() {
	if !altScreenOn.Swap(false) {
		return
	}
	os.Stdout.WriteString(altScreenExit)
}

// boxRule is the composer's gray ─ full-width rule (real's prompt-box
// chrome — a horizontal rule above and below the input, not a │ box).
func boxRule() string {
	w := compW
	if w <= 0 {
		w = 80
	}
	return "\x1b[38;5;244m" + strings.Repeat("─", w) + "\x1b[39m"
}

// footerText returns the status footer with the live vim badge prepended —
// the badge must re-evaluate at draw time because esc flips modes between
// printStatus calls.
func (e *editor) footerText() string {
	if !e.vim {
		return e.footer
	}
	mode := e.mode
	if mode == "" {
		mode = "insert"
	}
	return "vim:" + mode + " · " + e.footer
}
