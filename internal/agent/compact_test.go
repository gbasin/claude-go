package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"claude-go/internal/api"
	"claude-go/internal/auth"
	"claude-go/internal/mock"
	"claude-go/internal/session"
)

// compactLoop builds a mock-backed loop plus a real transcript store under a
// temp CLAUDE_CONFIG_DIR, wired like main.go (boundary + span logging).
func compactLoop(t *testing.T) (*mock.Server, *Loop, *session.Store) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	srv, err := mock.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	os.Setenv("ANTHROPIC_BASE_URL", srv.Addr)
	creds := &auth.Credentials{AccessToken: "mock"}
	l := NewLoop(api.NewClient(creds, "test-session", api.EntrySDKCLI), t.TempDir())
	l.ToolCtx.Permission = func(string, json.RawMessage, string) bool { return true }
	st, err := session.Open(l.ToolCtx.CWD, "test-session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	l.Transcript = st.Path
	l.Events.OnCompactSpan = func(span []api.Message) {
		for _, m := range span {
			st.Log(m)
		}
	}
	l.Events.OnCompact = func() {
		_, trig := l.CompactMeta()
		st.LogCompactBoundaryTrig(l.LastInputTokens(), len(l.Messages), trig)
	}
	return srv, l, st
}

// requestMessages decodes the messages array out of a raw request body.
func requestMessages(t *testing.T, body []byte) []api.Message {
	t.Helper()
	var req api.Request
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("request body did not parse: %v", err)
	}
	return req.Messages
}

// blocks decodes a message's content blocks.
func blocks(t *testing.T, m api.Message) []api.ContentBlock {
	t.Helper()
	var bs []api.ContentBlock
	if err := json.Unmarshal(m.Content, &bs); err != nil {
		t.Fatalf("message content did not parse: %v", err)
	}
	return bs
}

// countBreakpoints counts cache_control markers in a raw request body.
func countBreakpoints(body []byte) int {
	return bytes.Count(body, []byte(`"cache_control"`))
}

// TestAutoCompactE2E drives a multi-turn conversation until usage crosses the
// auto-compact threshold, then verifies:
//   - the summarize request fires and is wire-correct (≤4 breakpoints, no
//     signature-only thinking replayed, compact prompt last, model/system set)
//   - history is replaced by the isCompactSummary continuation
//   - the transcript gains the span records, compact_boundary, and summary
//   - the next turn sends the continuation as message 0
func TestAutoCompactE2E(t *testing.T) {
	// threshold = window - 20000 - 8000 - 4000 = 8000
	t.Setenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW", "40000")
	srv, l, st := compactLoop(t)

	ctx := context.Background()

	// turn 1: usage below threshold — no compact
	if _, err := l.Ask(ctx, "usage:2000"); err != nil {
		t.Fatal(err)
	}
	if srv.Requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", srv.Requests.Load())
	}
	if len(l.Messages) != 3 { // user + env system + assistant
		t.Fatalf("messages = %d", len(l.Messages))
	}

	// inject history the API would reject on a compact request:
	// a signature-only thinking message (interrupted stream replay) and
	// stale cache_control breakpoints pushing the total past 4.
	l.Messages = append(l.Messages,
		api.BlocksMessage("assistant", []api.ContentBlock{
			{Type: "thinking", Signature: "sig-only"}, // thinking text empty
		}),
		api.TextMessage("user", "mid-turn note"),
		api.BlocksMessage("user", []api.ContentBlock{
			{Type: "text", Text: "bp-a", CacheControl: &api.CacheControl{Type: "ephemeral"}},
			{Type: "text", Text: "bp-b", CacheControl: &api.CacheControl{Type: "ephemeral", TTL: "5m"}},
			{Type: "text", Text: "bp-c", CacheControl: &api.CacheControl{Type: "ephemeral", TTL: "5m"}},
		}),
	)

	// turn 2: usage crosses threshold — auto-compact fires inside Ask
	if _, err := l.Ask(ctx, "usage:9000"); err != nil {
		t.Fatal(err)
	}

	// the summarize request is the last request sent
	var req api.Request
	if err := json.Unmarshal(srv.LastBody, &req); err != nil {
		t.Fatalf("compact request did not parse: %v", err)
	}
	if req.Model != l.Model {
		t.Errorf("compact model = %q, want %q", req.Model, l.Model)
	}
	if len(req.System) == 0 {
		t.Error("compact request missing system blocks")
	}
	if len(req.Tools) != 0 {
		t.Error("compact request must carry no tools")
	}
	if len(req.Messages) == 0 {
		t.Fatal("compact request has no messages")
	}
	last := req.Messages[len(req.Messages)-1]
	lb := blocks(t, last)
	if last.Role != "user" || len(lb) == 0 ||
		!strings.Contains(lb[len(lb)-1].Text, "detailed summary of the conversation") {
		t.Errorf("last compact message should be the compact prompt, got %+v", lb)
	}
	// a mid-array role:"system" env block must precede an assistant message;
	// any trailing one must remain last — never before a user message
	for i, m := range req.Messages[:len(req.Messages)-1] {
		if m.Role == "system" && i+1 < len(req.Messages) &&
			req.Messages[i+1].Role == "user" {
			t.Errorf("env system message at %d directly precedes a user message", i)
		}
	}
	// no signature-only thinking blocks on the wire
	for i, m := range req.Messages {
		for j, b := range blocks(t, m) {
			if b.Type == "thinking" && b.Thinking == "" {
				t.Errorf("compact request replays empty thinking block msg %d block %d", i, j)
			}
		}
	}
	if n := countBreakpoints(srv.LastBody); n > 4 {
		t.Errorf("compact request has %d cache_control blocks, max 4", n)
	}

	// history replaced by the continuation
	if len(l.Messages) != 1 {
		t.Fatalf("post-compact messages = %d, want 1", len(l.Messages))
	}
	if !l.Messages[0].IsCompactSummary {
		t.Error("continuation message missing IsCompactSummary")
	}
	if !strings.Contains(string(l.Messages[0].Content), "continued from a previous conversation") {
		t.Error("continuation text missing")
	}

	// transcript: span records + compact_boundary(trigger:auto) +
	// isCompactSummary user record (writer logs l.Messages like runPrint)
	for _, m := range l.Messages {
		st.LogMessage(m)
	}
	st.Close()
	recs := readTranscript(t, st.Path)
	var sawBoundary, sawSummary bool
	var boundaryIdx, summaryIdx int
	for i, r := range recs {
		if r["type"] == "system" && r["subtype"] == "compact_boundary" {
			sawBoundary = true
			boundaryIdx = i
			if md, _ := r["compactMetadata"].(map[string]any); md["trigger"] != "auto" {
				t.Errorf("boundary trigger = %v, want auto", md["trigger"])
			}
		}
		if r["type"] == "user" && r["isCompactSummary"] == true {
			sawSummary = true
			summaryIdx = i
		}
	}
	if !sawBoundary {
		t.Error("transcript missing compact_boundary record")
	}
	if !sawSummary {
		t.Error("transcript missing isCompactSummary record")
	}
	if sawBoundary && sawSummary && summaryIdx < boundaryIdx {
		t.Error("summary record must follow the compact boundary")
	}

	// turn 3 continues on compacted history
	if _, err := l.Ask(ctx, "usage:100"); err != nil {
		t.Fatal(err)
	}
	req2 := requestMessages(t, srv.LastBody)
	if len(req2) == 0 || !strings.Contains(string(req2[0].Content),
		"continued from a previous conversation") {
		t.Fatalf("post-compact request must lead with the continuation, got %s",
			truncForTest(string(req2[0].Content)))
	}
	if len(l.Messages) != 3 { // continuation + user + assistant
		t.Fatalf("messages after next turn = %d", len(l.Messages))
	}
}

// TestAutoCompactEnvLast covers the interrupted-turn edge: when l.Messages
// ends with the env "system" block, the compact prompt must not leave that
// block mid-array followed by a user message.
func TestAutoCompactEnvLast(t *testing.T) {
	t.Setenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW", "40000")
	srv, l, _ := compactLoop(t)
	ctx := context.Background()
	if _, err := l.Ask(ctx, "usage:2000"); err != nil {
		t.Fatal(err)
	}
	// simulate an interrupted Ask: env system block left at the tail
	var env api.Message
	var rest []api.Message
	for _, m := range l.Messages {
		if m.Role == "system" {
			env = m
		} else {
			rest = append(rest, m)
		}
	}
	l.Messages = append(rest, env)
	if _, err := l.Ask(ctx, "usage:9000"); err != nil {
		t.Fatal(err)
	}
	req := requestMessages(t, srv.LastBody)
	last := req[len(req)-1]
	if last.Role != "system" {
		t.Fatalf("trailing env system block must stay last, got role %q", last.Role)
	}
}

// TestCompactSummaryConsumed verifies TakeCompactPending reports the kept
// tail size once, so transcript writers skip already-logged messages.
func TestCompactSummaryConsumed(t *testing.T) {
	l := &Loop{}
	l.applySummary("sum", "", 0)
	if !l.Messages[0].IsCompactSummary {
		t.Fatal("applySummary did not flag continuation")
	}
	if kept := l.TakeCompactPending(); kept != 0 {
		t.Fatalf("kept = %d, want 0", kept)
	}
	if kept := l.TakeCompactPending(); kept != -1 {
		t.Fatalf("second take = %d, want -1", kept)
	}
}

// readTranscript parses a JSONL transcript into raw records.
func readTranscript(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad transcript line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func truncForTest(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
