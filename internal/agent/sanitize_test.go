package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"claude-go/internal/api"
	"claude-go/internal/tools"
)

func apiErr(msg string) error {
	return &api.ErrAPI{E: &api.APIError{Type: "invalid_request_error", Message: msg}}
}

func TestClassify400(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{apiErr("prompt is too long"), "prompt_too_long"},
		{apiErr("request_too_large"), "prompt_too_long"},
		{fmt.Errorf("api 413 Payload Too Large"), "prompt_too_long"},
		{apiErr("messages.2.content.0: advisor_tool_result not allowed"), "advisor_tool_result"},
		{apiErr("Invalid signature in thinking block"), "thinking"},
		{apiErr("adaptive thinking is not supported"), "thinking"},
		{apiErr("invalid data in redacted_thinking block"), "thinking"},
		{apiErr("could not decrypt encrypted content"), "encrypted"},
		{apiErr("Unexpected value `clear_at`"), "unexpected_value"},
		{apiErr("some unrelated 400"), ""},
		{errors.New("connection reset"), ""},
	}
	for _, c := range cases {
		if got := classify400(c.err); got != c.want {
			t.Errorf("classify400(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestStripThinking(t *testing.T) {
	msgs := []api.Message{
		api.BlocksMessage("assistant", []api.ContentBlock{
			{Type: "thinking", Thinking: "hmm", Signature: "sig"},
			{Type: "text", Text: "answer"},
		}),
		api.TextMessage("user", "next"),
	}
	l := &Loop{Messages: msgs}
	l.applySanitize("thinking")
	if !l.noThinking {
		t.Error("noThinking not set")
	}
	var blocks []api.ContentBlock
	json.Unmarshal(l.Messages[0].Content, &blocks)
	if len(blocks) != 1 || blocks[0].Type != "text" {
		t.Fatalf("blocks = %+v", blocks)
	}
}

func TestStripAdvisor(t *testing.T) {
	msgs := []api.Message{
		api.BlocksMessage("user", []api.ContentBlock{
			{Type: "advisor_tool_result", Content: json.RawMessage(`[]`)},
		}),
	}
	l := &Loop{Messages: msgs}
	l.applySanitize("advisor_tool_result")
	var blocks []map[string]any
	json.Unmarshal(l.Messages[0].Content, &blocks)
	if len(blocks) != 1 || blocks[0]["text"] != "[Advisor response]" {
		t.Fatalf("blocks = %v", blocks)
	}
}

func TestStripClearAt(t *testing.T) {
	m := api.BlocksMessage("user", []api.ContentBlock{{Type: "text", Text: "x"}})
	m.OutputConfig = json.RawMessage(`{"effort":"high"}`)
	l := &Loop{Messages: []api.Message{m}}
	l.applySanitize("unexpected_value")
	if l.Messages[0].OutputConfig != nil {
		t.Error("output_config not dropped")
	}
}

func TestDropOldestForRetry(t *testing.T) {
	l := &Loop{Messages: []api.Message{
		api.TextMessage("user", "first"),
		api.BlocksMessage("system", []api.ContentBlock{{Type: "text", Text: "env"}}),
		api.TextMessage("assistant", "a1"),
		api.TextMessage("user", "latest"),
	}}
	if !l.dropOldestForRetry(1) {
		t.Fatal("drop failed")
	}
	if len(l.Messages) != 3 || l.Messages[0].Role != "system" {
		t.Fatalf("messages = %+v", l.Messages)
	}
	// newest message is never dropped
	l2 := &Loop{Messages: []api.Message{api.TextMessage("user", "only")}}
	if l2.dropOldestForRetry(1) {
		t.Fatal("dropped sole message")
	}
}

func TestRepairToolPairing(t *testing.T) {
	msgs := []api.Message{
		api.BlocksMessage("user", []api.ContentBlock{
			{Type: "tool_result", ToolUseID: "missing", Content: json.RawMessage(`[]`)},
			{Type: "text", Text: "kept"},
		}),
	}
	out := repairToolPairing(msgs)
	var blocks []map[string]any
	json.Unmarshal(out[0].Content, &blocks)
	if len(blocks) != 1 || blocks[0]["type"] != "text" {
		t.Fatalf("blocks = %v", blocks)
	}
}

func TestEphemeralBreakpoint(t *testing.T) {
	msgs := []api.Message{
		api.TextMessage("user", "first"),
		api.TextMessage("assistant", "a"),
		api.TextMessage("user", "last"),
	}
	out := withEphemeralBreakpoint(msgs)
	// real puts the breakpoint on the last ASSISTANT block (captured:
	// the tool_use in a tool-loop request), not the trailing user record
	var blocks []api.ContentBlock
	json.Unmarshal(out[1].Content, &blocks)
	if blocks[0].CacheControl == nil || blocks[0].CacheControl.Type != "ephemeral" {
		t.Fatal("no breakpoint on last assistant message")
	}
	var ublocks []api.ContentBlock
	json.Unmarshal(out[2].Content, &ublocks)
	if ublocks[0].CacheControl != nil {
		t.Fatal("user message should not carry the breakpoint")
	}
	// original untouched
	var orig []api.ContentBlock
	json.Unmarshal(msgs[2].Content, &orig)
	if orig[0].CacheControl != nil {
		t.Fatal("mutated stored history")
	}
}

func TestFastModeRequest(t *testing.T) {
	l := &Loop{Model: "claude-sonnet-5-5", Effort: "high", FastMode: true,
		ToolCtx: &tools.Context{Ctx: context.Background(), CWD: t.TempDir()},
		Client:  &api.Client{}}
	req := l.buildRequest()
	var oc map[string]string
	json.Unmarshal(req.OutputConfig, &oc)
	if oc["speed"] != "fast" || oc["effort"] != "high" {
		t.Fatalf("output_config = %v", oc)
	}
	l.SetFastMode(false)
	if l.FastMode {
		t.Fatal("SetFastMode(false) did not toggle")
	}
}

func TestPartialCompactSpan(t *testing.T) {
	l := &Loop{Messages: []api.Message{
		api.TextMessage("user", "m0"),
		api.TextMessage("assistant", "m1"),
		api.TextMessage("user", "m2"),
	}}
	l.applySummary("summary text", "", 2)
	if len(l.Messages) != 2 {
		t.Fatalf("messages = %d", len(l.Messages))
	}
	var blocks []api.ContentBlock
	json.Unmarshal(l.Messages[0].Content, &blocks)
	if blocks[0].Text == "" || l.Messages[1].Role != "user" {
		t.Fatalf("bad partial history: %+v", l.Messages)
	}
}
