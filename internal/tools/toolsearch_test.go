package tools

import (
	"context"
	"encoding/json"
	"testing"
)

func TestDeferredExcludedAndDiscovered(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "1")
	ctx := &Context{}
	defs := RequestDefs(ctx)
	var names []string
	for _, d := range defs {
		names = append(names, d.Name)
	}
	// WebFetch is shouldDefer:!0 in real — absent from the wire tools.
	for _, d := range defs {
		if d.Name == "WebFetch" {
			t.Fatal("deferred tool WebFetch must not be in the request tools")
		}
	}
	// real's captured wire order (2.1.289): ..., Skill, ToolSearch,
	// Workflow, DeferredToolPlaceholder, Write, advisor
	idx := map[string]int{}
	for i, d := range defs {
		idx[d.Name] = i
	}
	if !(idx["Skill"]+1 == idx["ToolSearch"] &&
		idx["Workflow"]+1 == idx["DeferredToolPlaceholder"] &&
		idx["DeferredToolPlaceholder"]+1 == idx["Write"]) {
		t.Fatalf("order = %v", names)
	}
	// advisor is a wire-gated server block — absent until configured.
	if _, ok := idx["advisor"]; ok {
		t.Fatal("advisor must not ship without a configured model")
	}
	t.Setenv("CLAUDE_GO_ADVISOR", "claude-sonnet-5-5")
	defsA := RequestDefs(&Context{})
	var adv map[string]any
	foundAdv := false
	for _, d := range defsA {
		if d.Name == "advisor" {
			foundAdv = true
			json.Unmarshal(d.Raw, &adv)
		}
	}
	if !foundAdv || adv["type"] != "advisor_20260301" || adv["model"] != "claude-sonnet-5-5" {
		t.Fatalf("configured advisor must tail tools[] with its model, got %v", adv)
	}
	t.Setenv("CLAUDE_GO_ADVISOR", "")
	phIdx := idx["DeferredToolPlaceholder"]
	if !defs[phIdx].DeferLoading {
		t.Error("placeholder must carry defer_loading:true")
	}
	var ph map[string]any
	if err := json.Unmarshal(defs[phIdx].InputSchema, &ph); err != nil {
		t.Fatal(err)
	}

	// select: loads the tool — result is tool_reference blocks (Refs).
	r := Dispatch(ctx, "ToolSearch", json.RawMessage(`{"query":"select:WebFetch"}`))
	if len(r.Refs) != 1 || r.Refs[0] != "WebFetch" {
		t.Fatalf("select result = %+v", r)
	}
	if !ctx.IsDiscovered("WebFetch") {
		t.Fatal("WebFetch should be discovered after select")
	}
	// next request includes the schema
	found := false
	for _, d := range RequestDefs(ctx) {
		if d.Name == "WebFetch" {
			found = true
		}
	}
	if !found {
		t.Error("discovered WebFetch must join the tools array")
	}
}

func TestToolSearchKeywordAndRequired(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "1")
	ctx := &Context{}
	r := Dispatch(ctx, "ToolSearch", json.RawMessage(`{"query":"notebook jupyter"}`))
	if len(r.Refs) == 0 {
		t.Fatal("keyword search found nothing")
	}
	// +term requires the term in the name (k5n A branch)
	r2 := Dispatch(&Context{}, "ToolSearch", json.RawMessage(`{"query":"+notebook zzz"}`))
	for _, n := range r2.Refs {
		if n != "NotebookEdit" && n != "NotebookRead" {
			t.Fatalf("+notebook matched %q", n)
		}
	}
	// unknown name → the empty-result text
	r3 := Dispatch(&Context{}, "ToolSearch", json.RawMessage(`{"query":"select:NoSuchTool999"}`))
	if len(r3.Refs) != 0 || r3.Content != "No matching deferred tools found" {
		t.Fatalf("empty select = %+v", r3)
	}
}

func TestToolSearchMCPTools(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "1")
	RegisterMCP("slack", testMCPCaller{}, "post_message", "post a slack message", nil)
	defer func() { mcpTools = mcpTools[:len(mcpTools)-1] }()
	ctx := &Context{}
	// MCP tools defer automatically (isMcp===true)
	if !IsDeferredName("mcp__slack__post_message") {
		t.Fatal("mcp tools must defer")
	}
	// trailing-name select matches (PMe's toolName arm)
	r := Dispatch(ctx, "ToolSearch", json.RawMessage(`{"query":"select:post_message"}`))
	if len(r.Refs) != 1 || r.Refs[0] != "mcp__slack__post_message" {
		t.Fatalf("mcp select = %+v", r)
	}
}

type testMCPCaller struct{}

func (testMCPCaller) CallTool(_ context.Context, _ string, _ json.RawMessage) (string, bool, error) {
	return "", false, nil
}

func TestToolSearchDisabled(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "0")
	ctx := &Context{}
	defs := RequestDefs(ctx)
	for _, d := range defs {
		if d.Name == "ToolSearch" || d.Name == "DeferredToolPlaceholder" {
			t.Fatalf("disabled lane must not emit %s", d.Name)
		}
	}
	var hasFetch bool
	for _, d := range defs {
		if d.Name == "WebFetch" {
			hasFetch = true
		}
	}
	if !hasFetch {
		t.Error("disabled lane ships all tools inline")
	}
	if len(ctx.DeferredPool()) != 0 {
		t.Error("pool must be empty when disabled")
	}
}
