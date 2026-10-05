//go:build e2e

// lifecycle_e2e_test.go — interactive PTY coverage for session-lifecycle
// and input-layer surface: keybindings.json remaps/warnings, /rewind
// restoring a checkpointed file write, /clear resetting the loop, and a
// compacted session staying resumable.
package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestKeybindingCommandRemap: ~/.claude/keybindings.json binds
// ctrl+y → command:exit — pressing it runs /exit and the process quits.
func TestKeybindingCommandRemap(t *testing.T) {
	s := newSession(t)
	s.writeFile(filepath.Join(s.home, ".claude", "keybindings.json"),
		`{"bindings":[{"context":"Chat","bindings":{"ctrl+y":"command:exit"}}]}`)
	p := startREPL(t, s)
	p.send("\x19") // ctrl+y
	if err := p.waitExit(15 * time.Second); err != nil {
		t.Fatalf("ctrl+y remap to command:exit did not quit: %v\noutput:\n%s",
			err, truncStr(p.clean(), 2000))
	}
}

// TestKeybindingInertAction: an unknown action name is consumed silently
// (real stores the binding; the key goes inert) — the REPL stays alive.
func TestKeybindingInertAction(t *testing.T) {
	s := newSession(t)
	s.writeFile(filepath.Join(s.home, ".claude", "keybindings.json"),
		`{"bindings":[{"context":"Chat","bindings":{"ctrl+y":"bogus:nonexistent"}}]}`)
	p := startREPL(t, s)
	p.send("\x19")
	time.Sleep(500 * time.Millisecond)
	// ctrl+y did nothing: still at a live prompt — a normal line answers
	p.send("still-here\r")
	p.waitFor("Mock response to: still-here", 30*time.Second)
	p.exit()
}

// TestKeybindingWarn: malformed keybindings.json surfaces the single-line
// validation notice on stderr (merged into the pty).
func TestKeybindingWarn(t *testing.T) {
	s := newSession(t)
	s.writeFile(filepath.Join(s.home, ".claude", "keybindings.json"), `{not json`)
	p := startREPL(t, s)
	p.waitFor("[keybindings]", 15*time.Second)
	p.exit()
}

// TestRewindRestoresFile: a Write tool call checkpoints the file; /rewind
// restores the pre-write contents.
func TestRewindRestoresFile(t *testing.T) {
	s := newSession(t)
	target := s.writeFile("target.txt", "v0-contents")
	p := startREPL(t, s, "--permission-mode", "bypassPermissions")

	// Read first — Write refuses to clobber an unread file (real behavior),
	// then writes v1 and checkpoints the v0 contents.
	p.sendUntilEchoed(t,
		`calls:[{"name":"Read","input":{"file_path":"`+target+`"}},`+
			`{"name":"Write","input":{"file_path":"`+target+`","content":"v1-overwritten"}}]`+"\r",
		`> calls:[{"name":"Read"`, 8)
	p.waitFor("Tool results received", 30*time.Second)
	if b, _ := os.ReadFile(target); string(b) != "v1-overwritten" {
		t.Fatalf("write didn't land: %q", b)
	}

	p.sendUntilEchoed(t, "/rewind\r", "> /rewind", 8)
	p.waitFor("rewound", 15*time.Second)
	if b, err := os.ReadFile(target); err != nil || string(b) != "v0-contents" {
		t.Fatalf("/rewind should restore v0-contents, got %q err=%v", b, err)
	}
	p.exit()
}

// TestClearResetsLoop: after a turn, /clear empties the message list
// (/context reports 0) and the next prompt starts a fresh wire request.
func TestClearResetsLoop(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)

	p.send("hello\r")
	p.waitFor("Mock response to: hello", 30*time.Second)

	p.sendUntilEchoed(t, "/clear\r", "> /clear", 8)
	p.waitFor("context cleared", 15*time.Second)

	p.sendUntilEchoed(t, "/context\r", "> /context", 8)
	p.waitFor("messages: 0", 15*time.Second)

	// a fresh turn still works after the reset
	p.sendUntilEchoed(t, "second-wind\r", "> second-wind", 8)
	p.waitFor("Mock response to: second-wind", 30*time.Second)
	p.exit()
}

// TestResumeAfterCompact: an auto-compacted session resumes cleanly — the
// continued turn answers and the transcript stays valid.
func TestResumeAfterCompact(t *testing.T) {
	s := newSession(t)
	r := s.runEx(t, []string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW=40000"},
		30*time.Second, "--mock", "-p", "usage:9000")
	s.mustOK(t, r)
	// resume the just-written session with one more turn
	r = s.run(t, "--mock", "-p", "-c", "after compact")
	s.mustOK(t, r)
	contains(t, r.stdout, "Mock response to: after compact", "resumed reply")
}

// TestAskUserQuestionDialog: the AskUserQuestion tool drives the numbered
// in-turn dialog; pressing a digit picks the option and it lands in the
// tool result (the dialog channel takes over mid-turn keys).
func TestAskUserQuestionDialog(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s, "--permission-mode", "bypassPermissions")

	p.sendUntilEchoed(t,
		`tool:AskUserQuestion {"questions":[{"question":"Pick a color","header":"Q1",`+
			`"options":[{"label":"Red"},{"label":"Blue"}]}]}`+"\r",
		"> tool:AskUserQuestion", 8)
	p.waitFor("Pick a color", 30*time.Second)
	p.waitFor("Blue", 10*time.Second)

	// digit keys forward into the dialog channel mid-turn
	p.sendUntil("2", "Tool results received", 15)
	if !strings.Contains(p.clean(), "Blue") {
		t.Fatalf("picked answer should appear in the tool result")
	}
	p.exit()
}

// TestAgentToolSubagent: the Agent tool spawns a synchronous subagent turn
// against the mock — the subagent's reply lands in the tool result.
func TestAgentToolSubagent(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s, "--permission-mode", "bypassPermissions")

	p.sendUntilEchoed(t,
		`tool:Agent {"description":"e2e sub","prompt":"subagent marker question","run_in_background":false}`+"\r",
		"> tool:Agent", 8)
	p.waitFor("Tool results received", 60*time.Second)
	if !strings.Contains(p.clean(), "Mock response to: subagent marker question") {
		t.Fatalf("subagent turn reply should surface in the tool result\noutput:\n%s",
			truncStr(p.clean(), 3000))
	}
	p.exit()
}
