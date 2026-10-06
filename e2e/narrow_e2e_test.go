//go:build e2e

// narrow_e2e_test.go — PTY coverage at narrow widths (40-60 cols), the bug
// class fixed in the compositor/render pass: tracked-wrap \r\n emission,
// pinned-zone repaint, dialog wrap. Asserts a streamed multi-line reply, a
// thinking block, a tool card and the permission dialog all render and the
// REPL stays functional — corruption shows up as lost text, invalid utf8,
// or a dead prompt.
package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestNarrowTurn50: at 50 cols a wrapping streamed reply, a permission
// dialog, and a tool call all render; the prompt returns after each.
func TestNarrowTurn50(t *testing.T) {
	s := newSession(t)
	p := s.runPTYSize(t, nil, 50, 24, "--mock")
	p.waitFor("❯", 30*time.Second)

	// long prompt → multi-line wrapped reply
	p.send("alpha bravo charlie delta echo foxtrot\r")
	p.waitFor("Mock response to:", 30*time.Second)
	out := p.clean()
	for _, w := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"} {
		if !strings.Contains(out, w) {
			t.Fatalf("wrapped reply lost word %q at 50 cols\noutput:\n%s", w, truncStr(out, 4000))
		}
	}

	// permission dialog at narrow width (no bypass → dialog path).
	// sendUntilEchoed, not sendUntil: retrying a submitted line would run a
	// duplicate turn whose tool_use ids collide with the first (the mock
	// reuses toolu_mock_N per response) → validator 400.
	p.sendUntilEchoed(t, `tool:Bash {"command":"echo narrow-ok"}`+"\r", "❯ tool:Bash", 10)
	p.waitFor("─ permission", 30*time.Second)
	p.sendUntil("1", "Tool results received", 15)
	out = p.clean()
	if !strings.Contains(out, "narrow-ok") {
		t.Fatalf("tool output missing at 50 cols\noutput:\n%s", truncStr(out, 4000))
	}
	if !utf8OK(out) {
		t.Fatal("output is not valid utf8 at 50 cols")
	}
	p.exit()
}

// TestNarrowThink40: at 40 cols the thinking block + streamed text render
// without overwriting each other, and a follow-up turn still works.
func TestNarrowThink40(t *testing.T) {
	s := newSession(t)
	p := s.runPTYSize(t, nil, 40, 24, "--mock")
	p.waitFor("❯", 30*time.Second)

	p.send("think:narrow-width-reasoning\r")
	p.waitFor("Done thinking", 30*time.Second)

	// REPL survives: next prompt answers — retry only until the echo lands
	// (a resend after submission would queue a duplicate turn)
	p.sendUntilEchoed(t, "ping-narrow\r", "❯ ping-narrow", 8)
	p.waitFor("Mock response to: ping-narrow", 30*time.Second)
	if !utf8OK(p.clean()) {
		t.Fatal("output is not valid utf8 at 40 cols")
	}
	p.exit()
}
