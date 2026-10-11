package mcp

// `claude mcp serve` — expose the agent's builtin tool surface as an MCP
// server over stdio (real's serve: "Start the Claude Code MCP server").
// Line-delimited JSON-RPC, same framing as a stdio transport server.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/gbasin/claude-go/internal/api"
	"github.com/gbasin/claude-go/internal/tools"
)

// ServeOpts carries the serve-mode options (`-d/--debug`, --verbose).
type ServeOpts struct {
	Verbose bool
	Debug   bool
}

// Serve runs the MCP server on the given streams until EOF. It never
// writes anything but JSON-RPC frames to w — logs go to errOut.
func Serve(ctx context.Context, in io.Reader, w, errOut io.Writer, o ServeOpts) int {
	if errOut == nil {
		errOut = os.Stderr
	}
	cwd, _ := os.Getwd()
	tc := &tools.Context{
		Ctx: ctx,
		CWD: cwd,
		// serve mode is an explicit opt-in to expose the tool surface;
		// the calling client drives it (real applies the same posture)
		Permission:    func(string, json.RawMessage, string) bool { return true },
		PermissionAsk: func() bool { return false },
		AskUser: func(string, []string) (string, error) {
			return "", fmt.Errorf("interactive prompts unavailable in mcp serve")
		},
		BgTasks: tools.NewBackground(),
	}
	dbg := func(f string, a ...any) {
		if o.Verbose || o.Debug {
			fmt.Fprintf(errOut, "[mcp serve] "+f+"\n", a...)
		}
	}

	wr := bufio.NewWriter(w)
	write := func(v any) {
		b, _ := json.Marshal(v)
		wr.Write(b)
		wr.WriteByte('\n')
		wr.Flush()
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if json.Unmarshal(line, &req) != nil {
			continue
		}
		hasID := len(req.ID) > 0 && string(req.ID) != "null"
		dbg("%s id=%s", req.Method, string(req.ID))
		switch req.Method {
		case "initialize":
			write(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"protocolVersion": ProtocolVersion,
					"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
					"serverInfo":      map[string]string{"name": "claude-go", "version": api.Version},
				},
			})
		case "ping":
			write(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
		case "tools/list":
			var list []map[string]any
			for _, t := range tools.All() {
				if t.IsMCP || t.IsHidden {
					continue
				}
				list = append(list, map[string]any{
					"name":        t.Def.Name,
					"description": t.Def.Description,
					"inputSchema": t.Def.InputSchema,
				})
			}
			write(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": list}})
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			res := tools.Dispatch(tc, p.Name, p.Arguments)
			content := []map[string]any{}
			if len(res.Refs) > 0 {
				for _, r := range res.Refs {
					content = append(content, map[string]any{
						"type": "tool_reference", "tool_name": r})
				}
			} else {
				content = append(content, map[string]any{"type": "text", "text": res.Content})
			}
			write(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": content, "isError": res.IsError,
			}})
		case "resources/list":
			write(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"resources": []any{}}})
		case "resources/templates/list":
			write(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"resourceTemplates": []any{}}})
		case "prompts/list":
			write(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"prompts": []any{}}})
		default:
			if !hasID {
				continue // notification — nothing to answer
			}
			write(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32601,
					"message": "method not found: " + req.Method},
			})
		}
	}
	return 0
}
