package agent

import (
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

	if len(l.Messages) != 2 { // user + assistant
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
