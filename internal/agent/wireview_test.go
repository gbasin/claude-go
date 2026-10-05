package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"claude-go/internal/api"
	"claude-go/internal/tools"
)

// wireMessages shape vs captured real wire: req1 keeps the env composite
// (hook ctx merged in); req2+ replaces it with a bare-string hook record
// and appends [total_tokens block][parallel-instruction string].
func TestWireMessagesShape(t *testing.T) {
	l := &Loop{
		Model:          "claude-fable-5-1",
		SessionHookCtx: "<hook-ctx>",
		tokensLeft:     15000000,
		ToolCtx:        &tools.Context{},
		Client:         &api.Client{},
	}
	env := api.BlocksMessage("system", []api.ContentBlock{{Type: "text", Text: "ENV"}})
	l.Messages = []api.Message{api.TextMessage("user", "hi"), env}
	l.envIdx = 1

	r1 := l.wireMessages()
	if len(r1) != 2 || r1[1].Role != "system" {
		t.Fatalf("req1 shape: %d msgs", len(r1))
	}
	var cc []api.ContentBlock
	json.Unmarshal(r1[1].Content, &cc)
	if len(cc) == 0 {
		t.Fatal("req1 env should stay a block message")
	}

	// req2 after a tool loop
	l.Messages = append(l.Messages,
		api.BlocksMessage("assistant", []api.ContentBlock{{Type: "tool_use", ID: "t1", Name: "Bash"}}),
		api.BlocksMessage("user", []api.ContentBlock{{Type: "tool_result", ToolUseID: "t1"}}))
	r2 := l.wireMessages()
	// [user, sys-string hook, asst, user, sys total_tokens, sys parallel]
	if len(r2) != 6 {
		t.Fatalf("req2 len %d", len(r2))
	}
	// msg1: bare string form
	var s string
	if err := json.Unmarshal(r2[1].Content, &s); err != nil || !strings.Contains(s, "hook-ctx") {
		t.Fatalf("msg1 should be bare-string hook ctx: %s", r2[1].Content)
	}
	// msg4: total_tokens block w/ cc
	var tt []api.ContentBlock
	json.Unmarshal(r2[4].Content, &tt)
	if !strings.Contains(tt[0].Text, "total_tokens") || tt[0].CacheControl == nil {
		t.Fatal("total_tokens block missing cc")
	}
	// msg5: parallel instruction bare string
	var ps string
	if err := json.Unmarshal(r2[5].Content, &ps); err != nil || !strings.Contains(ps, "First privately list") {
		t.Fatalf("msg5 wrong: %s", r2[5].Content)
	}
	// breakpoint on the assistant tool_use block
	var ab []api.ContentBlock
	json.Unmarshal(r2[2].Content, &ab)
}

// New-turn ambient block: when ambientPending, the persisted string
// record rides on the wire as the trailing cached system block
// (captured post-compact request shape).
func TestWireMessagesAmbientTail(t *testing.T) {
	l := &Loop{
		Model:          "claude-fable-5-1",
		SessionHookCtx: "<hook>",
		tokensLeft:     15000000,
		ambientPending: true,
		envSent:        true,
		envIdx:         1,
		ToolCtx:        &tools.Context{},
		Client:         &api.Client{},
	}
	l.Messages = []api.Message{
		api.TextMessage("user", "ctx"),
		api.StringMessage("system", "SessionStart hook additional context: <hook>"),
		api.TextMessage("assistant", "reply"),
		api.StringMessage("user", "next prompt"),
		api.StringMessage("system", "The following MCP servers are configured but failed to connect — X"),
	}
	out := l.wireMessages()
	if len(out) != 5 {
		t.Fatalf("len %d", len(out))
	}
	// last msg must be the ambient BLOCK with cc, not the bare string
	var blocks []api.ContentBlock
	if err := json.Unmarshal(out[4].Content, &blocks); err != nil || len(blocks) == 0 {
		t.Fatalf("ambient tail should be block form: %s", out[4].Content)
	}
	if blocks[0].CacheControl == nil || blocks[0].CacheControl.TTL != "1h" {
		t.Fatal("ambient block missing 1h cache_control")
	}
	// mid-array ambient stays a bare string
	var s string
	if err := json.Unmarshal(out[1].Content, &s); err != nil {
		t.Fatalf("msg1 should stay bare string: %s", out[1].Content)
	}
}
