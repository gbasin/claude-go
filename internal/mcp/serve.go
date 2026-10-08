// serve.go — `claude mcp serve`: run claude-go AS an MCP server over
// stdio, exposing the builtin tool registry to MCP clients (real's
// startMCPServer, mod_2170.js). NDJSON framing: one JSON-RPC message
// per line on stdin/stdout.
package mcp

import (
	"bufio"
	"claude-go/internal/api"
	"claude-go/internal/tools"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

type serveReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type serveResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *serveErr       `json:"error,omitempty"`
}

type serveErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve runs the MCP server loop on stdin/stdout until EOF. ctx carries
// the tool execution environment (cwd, session, permission policy).
func Serve(ctx *tools.Context, stdin io.Reader, stdout io.Writer) error {
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	out := bufio.NewWriter(stdout)
	write := func(v any) {
		b, _ := json.Marshal(v)
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req serveReq
		if json.Unmarshal(line, &req) != nil {
			continue
		}
		isNotify := len(req.ID) == 0
		switch req.Method {
		case "initialize":
			write(serveResp{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
				"protocolVersion": ProtocolVersion,
				"capabilities": map[string]any{
					"tools": map[string]any{"listChanged": true},
				},
				"serverInfo": map[string]any{
					"name": "claude-go", "version": api.Version,
				},
			}})
		case "ping":
			write(serveResp{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}})
		case "notifications/initialized", "notifications/cancelled",
			"notifications/progress", "notifications/message":
			// server ignores notifications
		case "tools/list":
			write(serveResp{JSONRPC: "2.0", ID: req.ID,
				Result: map[string]any{"tools": serveToolDefs(ctx)}})
		case "tools/call":
			write(toolsCall(ctx, req))
		case "resources/list":
			write(serveResp{JSONRPC: "2.0", ID: req.ID,
				Result: map[string]any{"resources": []any{}}})
		case "prompts/list":
			write(serveResp{JSONRPC: "2.0", ID: req.ID,
				Result: map[string]any{"prompts": []any{}}})
		default:
			if !isNotify {
				write(serveResp{JSONRPC: "2.0", ID: req.ID,
					Error: &serveErr{Code: -32601, Message: "method not found: " + req.Method}})
			}
		}
	}
	return sc.Err()
}

// serveToolDefs exposes non-interactive tools — real's serve set is the
// enabled registry minus interactive-only/UI tools.
func serveToolDefs(ctx *tools.Context) []map[string]any {
	var out []map[string]any
	for _, t := range tools.All() {
		var schema map[string]any
		if json.Unmarshal(t.Def.InputSchema, &schema) != nil || schema == nil {
			// MCP spec requires an object — bare/null schemas get a
			// permissive default (real's clients reject null inputSchema)
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"name":        t.Def.Name,
			"description": t.Def.Description,
			"inputSchema": schema,
		})
	}
	return out
}

func toolsCall(ctx *tools.Context, req serveReq) serveResp {
	var in struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(req.Params, &in) != nil || in.Name == "" {
		return serveResp{JSONRPC: "2.0", ID: req.ID,
			Error: &serveErr{Code: -32602, Message: "tools/call requires name"}}
	}
	r := tools.Dispatch(ctx, in.Name, in.Args)
	return serveResp{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": r.Content},
		},
		"isError": r.IsError,
	}}
}

// ServeStdio is the `claude mcp serve` entry — MCP stdio transport on
// process stdin/stdout.
func ServeStdio(cwd string) int {
	ctx := &tools.Context{Ctx: context.Background(), CWD: cwd}
	if err := Serve(ctx, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "mcp serve: "+err.Error())
		return 1
	}
	return 0
}
