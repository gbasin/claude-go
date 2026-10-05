package repl

import "testing"

// Regression: backslash-enter stripped the trailing '\' but left e.pos
// past the new buffer end — insert('\n') sliced out of range and the
// REPL crashed on the common type-\-press-enter path.
func TestSubmitKeyBackslashContinuation(t *testing.T) {
	e := newEditor()
	e.buf = []rune("line one\\")
	e.pos = len(e.buf)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic: %v", r)
		}
	}()
	e.submitKey()
	if string(e.buf) != "line one\n" {
		t.Fatalf("buf = %q, want %q", string(e.buf), "line one\n")
	}
}
