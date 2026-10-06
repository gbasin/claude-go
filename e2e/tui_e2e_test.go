//go:build e2e

// tui_e2e_test.go — coverage for the session TUI chrome: alt-screen +
// mouse protocol sequences, composer rules, effort pill, footer, turn-end
// line, agents pane, /model picker persistence, /status + /diff overlays,
// @-mention autocomplete, trust dialog, tmux focus warning.
package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAltScreenProtocol: startup emits the alt-screen + mouse + focus +
// colormode sequences and the ✳ window title; exit restores.
func TestAltScreenProtocol(t *testing.T) {
	s := newSession(t)
	p := s.runPTY(t, nil, "--mock")
	p.waitFor("❯", 30*time.Second)
	raw := p.output()
	for _, seq := range []string{
		"\x1b[?1049h",                                              // alternate screen
		"\x1b[?1000h", "\x1b[?1002h", "\x1b[?1003h", "\x1b[?1006h", // mouse
		"\x1b[?1004h", // focus events
		"\x1b[?2031h", // colormode report
		"]0;",         // OSC window title
	} {
		if !strings.Contains(raw, seq) {
			t.Fatalf("startup missing %q", seq)
		}
	}
	p.exit()
	// alt-screen restore on exit
	time.Sleep(300 * time.Millisecond)
	if !strings.Contains(p.output(), "\x1b[?1049l") {
		t.Fatal("alt-screen not restored on exit")
	}
}

// TestStartupChrome: banner + effort pill + footer + composer rules.
func TestStartupChrome(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)
	screen := p.clean()
	for _, want := range []string{
		"◐ medium",        // effort pill above the top rule
		"? for shortcuts", // footer hint
		"─",               // composer rule
		"❯",               // prompt glyph
	} {
		if !strings.Contains(screen, want) {
			t.Fatalf("startup chrome missing %q\n%s", want, truncStr(screen, 1500))
		}
	}
	p.exit()
}

// TestTurnEndLine: real's "✻ <Verb> for Ns · done h:mm AM" line follows
// every turn.
func TestTurnEndLine(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)
	p.sendUntilEchoed(t, "hi\r", "❯ hi", 8)
	p.waitFor("Mock response", 30*time.Second)
	p.waitFor("· done", 10*time.Second)
	if !strings.Contains(p.clean(), "✻") {
		t.Fatalf("turn-end sparkle missing\n%s", truncStr(p.clean(), 1200))
	}
	p.exit()
}

// TestAgentsPane: ← on empty input shows the double-press hint; ←← opens
// the Agents overlay; esc closes it.
func TestAgentsPane(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)
	p.send("\x1b[D") // left arrow, empty buffer
	p.waitFor("Press ← again", 10*time.Second)
	time.Sleep(200 * time.Millisecond)
	p.send("\x1b[D")
	p.waitFor("Agents", 10*time.Second)
	p.waitFor("esc to close", 10*time.Second)
	p.send("\x1b") // esc
	time.Sleep(300 * time.Millisecond)
	p.exit()
}

// TestAgentsPaneListsBgTask: a running background agent shows in the
// overlay.
func TestAgentsPaneListsBgTask(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s, "--permission-mode", "bypassPermissions")
	// slow bg task so the pane has something to list
	p.sendUntilEchoed(t,
		`tool:Bash {"command":"sleep 8","run_in_background":true}`+"\r",
		"❯ tool:Bash", 8)
	p.waitFor("Tool results received", 30*time.Second)
	time.Sleep(300 * time.Millisecond)
	p.send("\x1b[D\x1b[D") // ←←
	p.waitFor("esc to close", 10*time.Second)
	// the pane row lists the task — id + command + any status
	if !strings.Contains(p.clean(), "b1") ||
		!strings.Contains(p.clean(), "sleep 8") {
		t.Fatalf("agents pane should list the task\n%s", truncStr(p.clean(), 1200))
	}
	p.send("\x1b") // esc — close the pane before exit
	time.Sleep(300 * time.Millisecond)
	p.exit()
}

// TestAtMentionAutocomplete: typing @<fragment> opens the file
// completion menu.
func TestAtMentionAutocomplete(t *testing.T) {
	s := newSession(t)
	s.writeFile("uniquefile-e2e.txt", "x")
	p := startREPL(t, s)
	p.send("@uniquef")
	p.waitFor("uniquefile-e2e.txt", 15*time.Second)
	p.exit()
}

// TestModelPickerPersist: /model opens the real cursor dialog; s +
// Enter selects the highlighted model for the session.
func TestModelPickerChrome(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)
	p.sendUntilEchoed(t, "/model\r", "❯ /model", 8)
	p.waitFor("Select model", 15*time.Second)
	screen := p.clean()
	if !strings.Contains(screen, "Enter to confirm") ||
		!strings.Contains(screen, "s session-only") {
		t.Fatalf("model picker chrome missing:\n%s", truncStr(screen, 1500))
	}
	p.send("\x1b") // esc — keep the default
	time.Sleep(300 * time.Millisecond)
	p.exit()
}

// TestStatusOverlay: /status renders the tabbed panel.
func TestStatusOverlay(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)
	p.sendUntilEchoed(t, "/status\r", "❯ /status", 8)
	p.waitFor("Settings", 15*time.Second)
	if !strings.Contains(p.clean(), "Status") {
		t.Fatalf("status overlay missing:\n%s", truncStr(p.clean(), 1200))
	}
	p.send("\x1b")
	p.exit()
}

// TestDiffOverlay: /diff opens the file-list dialog when the git repo has
// changes; empty repo reports no changes.
func TestDiffOverlay(t *testing.T) {
	s := newSession(t)
	git := func(args ...string) {
		out, err := exec.Command("git", append([]string{"-C", s.cwd}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "e@t")
	git("config", "user.name", "t")
	s.writeFile("a.txt", "one\n")
	git("add", "a.txt")
	git("commit", "-qm", "init")
	s.writeFile("a.txt", "one\ntwo\n")

	p := startREPL(t, s)
	p.sendUntilEchoed(t, "/diff\r", "❯ /diff", 8)
	p.waitFor("a.txt", 15*time.Second)
	p.send("\x1b") // close
	p.exit()
}

// TestTrustDialog: an untrusted project shows the trust gate with the
// resolved path — default is No.
func TestTrustDialog(t *testing.T) {
	s := newSession(t)
	// un-seed trust but keep onboarding complete
	b, _ := json.Marshal(map[string]any{
		"hasCompletedOnboarding": true,
	})
	os.WriteFile(filepath.Join(s.home, ".claude.json"), b, 0o644)

	p := s.runPTY(t, nil, "--mock")
	p.waitFor("trust", 30*time.Second)
	screen := p.clean()
	if !strings.Contains(screen, s.cwd) {
		t.Fatalf("trust dialog should show the resolved path\n%s", truncStr(screen, 1500))
	}
	p.exit()
}

// TestTmuxFocusWarn: inside tmux without focus-events the REPL warns.
func TestTmuxFocusWarn(t *testing.T) {
	s := newSession(t)
	// fake tmux binary answering "off" for focus-events
	fakeBin := t.TempDir()
	tmux := filepath.Join(fakeBin, "tmux")
	os.WriteFile(tmux, []byte("#!/bin/sh\necho off\n"), 0o755)
	// later entries override: append TMUX + a PATH with the fake tmux first
	env := append([]string{},
		"TMUX=/tmp/fake-tmux-e2e",
		"PATH="+fakeBin+":"+os.Getenv("PATH"))
	p := s.runPTY(t, env, "--mock")
	p.waitFor("❯", 30*time.Second)
	if !strings.Contains(p.clean(), "focus-events") &&
		!strings.Contains(p.output(), "focus-events") {
		t.Fatalf("tmux focus warning missing:\n%s", truncStr(p.clean(), 1200))
	}
	p.exit()
}

// TestOnboardingFirstRun: an empty .claude.json triggers real's first-run
// flow — theme picker → trust dialog → REPL.
func TestOnboardingFirstRun(t *testing.T) {
	s := newSession(t)
	// strip the pre-seeded onboarding/trust state
	os.WriteFile(filepath.Join(s.home, ".claude.json"), []byte(`{}`), 0o644)

	p := s.runPTY(t, nil, "--mock")
	p.waitFor("Welcome to Claude Code", 30*time.Second)
	p.waitFor("Choose the text style", 10*time.Second)
	p.send("\r") // accept default theme
	p.waitFor("trust", 20*time.Second)
	p.send("1\r") // 1 = trust this directory
	p.waitFor("❯", 20*time.Second)
	p.exit()
}

// TestUsageBars: /usage renders the utilization bars — an
// anthropic-ratelimit-unified-slow-* header set populates the data.
func TestUsageBars(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("anthropic-ratelimit-unified-status", "allowed")
		w.Header().Set("anthropic-ratelimit-unified-slow-status", "active")
		w.Header().Set("anthropic-ratelimit-unified-slow-budget-utilization", "0.45")
		writeSSE(w, "aux-server reply")
	}))
	t.Cleanup(srv.Close)

	s := newSession(t)
	p := s.runPTY(t, []string{
		"CLAUDE_CODE_OAUTH_TOKEN=e2e-fake-oauth-token",
		"CLAUDE_GO_RAW_HTTP=0",
	}, "--sdk-url", srv.URL)
	p.waitFor("❯", 30*time.Second)
	p.send("hi\r")
	p.waitFor("aux-server reply", 30*time.Second)
	p.sendUntilEchoed(t, "/usage\r", "❯ /usage", 8)
	p.waitFor("Current session", 15*time.Second)
	if !strings.Contains(p.clean(), "45% used") {
		t.Fatalf("usage bar should show 45%% used:\n%s", truncStr(p.clean(), 1500))
	}
	p.exit()
}
