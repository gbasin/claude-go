package repl

import (
	"strings"
	"testing"
)

// newTestEditor builds an editor with isolated config/history dirs and a
// fixed wrap width. redraw() writes ANSI to stdout — harmless in tests.
func newTestEditor(t *testing.T) *editor {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	compW = 80
	e := newEditor()
	return e
}

// send routes a key like readLoop does: non-insert vim modes go to vimKey.
func send(e *editor, k key) *readResult {
	if e.vim && e.mode != "insert" {
		return e.vimKey(k)
	}
	return e.insertKey(k)
}

func typeStr(e *editor, s string) {
	for _, r := range s {
		send(e, key{b: byte(r), r: r})
	}
}

func escK() key        { return key{b: 0x1b} }
func ctrlK(b byte) key { return key{b: b} }

// --- esc: first press warns, second clears into history (real Wt) ---

func TestEscWarnThenClearSavesHistory(t *testing.T) {
	e := newTestEditor(t)
	typeStr(e, "draft-one")
	send(e, escK())
	if len(e.buf) == 0 {
		t.Fatal("first esc should not clear — it warns")
	}
	send(e, escK())
	if len(e.buf) != 0 {
		t.Fatal("second esc should clear the draft")
	}
	if len(e.history) == 0 || e.history[len(e.history)-1] != "draft-one" {
		t.Fatalf("cleared draft should land in history, got %v", e.history)
	}
}

func TestEscEscOnEmptyOpensRewind(t *testing.T) {
	e := newTestEditor(t)
	send(e, escK())
	if r := send(e, escK()); r == nil || !r.rewind {
		t.Fatalf("esc-esc on empty input should open rewind, got %+v", r)
	}
}

// --- ctrl+c: holds cleared draft, Up restores, gate exits ---

func TestCtrlCHoldsClearedUpRestores(t *testing.T) {
	e := newTestEditor(t)
	typeStr(e, "held-draft")
	send(e, ctrlK(3))
	if len(e.buf) != 0 {
		t.Fatal("ctrl+c should clear the buffer")
	}
	if !e.exitArm {
		t.Fatal("ctrl+c should arm the exit gate")
	}
	send(e, key{seq: "[A"}) // Up — real restores the held draft before history
	if string(e.buf) != "held-draft" {
		t.Fatalf("Up should restore the held draft, got %q", e.buf)
	}
}

func TestCtrlCSecondPressExits(t *testing.T) {
	e := newTestEditor(t)
	send(e, ctrlK(3))
	r := send(e, ctrlK(3))
	if r == nil || !r.quit {
		t.Fatalf("second ctrl+c should quit, got %+v", r)
	}
}

func TestCtrlCClearedFlushesToHistoryOnEdit(t *testing.T) {
	e := newTestEditor(t)
	typeStr(e, "abandoned")
	send(e, ctrlK(3))
	typeStr(e, "x") // typing commits the held draft to history
	found := false
	for _, h := range e.history {
		if h == "abandoned" {
			found = true
		}
	}
	if !found {
		t.Fatalf("held draft should flush to history on next edit: %v", e.history)
	}
}

func TestExitGateDisarmOnOtherKey(t *testing.T) {
	e := newTestEditor(t)
	send(e, ctrlK(4)) // ctrl+d arms
	if !e.exitArm {
		t.Fatal("first ctrl+d should arm")
	}
	typeStr(e, "a")
	if e.exitArm {
		t.Fatal("an unrelated key should disarm the gate")
	}
	if r := send(e, ctrlK(4)); r != nil && r.quit {
		t.Fatal("ctrl+d after disarm should re-arm, not quit")
	}
}

func TestCtrlDDeletesForward(t *testing.T) {
	e := newTestEditor(t)
	typeStr(e, "abc")
	e.pos = 1
	send(e, ctrlK(4))
	if string(e.buf) != "ac" {
		t.Fatalf("ctrl+d mid-buffer should delete forward, got %q", e.buf)
	}
}

// --- multiline arrows move the cursor before history ---

func TestMultilineUpDownMovesWithinDraft(t *testing.T) {
	e := newTestEditor(t)
	typeStr(e, "aaa\nbbb")
	e.history = []string{"older-cmd"}
	send(e, key{seq: "[A"}) // up: cursor to row above, not history
	if string(e.buf) != "aaa\nbbb" {
		t.Fatalf("up inside a multiline draft must not load history, got %q", e.buf)
	}
	if e.pos > 3 {
		t.Fatalf("up should move the cursor to the first line, pos=%d", e.pos)
	}
	send(e, key{seq: "[A"}) // already at top row: now history
	if string(e.buf) != "older-cmd" {
		t.Fatalf("up at row 0 should page history, got %q", e.buf)
	}
}

func TestCtrlNMovesCursorDown(t *testing.T) {
	e := newTestEditor(t)
	typeStr(e, "line-a\nline-b")
	e.pos = 1
	send(e, ctrlK(14)) // ctrl+n — row down like Down arrow
	if e.pos <= 6 {
		t.Fatalf("ctrl+n should move cursor to the next row, pos=%d", e.pos)
	}
}

// --- emacs + kill-ring ---

func TestEmacsMotionKeys(t *testing.T) {
	e := newTestEditor(t)
	typeStr(e, "one two three")
	e.pos = len(e.buf)
	send(e, ctrlK(2)) // ctrl+b — char left
	if e.pos != len(e.buf)-1 {
		t.Fatalf("ctrl+b pos=%d", e.pos)
	}
	send(e, ctrlK(6)) // ctrl+f — char right
	if e.pos != len(e.buf) {
		t.Fatalf("ctrl+f pos=%d", e.pos)
	}
	send(e, key{b: 'b', r: 'b', mod: modAlt}) // meta+b
	if e.pos != 8 {
		t.Fatalf("meta+b should land at word start, pos=%d", e.pos)
	}
	send(e, key{b: 'f', r: 'f', mod: modAlt}) // meta+f
	if e.pos != len(e.buf) {
		t.Fatalf("meta+f should land at line end, pos=%d", e.pos)
	}
}

func TestKillRingCoalesceAndYankPop(t *testing.T) {
	e := newTestEditor(t)
	typeStr(e, "one two")
	e.pos = len(e.buf)
	send(e, ctrlK(23)) // ctrl+w — kills "two", ring[0]="two"
	send(e, ctrlK(23)) // consecutive kill coalesces → ring[0]="one two"
	if len(e.killRing) != 1 || e.killRing[0] != "one two" {
		t.Fatalf("consecutive kills should coalesce: %v", e.killRing)
	}
	send(e, ctrlK(25)) // ctrl+y — yank ring head
	if string(e.buf) != "one two" {
		t.Fatalf("ctrl+y should yank, got %q", e.buf)
	}
}

func TestYankPopCyclesRing(t *testing.T) {
	e := newTestEditor(t)
	e.killRing = []string{"first", "second"}
	typeStr(e, "")
	e.buf = []rune("x")
	e.pos = 1
	send(e, ctrlK(25))
	if string(e.buf) != "xfirst" {
		t.Fatalf("yank ring head: %q", e.buf)
	}
	send(e, key{b: 'y', r: 'y', mod: modAlt}) // meta+y — yank-pop
	if string(e.buf) != "xsecond" {
		t.Fatalf("yank-pop should swap to next ring entry: %q", e.buf)
	}
}

func TestCtrlUUKillsLinePortion(t *testing.T) {
	e := newTestEditor(t)
	typeStr(e, "line-a\nline-b")
	e.pos = len(e.buf)
	send(e, ctrlK(21)) // ctrl+u — kill to start of CURRENT line only
	if string(e.buf) != "line-a\n" {
		t.Fatalf("ctrl+u should kill to line start, got %q", e.buf)
	}
	if len(e.killRing) == 0 || e.killRing[0] != "line-b" {
		t.Fatalf("killed text should enter the ring: %v", e.killRing)
	}
}

// --- ctrl+s stash toggle ---

func TestStashToggle(t *testing.T) {
	e := newTestEditor(t)
	typeStr(e, "stashed-me")
	send(e, ctrlK(19)) // ctrl+s stashes
	if len(e.buf) != 0 || e.stashed == nil {
		t.Fatal("ctrl+s should stash the draft")
	}
	send(e, ctrlK(19)) // ctrl+s again pops (real popStash)
	if string(e.buf) != "stashed-me" {
		t.Fatalf("ctrl+s should pop the stash, got %q", e.buf)
	}
}

// --- paste thresholds (real mod_0304/mod_1600) ---

func TestExpandPasteThresholds(t *testing.T) {
	e := newTestEditor(t)
	if out := e.expandPaste("a\nb\nc"); out != "a\nb\nc" {
		t.Fatalf("3-line paste (nl=2) should insert raw, got %q", out)
	}
	out := e.expandPaste("a\nb\nc\nd")
	if !strings.Contains(out, "+3 lines") {
		t.Fatalf("4-line paste should show +3 lines (newline count), got %q", out)
	}
	long := strings.Repeat("x", 900)
	out = e.expandPaste(long)
	if !strings.HasPrefix(out, "[Pasted text #2]") {
		t.Fatalf("900-char single line should collapse, got %q", out)
	}
	if e.pastes[out] == "" {
		t.Fatal("placeholder should map to pasted_content")
	}
}

// --- history search (real HistorySearch ctx) ---

func TestSearchEnterExecutes(t *testing.T) {
	e := newTestEditor(t)
	e.history = []string{"alpha", "beta-first"}
	e.startSearch()
	send(e, key{b: 'a', r: 'a'})
	send(e, key{b: 'l', r: 'l'})
	r := send(e, key{b: '\r'})
	if r == nil || r.line != "alpha" {
		t.Fatalf("enter should execute the newest match, got %+v", r)
	}
}

func TestSearchEscAcceptsCtrlCCancels(t *testing.T) {
	e := newTestEditor(t)
	typeStr(e, "wip-draft")
	e.history = []string{"alpha"}
	e.startSearch()
	send(e, key{b: 'a', r: 'a'})
	send(e, escK()) // esc accepts the match into the buffer (real)
	if string(e.buf) != "alpha" {
		t.Fatalf("esc should accept the match, got %q", e.buf)
	}

	e2 := newTestEditor(t)
	typeStr(e2, "wip-draft")
	e2.history = []string{"alpha"}
	e2.startSearch()
	send(e2, key{b: 'a', r: 'a'})
	send(e2, ctrlK(3)) // ctrl+c cancels back to the pre-search draft
	if string(e2.buf) != "wip-draft" {
		t.Fatalf("ctrl+c should restore the pre-search draft, got %q", e2.buf)
	}
}

func TestSearchCtrlRCycles(t *testing.T) {
	e := newTestEditor(t)
	e.history = []string{"foo-old", "foo-new"}
	e.startSearch()
	send(e, key{b: 'f', r: 'f'})
	if m := e.searchMatch(); m != "foo-new" {
		t.Fatalf("first match should be newest: %q", m)
	}
	e.searchNext()
	if m := e.searchMatch(); m != "foo-old" {
		t.Fatalf("ctrl+r should cycle to the older match: %q", m)
	}
	e.searchNext()
	if m := e.searchMatch(); m != "foo-new" {
		t.Fatalf("ctrl+r should wrap to newest: %q", m)
	}
}

// --- vim ---

func TestVimDwDiw(t *testing.T) {
	e := newTestEditor(t)
	e.vim = true
	e.mode = "normal"
	typeStr(e, "")
	e.buf = []rune("one two three")
	e.pos = 0
	send(e, key{b: 'd', r: 'd'})
	send(e, key{b: 'w', r: 'w'})
	if string(e.buf) != "two three" {
		t.Fatalf("dw should delete the first word, got %q", e.buf)
	}
	send(e, key{b: 'd', r: 'd'})
	send(e, key{b: 'i', r: 'i'})
	send(e, key{b: 'w', r: 'w'})
	if string(e.buf) != " three" {
		t.Fatalf("diw should delete the inner word, got %q", e.buf)
	}
}

func TestVimVisualLine(t *testing.T) {
	e := newTestEditor(t)
	e.vim = true
	e.mode = "normal"
	e.buf = []rune("first\nsecond\nthird")
	e.pos = 8 // second line
	send(e, key{b: 'V', r: 'V'})
	if e.mode != "vline" {
		t.Fatal("V should enter VISUAL-LINE")
	}
	send(e, key{b: 'd', r: 'd'})
	if string(e.buf) != "first\nthird" {
		t.Fatalf("Vd should delete the whole line, got %q", e.buf)
	}
	if e.mode != "normal" {
		t.Fatalf("vline op should return to normal mode, got %q", e.mode)
	}
}

func TestVimDotRepeatInsert(t *testing.T) {
	e := newTestEditor(t)
	e.vim = true
	e.mode = "normal"
	e.buf = []rune("one two")
	e.pos = 0
	send(e, key{b: 'd', r: 'd'})
	send(e, key{b: 'w', r: 'w'}) // deletes "one "
	if string(e.buf) != "two" {
		t.Fatalf("dw: %q", e.buf)
	}
	send(e, key{b: '.', r: '.'}) // . replays dw — buffer empties
	if string(e.buf) != "" {
		t.Fatalf(". should repeat dw, got %q", e.buf)
	}
}

func TestVimDotRepeatInsertText(t *testing.T) {
	e := newTestEditor(t)
	e.vim = true
	e.mode = "normal"
	e.buf = []rune("x")
	e.pos = 0
	send(e, key{b: 'o', r: 'o'}) // open line + insert
	typeStr(e, "hi")
	send(e, escK()) // back to normal
	if string(e.buf) != "x\nhi" {
		t.Fatalf("o + text: %q", e.buf)
	}
	send(e, key{b: '.', r: '.'})
	if string(e.buf) != "x\nhi\nhi" {
		t.Fatalf(". should replay open-line + inserted text, got %q", e.buf)
	}
}

func TestVimReplaceChar(t *testing.T) {
	e := newTestEditor(t)
	e.vim = true
	e.mode = "normal"
	e.buf = []rune("cat")
	e.pos = 0
	send(e, key{b: 'r', r: 'r'})
	send(e, key{b: 'b', r: 'b'})
	if string(e.buf) != "bat" {
		t.Fatalf("r<char> should replace: %q", e.buf)
	}
}

// --- resolved keybinding hints ---

func TestResolvedChord(t *testing.T) {
	e := newTestEditor(t)
	if got := e.resolvedChord("Global", "app:toggleTranscript", "x"); got != "ctrl+o" {
		t.Fatalf("toggleTranscript should resolve to ctrl+o, got %q", got)
	}
	if got := e.resolvedChord("Chat", "chat:submit", "enter"); got != "enter" {
		t.Fatalf("submit should resolve to enter, got %q", got)
	}
}
