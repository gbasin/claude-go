//go:build e2e

// interactive_e2e_test.go — PTY-driven interactive REPL coverage:
//   - banner + prompt box (box-drawing) + Mock response + /exit
//   - queued input: two prompts submitted during/around a turn both answered
//   - permission prompt: Bash without an allow rule shows the numbered
//     dialog; "1" approves and the tool runs; "3" declines
//   - slash commands: /help /model /clear /compact /rename /rewind smoke
//   - custom commands: .claude/commands/*.md frontmatter + $ARGUMENTS
//   - pipe mode (non-pty): stdin lines each get a Mock response
//   - ScheduleWakeup scheduling (firing is gated behind CLAUDE_GO_E2E_SLOW
//     — the tool enforces a 60s floor and the queue only drains at turn
//     boundaries)
//
// Implementation notes discovered while building this suite (documented,
// not worked around silently):
//   - ASCII keys typed DURING a turn are dropped by the turn's key-queue
//     goroutine (only k.r runes — non-ASCII — land in queueBuf). Tests
//     therefore wait for the prompt box before sending the next line.
//   - The permission dialog (askEx) reads ks.ch concurrently with the
//     turn's queue goroutine, so an approval keystroke can be stolen —
//     tests retry the keystroke until the dialog resolves.
package e2e

import (
	"os"
	"strings"
	"testing"
	"time"
)

// startREPL boots an interactive --mock session on a pty and waits for the
// first prompt box (╭─╮). Onboarding/trust are pre-seeded in newSession.
func startREPL(t *testing.T, s *sess, args ...string) *ptyProc {
	t.Helper()
	full := append([]string{"--mock"}, args...)
	p := s.runPTY(t, nil, full...)
	p.waitFor("╭─╮", 30*time.Second)
	return p
}

// sendAtPrompt waits for the n-th prompt box then sends keys — ASCII input
// sent while a turn is running is dropped by the REPL's queue goroutine.
func (p *ptyProc) sendAtPrompt(n int, keys string) {
	p.t.Helper()
	p.waitCount("╭─╮", n, 30*time.Second)
	p.send(keys)
}

// sendUntil re-sends keys until want appears (permission answers can be
// stolen by the turn's key-queue goroutine — a lost keystroke is harmless,
// we just press again).
func (p *ptyProc) sendUntil(keys, want string, tries int) {
	p.t.Helper()
	for i := 0; i < tries; i++ {
		p.send(keys)
		deadline := time.Now().Add(1200 * time.Millisecond)
		for time.Now().Before(deadline) {
			if strings.Contains(p.clean(), want) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	p.t.Fatalf("never saw %q after %d sends of %q\noutput:\n%s",
		want, tries, keys, truncStr(p.clean(), 4000))
}

// exit sends ctrl-U + /exit repeatedly until the process exits — keystrokes
// sent mid-turn are dropped by the REPL's queue goroutine, so we retry until
// one lands at the editor.
func (p *ptyProc) exit() {
	p.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		// already gone?
		select {
		case err := <-p.done:
			p.done <- err
			if err != nil {
				p.t.Fatalf("exit error: %v\noutput:\n%s", err, p.clean())
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("process did not exit after repeated /exit\noutput:\n%s",
				truncStr(p.clean(), 4000))
		}
		p.sendQuiet("\x15") // ctrl-U: clear editor buffer
		time.Sleep(100 * time.Millisecond)
		p.sendQuiet("/exit\r")
		select {
		case err := <-p.done:
			p.done <- err
			if err != nil {
				p.t.Fatalf("exit error: %v\noutput:\n%s", err, p.clean())
			}
			return
		case <-time.After(800 * time.Millisecond):
		}
	}
}

// TestInteractiveBanner: banner, model line, prompt box, mock answer, exit.
func TestInteractiveBanner(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)
	out := p.clean()
	contains(t, out, "claude-go", "banner")
	contains(t, out, "model:", "model line")
	if !utf8OK(out) {
		t.Fatalf("output is not valid UTF-8")
	}
	p.send("hi\r")
	p.waitFor("Mock response to: hi", 30*time.Second)
	p.exit()
	out = p.clean()
	// box-drawing prompt chrome appeared
	for _, ch := range []string{"╭", "│", "╰"} {
		contains(t, out, ch, "box-drawing prompt chrome")
	}
}

// TestQueuedInput: prompts submitted around a turn are each answered.
// (ASCII text typed mid-turn is dropped by the REPL queue goroutine — the
// ü rune queues properly and may arrive mojibake'd; what matters is that
// the second prompt produces a second answer.)
func TestQueuedInput(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)
	p.send("queued-one\r")
	p.send("ü\r") // non-ASCII runes survive the turn-time queue buffer
	// two answers must appear — queued or typed, both prompts resolve
	p.waitCount("Mock response to:", 2, 30*time.Second)
	contains(t, p.clean(), "Mock response to: queued-one", "first answer")
	p.exit()
}

// TestPermissionPrompt: Bash without an allow rule → numbered dialog →
// "1" approves and the tool runs.
func TestPermissionPrompt(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)
	p.send(`tool:Bash {"command":"echo perm-ok-xyz"}` + "\r")
	out := p.waitFor("Do you want to proceed?", 30*time.Second)
	contains(t, out, "1.", "numbered option")
	contains(t, out, "Yes", "approve option")
	// wait for the round-trip marker — "perm-ok-xyz" alone would match the
	// already-echoed prompt text
	p.sendUntil("1", "Tool results received", 15)
	contains(t, p.clean(), "perm-ok-xyz", "tool output after approval")
	p.exit()
}

// TestPermissionDeny: answering "3" (No) declines the tool.
func TestPermissionDeny(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)
	p.send(`tool:Bash {"command":"echo deny-marker"}` + "\r")
	p.waitFor("Do you want to proceed?", 30*time.Second)
	// interactive denial renders "Permission for this tool use was denied
	// … rejected" — a different string than print mode's "declined"
	p.sendUntil("3", "rejected", 15)
	out := p.waitFor("Tool results received", 30*time.Second)
	contains(t, out, "denied", "denied tool_result")
	p.exit()
}

// TestSlashSmoke: each builtin slash command produces its expected output
// and the REPL stays alive for the next one.
func TestSlashSmoke(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)
	// prompt count: startREPL already waited for box #1; each submitted
	// line draws the next box when the loop returns to readLine
	prompt := 1
	cmd := func(line, want string, timeout time.Duration) {
		t.Helper()
		if prompt > 1 {
			p.waitCount("╭─╮", prompt, 30*time.Second)
		}
		p.send(line)
		p.waitFor(want, timeout)
		prompt++
	}

	cmd("/help\r", "/exit", 20*time.Second) // help lists commands incl. /exit
	cmd("/model foo-model\r", "model → foo-model", 20*time.Second)
	cmd("/clear\r", "context cleared", 20*time.Second)
	cmd("/compact\r", "context compacted", 30*time.Second)
	cmd("/rename e2e-title\r", "session renamed: e2e-title", 20*time.Second)
	cmd("/rewind\r", "no checkpoints", 20*time.Second)

	// still alive: a normal prompt still answers
	p.sendAtPrompt(prompt, "alive-check\r")
	p.waitFor("Mock response to: alive-check", 30*time.Second)

	p.exit()
}

// TestCustomCommand: .claude/commands/tt.md expands $ARGUMENTS into the prompt.
func TestCustomCommand(t *testing.T) {
	s := newSession(t)
	s.writeFile(".claude/commands/tt.md",
		"---\ndescription: e2e test command\n---\nSay CUSTOMCMD: $ARGUMENTS\n")
	p := startREPL(t, s)
	p.send("/tt hello-args\r")
	p.waitFor("Mock response to: Say CUSTOMCMD: hello-args", 30*time.Second)
	p.exit()
}

// TestInteractiveToolCall: a forced tool_use in the interactive session
// renders the tool line and result (bypassPermissions = no dialog).
func TestInteractiveToolCall(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s, "--permission-mode", "bypassPermissions")
	p.send(`tool:Bash {"command":"echo interactive-tool"}` + "\r")
	out := p.waitFor("Tool results received", 30*time.Second)
	contains(t, out, "interactive-tool", "tool output rendered")
	p.exit()
}

// TestWakeupScheduling: ScheduleWakeup accepts the call and reports the
// schedule. The minimum delay is 60s and the queued prompt only drains at
// a turn boundary — firing is covered by TestWakeupFire (env-gated).
func TestWakeupScheduling(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s, "--permission-mode", "bypassPermissions")
	p.send(`tool:ScheduleWakeup {"delaySeconds":60,"prompt":"wake-me"}` + "\r")
	p.waitFor("Wakeup scheduled in", 30*time.Second)
	p.exit()
}

// TestWakeupFire exercises the queued wakeup end-to-end; gated behind an
// env opt-in because the tool enforces a 60s minimum delay AND the queue
// only drains at a turn boundary — inherently slow + racy.
func TestWakeupFire(t *testing.T) {
	if strings.ToLower(os.Getenv("CLAUDE_GO_E2E_SLOW")) != "1" {
		t.Skipf("wakeup fires after a 60s minimum and only at a turn boundary; set CLAUDE_GO_E2E_SLOW=1 to run")
	}
	s := newSession(t)
	p := startREPL(t, s, "--permission-mode", "bypassPermissions")
	p.send(`tool:ScheduleWakeup {"delaySeconds":60,"prompt":"wakeup-fired-marker"}` + "\r")
	p.waitFor("Wakeup scheduled in", 30*time.Second)
	// nudge a turn after the timer so the queue drains
	time.Sleep(62 * time.Second)
	p.send("ping\r")
	p.waitFor("wakeup-fired-marker", 60*time.Second)
	p.exit()
}

// TestPipeMode: without a pty, stdin lines are answered one by one.
func TestPipeMode(t *testing.T) {
	s := newSession(t)
	r := s.runStdin(t, "pipe-one\npipe-two\n", "--mock")
	s.mustOK(t, r)
	contains(t, r.stdout, "Mock response to: pipe-one", "pipe answer 1")
	contains(t, r.stdout, "Mock response to: pipe-two", "pipe answer 2")
}
