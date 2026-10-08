package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"claude-go/internal/api"
	"claude-go/internal/auth"
	"claude-go/internal/hooks"
	"claude-go/internal/mock"
)

func mockLoop(t *testing.T) *Loop {
	srv, err := mock.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	os.Setenv("ANTHROPIC_BASE_URL", srv.Addr)
	creds := &auth.Credentials{AccessToken: "mock"}
	l := NewLoop(api.NewClient(creds, "test-session", api.EntrySDKCLI), t.TempDir())
	l.ToolCtx.Permission = func(string, json.RawMessage, string) bool { return true }
	return l
}

func TestSimpleTurn(t *testing.T) {
	l := mockLoop(t)
	var text string
	l.Events.OnTextDelta = func(s string) { text += s }
	_, err := l.Ask(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if text == "" {
		t.Error("no text streamed")
	}

	// user + system(env) + assistant
	if len(l.Messages) != 3 {
		t.Errorf("messages = %d", len(l.Messages))
	}
}

func TestToolLoop(t *testing.T) {
	l := mockLoop(t)
	tmp := t.TempDir()
	l.ToolCtx.CWD = tmp
	_, err := l.Ask(context.Background(),
		`tool:Bash {"command":"echo hello-mock"}`)
	if err != nil {
		t.Fatal(err)
	}
	// last assistant msg should be text after tool round-trip
	last := l.Messages[len(l.Messages)-1]
	var blocks []api.ContentBlock
	json.Unmarshal(last.Content, &blocks)
	var hasText bool
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			hasText = true
		}
	}
	if !hasText {
		t.Error("expected final text after tool result")
	}
	// verify the tool_result message exists
	var sawResult bool
	for _, m := range l.Messages {
		var bs []api.ContentBlock
		if json.Unmarshal(m.Content, &bs) != nil {
			continue
		}
		for _, b := range bs {
			if b.Type == "tool_result" {
				sawResult = true
				var inner []api.ContentBlock
				json.Unmarshal(b.Content, &inner)
				if len(inner) > 0 && inner[0].Text != "" &&
					!json.Valid([]byte(inner[0].Text)) {
					_ = inner
				}
			}
		}
	}
	if !sawResult {
		t.Error("no tool_result in history")
	}
}

func TestFirstMessageShape(t *testing.T) {
	l := mockLoop(t)
	if _, err := l.Ask(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	var blocks []api.ContentBlock
	if err := json.Unmarshal(l.Messages[0].Content, &blocks); err != nil {
		t.Fatal(err)
	}
	last := blocks[len(blocks)-1]
	if last.Type != "text" || last.Text != "hi" {
		t.Errorf("last block = %+v", last)
	}
}

// Deferred tools: first request omits deferred defs (ToolSearch +
// DeferredToolPlaceholder instead) and announces names in a
// <system-reminder>; a ToolSearch call loads them for later requests.
func TestDeferredToolFlow(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "1")
	srv, err := mock.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	os.Setenv("ANTHROPIC_BASE_URL", srv.Addr)
	creds := &auth.Credentials{AccessToken: "mock"}
	l := NewLoop(api.NewClient(creds, "test-session", api.EntrySDKCLI), t.TempDir())
	l.ToolCtx.Permission = func(string, json.RawMessage, string) bool { return true }
	if _, err := l.Ask(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	var req struct {
		Tools []struct {
			Name         string `json:"name"`
			DeferLoading bool   `json:"defer_loading"`
		} `json:"tools"`
	}
	body := srv.LastBody
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	var sawPlaceholder, sawSearch, sawFetch bool
	for _, tl := range req.Tools {
		switch tl.Name {
		case "DeferredToolPlaceholder":
			sawPlaceholder = true
			if !tl.DeferLoading {
				t.Error("placeholder missing defer_loading:true")
			}
		case "ToolSearch":
			sawSearch = true
		case "WebFetch":
			sawFetch = true
		}
	}
	if !sawPlaceholder || !sawSearch {
		t.Error("first request must carry placeholder + ToolSearch")
	}
	if sawFetch {
		t.Error("deferred WebFetch must not be in the first request's tools")
	}
	if !bytes.Contains(body, []byte("deferred tools are now available via ToolSearch")) {
		t.Error("deferred announcement reminder missing from first request")
	}

	// model calls ToolSearch to load WebFetch
	if _, err := l.Ask(context.Background(),
		`tool:ToolSearch {"query":"select:WebFetch"}`); err != nil {
		t.Fatal(err)
	}
	// history carries the tool_reference result blocks
	var sawRef bool
	for _, m := range l.Messages {
		var bs []api.ContentBlock
		if json.Unmarshal(m.Content, &bs) != nil {
			continue
		}
		for _, b := range bs {
			if b.Type != "tool_result" {
				continue
			}
			var inner []api.ContentBlock
			json.Unmarshal(b.Content, &inner)
			for _, ib := range inner {
				if ib.Type == "tool_reference" && ib.ToolName == "WebFetch" {
					sawRef = true
				}
			}
		}
	}
	if !sawRef {
		t.Error("ToolSearch result must be tool_reference blocks")
	}
	// the follow-up request (carrying the tool_result) now includes WebFetch
	body = srv.LastBody
	req.Tools = nil
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	sawFetch = false
	for _, tl := range req.Tools {
		if tl.Name == "WebFetch" {
			sawFetch = true
		}
	}
	if !sawFetch {
		t.Error("discovered WebFetch must join the tools array")
	}
}

// Calling a still-deferred tool directly fails like real's
// InputValidationError.
func TestDeferredCallRefused(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "1")
	l := mockLoop(t)
	l.Ask(context.Background(), "hi") // announce deferred names
	_, err := l.Ask(context.Background(), `tool:WebFetch {"url":"https://x","prompt":"y"}`)
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, m := range l.Messages {
		if bytes.Contains(m.Content, []byte("InputValidationError")) {
			saw = true
		}
	}
	if !saw {
		t.Error("un-loaded deferred call must fail with InputValidationError")
	}
}

// TestHookBasePayload: runHook's stdin carries real's xd() base context —
// prompt_id, effort{level} on per-turn-effort models, scratchpad_dir.
func TestHookBasePayload(t *testing.T) {
	l := mockLoop(t)
	tmp := t.TempDir()
	l.ToolCtx.CWD = tmp
	dump := filepath.Join(tmp, "payload.json")
	l.Hooks = hooks.Config{
		string(hooks.UserPromptSubmit): []hooks.Matcher{{Hooks: []hooks.Command{
			{Type: "command", Command: "cat > " + dump},
		}}},
	}
	if _, err := l.Ask(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal("hook stdin not dumped")
	}
	var p map[string]any
	json.Unmarshal(b, &p)
	if p["prompt_id"] == "" || p["prompt_id"] == nil {
		t.Error("prompt_id missing from hook payload")
	}
	if p["hook_event_name"] != "UserPromptSubmit" {
		t.Error("hook_event_name missing")
	}
	if eff, ok := p["effort"].(map[string]any); !ok || eff["level"] == "" {
		t.Error("effort{level} missing on xhigh model")
	}
	if p["scratchpad_dir"] == "" || p["scratchpad_dir"] == nil {
		t.Error("scratchpad_dir missing")
	}
	if p["permission_mode"] == "" {
		t.Error("permission_mode missing")
	}
}

// TestHookAllowAutoClassifier: in auto mode a PreToolUse hook's allow does
// NOT bypass the classifier — real requires a classifier-sourced verdict.
func TestHookAllowAutoClassifier(t *testing.T) {
	l := mockLoop(t)
	tmp := t.TempDir()
	l.ToolCtx.CWD = tmp
	l.PermMode = "auto"
	var lastAsk atomic.Bool
	l.ToolCtx.PermissionAsk = func() bool { return lastAsk.Load() }
	l.ToolCtx.Permission = func(string, json.RawMessage, string) bool {
		lastAsk.Store(true)
		return false // unresolved → ask lane
	}
	var evals atomic.Int64
	l.SafetyEval = func(ctx context.Context, name string, input json.RawMessage) (int, string, error) {
		evals.Add(1)
		return 10, "", nil // allow verdict
	}
	l.Hooks = hooks.Config{
		string(hooks.PreToolUse): []hooks.Matcher{{Hooks: []hooks.Command{
			{Type: "command", Command: `echo '{"hookSpecificOutput":{"permissionDecision":"allow"}}'`},
		}}},
	}
	if _, err := l.Ask(context.Background(), `tool:Bash {"command":"echo hi"}`); err != nil {
		t.Fatal(err)
	}
	if evals.Load() == 0 {
		t.Error("auto mode must still run the classifier despite a hook allow")
	}
}

// TestHookAllowNoClassifierDefaultMode: outside auto/plan mode a hook
// allow dispatches directly (classifier not consulted).
func TestHookAllowNoClassifierDefaultMode(t *testing.T) {
	l := mockLoop(t)
	l.ToolCtx.Permission = func(string, json.RawMessage, string) bool { return false }
	var evals atomic.Int64
	l.SafetyEval = func(ctx context.Context, name string, input json.RawMessage) (int, string, error) {
		evals.Add(1)
		return 10, "", nil
	}
	l.Hooks = hooks.Config{
		string(hooks.PreToolUse): []hooks.Matcher{{Hooks: []hooks.Command{
			{Type: "command", Command: `echo '{"hookSpecificOutput":{"permissionDecision":"allow"}}'`},
		}}},
	}
	if _, err := l.Ask(context.Background(), `tool:Bash {"command":"echo hi"}`); err != nil {
		t.Fatal(err)
	}
	if evals.Load() != 0 {
		t.Error("default-mode hook allow should dispatch without the classifier")
	}
}
