package browsermcp

import (
	"bufio"
	"encoding/json"
	"io"
	"sync"
)

// server.go — the MCP-protocol side of `--claude-in-chrome-mcp`, mirroring
// vUn() (mod_0803.js): a @modelcontextprotocol/sdk `Server` on a
// StdioServerTransport.
//
// Framing: newline-delimited JSON-RPC 2.0 — the mirror of
// internal/mcp/client.go's stdioTransport (one JSON object per line in
// both directions). NOT the 4-byte-LE native-messaging framing — that is
// what `--chrome-native-host` speaks to the extension (nativehost.go) and
// what the socket transport speaks to the native host (transport.go).
//
// Registered handlers (real):
//   initialize             → {protocolVersion, capabilities, serverInfo}
//   ping                   → {}
//   logging/setLevel       → {} (capabilities.logging = {})
//   tools/list             → {tools} or {tools:[]} when isDisabled
//   tools/call             → dispatch() — {content, isError?, _meta?}
//   notifications/*        → ignored (initialized, cancelled, ...)
//   unknown request method → -32601 "Method not found"

// protocolVersion is echoed back when the client asks for a revision we
// support; real's SDK negotiates against SUPPORTED_PROTOCOL_VERSIONS.
const protocolVersion = "2025-06-18"

var supportedProtocolVersions = map[string]bool{
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

// rpcRequest is an inbound JSON-RPC message.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// callParams is tools/call's params shape.
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Meta      json.RawMessage `json:"_meta"`
}

// server is one stdio MCP endpoint.
type server struct {
	ctx  *Context
	tr   toolTransport
	out  io.Writer
	wmu  sync.Mutex
	scan *bufio.Scanner
}

// newServer mirrors vUn(e, transport?): builds the Server, registers the
// two request handlers, wires transport→client notification forwarding.
func newServer(c *Context, tr toolTransport, in io.Reader, out io.Writer) *server {
	s := &server{ctx: c, tr: tr, out: out}
	s.scan = bufio.NewScanner(in)
	s.scan.Buffer(make([]byte, 1<<20), 16<<20)
	tr.setNotificationHandler(func(method string, params json.RawMessage) {
		c.Logger.Infof("[%s] Forwarding MCP notification: %s", c.ServerName, scrubForLog(method))
		s.notify(method, params)
	})
	return s
}

// scrubForLog mirrors uTe(e,64): JSON.stringify of a truncated string.
func scrubForLog(v string) string {
	if len(v) > 64 {
		v = v[:64]
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// serve reads lines until EOF (stdin "end" → real exits 0).
func (s *server) serve() error {
	for s.scan.Scan() {
		line := s.scan.Bytes()
		if len(line) == 0 {
			continue
		}
		s.dispatch(line)
	}
	return s.scan.Err()
}

// notify emits a server→client JSON-RPC notification (i.notification()).
func (s *server) notify(method string, params json.RawMessage) {
	m := map[string]any{"jsonrpc": "2.0", "method": method}
	if len(params) > 0 {
		m["params"] = params
	}
	s.writeJSON(m)
}

func (s *server) writeJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.out.Write(append(b, '\n'))
}

func (s *server) reply(id json.RawMessage, result any) {
	s.writeJSON(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
}

func (s *server) replyError(id json.RawMessage, code int, msg string) {
	s.writeJSON(map[string]any{
		"jsonrpc": "2.0", "id": json.RawMessage(id),
		"error": map[string]any{"code": code, "message": msg},
	})
}

// dispatch routes one inbound line.
func (s *server) dispatch(line []byte) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		s.replyError(nil, -32700, "Parse error")
		return
	}
	hasID := len(req.ID) > 0 && string(req.ID) != "null"
	if !hasID {
		// notifications/* and anything else without an id: no response.
		return
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		v := protocolVersion
		if supportedProtocolVersions[p.ProtocolVersion] {
			v = p.ProtocolVersion
		}
		s.reply(req.ID, map[string]any{
			"protocolVersion": v,
			"capabilities": map[string]any{
				"tools":   map[string]any{"listChanged": true},
				"logging": map[string]any{},
			},
			"serverInfo": map[string]string{"name": s.ctx.ServerName, "version": "1.0.0"},
		})
	case "ping":
		s.reply(req.ID, map[string]any{})
	case "logging/setLevel":
		s.reply(req.ID, map[string]any{})
	case "tools/list":
		if s.ctx.IsDisabled != nil && s.ctx.IsDisabled() {
			s.reply(req.ID, map[string]any{"tools": []any{}})
			return
		}
		s.reply(req.ID, map[string]any{"tools": s.ctx.listTools()})
	case "tools/call":
		var p callParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			s.replyError(req.ID, -32602, "Invalid params")
			return
		}
		s.ctx.Logger.Infof("[%s] Executing tool: %s", s.ctx.ServerName, scrubForLog(p.Name))
		res := s.dispatchCall(p)
		s.reply(req.ID, res)
	default:
		s.replyError(req.ID, -32601, "Method not found")
	}
}
