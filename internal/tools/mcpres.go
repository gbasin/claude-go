package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ResourceClient extends MCPCaller with resource operations.
type ResourceClient interface {
	MCPCaller
	ListResources(ctx context.Context) (string, error)
	ReadResource(ctx context.Context, uri string) (string, error)
}

// resourceLister is implemented by MCP clients that can return the raw
// resources/list result for structured output.
type resourceLister interface {
	ListResourcesJSON(ctx context.Context) (json.RawMessage, error)
}

// dirReader is implemented by MCP clients exposing the
// resources/directory/read RPC (directory listing support).
type dirReader interface {
	ReadResourceDir(ctx context.Context, uri string) (string, error)
}

// rawCaller is implemented by clients exposing a raw JSON-RPC call path;
// used to reach resources/directory/read when no dedicated method exists.
type rawCaller interface {
	Call(ctx context.Context, method string, params any, out any) error
}

var (
	resMu   sync.Mutex
	resSvrs = map[string]ResourceClient{}
	// server status for /mcp — name → {tools, err}; real shows every
	// configured server with its connect state
	srvStatus = map[string]struct {
		Tools int
		Err   error
	}{}
)

// RegisterMCPServer stores a resource-capable server for the MCP resource tools.
func RegisterMCPServer(name string, c ResourceClient) {
	resMu.Lock()
	resSvrs[name] = c
	resMu.Unlock()
}

// NoteMCPServerStatus records a connect attempt's outcome for /mcp.
func NoteMCPServerStatus(name string, toolCount int, err error) {
	resMu.Lock()
	srvStatus[name] = struct {
		Tools int
		Err   error
	}{toolCount, err}
	resMu.Unlock()
}

// MCPServerStatus returns configured-server status rows for /mcp.
func MCPServerStatus() []struct {
	Name  string
	Tools int
	Err   error
} {
	resMu.Lock()
	defer resMu.Unlock()
	var out []struct {
		Name  string
		Tools int
		Err   error
	}
	for n, s := range srvStatus {
		out = append(out, struct {
			Name  string
			Tools int
			Err   error
		}{n, s.Tools, s.Err})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func serverFor(name string) (ResourceClient, error) {
	resMu.Lock()
	defer resMu.Unlock()
	if name == "" {
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
		sort.Strings(names)
		return nil, fmt.Errorf("no MCP server %q (connected: %s)", name, strings.Join(names, ", "))
	}
	return c, nil
}

func namedServer(name string) (string, ResourceClient, error) {
	c, err := serverFor(name)
	if err != nil {
		return "", nil, err
	}
	if name == "" {
		resMu.Lock()
		for n, cl := range resSvrs {
			if cl == c {
				resMu.Unlock()
				return n, c, nil
			}
		}
		resMu.Unlock()
	}
	return name, c, nil
}

func allServers() map[string]ResourceClient {
	resMu.Lock()
	defer resMu.Unlock()
	out := make(map[string]ResourceClient, len(resSvrs))
	for n, c := range resSvrs {
		out[n] = c
	}
	return out
}

func init() {
	Register(&Tool{Def: loadSchema("ListMcpResourcesTool"), Exec: execListMcpRes})
	Register(&Tool{Def: loadSchema("ReadMcpResourceTool"), Exec: execReadMcpRes})
	Register(&Tool{Def: loadSchema("ReadMcpResourceDirTool"), Exec: execReadMcpResDir})
}

type resIn struct {
	Server string `json:"server"`
	URI    string `json:"uri"`
}

// resEntry is one resource with the server it came from — the shape the
// real ListMcpResourcesTool returns (every resource carries a `server`
// field; a bare call aggregates all connected servers).
type resEntry struct {
	URI         string `json:"uri"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	Server      string `json:"server"`
}

// resListFor one server's resources, via the structured lister when the
// client provides one and falling back to the text listing's fixed
// "uri  name  description" columns otherwise.
func resListFor(ctx context.Context, name string, cl ResourceClient) ([]resEntry, error) {
	if rl, ok := cl.(resourceLister); ok {
		raw, err := rl.ListResourcesJSON(ctx)
		if err != nil {
			return nil, err
		}
		var wrapped struct {
			Resources []resEntry `json:"resources"`
		}
		var list []resEntry
		if json.Unmarshal(raw, &wrapped) == nil && wrapped.Resources != nil {
			list = wrapped.Resources
		} else if json.Unmarshal(raw, &list) != nil {
			return nil, fmt.Errorf("unparseable resources/list result")
		}
		for i := range list {
			list[i].Server = name
		}
		return list, nil
	}
	out, err := cl.ListResources(ctx)
	if err != nil {
		return nil, err
	}
	var list []resEntry
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		line = strings.TrimRight(line, " ")
		if line == "" {
			continue
		}
		// client.go writes "uri  name  description" (two-space separators)
		e := resEntry{Server: name}
		cols := strings.SplitN(line, "  ", 3)
		e.URI = strings.TrimSpace(cols[0])
		if len(cols) > 1 {
			e.Name = strings.TrimSpace(cols[1])
		}
		if len(cols) > 2 {
			e.Description = strings.TrimSpace(cols[2])
		}
		list = append(list, e)
	}
	return list, nil
}

func execListMcpRes(c *Context, raw json.RawMessage) Result {
	var in resIn
	json.Unmarshal(raw, &in)
	var out []resEntry
	if in.Server != "" {
		cl, err := serverFor(in.Server)
		if err != nil {
			return Result{Content: err.Error(), IsError: true}
		}
		list, err := resListFor(c.Ctx, in.Server, cl)
		if err != nil {
			return Result{Content: "mcp: " + err.Error(), IsError: true}
		}
		out = list
	} else {
		svrs := allServers()
		if len(svrs) == 0 {
			return Result{Content: "no MCP servers connected", IsError: true}
		}
		var names []string
		for n := range svrs {
			names = append(names, n)
		}
		sort.Strings(names)
		var errs []string
		for _, n := range names {
			list, err := resListFor(c.Ctx, n, svrs[n])
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", n, err))
				continue
			}
			out = append(out, list...)
		}
		if out == nil && len(errs) > 0 {
			return Result{Content: "mcp: " + strings.Join(errs, "; "), IsError: true}
		}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return Result{Content: string(b)}
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

// execReadMcpResDir calls the resources/directory/read RPC: the
// non-recursive listing of a directory resource's direct children.
// It prefers a dedicated client method and otherwise goes through a raw
// JSON-RPC call when the client exposes one.
func execReadMcpResDir(c *Context, raw json.RawMessage) Result {
	var in resIn
	json.Unmarshal(raw, &in)
	if in.Server == "" {
		return Result{Content: "server is required", IsError: true}
	}
	if in.URI == "" {
		return Result{Content: "uri is required", IsError: true}
	}
	name, s, err := namedServer(in.Server)
	if err != nil {
		return Result{Content: err.Error(), IsError: true}
	}
	if dr, ok := s.(dirReader); ok {
		out, err := dr.ReadResourceDir(c.Ctx, in.URI)
		if err != nil {
			return Result{Content: "mcp: " + err.Error(), IsError: true}
		}
		return Result{Content: out}
	}
	if rc, ok := s.(rawCaller); ok {
		var res struct {
			Contents []struct {
				URI      string `json:"uri"`
				Name     string `json:"name"`
				MimeType string `json:"mimeType"`
			} `json:"contents"`
		}
		if err := rc.Call(c.Ctx, "resources/directory/read",
			map[string]any{"uri": in.URI}, &res); err != nil {
			return Result{Content: "mcp: " + err.Error(), IsError: true}
		}
		b, _ := json.MarshalIndent(map[string]any{
			"server":   name,
			"uri":      in.URI,
			"contents": res.Contents,
		}, "", "  ")
		return Result{Content: string(b)}
	}
	return Result{Content: fmt.Sprintf(
		"MCP server %q does not support directory listing (no resources/directory/read)",
		name), IsError: true}
}
