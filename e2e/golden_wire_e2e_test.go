//go:build e2e

package e2e

// Golden-wire parity: real claude's /v1/messages requests (captured through
// the tls capture+relay, redacted) vs our binary's requests to the mock.
// Structural comparison — the same field set, system layout, tool list,
// and scalars must match; per-run values (uuids, session ids, message
// content) are ignored.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gbasin/claude-go/internal/extract"
	"github.com/gbasin/claude-go/internal/session"
)

type wireCapture struct {
	RequestLine string            `json:"request_line"`
	Headers     map[string]string `json:"headers"`
	Body        struct {
		Model             string            `json:"model"`
		MaxTokens         int               `json:"max_tokens"`
		System            []json.RawMessage `json:"system"`
		Messages          []json.RawMessage `json:"messages"`
		Tools             []json.RawMessage `json:"tools"`
		Stream            bool              `json:"stream"`
		Thinking          json.RawMessage   `json:"thinking"`
		ContextManagement json.RawMessage   `json:"context_management"`
		Diagnostics       json.RawMessage   `json:"diagnostics"`
	} `json:"body"`
}

// TestExtractSubcommand: end-to-end `claude-go extract` against the
// real binary — requires a local claude install (skipped in CI).
func TestExtractSubcommand(t *testing.T) {
	realClaudeBin(t) // skips when absent

	s := newSession(t)
	dir := filepath.Join(t.TempDir(), "extracted")
	r := s.runEx(t, []string{"CLAUDE_GO_EXTRACTED_DIR=" + dir},
		180*time.Second, "extract")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "extracted") {
		t.Fatalf("extract output missing summary:\n%s", truncStr(r.stdout+r.stderr, 2000))
	}
	for _, want := range []string{
		"meta.json", "tools.json",
		"system_main_fable.txt", "system_main_opus.txt", "system_main_haiku.txt",
		"realwire_print_default.json", "skills_index.txt",
	} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("extracted dir missing %s", want)
		}
	}
}

func TestGoldenRequestParity(t *testing.T) {
	// fixtures were unvendored (Anthropic text); regenerate locally with
	// `claude-go extract` — the cache dir carries the same capture format.
	gb, err := os.ReadFile("testdata/realwire_print_294.json")
	if err != nil {
		gb, err = os.ReadFile(filepath.Join(extract.Dir(), "realwire_print_default.json"))
	}
	if err != nil {
		t.Skip("no golden capture (run `claude-go extract` to regenerate)")
	}
	var golden wireCapture
	if err := json.Unmarshal(gb, &golden); err != nil {
		t.Fatal(err)
	}

	s := newSession(t)
	recDir := filepath.Join(t.TempDir(), "reqs")
	r := s.runEx(t, goldenEnv(recDir), 30*time.Second,
		"--mock", "-p", "say hi")
	s.mustOK(t, r)

	ents, err := os.ReadDir(recDir)
	if err != nil || len(ents) == 0 {
		t.Fatalf("mock recorded no requests: %v", err)
	}
	last, err := os.ReadFile(filepath.Join(recDir, ents[len(ents)-1].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var ours struct {
		Model             string            `json:"model"`
		MaxTokens         int               `json:"max_tokens"`
		System            []json.RawMessage `json:"system"`
		Messages          []json.RawMessage `json:"messages"`
		Tools             []json.RawMessage `json:"tools"`
		Stream            bool              `json:"stream"`
		Thinking          json.RawMessage   `json:"thinking"`
		ContextManagement json.RawMessage   `json:"context_management"`
		Diagnostics       json.RawMessage   `json:"diagnostics"`
	}
	if err := json.Unmarshal(last, &ours); err != nil {
		t.Fatal(err)
	}

	// request line
	if golden.RequestLine != "POST /v1/messages?beta=true HTTP/1.1" {
		t.Fatalf("golden line changed: %s", golden.RequestLine)
	}
	// model + scalars
	if ours.Model != golden.Body.Model {
		t.Fatalf("model %q vs golden %q", ours.Model, golden.Body.Model)
	}
	if ours.MaxTokens != golden.Body.MaxTokens {
		t.Fatalf("max_tokens %d vs golden %d", ours.MaxTokens, golden.Body.MaxTokens)
	}
	if ours.Stream != golden.Body.Stream {
		t.Fatal("stream flag differs")
	}
	// system block count + cache_control placement
	if len(ours.System) != len(golden.Body.System) {
		t.Fatalf("system blocks: ours %d vs golden %d", len(ours.System), len(golden.Body.System))
	}
	for i := range golden.Body.System {
		var gs, os_ struct {
			Type         string          `json:"type"`
			CacheControl json.RawMessage `json:"cache_control"`
		}
		json.Unmarshal(golden.Body.System[i], &gs)
		json.Unmarshal(ours.System[i], &os_)
		if gs.Type != os_.Type {
			t.Fatalf("system[%d] type %q vs %q", i, os_.Type, gs.Type)
		}
		if normJSON(gs.CacheControl) != normJSON(os_.CacheControl) {
			t.Fatalf("system[%d] cache_control %s vs %s", i, os_.CacheControl, gs.CacheControl)
		}
	}
	// tool name list — exact match including order
	var gtools, otools []string
	for _, g := range golden.Body.Tools {
		var s struct{ Name string }
		json.Unmarshal(g, &s)
		gtools = append(gtools, s.Name)
	}
	for _, o := range ours.Tools {
		var s struct{ Name string }
		json.Unmarshal(o, &s)
		otools = append(otools, s.Name)
	}
	if len(gtools) != len(otools) {
		t.Fatalf("tool count ours %d vs golden %d: %v", len(otools), len(gtools), otools)
	}
	for i := range gtools {
		if gtools[i] != otools[i] {
			t.Fatalf("tool[%d] ours %q vs golden %q", i, otools[i], gtools[i])
		}
	}
	// thinking + context_management + diagnostics identical
	if normJSON(ours.Thinking) != normJSON(golden.Body.Thinking) {
		t.Fatalf("thinking %s vs %s", ours.Thinking, golden.Body.Thinking)
	}
	if normJSON(ours.ContextManagement) != normJSON(golden.Body.ContextManagement) {
		t.Fatalf("context_management %s vs %s", ours.ContextManagement, golden.Body.ContextManagement)
	}
	if normJSON(ours.Diagnostics) != normJSON(golden.Body.Diagnostics) {
		t.Fatalf("diagnostics %s vs %s", ours.Diagnostics, golden.Body.Diagnostics)
	}
	// the headers real sends on every main request must exist in the golden
	// (2.1.296 set — the 289-era request-identity headers are gone)
	for _, must := range []string{
		"anthropic-beta", "anthropic-version", "x-app",
		"x-stainless-runtime", "x-claude-code-session-id",
	} {
		if _, ok := golden.Headers[must]; !ok {
			t.Fatalf("golden missing expected header %q", must)
		}
	}
}

// TestGoldenToolLoopParity: real's second (post-tool_use) request shape —
// [user, sys-string hook ctx, asst tool_use+cc, user tool_result,
// sys total_tokens+cc, sys-string parallel instruction].
func TestGoldenToolLoopParity(t *testing.T) {
	gb, err := os.ReadFile("testdata/realwire_toolloop_294.json")
	if err != nil {
		gb, err = os.ReadFile(filepath.Join(extract.Dir(), "realwire_toolloop_default.json"))
	}
	if err != nil {
		t.Skip("no golden capture")
	}
	var golden wireCapture
	if err := json.Unmarshal(gb, &golden); err != nil {
		t.Fatal(err)
	}

	s := newSession(t)
	recDir := filepath.Join(t.TempDir(), "reqs")
	r := s.runEx(t, goldenEnv(recDir), 60*time.Second,
		"--mock", "-p", `tool:Bash {"command":"echo hi"}`,
		"--permission-mode", "bypassPermissions")
	s.mustOK(t, r)

	ents, err := os.ReadDir(recDir)
	if err != nil || len(ents) < 2 {
		t.Fatalf("expected >=2 recorded requests, got %v err=%v", len(ents), err)
	}
	// find the request containing a tool_result
	type msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	var ours []msg
	for _, e := range ents {
		raw, _ := os.ReadFile(filepath.Join(recDir, e.Name()))
		var req struct {
			Messages []msg `json:"messages"`
		}
		json.Unmarshal(raw, &req)
		for _, m := range req.Messages {
			if strings.Contains(string(m.Content), "tool_result") {
				ours = req.Messages
			}
		}
	}
	if ours == nil {
		t.Fatal("no request carried a tool_result — tool loop never ran")
	}

	var gmsgs []msg
	if err := json.Unmarshal(mustJSON(golden.Body.Messages), &gmsgs); err != nil {
		t.Fatal(err)
	}

	// the SessionStart hook-context system message at index 1 is
	// env-conditional (the golden's account has a hook; the sandbox has
	// none) — drop it from the golden before comparing shapes
	if len(gmsgs) > 1 && gmsgs[1].Role == "system" &&
		strings.Contains(string(gmsgs[1].Content), "SessionStart hook") {
		gmsgs = append(gmsgs[:1], gmsgs[2:]...)
	}
	if len(ours) > 1 && ours[1].Role == "system" {
		if s := contentShape(ours[1].Content); s != "string" {
			t.Fatalf("messages[1] post-first request must be bare-string hook ctx or absent — got %q", s)
		}
	}

	// shape diff: roles + per-message block-type sequences. msg[0]'s
	// leading context blocks (CLAUDE.md reminder, userEmail ctx) are
	// env-conditional — compare roles everywhere but block shapes only
	// from index 1 onward.
	if len(ours) != len(gmsgs) {
		t.Fatalf("message count ours %d vs golden %d", len(ours), len(gmsgs))
	}
	if contentShape(ours[0].Content) == "" || !strings.HasPrefix(contentShape(ours[0].Content), "[text") {
		t.Fatalf("messages[0] must start with text blocks, got %q", contentShape(ours[0].Content))
	}
	for i := 1; i < len(gmsgs); i++ {
		if ours[i].Role != gmsgs[i].Role {
			t.Fatalf("messages[%d] role ours %q vs golden %q", i, ours[i].Role, gmsgs[i].Role)
		}
		gt := contentShape(gmsgs[i].Content)
		ot := contentShape(ours[i].Content)
		if gt != ot {
			t.Fatalf("messages[%d] content shape ours %q vs golden %q", i, ot, gt)
		}
	}
	// breakpoints must land where golden has them
	for i := 1; i < len(gmsgs); i++ {
		var gb, ob []apiBlock
		json.Unmarshal(gmsgs[i].Content, &gb)
		json.Unmarshal(ours[i].Content, &ob)
		if len(gb) != len(ob) {
			continue
		}
		for j := range gb {
			gcc := gb[j].CacheControl != nil
			occ := ob[j].CacheControl != nil
			if gcc != occ {
				t.Fatalf("messages[%d].content[%d] cache_control ours=%v golden=%v", i, j, occ, gcc)
			}
		}
	}
}

type apiBlock struct {
	Type         string          `json:"type"`
	CacheControl json.RawMessage `json:"cache_control"`
}

// contentShape: "string" for bare-string content, else the block type list.
func contentShape(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return "string"
	}
	var bs []apiBlock
	if json.Unmarshal(raw, &bs) != nil {
		return "?"
	}
	var kinds []string
	for _, b := range bs {
		kinds = append(kinds, b.Type)
	}
	return "[" + strings.Join(kinds, ",") + "]"
}

// normJSON reparses a RawMessage so key order/indentation don't count.
func normJSON(r json.RawMessage) string {
	var v any
	if json.Unmarshal(r, &v) != nil {
		return string(r)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// TestGoldenResumeParity: seeded transcript resumed via -c -p must hit the
// wire the way real's resume request does (captured req 440):
//   - env composite is NOT re-sent (no system block record)
//   - consecutive same-role transcript records coalesce into one message
//   - the new prompt is bare-string user content
//
// Structural diff against the realwire resume fixture where env-conditional.
func TestGoldenResumeParity(t *testing.T) {
	s := newSession(t)
	recDir := filepath.Join(t.TempDir(), "reqs")

	// seed ~/.claude/projects/<slug>/<id>.jsonl — a prior turn plus two
	// consecutive user records (real merges them into one wire message)
	slug := session.Slug(s.cwd)
	proj := filepath.Join(s.home, ".claude", "projects", slug)
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	recs := []map[string]any{
		{"type": "user", "message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "ctx-a"},
			map[string]any{"type": "text", "text": "first question"},
		}}},
		{"type": "system", "message": map[string]any{"role": "system", "content": "SessionStart hook additional context: hook-ctx"}},
		{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "first answer"},
		}}},
		{"type": "user", "message": map[string]any{"role": "user", "content": "second question"}},
		{"type": "user", "message": map[string]any{"role": "user", "content": "third question"}},
		{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "second answer"},
		}}},
	}
	var sb strings.Builder
	for _, r := range recs {
		b, _ := json.Marshal(r)
		sb.Write(b)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(proj, "aabbccdd-1111-2222-3333-444455556666.jsonl"),
		[]byte(sb.String()), 0o644); err != nil {
		t.Fatalf("seed transcript: %v", err)
	}

	r := s.runEx(t, goldenEnv(recDir), 30*time.Second,
		"--mock", "-c", "-p", "fresh prompt")
	s.mustOK(t, r)

	ents, err := os.ReadDir(recDir)
	if err != nil || len(ents) == 0 {
		t.Fatalf("no recorded requests: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(recDir, ents[len(ents)-1].Name()))
	var req struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(b, &req); err != nil {
		t.Fatalf("request parse: %v", err)
	}
	msgs := req.Messages

	// no system BLOCK record — real does not re-send the env composite
	for i, m := range msgs {
		if m["role"] != "system" {
			continue
		}
		if _, ok := m["content"].([]any); ok {
			t.Fatalf("msg[%d]: env composite re-sent on resume (real sends hook string only)", i)
		}
	}

	// consecutive user records coalesced: "second"+"third" merged into ONE
	// user message with 2 text blocks
	var merged bool
	for _, m := range msgs {
		if m["role"] != "user" {
			continue
		}
		blocks, ok := m["content"].([]any)
		if !ok || len(blocks) != 2 {
			continue
		}
		var texts []string
		for _, b2 := range blocks {
			if bm, ok := b2.(map[string]any); ok {
				texts = append(texts, fmt.Sprint(bm["text"]))
			}
		}
		if len(texts) == 2 && texts[0] == "second question" && texts[1] == "third question" {
			merged = true
		}
	}
	if !merged {
		t.Fatalf("consecutive user records not coalesced; msgs=%v", roleForm(msgs))
	}

	// last user message = the new prompt as a bare string (real's form)
	var lastUserStr bool
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i]["role"] == "user" {
			lastUserStr = msgs[i]["content"] == "fresh prompt"
			break
		}
	}
	if !lastUserStr {
		t.Fatalf("new prompt should be bare-string user content; msgs=%v", roleForm(msgs))
	}
}

// roleForm renders the wire message array's role/content-form sequence
// for failure output.
func roleForm(msgs []map[string]any) string {
	var b strings.Builder
	for i, m := range msgs {
		form := "str"
		if blk, ok := m["content"].([]any); ok {
			form = fmt.Sprintf("blk:%d", len(blk))
		}
		fmt.Fprintf(&b, "%d:%s(%s) ", i, m["role"], form)
	}
	return b.String()
}

// goldenEnv points the child at the user's extraction cache so golden
// parity checks exercise the real prompt text when available.
func goldenEnv(recDir string) []string {
	env := []string{"CLAUDE_GO_MOCK_RECORD=" + recDir}
	if extract.Available() {
		env = append(env, "CLAUDE_GO_EXTRACTED_DIR="+extract.Dir())
	}
	return env
}
