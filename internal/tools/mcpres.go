package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// ResourceClient extends MCPCaller with resource operations.
type ResourceClient interface {
	MCPCaller
	ListResources(ctx context.Context) (string, error)
	ReadResource(ctx context.Context, uri string) (string, error)
}

var (
	resMu   sync.Mutex
	resSvrs = map[string]ResourceClient{}
)

// RegisterMCPServer stores a resource-capable server for the MCP resource tools.
func RegisterMCPServer(name string, c ResourceClient) {
	resMu.Lock()
	resSvrs[name] = c
	resMu.Unlock()
}

func serverFor(name string) (ResourceClient, error) {
	resMu.Lock()
	defer resMu.Unlock()
	if name == "" {
		// default to first server
		for _, c := range resSvrs {
			return c, nil
		}
		return nil, fmt.Errorf("no MCP servers connected")
	}
	c, ok := resSvrs[name]
	if !ok {
		var names []string
		for n := range resSvrs {
			names = append(names, n)
		}
		return nil, fmt.Errorf("no MCP server %q (connected: %s)", name, strings.Join(names, ", "))
	}
	return c, nil
}

func init() {
	Register(&Tool{Def: loadSchema("ListMcpResourcesTool"), Exec: execListMcpRes})
	Register(&Tool{Def: loadSchema("ReadMcpResourceTool"), Exec: execReadMcpRes})
	Register(&Tool{Def: loadSchema("ReadMcpResourceDirTool"), Exec: execReadMcpRes})
}

type resIn struct {
	Server string `json:"server"`
	URI    string `json:"uri"`
}

func execListMcpRes(c *Context, raw json.RawMessage) Result {
	var in resIn
	json.Unmarshal(raw, &in)
	s, err := serverFor(in.Server)
	if err != nil {
		return Result{Content: err.Error(), IsError: true}
	}
	out, err := s.ListResources(c.Ctx)
	if err != nil {
		return Result{Content: "mcp: " + err.Error(), IsError: true}
	}
	if out == "" {
		out = "(no resources)"
	}
	return Result{Content: out}
}

func execReadMcpRes(c *Context, raw json.RawMessage) Result {
	var in resIn
	json.Unmarshal(raw, &in)
	if in.URI == "" {
		return Result{Content: "uri is required", IsError: true}
	}
	s, err := serverFor(in.Server)
	if err != nil {
		return Result{Content: err.Error(), IsError: true}
	}
	out, err := s.ReadResource(c.Ctx, in.URI)
	if err != nil {
		return Result{Content: "mcp: " + err.Error(), IsError: true}
	}
	return Result{Content: out}
}
