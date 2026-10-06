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

// MCPToolEntry is the minimal shape SyncMCPTools needs (kept local —
// tools doesn't import the mcp package).
type MCPToolEntry struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// SyncMCPTools registers any tool in the list that isn't registered
// yet — called after a tools/list_changed notification refreshes the
// client's tool list (real reloads the surface in place).
func SyncMCPTools(server string, caller MCPCaller, list []MCPToolEntry) {
	for _, t := range list {
		full := "mcp__" + server + "__" + t.Name
		if _, ok := registry[full]; ok {
			continue
		}
		RegisterMCP(server, caller, t.Name, t.Description, t.InputSchema)
	}
}
