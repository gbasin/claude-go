package tools

import (
	"context"
	"encoding/json"
	"strings"

	"claude-go/internal/api"
)

// MCPCaller abstracts an MCP server connection (avoids import cycle).
type MCPCaller interface {
	CallTool(ctx context.Context, name string, args json.RawMessage) (string, bool, error)
}

var mcpTools []*Tool

// RegisterMCP adds an external mcp__server__tool tool.
func RegisterMCP(server string, caller MCPCaller, name, desc string, schema json.RawMessage) {
	full := "mcp__" + server + "__" + name
	if schema == nil {
		schema = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	t := &Tool{
		IsMCP: true, // every MCP tool defers (mod_0314.js h(): isMcp===true)
		Def:   api.Tool{Name: full, Description: desc, InputSchema: schema},
		Exec: func(c *Context, input json.RawMessage) Result {
			out, isErr, err := caller.CallTool(c.Ctx, name, input)
			if err != nil {
				return Result{Content: "mcp error: " + err.Error(), IsError: true}
			}
			return Result{Content: out, IsError: isErr}
		},
	}
	mcpTools = append(mcpTools, t)
	// registry membership is what Dispatch resolves — deferred tools stay
	// refused in the loop until ToolSearch discovery, then must dispatch.
	registry[full] = t
}

// UnregisterMCPServer drops a server's registered tools (list_changed
// refresh replaces them via ReregisterMCPServer).
func UnregisterMCPServer(server string) {
	prefix := "mcp__" + server + "__"
	var keep []*Tool
	for _, t := range mcpTools {
		if strings.HasPrefix(t.Def.Name, prefix) {
			delete(registry, t.Def.Name)
			continue
		}
		keep = append(keep, t)
	}
	mcpTools = keep
}

// mcpPrompts maps the slash-command name (mcp__server__prompt) to a fetch
// closure — real surfaces MCP prompts as slash commands.
var mcpPrompts = map[string]func(ctx context.Context, args map[string]string) (string, error){}

// RegisterMCPPrompt makes an MCP prompt invocable as /mcp__server__name;
// fetch runs prompts/get and returns the flattened message text.
func RegisterMCPPrompt(server, prompt string, fetch func(ctx context.Context, args map[string]string) (string, error)) {
	mcpPrompts["mcp__"+server+"__"+prompt] = fetch
}

// ResolveMCPPrompt returns the fetch closure for a /mcp__ prompt command.
func ResolveMCPPrompt(cmd string) (func(context.Context, map[string]string) (string, error), bool) {
	f, ok := mcpPrompts[cmd]
	return f, ok
}
