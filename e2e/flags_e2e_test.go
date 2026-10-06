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
