//go:build e2e

// e2e harness — builds the claude-go binary once per suite run and provides
// per-test sandboxes: an isolated HOME (so ~/.claude state, transcripts and
// trust/onboarding markers are test-local), an isolated project cwd, and a
// minimal env. Every test in this package runs the real binary against the
// in-process mock API (--mock), so no credentials or network are needed.
//
// Mock protocol (internal/mock/server.go):
//   - last user text "tool:<Name> <json-input>" → one tool_use block
//   - last user text "calls:[{name,input},...]" → N tool_use blocks
//   - last user text "error:429" / "error:5*" → HTTP error
//   - last user text "think:<text>" → thinking + text blocks
//   - otherwise → "Mock response to: <text>" (streamed word by word)
//   - a user message containing tool_result → "Tool results received:\n<summary>"
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var binPath string

// TestMain builds the binary once for the whole suite.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "claude-go-e2e-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: mktemp:", err)
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "claude-go")
	root, err := filepath.Abs("..")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: repo root:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binPath, ".")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// sess is one isolated claude-go invocation context: fresh HOME + cwd.
type sess struct {
	t    *testing.T
	cwd  string // project dir (symlink-resolved)
	home string
	env  []string
}

// newSession creates the sandbox. ~/.claude.json is pre-seeded with
// hasCompletedOnboarding + hasTrustDialogAccepted for cwd so interactive
// runs don't block on the theme/trust prompts.
func newSession(t *testing.T) *sess {
	t.Helper()
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval cwd: %v", err)
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval home: %v", err)
	}
	s := &sess{t: t, cwd: cwd, home: home}
	s.env = []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"SHELL=/bin/bash",
		"TERM=xterm-256color",
		"LANG=en_US.UTF-8",
		"TMPDIR=" + os.Getenv("TMPDIR"),
		"USER=" + os.Getenv("USER"),
		// make sure a real key/token can't leak into mock runs
		"ANTHROPIC_API_KEY=",
		"ANTHROPIC_AUTH_TOKEN=",
		"CLAUDE_CODE_OAUTH_TOKEN=",
		"CLAUDE_GO_TOKEN=",
	}
	claudeJSON := map[string]any{
		"hasCompletedOnboarding": true,
		"projects": map[string]any{
			cwd: map[string]any{"hasTrustDialogAccepted": true},
		},
	}
	b, _ := json.Marshal(claudeJSON)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), b, 0o644); err != nil {
		t.Fatalf("seed .claude.json: %v", err)
	}
	// ~/.claude must exist for `config set` (it doesn't MkdirAll) and is
	// where transcripts/plugins land anyway
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir ~/.claude: %v", err)
	}
	return s
}

// writeFile creates path (under s.cwd unless absolute) with content.
func (s *sess) writeFile(path, content string) string {
	s.t.Helper()
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.cwd, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		s.t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// writeSettings writes <cwd>/.claude/settings.json from raw JSON.
func (s *sess) writeSettings(raw string) string {
	return s.writeFile(".claude/settings.json", raw)
}

type runResult struct {
	stdout   string
	stderr   string
	code     int
	timedOut bool
}

// run executes the binary with args (default timeout 60s).
func (s *sess) run(t *testing.T, args ...string) runResult {
	t.Helper()
	return s.runEx(t, nil, 60*time.Second, args...)
}

// runEx executes the binary with extra env vars and a custom timeout.
func (s *sess) runEx(t *testing.T, extraEnv []string, timeout time.Duration, args ...string) runResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Dir = s.cwd
	cmd.Env = append(append([]string{}, s.env...), extraEnv...)
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	r := runResult{stdout: so.String(), stderr: se.String(), timedOut: ctx.Err() == context.DeadlineExceeded}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			r.code = ee.ExitCode()
		} else {
			r.code = -1
		}
	}
	return r
}

// runStdin executes the binary with stdin data (for pipe-mode / stream-json input).
func (s *sess) runStdin(t *testing.T, input string, args ...string) runResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Dir = s.cwd
	cmd.Env = s.env
	cmd.Stdin = strings.NewReader(input)
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	r := runResult{stdout: so.String(), stderr: se.String(), timedOut: ctx.Err() == context.DeadlineExceeded}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			r.code = ee.ExitCode()
		} else {
			r.code = -1
		}
	}
	return r
}

// --- assertions -------------------------------------------------------------

func (s *sess) mustOK(t *testing.T, r runResult) {
	t.Helper()
	if r.timedOut {
		t.Fatalf("process timed out\nstdout: %s\nstderr: %s", r.stdout, r.stderr)
	}
	if r.code != 0 {
		t.Fatalf("exit code %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
}

func contains(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("expected %s to contain %q\ngot: %s", what, needle, truncStr(haystack, 2000))
	}
}

func notContains(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("expected %s NOT to contain %q\ngot: %s", what, needle, truncStr(haystack, 2000))
	}
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…[truncated]"
}

// --- transcripts ------------------------------------------------------------

// transcriptPaths returns all session transcripts under the test HOME.
func (s *sess) transcriptPaths(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(s.home, ".claude", "projects", "*", "*.jsonl"))
	if err != nil {
		t.Fatalf("glob transcripts: %v", err)
	}
	return matches
}

// transcriptPath returns the single transcript for sessionID.
func (s *sess) transcriptPath(t *testing.T, sessionID string) string {
	t.Helper()
	for _, p := range s.transcriptPaths(t) {
		if filepath.Base(p) == sessionID+".jsonl" {
			return p
		}
	}
	return ""
}

// records parses a transcript file into JSONL records.
func readRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open transcript %s: %v", path, err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("transcript %s has invalid JSON line: %v", path, err)
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan transcript: %v", err)
	}
	return out
}

// --- stream-json output parsing ---------------------------------------------

// parseStreamJSON splits print-mode --output-format stream-json output into
// the single-line events plus the trailing pretty-printed result object.
func parseStreamJSON(t *testing.T, out string) (events []map[string]any, result map[string]any) {
	t.Helper()
	// result object is pretty-printed and always last; it begins at the last
	// line that is exactly "{".
	idx := strings.LastIndex(out, "\n{")
	if idx < 0 && strings.HasPrefix(out, "{") {
		idx = -1 // whole output is the object (unlikely)
	}
	eventPart := out
	if idx >= 0 {
		eventPart = out[:idx]
		if err := json.Unmarshal([]byte(out[idx+1:]), &result); err != nil {
			t.Fatalf("result object did not parse: %v\nraw: %s", err, truncStr(out[idx+1:], 500))
		}
	}
	for _, line := range strings.Split(eventPart, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("stream-json line did not parse: %v\nline: %s", err, truncStr(line, 300))
		}
		events = append(events, m)
	}
	return events, result
}

// findEvent returns the first event matching type/subtype (empty = wildcard).
func findEvent(events []map[string]any, typ, subtype string) map[string]any {
	for _, e := range events {
		if typ != "" && e["type"] != typ {
			continue
		}
		if subtype != "" && e["subtype"] != subtype {
			continue
		}
		return e
	}
	return nil
}

// toolResultTexts extracts the text content of every stream-json tool_result
// user event (they carry {"type":"tool_result","content": "..."}).
func toolResultTexts(events []map[string]any) []string {
	var out []string
	for _, e := range events {
		if e["type"] != "user" {
			continue
		}
		msg, _ := e["message"].(map[string]any)
		blocks, _ := msg["content"].([]any)
		for _, b := range blocks {
			bm, _ := b.(map[string]any)
			if bm["type"] == "tool_result" {
				if s, ok := bm["content"].(string); ok {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// hasBinary reports whether name is resolvable on PATH (for skip checks).
func hasBinary(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
