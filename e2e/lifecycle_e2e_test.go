//go:build e2e

// lifecycle_e2e_test.go — interactive PTY coverage for session-lifecycle
// and input-layer surface: keybindings.json remaps/warnings, /rewind
// restoring a checkpointed file write, /clear resetting the loop, and a
// compacted session staying resumable.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
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
		`❯ calls:[{"name":"Read"`, 8)
	p.waitFor("Tool results received", 30*time.Second)
	if b, _ := os.ReadFile(target); string(b) != "v1-overwritten" {
		t.Fatalf("write didn't land: %q", b)
	}

	p.sendUntilEchoed(t, "/rewind\r", "❯ /rewind", 8)
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

	p.sendUntilEchoed(t, "/clear\r", "❯ /clear", 8)
	p.waitFor("context cleared", 15*time.Second)

	p.sendUntilEchoed(t, "/context\r", "❯ /context", 8)
	p.waitFor("messages: 0", 15*time.Second)

	// a fresh turn still works after the reset
	p.sendUntilEchoed(t, "second-wind\r", "❯ second-wind", 8)
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
		"❯ tool:AskUserQuestion", 8)
	p.waitFor("Pick a color", 30*time.Second)
	p.waitFor("Blue", 10*time.Second)

	// cursor-select dialog: down to Blue, Enter confirms
	p.sendUntil("\x1b[B\r", "Tool results received", 15)
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
		"❯ tool:Agent", 8)
	// waitFor polls the live screen each tick — the subagent's reply
	// surfaces inside the parent turn's tool result while displayed (a
	// post-hoc clean() only sees the current screen, which scrolls past)
	p.waitFor("Mock response to:", 60*time.Second)
	p.exit()
}

// TestInterruptMidTurn: esc during a retrying turn (mock 5xx → SDK
// backoff) interrupts the turn; the REPL stays alive afterward.
func TestInterruptMidTurn(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)

	p.sendUntilEchoed(t, "error:5*\r", "❯ error:5*", 8)
	p.waitFor("esc to interrupt", 30*time.Second) // spinner up = turn running
	p.send("\x1b")
	p.waitFor("Interrupted", 15*time.Second)

	p.sendUntilEchoed(t, "alive-after-interrupt\r", "❯ alive-after-interrupt", 8)
	p.waitFor("Mock response to: alive-after-interrupt", 30*time.Second)
	p.exit()
}

// TestExitPlanModeApproval: ExitPlanMode surfaces the approval dialog;
// answering 1 exits plan mode and the tool result reports it.
func TestExitPlanModeApproval(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s, "--permission-mode", "bypassPermissions")

	// ExitPlanMode is deferred — ToolSearch loads its schema first
	p.sendUntilEchoed(t,
		`calls:[{"name":"ToolSearch","input":{"query":"select:ExitPlanMode"}},`+
			`{"name":"ExitPlanMode","input":{"plan":"do the thing"}}]`+"\r",
		`❯ calls:[{"name":"ToolSearch"`, 8)
	// approval renders as the numbered permission dialog ("don't ask again
	// for ExitPlanMode in <dir>") — 1 = Yes
	p.waitFor("Do you want to proceed?", 30*time.Second)
	p.sendUntil("1", "Tool results received", 15)
	if !strings.Contains(p.clean(), "Exited Plan Mode") {
		t.Fatalf("approval should exit plan mode\noutput:\n%s", truncStr(p.clean(), 3000))
	}
	p.exit()
}

// TestSessionEndHook: the SessionEnd hook fires on the interactive quit
// path (side-effect file proves it ran regardless of stdout handling).
func TestSessionEndHook(t *testing.T) {
	s := newSession(t)
	marker := filepath.Join(s.cwd, "sessend.marker")
	s.writeSettings(fmt.Sprintf(
		`{"hooks":{"SessionEnd":[{"hooks":[{"type":"command","command":"touch %s"}]}]}}`,
		marker))
	p := startREPL(t, s)
	p.exit()
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("SessionEnd hook did not run on exit")
}

// TestWebFetchPrivateRefusal: the SSRF guard refuses loopback/private
// targets e2e — HTTP is upgraded to HTTPS and private IPs are rejected.
func TestWebFetchPrivateRefusal(t *testing.T) {
	s := newSession(t)
	// WebFetch is deferred — ToolSearch loads its schema in the same turn
	r := s.run(t, "--mock", "-p",
		`calls:[{"name":"ToolSearch","input":{"query":"select:WebFetch"}},`+
			`{"name":"WebFetch","input":{"url":"http://127.0.0.1:1/x","prompt":"summarize"}}]`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "loopback or private") {
		t.Fatalf("private-address fetch should be refused: %s", truncStr(r.stdout, 2000))
	}
}

// TestEscEscRewindPicker: double-esc opens the rewind selector listing past
// user messages; picking 1 truncates the conversation to before it.
func TestEscEscRewindPicker(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)

	p.send("first-msg\r")
	p.waitFor("Mock response to: first-msg", 30*time.Second)
	p.sendUntilEchoed(t, "second-msg\r", "❯ second-msg", 8)
	p.waitFor("Mock response to: second-msg", 30*time.Second)

	// double-esc within 400ms opens the picker — retry until it lands
	// (keys sent while the turn is still closing get dropped; an extra
	// esc-esc on an open picker would cancel it, so poll between sends)
	opened := false
	for i := 0; i < 10 && !opened; i++ {
		p.send("\x1b\x1b")
		deadline := time.Now().Add(900 * time.Millisecond)
		for time.Now().Before(deadline) {
			if strings.Contains(p.clean(), "Rewind to:") {
				opened = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !opened {
		t.Fatalf("rewind picker never opened\noutput:\n%s", truncStr(p.clean(), 3000))
	}
	out := p.clean()
	if !strings.Contains(out, "first-msg") || !strings.Contains(out, "second-msg") {
		t.Fatalf("picker should list both user messages\noutput:\n%s", truncStr(out, 3000))
	}
	// cursor-select: ↑ picks the earlier message, Enter rewinds
	p.send("\x1b[A\r")
	p.waitFor("rewound to before", 15*time.Second)

	// the picker can still own the key channel momentarily — resend
	// /context until its output lands (lost keystrokes are harmless)
	p.sendUntil("/context\r", "messages:", 12)
	p.exit()
}

// TestVimModeEditor: editorMode:vim in ~/.claude.json starts the editor in
// modal mode — esc enters normal mode where letters don't insert, i
// returns to insert.
func TestVimModeEditor(t *testing.T) {
	s := newSession(t)
	// rewrite .claude.json keeping the seeded trust/onboarding state
	s.writeFile(filepath.Join(s.home, ".claude.json"), fmt.Sprintf(
		`{"hasCompletedOnboarding":true,"editorMode":"vim",`+
			`"projects":{%q:{"hasTrustDialogAccepted":true}}}`, s.cwd))
	p := startREPL(t, s)

	mark := len(p.output())
	// esc → normal mode; letters are motions, not text
	p.send("\x1b")
	time.Sleep(300 * time.Millisecond)
	p.send("hj") // motions, not text
	time.Sleep(400 * time.Millisecond)
	if strings.Contains(stripANSI(p.output()[mark:]), "❯ hj") {
		t.Fatalf("normal-mode keys should not insert text")
	}
	// i → insert mode; typing echoes
	p.send("i")
	time.Sleep(300 * time.Millisecond)
	p.send("vim-typed\r")
	p.waitFor("Mock response to: vim-typed", 30*time.Second)
	p.exit()
}

// TestLoopCloudOffer: /loop with an interval ≥60m triggers real's
// cloud-schedule upsell; picking "This session only" arms the local ticker.
func TestLoopCloudOffer(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)

	p.sendUntilEchoed(t, "/loop 2h check the build\r", "❯ /loop 2h", 8)
	p.waitFor("cloud schedule instead", 15*time.Second)
	p.sendUntil("2\r", "loop armed: every 7200s", 15)
	p.exit()
}

// TestLoopUsage: bare /loop and /loop 5m (interval without prompt) print
// usage, not a silent no-op.
func TestLoopUsage(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)
	p.sendUntilEchoed(t, "/loop\r", "❯ /loop", 8)
	p.waitFor("usage: /loop", 15*time.Second)
	p.exit()
}

// TestAgentToolBackground: default run_in_background launches the subagent
// and returns the task handle; its completion notice lands on idle.
func TestAgentToolBackground(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s, "--permission-mode", "bypassPermissions")

	p.sendUntilEchoed(t,
		`tool:Agent {"description":"bg e2e","prompt":"bg marker question"}`+"\r",
		"❯ tool:Agent", 8)
	p.waitFor("Launched agent", 30*time.Second)

	// the subagent turn runs against the mock; the completion notice is
	// delivered on idle — poke the REPL so it flushes
	p.sendUntilEchoed(t, "wake\r", "❯ wake", 8)
	p.waitFor("Mock response to: wake", 30*time.Second)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(p.clean(), "completed") {
			p.exit()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("background agent completion notice never surfaced\noutput:\n%s",
		truncStr(p.clean(), 3000))
}

// TestResumePickerInteractive: /resume opens the session picker listing
// prior sessions for this cwd; enter resumes the top entry.
func TestResumePickerInteractive(t *testing.T) {
	s := newSession(t)
	// seed a prior session transcript
	r := s.run(t, "--mock", "-p", "seed prior session")
	s.mustOK(t, r)

	p := startREPL(t, s)
	p.sendUntilEchoed(t, "/resume\r", "❯ /resume", 8)
	p.waitFor("seed prior session", 15*time.Second) // picker lists the preview
	p.send("\r")
	p.waitFor("resumed session", 15*time.Second)

	// resumed session answers normally
	p.sendUntilEchoed(t, "resumed-alive\r", "❯ resumed-alive", 8)
	p.waitFor("Mock response to: resumed-alive", 30*time.Second)
	p.exit()
}

// TestWebSearchBackend: the deferred WebSearch tool dispatches through the
// Searcher backend — a haiku request carrying the server-side
// web_search_20250305 tool with max_uses and domain filters intact.
func TestWebSearchBackend(t *testing.T) {
	s := newSession(t)
	recDir := filepath.Join(t.TempDir(), "reqs")
	r := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + recDir},
		60*time.Second, "--mock", "-p",
		`calls:[{"name":"ToolSearch","input":{"query":"select:WebSearch"}},`+
			`{"name":"WebSearch","input":{"query":"e2e query","allowed_domains":["example.com"]}}]`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "Mock response to: e2e query") {
		t.Fatalf("search backend reply should surface in the tool result: %s",
			truncStr(r.stdout, 2000))
	}

	// the search request is a second /v1/messages — find it in the recording
	ents, err := os.ReadDir(recDir)
	if err != nil || len(ents) < 2 {
		t.Fatalf("expected ≥2 recorded requests (main + search), got %v err=%v", ents, err)
	}
	var found bool
	for _, e := range ents {
		raw, _ := os.ReadFile(filepath.Join(recDir, e.Name()))
		if !bytes.Contains(raw, []byte("web_search_20250305")) {
			continue
		}
		found = true
		var req struct {
			Model string            `json:"model"`
			Tools []json.RawMessage `json:"tools"`
		}
		json.Unmarshal(raw, &req)
		if !strings.Contains(req.Model, "haiku") {
			t.Fatalf("search should use the haiku lane, got model %q", req.Model)
		}
		var ws struct {
			MaxUses        int      `json:"max_uses"`
			AllowedDomains []string `json:"allowed_domains"`
		}
		for _, tl := range req.Tools {
			if bytes.Contains(tl, []byte("web_search_20250305")) {
				json.Unmarshal(tl, &ws)
			}
		}
		if ws.MaxUses == 0 {
			t.Fatal("web_search tool block missing max_uses")
		}
		if len(ws.AllowedDomains) != 1 || ws.AllowedDomains[0] != "example.com" {
			t.Fatalf("allowed_domains not forwarded: %v", ws.AllowedDomains)
		}
	}
	if !found {
		t.Fatal("no recorded request carried the web_search server tool")
	}
}

// TestWebSearchBudget: CLAUDE_CODE_MAX_WEB_SEARCHES_PER_SESSION caps the
// tool per session — the second call gets real's budget refusal text.
func TestWebSearchBudget(t *testing.T) {
	s := newSession(t)
	r := s.runEx(t, []string{"CLAUDE_CODE_MAX_WEB_SEARCHES_PER_SESSION=1"},
		60*time.Second, "--mock", "-p",
		`calls:[{"name":"ToolSearch","input":{"query":"select:WebSearch"}},`+
			`{"name":"WebSearch","input":{"query":"first"}},`+
			`{"name":"WebSearch","input":{"query":"second"}}]`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "web search budget") {
		t.Fatalf("second search should hit the session budget: %s", truncStr(r.stdout, 2000))
	}
}

// TestAutoCompact: a tiny autoCompactWindow forces an auto-compact — the
// transcript gains a compact boundary + summary record.
func TestAutoCompact(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)
	p.sendUntilEchoed(t, "turn one\r", "❯ turn one", 8)
	p.waitFor("Mock response", 30*time.Second)
	p.exit()

	// reopen with a tiny window — next turn auto-compacts
	rec := filepath.Join(t.TempDir(), "reqs")
	r := s.runEx(t, []string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW=32000",
		"CLAUDE_GO_MOCK_RECORD=" + rec}, 60*time.Second,
		"--mock", "-c", "-p", "turn two")
	s.mustOK(t, r)

	// transcript carries a compact boundary/summary marker
	dir := filepath.Join(s.home, ".claude", "projects")
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*.jsonl"))
	var sawCompact bool
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if bytes.Contains(b, []byte("isCompactSummary")) ||
			bytes.Contains(b, []byte(`"compact"`)) {
			sawCompact = true
		}
	}
	if !sawCompact {
		t.Fatalf("auto-compact should leave a compact record; transcript files: %v", files)
	}
}

// TestCustomSlashCommand: .claude/commands/*.md expand at submit — the
// body + $ARGUMENTS substitution reaches the model.
func TestCustomSlashCommand(t *testing.T) {
	s := newSession(t)
	s.writeFile(".claude/commands/greet.md",
		"---\ndescription: greet someone\n---\nSay hi to $ARGUMENTS\n")

	rec := filepath.Join(t.TempDir(), "reqs")
	p := s.runPTY(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, "--mock")
	p.waitFor("❯", 30*time.Second)
	p.sendUntilEchoed(t, "/greet Bob\r", "❯ /greet Bob", 8)
	p.waitFor("Mock response", 30*time.Second)
	p.exit()

	ents, _ := os.ReadDir(rec)
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(rec, e.Name()))
		if bytes.Contains(b, []byte("Say hi to Bob")) {
			return
		}
	}
	t.Fatal("custom slash expansion did not reach the wire")
}

// TestToolSearchKeyword: a non-select ToolSearch query returns ranked
// tool references instead of loading schemas.
func TestToolSearchKeyword(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p",
		`tool:ToolSearch {"query":"notebook"}`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "NotebookEdit") {
		t.Fatalf("keyword search should rank NotebookEdit: %s", truncStr(r.stdout, 1500))
	}
}

// TestHookPromptType: a PreToolUse type:"prompt" hook calls the Judge
// (haiku lane) — the judge request hits the API. An OK verdict is not a
// permission grant: the call still asks → denies non-interactively.
func TestHookPromptType(t *testing.T) {
	s := newSession(t)
	s.writeSettings(`{"hooks":{"PreToolUse":[{"matcher":"Bash",
		"hooks":[{"type":"prompt","prompt":"Is this command fine? JUDGE-PROMPT-E2E"}]}]}}`)

	rec := filepath.Join(t.TempDir(), "reqs")
	r := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, 60*time.Second,
		"--mock", "-p", `tool:Bash {"command":"echo judged"}`,
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, `"is_error":true`) {
		t.Fatalf("judge-OK should still resolve through perms → deny in -p: %s",
			truncStr(r.stdout, 1500))
	}
	ents, _ := os.ReadDir(rec)
	var sawJudge bool
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(rec, e.Name()))
		if bytes.Contains(b, []byte("JUDGE-PROMPT-E2E")) {
			sawJudge = true
		}
	}
	if !sawJudge {
		t.Fatal("prompt-hook judge call did not hit the API")
	}
}

// TestHistorySearch: ctrl+r opens the search prompt and matches a past
// prompt by substring.
func TestHistorySearch(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)
	p.sendUntilEchoed(t, "searchable-marker-99\r", "❯ searchable-marker-99", 8)
	p.waitFor("Mock response", 30*time.Second)
	// ctrl+r → type the query — resend until the search prompt renders
	for i := 0; i < 12; i++ {
		p.send("\x12") // ctrl+r
		p.send("searchable")
		if strings.Contains(p.clean(), "search prompts") {
			break
		}
		time.Sleep(600 * time.Millisecond)
	}
	p.waitFor("search prompts `searchable`", 10*time.Second)
	p.waitFor("searchable-marker-99", 10*time.Second)
	p.exit()
}

// TestQueuedMessage: a prompt submitted while a turn is running queues
// and fires after the turn ends.
func TestQueuedMessage(t *testing.T) {
	s := newSession(t)
	rec := filepath.Join(t.TempDir(), "reqs")
	p := s.runPTY(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec},
		"--mock", "--permission-mode", "bypassPermissions")
	p.waitFor("❯", 30*time.Second)
	// a 5s python sleep gives a wide mid-turn window (foreground `sleep`
	// is refused by the Bash guard, which would make this a false pass)
	p.sendUntilEchoed(t,
		`tool:Bash {"command":"python3 -c \"import time;time.sleep(5)\""}`+"\r",
		"❯ tool:Bash", 8)
	time.Sleep(800 * time.Millisecond) // well inside the sleep window
	p.send("queued-marker-e2e\r")
	p.waitFor("Mock response to: queued-marker-e2e", 30*time.Second)
	p.exit()

	var sawQueued bool
	ents, _ := os.ReadDir(rec)
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(rec, e.Name()))
		if bytes.Contains(b, []byte("queued-marker-e2e")) {
			sawQueued = true
		}
	}
	if !sawQueued {
		t.Fatal("queued prompt never reached the wire")
	}
}

// TestBracketedPaste: a multi-line paste collapses to a [Pasted text #N]
// placeholder in the editor and expands to <pasted_content> on the wire.
func TestBracketedPaste(t *testing.T) {
	s := newSession(t)
	rec := filepath.Join(t.TempDir(), "reqs")
	p := s.runPTY(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, "--mock")
	p.waitFor("❯", 30*time.Second)
	p.send("\x1b[200~paste-one\npaste-two\npaste-three\x1b[201~\r")
	p.waitFor("Mock response", 30*time.Second)
	p.exit()

	ents, _ := os.ReadDir(rec)
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(rec, e.Name()))
		if bytes.Contains(b, []byte("pasted_content")) &&
			bytes.Contains(b, []byte("paste-three")) {
			return
		}
	}
	t.Fatal("pasted_content block missing from the wire")
}
