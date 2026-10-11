package agent

import (
	"testing"

	"github.com/gbasin/claude-go/internal/api"
	"github.com/gbasin/claude-go/internal/hooks"
	"github.com/gbasin/claude-go/internal/tools"
)

// runHook emits real's hook_system_message attachment when a hook returns
// a systemMessage — content, hookName "<Event>:<matcher>", toolUseID from
// the payload (tool events) or a generated hook- id.
func TestHookSystemMessageAttachment(t *testing.T) {
	l := &Loop{
		Client:  &api.Client{SessionID: "s1"},
		ToolCtx: &tools.Context{CWD: t.TempDir()},
		Hooks: hooks.Config{
			"PostToolUse": []hooks.Matcher{{
				Matcher: "Bash",
				Hooks: []hooks.Command{{
					Command: `printf '{"systemMessage":"hook says hi"}'`,
				}},
			}},
		},
	}
	var att map[string]any
	l.OnAttachment = func(a, extra map[string]any) { att = a }

	res := l.RunHookEvent(hooks.PostToolUse, map[string]any{
		"tool_name": "Bash", "tool_use_id": "tu_123",
	})
	if res.SystemMessage != "hook says hi" {
		t.Fatalf("SystemMessage = %q", res.SystemMessage)
	}
	if att == nil {
		t.Fatal("no hook_system_message attachment emitted")
	}
	if att["type"] != "hook_system_message" || att["content"] != "hook says hi" {
		t.Fatalf("attachment = %v", att)
	}
	if att["hookName"] != "PostToolUse:Bash" {
		t.Fatalf("hookName = %v", att["hookName"])
	}
	if att["toolUseID"] != "tu_123" || att["hookEvent"] != "PostToolUse" {
		t.Fatalf("attachment = %v", att)
	}
}

// non-tool hook events get a generated hook-<uuid> toolUseID (real's
// `hook-${id}` fallback) and the bare event name as hookName.
func TestHookSystemMessageNoTool(t *testing.T) {
	l := &Loop{
		Client:  &api.Client{SessionID: "s1"},
		ToolCtx: &tools.Context{CWD: t.TempDir()},
		Hooks: hooks.Config{
			"Stop": []hooks.Matcher{{
				Hooks: []hooks.Command{{
					Command: `printf '{"systemMessage":"stop note"}'`,
				}},
			}},
		},
	}
	var att map[string]any
	l.OnAttachment = func(a, _ map[string]any) { att = a }
	l.RunHookEvent(hooks.Stop, nil)
	if att == nil {
		t.Fatal("no attachment")
	}
	tid, _ := att["toolUseID"].(string)
	if len(tid) < 6 || tid[:5] != "hook-" {
		t.Fatalf("toolUseID = %q, want hook-<id>", tid)
	}
	if att["hookName"] != "Stop" {
		t.Fatalf("hookName = %v", att["hookName"])
	}
}

// fork agents stamp fork-context-ref at the head of their transcript —
// covered by the session store tests; here just assert the flag.
func TestForkFlagSet(t *testing.T) {
	l := NewLoop(&api.Client{SessionID: "p"}, t.TempDir())
	if l.forkedContext {
		t.Fatal("forkedContext default should be false")
	}
}
