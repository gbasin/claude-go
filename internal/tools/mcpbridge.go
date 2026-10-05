package tools

import (
	"context"
	"encoding/json"

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
