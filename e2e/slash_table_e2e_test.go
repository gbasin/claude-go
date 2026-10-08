//go:build e2e

// slash_table_e2e_test.go — table-driven PTY smoke over the deterministic
// slash-command surface. Each entry types "/cmd" and asserts the command's
// expected output renders and the REPL stays alive for the next one.
// Interactive pickers (/model, /resume, /permissions, /config), network-bound
// commands (/login, /schedule, /design-*, /ultraplan, /teleport) and
// turn-spawning prompts (/init, /review, /doctor, /hooks) are covered
// elsewhere or deliberately excluded.
//
// want strings are chosen so they never appear in the echoed input line
// ("/theme" can't assert "theme" — the echo contains it) and assertions only
// scan output appended after the send, so a previous command's text in
// scrollback can't false-positive.
package e2e

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestSlashTable(t *testing.T) {
	s := newSession(t)
	// stub `open` so /radio can't launch a real browser tab
	stubDir := t.TempDir()
	os.WriteFile(stubDir+"/open", []byte("#!/bin/sh\nexit 0\n"), 0o755)
	p := s.runPTY(t, []string{"PATH=" + stubDir + ":" + os.Getenv("PATH")}, "--mock")
	p.waitFor("❯", 30*time.Second)

	table := []struct {
		cmd  string
		want string
		alt  string // second accepted output
	}{
		{"/status", "Version:", ""},
		{"/context", "context tokens", ""},
		{"/cost", "this session:", ""},
		{"/usage", "this session:", ""},
		{"/stats", "this session:", ""},
		{"/agents", "wizard has been removed", ""},
		// /todos was an invented command — real 294 has no such row
		{"/vim", "moved to /config", ""},
		{"/keybindings", "keybindings.json", ""},
		{"/ide", "no IDE detected", "IDE connected"},
		{"/terminal-setup", "unsupported terminal", "macOS terminals only"}, // darwin: TERM_PROGRAM unset; linux: not a macOS terminal
		{"/privacy-settings", "telemetry", ""},
		{"/release-notes", "claude-go", ""},
		{"/plugin", "Plugins", ""},
		{"/wellbeing", "break reminders", ""},
		{"/powerup", "discover features", ""},
		{"/skills", "Skills:", ""},
		{"/mcp", "MCP servers", ""},
		{"/memory", "memory dir:", ""},
		{"/tasks", "background tasks", ""},
		{"/diff", "git", ""}, // temp cwd isn't a repo → "not a git repo or no changes"
		{"/env", "environment variables", ""},
		{"/experiments", "experimental", ""},
		{"/effort", "Faster", ""},
		{"/effort bogus", "effort levels", ""},
		{"/advisor", "advisor:", ""},
		{"/autocompact", "auto-compact window", ""},
		{"/scroll-speed", "scroll speed", ""},
		{"/settings-review", "no staged settings changes", ""},
		{"/output-style", "Available styles", ""},
		{"/btw", "usage: /btw", ""},
		{"/subtask", "usage: /subtask", ""},
		// real /import is tengu_import-gated (default off) → unknown
		{"/import", "unknown command", ""},
		{"/add-dir", "usage: /add-dir", ""},
		{"/loop", "usage: /loop", ""},
		// /alias was invented — not in real's 294 tables → unknown
		{"/alias bad-arg", "unknown command", ""},
		{"/extra-usage", "renamed to /usage-credits", ""},
		{"/usage-credits", "/login", ""},
		{"/theme", "theme:", "daltonian"},
		{"/chrome", "Chrome", ""},
		{"/export exp-out.jsonl", "exported to", "export failed"},
		// real /fork <directive> spawns a background fork agent — a bare
		// /fork is a usage error
		{"/fork", "usage: /fork", ""},
		{"/branch", "branched session", ""},
		{"/permissions", "Permissions", ""},
		{"/bug", "report issues at", ""},
		{"/issue", "file an issue", ""},
		{"/radio", "laude", ""}, // Claude FM stub or URL
		{"/stop", "nothing running", ""},
		{"/schedule", "claude.ai account", ""},
		{"/voice", "Voice mode", ""},
		{"/ant-trace", "trace output", ""},
		{"/cloud-plugins", "plugins", ""},
		{"/ultraplan", "ultraplan", ""},
		{"/heapdump", "heap", "disabled"},
		// state toggles last — they flip session-local flags
		{"/goal clear", "goal cleared", ""},
		{"/pause-memory", "memory paused", ""},
		{"/focus", "Focus view", ""},
		{"/brief", "brief mode", ""},
		{"/sandbox", "Sandbox Mode Overrides Config", ""},
		{"/fast", "fast mode", ""},
	}

	for i, tc := range table {
		mark := len(p.output()) // only scan output appended after this send
		p.send(tc.cmd + "\r")
		deadline := time.Now().Add(20 * time.Second)
		for {
			newOut := stripANSI(p.output()[mark:])
			if strings.Contains(newOut, tc.want) || (tc.alt != "" && strings.Contains(newOut, tc.alt)) {
				p.sendQuiet("\x1b") // dismiss modal dialogs (/status, /model…)
				time.Sleep(300 * time.Millisecond)
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("/%s (#%d): never saw %q (alt %q)\nnew output was:\n%s",
					tc.cmd, i, tc.want, tc.alt, truncStr(newOut, 3000))
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// still alive: a normal prompt still answers after ~50 commands
	p.send("alive-after-table\r")
	p.waitFor("Mock response to: alive-after-table", 30*time.Second)
	p.exit()
}

// TestCopyCmd: /copy copies the last assistant text to the clipboard
// (pbcopy stubbed on PATH — the print confirms the path ran).
func TestCopyCmd(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)
	p.sendUntilEchoed(t, "hi\r", "❯ hi", 8)
	p.waitFor("Mock response", 30*time.Second)
	p.sendUntilEchoed(t, "/copy\r", "❯ /copy", 8)
	p.waitFor("copied", 15*time.Second)
	p.exit()
}

// TestAdvisorFlag: --advisor <model> pre-seeds the advisor model.
func TestAdvisorFlag(t *testing.T) {
	s := newSession(t)
	p := s.runPTY(t, nil, "--mock", "--advisor", "claude-e2e-advisor-model")
	p.waitFor("❯", 30*time.Second)
	p.sendUntilEchoed(t, "/advisor\r", "❯ /advisor", 8)
	p.waitFor("claude-e2e-advisor-model", 15*time.Second)
	p.exit()
}
