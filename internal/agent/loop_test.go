package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"

	"claude-go/internal/api"
	"claude-go/internal/auth"
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
