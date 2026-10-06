package repl

import (
	"fmt"
	"testing"
)

// MCP prompt slash commands: registration → slash expansion → prompts/get
// text becomes the submitted user message.
func TestMCPPromptExpansion(t *testing.T) {
	var gotArgs map[string]string
	RegisterMCPPrompt("srv__testprompt", "a test prompt",
		func(args map[string]string) (string, error) {
			gotArgs = args
			return "expanded prompt text", nil
		})
	defer delete(customCommands, "srv__testprompt")
	got := expandCustomSlash("/srv__testprompt k=v extra words")
	if got != "expanded prompt text" {
		t.Fatalf("expansion: %q", got)
	}
	if gotArgs["k"] != "v" || gotArgs["arguments"] != "extra words" {
		t.Fatalf("args: %+v", gotArgs)
	}
	// failure must not submit a prompt
	RegisterMCPPrompt("srv__bad", "x", func(map[string]string) (string, error) {
		return "", fmt.Errorf("server gone")
	})
	defer delete(customCommands, "srv__bad")
	if expandCustomSlash("/srv__bad") != "" {
		t.Fatal("failed prompt must expand to nothing")
	}
}
