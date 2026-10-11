package agent

import (
	"encoding/json"
	"testing"

	"github.com/gbasin/claude-go/internal/tools"
)

// TestSubagentToolDisplayHook: a foreground subagent's inner tool calls
// surface through Runner.OnSubToolUse/OnSubToolResult for the host TUI's
// nested render.
func TestSubagentToolDisplayHook(t *testing.T) {
	parent := mockLoop(t)
	parent.ToolCtx.Permission = func(string, json.RawMessage, string) bool { return true }
	r := &Runner{
		Client: parent.Client, PermMode: parent.PermMode, Model: parent.Model,
		User: parent.User, PermRules: parent.PermRules, Parent: parent,
	}
	var uses []string
	var results []string
	r.OnSubToolUse = func(name string, _ json.RawMessage) { uses = append(uses, name) }
	r.OnSubToolResult = func(name string, _ tools.Result) { results = append(results, name) }

	res := r.RunAgent(parent.ToolCtx, "nested", `tool:Bash {"command":"echo inner"}`,
		"general-purpose", "", "", false)
	if res.IsError {
		t.Fatalf("agent error: %s", res.Content)
	}
	if len(uses) == 0 || uses[0] != "Bash" {
		t.Fatalf("OnSubToolUse = %v, want Bash", uses)
	}
	if len(results) == 0 || results[0] != "Bash" {
		t.Fatalf("OnSubToolResult = %v, want Bash", results)
	}
}
