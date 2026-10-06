//go:build e2e

// flags_e2e_test.go — CLI flag behavior verified through the binary, with
// MOCK_RECORD capturing the wire request where the flag's effect is a
// request-level change.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recordRequests runs -p with MOCK_RECORD enabled and returns the captured
// request bodies.
func recordRequests(t *testing.T, s *sess, args ...string) (runResult, [][]byte) {
	t.Helper()
	rec := filepath.Join(t.TempDir(), "reqs")
	all := append([]string{"--mock"}, args...)
	r := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, 60*time.Second, all...)
	var bodies [][]byte
	ents, _ := os.ReadDir(rec)
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(rec, e.Name()))
		bodies = append(bodies, b)
	}
	return r, bodies
}

func TestFlagModel(t *testing.T) {
	s := newSession(t)
	r, bodies := recordRequests(t, s, "-p", "hi", "--model", "claude-sonnet-5")
	s.mustOK(t, r)
	for _, b := range bodies {
		var req struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(b, &req) == nil && req.Model == "claude-sonnet-5" {
			return
		}
	}
	t.Fatal("no request carried --model claude-sonnet-5")
}

func TestFlagAppendSystemPrompt(t *testing.T) {
	s := newSession(t)
	r, bodies := recordRequests(t, s, "-p", "hi", "--append-system-prompt", "APP-SYS-E2E-MARKER")
	s.mustOK(t, r)
	for _, b := range bodies {
		if bytes.Contains(b, []byte("APP-SYS-E2E-MARKER")) {
			return
		}
	}
	t.Fatal("--append-system-prompt text missing from the wire")
}

func TestFlagSystemPrompt(t *testing.T) {
	s := newSession(t)
	r, bodies := recordRequests(t, s, "-p", "hi", "--system-prompt", "ONLY-SYS-E2E")
	s.mustOK(t, r)
	for _, b := range bodies {
		if bytes.Contains(b, []byte("ONLY-SYS-E2E")) {
			return
		}
	}
	t.Fatal("--system-prompt text missing from the wire")
}

func TestFlagAllowedDeniedTools(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p",
		`calls:[{"name":"Bash","input":{"command":"echo allowed-x"}},`+
			`{"name":"Bash","input":{"command":"rm -rf /tmp/nope"}}]`,
		"--allowedTools", "Bash(echo *)",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	// echo runs; the rm call is an unresolved ask → flat deny in -p (no
	// eval under mock)
	if !strings.Contains(r.stdout, "allowed-x") {
		t.Fatalf("allowed bash did not run: %s", truncStr(r.stdout, 1500))
	}
	if strings.Contains(r.stdout, `"content":"nope`) &&
		!strings.Contains(r.stdout, "is_error") {
		t.Fatalf("uncovered command should have been denied: %s", truncStr(r.stdout, 1500))
	}
}

func TestFlagDisallowedTools(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p",
		`tool:Bash {"command":"echo deny-me"}`,
		"--disallowedTools", "Bash",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, `"is_error":true`) {
		t.Fatalf("denied tool should produce an error result: %s", truncStr(r.stdout, 1500))
	}
}

func TestFlagToolsRestricts(t *testing.T) {
	s := newSession(t)
	r, bodies := recordRequests(t, s, "-p", "hi", "--tools", "Read")
	s.mustOK(t, r)
	for _, b := range bodies {
		var req struct {
			Tools []map[string]json.RawMessage `json:"tools"`
		}
		if json.Unmarshal(b, &req) != nil || len(req.Tools) == 0 {
			continue
		}
		var names []string
		for _, tl := range req.Tools {
			var n string
			json.Unmarshal(tl["name"], &n)
			names = append(names, n)
		}
		var sawRead, sawBash bool
		for _, n := range names {
			if n == "Read" {
				sawRead = true
			}
			if n == "Bash" {
				sawBash = true
			}
		}
		if !sawRead || sawBash {
			t.Fatalf("--tools Read should offer only Read: %v", names)
		}
		return
	}
	t.Fatal("no request tools array recorded")
}

// TestFlagAddDir: --add-dir advertises the extra dir in the env composite
// on the wire. (acceptEdits+outside-write currently allows — the
// workspace ask sits after the mode check; flagged for parity review.)
func TestFlagAddDir(t *testing.T) {
	s := newSession(t)
	outDir := t.TempDir()
	target := filepath.Join(outDir, "outside.txt")
	r, bodies := recordRequests(t, s, "-p",
		`tool:Write {"file_path":"`+target+`","content":"e2e-outside"}`,
		"--permission-mode", "acceptEdits",
		"--add-dir", outDir)
	s.mustOK(t, r)
	var advertised bool
	for _, b := range bodies {
		if bytes.Contains(b, []byte(outDir)) {
			advertised = true
		}
	}
	if !advertised {
		t.Fatal("--add-dir path missing from the env composite on the wire")
	}
	// and the dir is workspace — the write lands (no outside-workspace ask)
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("write inside an added dir should succeed: %v", err)
	}
}

func TestFlagAgent(t *testing.T) {
	s := newSession(t)
	s.writeFile(".claude/agents/e2e-agent.md",
		"---\nname: e2e-agent\ndescription: test agent\n---\nAGENT-SYS-PROMPT-E2E\n")
	r, bodies := recordRequests(t, s, "-p", "hi", "--agent", "e2e-agent")
	s.mustOK(t, r)
	for _, b := range bodies {
		if bytes.Contains(b, []byte("AGENT-SYS-PROMPT-E2E")) {
			return
		}
	}
	t.Fatal("--agent prompt did not reach the wire system array")
}

func TestFlagMaxTurns(t *testing.T) {
	s := newSession(t)
	rec := filepath.Join(t.TempDir(), "reqs")
	r := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, 60*time.Second,
		"--mock", "-p",
		`calls:[{"name":"Bash","input":{"command":"echo t1"}},`+
			`{"name":"Bash","input":{"command":"echo t2"}}]`,
		"--permission-mode", "bypassPermissions",
		"--max-turns", "1",
		"--output-format", "stream-json")
	// max-turns aborts the run with an error result after one request
	if !strings.Contains(r.stdout, "max turns (1) reached") {
		t.Fatalf("expected max-turns error: %s", truncStr(r.stdout, 1500))
	}
	ents, _ := os.ReadDir(rec)
	if len(ents) != 1 {
		t.Fatalf("--max-turns 1 should cap at one request, got %d", len(ents))
	}
}

func TestFlagSessionID(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p", "hi",
		"--session-id", "e2e-fixed-session-id",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "e2e-fixed-session-id") {
		t.Fatalf("fixed session id not in output: %s", truncStr(r.stdout, 800))
	}
}

func TestFlagFile(t *testing.T) {
	s := newSession(t)
	f := s.writeFile("attach.txt", "ATTACH-E2E-CONTENT")
	r, bodies := recordRequests(t, s, "-p", "hi", "--file", f)
	s.mustOK(t, r)
	for _, b := range bodies {
		if bytes.Contains(b, []byte("ATTACH-E2E-CONTENT")) {
			return
		}
	}
	t.Fatal("--file contents missing from the wire")
}

func TestFlagNoPersistence(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p", "hi", "--no-session-persistence")
	s.mustOK(t, r)
	matches, _ := filepath.Glob(filepath.Join(s.home, ".claude", "projects", "*", "*.jsonl"))
	if len(matches) > 0 {
		t.Fatalf("--no-session-persistence left transcripts: %v", matches)
	}
}

func TestFlagPartialMessages(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p", "hi",
		"--output-format", "stream-json", "--include-partial-messages")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, `"stream_event"`) {
		t.Fatalf("--include-partial-messages should emit stream_event deltas: %s",
			truncStr(r.stdout, 1000))
	}
}

func TestFlagReplayUser(t *testing.T) {
	s := newSession(t)
	r := s.runStdin(t,
		`{"type":"user","message":{"role":"user","content":"replay-e2e-mark"}}`+"\n",
		"--mock", "-p", "--input-format", "stream-json",
		"--output-format", "stream-json", "--replay-user-messages")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "replay-e2e-mark") {
		t.Fatalf("replayed input missing: %s", truncStr(r.stdout, 1000))
	}
}

func TestFlagJSONSchema(t *testing.T) {
	s := newSession(t)
	// mock reply isn't valid JSON → real wraps it as {"result": ...}
	r := s.run(t, "--mock", "-p", "hi",
		"--output-format", "json",
		"--json-schema", `{"type":"object","properties":{"r":{"type":"string"}}}`)
	s.mustOK(t, r)
	var out struct {
		StructuredOutput any `json:"structured_output"`
		Result           any `json:"result"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatalf("schema output didn't parse: %v\n%s", err, truncStr(r.stdout, 800))
	}
}

// TestFlagStrictMCPConfig: --strict-mcp-config ignores project .mcp.json —
// only --mcp-config entries connect.
func TestFlagStrictMCPConfig(t *testing.T) {
	srv := newSSEMCPServer(t)
	s := newSession(t)
	s.writeFile(".mcp.json", fmt.Sprintf(
		`{"mcpServers":{"ignored":{"type":"sse","url":%q}}}`, srv.srv.URL+"/sse"))
	s.writeSettings(`{"enableAllProjectMcpServers":true}`)

	r := s.run(t, "--mock", "-p", "hi", "--strict-mcp-config",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	for _, m := range srv.sawMethods() {
		if m == "initialize" {
			t.Fatal("project .mcp.json server connected despite --strict-mcp-config")
		}
	}
}

func TestFlagInitOnly(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p", "hi", "--init-only")
	s.mustOK(t, r)
	// init-only exits before any model call — no turn output expected
	if strings.Contains(r.stdout, "Mock response") {
		t.Fatalf("init-only should not hit the model: %s", truncStr(r.stdout, 800))
	}
}

// TestFlagWorktree: --worktree creates a git worktree and the session's
// writes land inside it.
func TestFlagWorktree(t *testing.T) {
	s := newSession(t)
	git := func(args ...string) {
		out, err := exec.Command("git", append([]string{"-C", s.cwd}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "e2e@test")
	git("config", "user.name", "e2e")
	s.writeFile("seed.txt", "x")
	git("add", "seed.txt")
	git("commit", "-qm", "init")

	// --worktree creates <cwd>/.claude/worktrees/e2e-wt and cds the session
	// there — an absolute write inside the worktree lands on disk.
	target := filepath.Join(s.cwd, ".claude", "worktrees", "e2e-wt", "wt-marker.txt")
	r := s.run(t, "--mock", "-p",
		`tool:Write {"file_path":"`+target+`","content":"in-worktree"}`,
		"--worktree", "e2e-wt",
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	b, err := os.ReadFile(target)
	if err != nil || !strings.Contains(string(b), "in-worktree") {
		t.Fatalf("write should land in the worktree: %v", err)
	}
}

// TestFlagForkSession: -r + --fork-session resumes history into a NEW
// session id and logs the fork record.
func TestFlagForkSession(t *testing.T) {
	s := newSession(t)
	r1 := s.run(t, "--mock", "-p", "hi", "--output-format", "stream-json")
	s.mustOK(t, r1)
	var init struct {
		SessionID string `json:"session_id"`
	}
	for _, line := range strings.Split(r1.stdout, "\n") {
		if strings.Contains(line, `"subtype":"init"`) {
			json.Unmarshal([]byte(line), &init)
		}
	}
	if init.SessionID == "" {
		t.Fatal("no session id in first run")
	}

	r2 := s.run(t, "--mock", "-r", init.SessionID, "--fork-session",
		"-p", "hi", "--output-format", "stream-json")
	s.mustOK(t, r2)
	if !strings.Contains(r2.stderr, "forked session") {
		t.Fatalf("expected fork notice on stderr: %q", r2.stderr)
	}
	var init2 struct {
		SessionID string `json:"session_id"`
	}
	for _, line := range strings.Split(r2.stdout, "\n") {
		if strings.Contains(line, `"subtype":"init"`) {
			json.Unmarshal([]byte(line), &init2)
		}
	}
	if init2.SessionID == init.SessionID {
		t.Fatal("--fork-session should mint a new session id")
	}
}

// TestFlagEffort: --effort lands in output_config on the wire.
func TestFlagEffort(t *testing.T) {
	s := newSession(t)
	r, bodies := recordRequests(t, s, "-p", "hi", "--effort", "low")
	s.mustOK(t, r)
	for _, b := range bodies {
		var req struct {
			OutputConfig json.RawMessage `json:"output_config"`
		}
		if json.Unmarshal(b, &req) == nil &&
			bytes.Contains(req.OutputConfig, []byte(`"effort":"low"`)) {
			return
		}
	}
	t.Fatal("--effort low did not reach output_config")
}

// TestFlagResumeSessionAt: --resume-session-at <uuid> truncates the
// resumed chain at that message — the follow-up request only carries
// history up to the cut.
func TestFlagResumeSessionAt(t *testing.T) {
	s := newSession(t)
	// two-turn session: turn1 "first-marker", turn2 "second-marker"
	r1 := s.run(t, "--mock", "-p", "first-marker")
	s.mustOK(t, r1)
	r2 := s.run(t, "--mock", "-c", "-p", "second-marker")
	s.mustOK(t, r2)

	// find the transcript + the uuid of the user message carrying
	// first-marker so resume-at cuts before second-marker
	dir := filepath.Join(s.home, ".claude", "projects")
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*.jsonl"))
	var cutUUID, seen string
	for _, f := range files {
		b, _ := os.ReadFile(f)
		lines := strings.Split(string(b), "\n")
		for _, ln := range lines {
			var rec struct {
				UUID    string `json:"uuid"`
				Message struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal([]byte(ln), &rec) != nil || rec.UUID == "" {
				continue
			}
			if bytes.Contains(rec.Message.Content, []byte("first-marker")) {
				cutUUID = rec.UUID
				seen = filepath.Base(f)
			}
		}
	}
	if cutUUID == "" {
		t.Fatalf("no uuid found for first-marker message in %v", files)
	}
	sid := strings.TrimSuffix(seen, ".jsonl")

	rec := filepath.Join(t.TempDir(), "reqs")
	r3 := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, 60*time.Second,
		"--mock", "-r", sid, "--resume-session-at", cutUUID,
		"-p", "after-cut")
	s.mustOK(t, r3)

	ents, _ := os.ReadDir(rec)
	var hasSecond, hasFirst bool
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(rec, e.Name()))
		if bytes.Contains(b, []byte("second-marker")) {
			hasSecond = true
		}
		if bytes.Contains(b, []byte("first-marker")) {
			hasFirst = true
		}
	}
	if !hasFirst || hasSecond {
		t.Fatalf("resume-at should carry first-marker but not second-marker (first=%v second=%v)",
			hasFirst, hasSecond)
	}
}

// TestFlagDebugFile: --debug-file captures the debug log on disk.
func TestFlagDebugFile(t *testing.T) {
	s := newSession(t)
	f := filepath.Join(s.cwd, "dbg.log")
	r := s.run(t, "--mock", "-p", "hi", "--debug-file", f)
	s.mustOK(t, r)
	if _, err := os.Stat(f); err != nil {
		t.Fatalf("debug file not created: %v", err)
	}
}

// TestFlagTeleportNoSessions: --teleport with no cloud sessions exits
// cleanly with a message.
func TestFlagTeleportNoSessions(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--teleport")
	// no cloud sessions → graceful message or exit, never a panic
	if strings.Contains(r.stderr, "panic") {
		t.Fatalf("teleport panicked: %s", r.stderr)
	}
}

// TestFlagSettingsFile: --settings merges permissions rules — an allow
// rule from the file lets Bash run in default-mode -p.
func TestFlagSettingsFile(t *testing.T) {
	s := newSession(t)
	f := s.writeFile("custom-settings.json",
		`{"permissions":{"allow":["Bash(echo settingsok)"]}}`)
	r := s.run(t, "--mock", "-p",
		`tool:Bash {"command":"echo settingsok"}`,
		"--settings", f,
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, `"content":"settingsok"`) {
		t.Fatalf("--settings allow rule not honored: %s", truncStr(r.stdout, 1500))
	}
}

// TestFlagSettingsJSON: --settings also accepts inline JSON.
func TestFlagSettingsJSON(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p",
		`tool:Bash {"command":"echo inline-ok"}`,
		"--settings", `{"permissions":{"allow":["Bash(echo inline-ok)"]}}`,
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, `"content":"inline-ok"`) {
		t.Fatalf("inline --settings not honored: %s", truncStr(r.stdout, 1500))
	}
}

// TestFlagFallbackModel: an overload error walks the fallback chain —
// the retry request carries the fallback model. (error:503 fails
// fast; 429 also walks but burns the ~30s retry floor.)
func TestFlagFallbackModel(t *testing.T) {
	s := newSession(t)
	rec := filepath.Join(t.TempDir(), "reqs")
	r := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, 60*time.Second,
		"--mock", "-p", "error:503",
		"--fallback-model", "claude-haiku-4-5-20251001")
	// both models fail — exit code may be nonzero; the request records are
	// the assertion
	_ = r
	ents, _ := os.ReadDir(rec)
	var sawMain, sawFallback bool
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(rec, e.Name()))
		if bytes.Contains(b, []byte(`"model":"claude-fable-5-1"`)) {
			sawMain = true
		}
		if bytes.Contains(b, []byte(`"model":"claude-haiku-4-5-20251001"`)) {
			sawFallback = true
		}
	}
	if !sawMain || !sawFallback {
		t.Fatalf("expected main+fallback requests (main=%v fallback=%v)", sawMain, sawFallback)
	}
}

// TestSettingsEnvInjection: settings.env injects vars into the process +
// Bash children.
func TestSettingsEnvInjection(t *testing.T) {
	s := newSession(t)
	s.writeSettings(`{"env":{"E2E_SETTINGS_VAR":"injected-e2e"}}`)
	r := s.run(t, "--mock", "-p",
		`tool:Bash {"command":"echo $E2E_SETTINGS_VAR"}`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "injected-e2e") {
		t.Fatalf("settings env not in bash child: %s", truncStr(r.stdout, 1200))
	}
}

// TestSettingsAskRule: an `ask` rule forces a permission prompt even when
// a broad allow rule would match (PTY: dialog appears).
func TestSettingsAskRule(t *testing.T) {
	s := newSession(t)
	s.writeSettings(`{"permissions":{
		"allow":["Bash"],
		"ask":["Bash(echo askme)"]}}`)
	p := startREPL(t, s)
	p.sendUntilEchoed(t, `tool:Bash {"command":"echo askme"}`+"\r", "> tool:Bash", 8)
	p.waitFor("1. Yes", 30*time.Second)
	p.exit()
}
