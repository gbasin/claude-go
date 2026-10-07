package repl

import (
	"context"
	"fmt"
	"testing"

	"claude-go/internal/tools"
)

// MCP prompt slash commands: tools.RegisterMCPPrompt → ResolveMCPPrompt
// fetch closure → prompts/get text becomes the submitted user message.
// The e2e path is covered by TestMCPPromptSlash.
func TestMCPPromptResolution(t *testing.T) {
	var gotArgs map[string]string
	tools.RegisterMCPPrompt("srv", "testprompt",
		func(ctx context.Context, args map[string]string) (string, error) {
			gotArgs = args
			return "expanded prompt text", nil
		})
	fetch, ok := tools.ResolveMCPPrompt("mcp__srv__testprompt")
	if !ok {
		t.Fatal("registered prompt not resolvable")
	}
	out, err := fetch(context.Background(), map[string]string{"input": "k=v"})
	if err != nil || out != "expanded prompt text" {
		t.Fatalf("fetch: %q %v", out, err)
	}
	if gotArgs["input"] != "k=v" {
		t.Fatalf("args: %+v", gotArgs)
	}
	if _, ok := tools.ResolveMCPPrompt("mcp__srv__other"); ok {
		t.Fatal("unregistered prompt must not resolve")
	}
	// a failed fetch surfaces an error — callers must not submit it
	tools.RegisterMCPPrompt("srv", "bad",
		func(context.Context, map[string]string) (string, error) {
			return "", fmt.Errorf("server gone")
		})
	if bad, _ := tools.ResolveMCPPrompt("mcp__srv__bad"); bad != nil {
		if _, err := bad(context.Background(), nil); err == nil {
			t.Fatal("expected fetch error")
		}
	}
}
