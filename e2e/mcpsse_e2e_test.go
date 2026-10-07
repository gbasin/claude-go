//go:build e2e

// mcpsse_e2e_test.go — end-to-end coverage for the legacy SSE MCP
// transport: a fake server implements the endpoint-event + POST-channel
// handshake (initialize → notifications/initialized → tools/list →
// tools/call) and the binary calls a real mcp__ tool through it inside a
// --mock PTY session. The mock emits the tool_use blocks; the wire hop to
// the SSE server is real HTTP.
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// sseMCPServer is a minimal legacy-SSE MCP server: GET /sse emits the
// endpoint event then streams JSON-RPC responses as message events; POST
// /msg accepts requests/notifications.
type sseMCPServer struct {
	srv *httptest.Server
	out chan []byte // JSON-RPC responses to stream back

	mu      sync.Mutex
	methods []string         // jsonrpc methods received, in order
	extra   []map[string]any // extra tools served by tools/list
	results []string         // client->server responses (roots/list etc)
}

func newSSEMCPServer(t *testing.T) *sseMCPServer {
	t.Helper()
	m := &sseMCPServer{out: make(chan []byte, 32)}
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", m.handleStream)
	mux.HandleFunc("/msg", m.handleMsg)
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *sseMCPServer) handleStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	emit := func(event string, data []byte) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		if fl != nil {
			fl.Flush()
		}
	}
	emit("endpoint", []byte("/msg"))
	for {
		select {
		case <-r.Context().Done():
			return
		case data := <-m.out:
			emit("message", data)
		}
	}
}

func (m *sseMCPServer) handleMsg(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var rpc struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	json.Unmarshal(body, &rpc)
	if rpc.Method != "" {
		m.mu.Lock()
		m.methods = append(m.methods, rpc.Method)
		m.mu.Unlock()
	}
	if len(rpc.ID) == 0 || string(rpc.ID) == "null" {
		w.WriteHeader(202) // notification — no response
		return
	}
	if rpc.Method == "" {
		// client->server response (e.g. roots/list reply) — record it
		m.mu.Lock()
		m.results = append(m.results, string(body))
		m.mu.Unlock()
		w.WriteHeader(202)
		return
	}
	var result any
	switch rpc.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}, "resources": map[string]any{}},
			"serverInfo":      map[string]any{"name": "e2e-sse", "version": "0.1"},
		}
	case "resources/list":
		result = map[string]any{"resources": []map[string]any{{
			"uri": "mem://note1", "name": "note1", "mimeType": "text/plain",
			"description": "e2e resource"}}}
	case "resources/read":
		result = map[string]any{"contents": []map[string]any{{
			"uri": "mem://note1", "mimeType": "text/plain", "text": "RES-CONTENT-E2E"}}}
	case "prompts/list":
		result = map[string]any{"prompts": []map[string]any{{
			"name":        "greet",
			"description": "e2e prompt",
			"arguments":   []map[string]any{{"name": "input", "required": false}},
		}}}
	case "prompts/get":
		result = map[string]any{
			"description": "e2e prompt",
			"messages": []map[string]any{{
				"role":    "user",
				"content": map[string]any{"type": "text", "text": "PROMPTBODY-E2E say hello"},
			}}}
	case "tools/list":
		m.mu.Lock()
		toolList := append([]map[string]any{{
			"name":        "ping",
			"description": "e2e ping tool",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		}}, m.extra...)
		m.mu.Unlock()
		result = map[string]any{"tools": toolList}
	case "tools/call":
		// decode params.name — error replies carry isError:true
		var call struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		json.Unmarshal(body, &call)
		if call.Params.Name == "fail" {
			result = map[string]any{
				"isError": true,
				"content": []map[string]any{{"type": "text", "text": "MCP-FAIL-E2E"}},
			}
		} else {
			result = map[string]any{"content": []map[string]any{{
				"type": "text", "text": "PONG-E2E"}}}
		}
	default:
		result = map[string]any{}
	}
	resp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": json.RawMessage(rpc.ID), "result": result})
	m.out <- resp
	w.WriteHeader(202)
}

// pushNotify streams a JSON-RPC notification (no id) to the client.
func (m *sseMCPServer) pushNotify(method string, params any) {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": method, "params": params})
	m.out <- b
}

// pushRequest streams a server->client JSON-RPC request (has an id — the
// client answers via POST /msg with the same id).
func (m *sseMCPServer) pushRequest(id int, method string, params any) {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	m.out <- b
}

func (m *sseMCPServer) sawResults() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string{}, m.results...)
}

// addToolAt registers a second tool immediately (for error-path tests).
func (m *sseMCPServer) addToolAt(name string) { m.addTool(name, "e2e "+name) }

// addTool appends to the dynamic tools/list result.
func (m *sseMCPServer) addTool(name, desc string) {
	m.mu.Lock()
	m.extra = append(m.extra, map[string]any{
		"name": name, "description": desc,
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	})
	m.mu.Unlock()
}

func (m *sseMCPServer) sawMethods() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string{}, m.methods...)
}

// TestMCPSSEToolCall: a project .mcp.json SSE server connects, lists one
// tool, and the model can call it after ToolSearch discovery (MCP tools are
// always deferred).
func TestMCPSSEToolCall(t *testing.T) {
	srv := newSSEMCPServer(t)
	s := newSession(t)
	s.writeSettings(`{"enableAllProjectMcpServers":true}`)
	s.writeFile(".mcp.json", fmt.Sprintf(
		`{"mcpServers":{"ssetest":{"type":"sse","url":%q}}}`, srv.srv.URL+"/sse"))

	p := startREPL(t, s, "--permission-mode", "bypassPermissions")
	// handshake is async — /mcp shows connected once initialize+tools/list done
	p.sendUntil("/mcp\r", "● ", 20)

	// ToolSearch discovers the deferred schema, then the call dispatches
	// over SSE. Both tool_use blocks run in one turn. Send-once semantics:
	// resending a submitted line would run a second turn whose tool_use ids
	// collide with the first (the mock reuses toolu_mock_N per response) —
	// so retry only while the editor echo hasn't appeared at all.
	line := `calls:[{"name":"ToolSearch","input":{"query":"select:mcp__ssetest__ping"}},` +
		`{"name":"mcp__ssetest__ping","input":{}}]`
	p.sendUntilEchoed(t, line+"\r", `❯ calls:[{"name":"ToolSearch"`, 8)
	p.waitFor("PONG-E2E", 30*time.Second)

	// the handshake + call sequence actually happened on the wire
	want := []string{"initialize", "notifications/initialized", "tools/list", "tools/call"}
	got := srv.sawMethods()
	pos := 0
	for _, w := range want {
		found := false
		for ; pos < len(got); pos++ {
			if got[pos] == w {
				found = true
				pos++
				break
			}
		}
		if !found {
			t.Fatalf("mcp method %q missing/out of order; got %v", w, got)
		}
	}
	p.exit()
}

// TestMCPSSEDeferredRefusal: calling a deferred mcp__ tool directly (never
// ToolSearch-discovered) must refuse with real's InputValidationError text.
func TestMCPSSEDeferredRefusal(t *testing.T) {
	srv := newSSEMCPServer(t)
	s := newSession(t)
	s.writeSettings(`{"enableAllProjectMcpServers":true}`)
	s.writeFile(".mcp.json", fmt.Sprintf(
		`{"mcpServers":{"ssetest":{"type":"sse","url":%q}}}`, srv.srv.URL+"/sse"))

	p := startREPL(t, s, "--permission-mode", "bypassPermissions")
	p.sendUntil("/mcp\r", "● ", 20)

	p.sendUntilEchoed(t, `tool:mcp__ssetest__ping {}`+"\r", "❯ tool:mcp__ssetest__ping", 8)
	p.waitFor("InputValidationError", 30*time.Second)

	// the deferred refusal happens client-side — no tools/call on the wire
	for _, mth := range srv.sawMethods() {
		if mth == "tools/call" {
			t.Fatalf("tools/call fired for an undiscovered deferred tool: %v", srv.sawMethods())
		}
	}
	p.exit()
}

// httpMCPServer is a minimal streamable-HTTP MCP server: POST /mcp carries
// JSON-RPC, responses come back as JSON bodies; Mcp-Session-Id is issued
// on initialize and must be echoed on later requests.
type httpMCPServer struct {
	srv *httptest.Server

	mu      sync.Mutex
	methods []string
	sidSeen []string // Mcp-Session-Id header per request
}

func newHTTPMCPServer(t *testing.T) *httpMCPServer {
	t.Helper()
	m := &httpMCPServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", m.handle)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404) // GET stream unsupported — client must tolerate
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *httpMCPServer) handle(w http.ResponseWriter, r *http.Request) {
	var rpc struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	json.NewDecoder(r.Body).Decode(&rpc)
	m.mu.Lock()
	m.methods = append(m.methods, rpc.Method)
	m.sidSeen = append(m.sidSeen, r.Header.Get("Mcp-Session-Id"))
	m.mu.Unlock()
	if len(rpc.ID) == 0 || string(rpc.ID) == "null" {
		w.WriteHeader(202)
		return
	}
	var result any
	switch rpc.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "e2e-http", "version": "0.1"},
		}
		w.Header().Set("Mcp-Session-Id", "e2e-sess-1")
	case "tools/list":
		result = map[string]any{"tools": []map[string]any{{
			"name":        "ping",
			"description": "e2e ping tool",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		}}}
	case "tools/call":
		result = map[string]any{"content": []map[string]any{{
			"type": "text", "text": "PONG-HTTP-E2E"}}}
	default:
		result = map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": json.RawMessage(rpc.ID), "result": result})
}

// TestMCPHTTPToolCall: streamable-HTTP transport — initialize handshake,
// session-id echo on subsequent posts, deferred ToolSearch gate, then a
// real tools/call over HTTP.
func TestMCPHTTPToolCall(t *testing.T) {
	srv := newHTTPMCPServer(t)
	s := newSession(t)
	s.writeSettings(`{"enableAllProjectMcpServers":true}`)
	s.writeFile(".mcp.json", fmt.Sprintf(
		`{"mcpServers":{"httptest":{"type":"http","url":%q}}}`, srv.srv.URL+"/mcp"))

	p := startREPL(t, s, "--permission-mode", "bypassPermissions")
	p.sendUntil("/mcp\r", "● ", 20)

	p.sendUntilEchoed(t,
		`calls:[{"name":"ToolSearch","input":{"query":"select:mcp__httptest__ping"}},`+
			`{"name":"mcp__httptest__ping","input":{}}]`+"\r",
		`❯ calls:[{"name":"ToolSearch"`, 8)
	p.waitFor("PONG-HTTP-E2E", 30*time.Second)

	// session id issued at initialize must be echoed on later requests
	srv.mu.Lock()
	defer srv.mu.Unlock()
	var sawSid bool
	for _, sid := range srv.sidSeen[1:] {
		if sid == "e2e-sess-1" {
			sawSid = true
		}
	}
	if !sawSid {
		t.Fatalf("Mcp-Session-Id not echoed after initialize: %v", srv.sidSeen)
	}
	var sawCall bool
	for _, m := range srv.methods {
		if m == "tools/call" {
			sawCall = true
		}
	}
	if !sawCall {
		t.Fatalf("no tools/call on the wire: %v", srv.methods)
	}
	p.exit()
}

// TestMCPResources: the deferred resource tools list and read MCP
// resources over the SSE transport — resources/list + resources/read on
// the wire, content into the tool result.
func TestMCPResources(t *testing.T) {
	srv := newSSEMCPServer(t)
	s := newSession(t)
	s.writeSettings(`{"enableAllProjectMcpServers":true}`)
	s.writeFile(".mcp.json", fmt.Sprintf(
		`{"mcpServers":{"ssetest":{"type":"sse","url":%q}}}`, srv.srv.URL+"/sse"))

	p := startREPL(t, s, "--permission-mode", "bypassPermissions")
	p.sendUntil("/mcp\r", "● ", 20)

	p.sendUntilEchoed(t,
		`calls:[{"name":"ToolSearch","input":{"query":"select:ListMcpResourcesTool,ReadMcpResourceTool"}},`+
			`{"name":"ListMcpResourcesTool","input":{}},`+
			`{"name":"ReadMcpResourceTool","input":{"server":"ssetest","uri":"mem://note1"}}]`+"\r",
		`❯ calls:[{"name":"ToolSearch"`, 8)
	p.waitFor("RES-CONTENT-E2E", 30*time.Second)
	p.waitFor("mem://note1", 5*time.Second)
	p.exit()
}

// TestMCPOAuthNeedsAuth: a 401 with WWW-Authenticate resource_metadata
// triggers OAuth discovery — the server's authorization endpoint surfaces
// in the connect error.
func TestMCPOAuthNeedsAuth(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth-meta":
			json.NewEncoder(w).Encode(map[string]any{
				"authorization_servers": []string{srv.URL}})
		case "/.well-known/oauth-authorization-server":
			json.NewEncoder(w).Encode(map[string]any{
				"authorization_endpoint": srv.URL + "/authorize-e2e",
				"token_endpoint":         srv.URL + "/token"})
		default:
			w.Header().Set("WWW-Authenticate",
				`Bearer resource_metadata="`+srv.URL+`/oauth-meta"`)
			w.WriteHeader(401)
		}
	}))
	t.Cleanup(srv.Close)

	s := newSession(t)
	cfg := s.writeFile("oauth-mcp.json", fmt.Sprintf(
		`{"mcpServers":{"osrv":{"type":"http","url":%q}}}`, srv.URL))
	r := s.run(t, "--mock", "-p", "hi", "--mcp-config", cfg)
	s.mustOK(t, r)
	if !strings.Contains(r.stderr, "authorization required") {
		t.Fatalf("needs-auth error not surfaced: %s", r.stderr)
	}
	if !strings.Contains(r.stderr, "/authorize-e2e") {
		t.Fatalf("discovered auth URL missing: %s", r.stderr)
	}
}

// TestMCPListChanged: notifications/tools/list_changed over SSE triggers
// a tools/list refresh and the new tool becomes dispatchable.
func TestMCPListChanged(t *testing.T) {
	srv := newSSEMCPServer(t)
	s := newSession(t)
	s.writeSettings(`{"enableAllProjectMcpServers":true}`)
	s.writeFile(".mcp.json", fmt.Sprintf(
		`{"mcpServers":{"ssetest":{"type":"sse","url":%q}}}`, srv.srv.URL+"/sse"))

	p := startREPL(t, s, "--permission-mode", "bypassPermissions")
	p.sendUntil("/mcp\r", "● ", 20)
	p.send("\x1b") // close status
	time.Sleep(300 * time.Millisecond)

	// add a second tool server-side, then notify
	srv.addTool("ping2", "e2e ping tool 2")
	srv.pushNotify("notifications/tools/list_changed", map[string]any{})

	// give the refresh a beat, then dispatch the new tool — if the
	// notification path is dead this stays "Unknown tool"
	time.Sleep(1500 * time.Millisecond)
	line := `calls:[{"name":"ToolSearch","input":{"query":"select:mcp__ssetest__ping2"}},` +
		`{"name":"mcp__ssetest__ping2","input":{}}]`
	p.sendUntilEchoed(t, line+"\r", `❯ calls:[{"name":"ToolSearch"`, 8)
	p.waitFor("PONG-E2E", 30*time.Second)
	p.exit()

	// the notification must have triggered a second tools/list
	count := 0
	for _, m := range srv.sawMethods() {
		if m == "tools/list" {
			count++
		}
	}
	if count < 2 {
		t.Fatalf("expected tools/list refresh after list_changed; methods: %v", srv.sawMethods())
	}
}

// TestMCPPromptSlash: an MCP prompt surfaces as /mcp__server__name —
// invoking it fetches prompts/get and runs the messages as a turn.
func TestMCPPromptSlash(t *testing.T) {
	srv := newSSEMCPServer(t)
	s := newSession(t)
	s.writeSettings(`{"enableAllProjectMcpServers":true}`)
	s.writeFile(".mcp.json", fmt.Sprintf(
		`{"mcpServers":{"ssetest":{"type":"sse","url":%q}}}`, srv.srv.URL+"/sse"))

	p := startREPL(t, s, "--permission-mode", "bypassPermissions")
	p.sendUntil("/mcp\r", "● ", 20)
	p.send("\x1b")
	time.Sleep(300 * time.Millisecond)

	p.sendUntilEchoed(t, "/mcp__ssetest__greet\r", "❯ /mcp__ssetest__greet", 8)
	p.waitFor("Mock response to: PROMPTBODY-E2E", 30*time.Second)
	p.exit()
}

// TestMCPRootsList: a server→client roots/list request over SSE gets a
// real answer — the workspace root, not -32601.
func TestMCPRootsList(t *testing.T) {
	srv := newSSEMCPServer(t)
	s := newSession(t)
	s.writeSettings(`{"enableAllProjectMcpServers":true}`)
	s.writeFile(".mcp.json", fmt.Sprintf(
		`{"mcpServers":{"ssetest":{"type":"sse","url":%q}}}`, srv.srv.URL+"/sse"))

	p := s.runPTY(t, nil, "--mock", "--permission-mode", "bypassPermissions",
		"--add-dir", "/tmp")
	p.waitFor("❯", 30*time.Second)
	p.sendUntil("/mcp\r", "● ", 20)
	p.send("\x1b")
	time.Sleep(300 * time.Millisecond)

	srv.pushRequest(77, "roots/list", map[string]any{})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		res := srv.sawResults()
		if len(res) > 0 {
			if !strings.Contains(res[0], s.cwd) {
				t.Fatalf("roots/list reply missing cwd %q: %s", s.cwd, res[0])
			}
			if !strings.Contains(res[0], "/tmp") {
				t.Fatalf("roots/list reply missing --add-dir /tmp: %s", res[0])
			}
			p.exit()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	p.exit()
	t.Fatalf("no roots/list response; methods=%v", srv.sawMethods())
}

// TestMCPNotificationMessage: a notifications/message from the server
// prints a status note on the session output.
func TestMCPNotificationMessage(t *testing.T) {
	srv := newSSEMCPServer(t)
	s := newSession(t)
	s.writeSettings(`{"enableAllProjectMcpServers":true}`)
	s.writeFile(".mcp.json", fmt.Sprintf(
		`{"mcpServers":{"ssetest":{"type":"sse","url":%q}}}`, srv.srv.URL+"/sse"))

	p := startREPL(t, s, "--permission-mode", "bypassPermissions")
	p.sendUntil("/mcp\r", "● ", 20)
	p.send("\x1b")
	time.Sleep(300 * time.Millisecond)

	srv.pushNotify("notifications/message",
		map[string]any{"level": "info", "data": "SRV-LOG-LINE-e2e"})
	p.waitFor("SRV-LOG-LINE-e2e", 15*time.Second)
	p.exit()
}

// TestMCPToolError: a tools/call isError result lands as an errored
// tool_result — the mock sees it on the wire.
func TestMCPToolError(t *testing.T) {
	srv := newSSEMCPServer(t)
	srv.addTool("fail", "always errors")
	s := newSession(t)
	s.writeSettings(`{"enableAllProjectMcpServers":true}`)
	s.writeFile(".mcp.json", fmt.Sprintf(
		`{"mcpServers":{"ssetest":{"type":"sse","url":%q}}}`, srv.srv.URL+"/sse"))

	p := startREPL(t, s, "--permission-mode", "bypassPermissions")
	p.sendUntil("/mcp\r", "● ", 20)
	p.send("\x1b")
	time.Sleep(300 * time.Millisecond)

	line := `calls:[{"name":"ToolSearch","input":{"query":"select:mcp__ssetest__fail"}},` +
		`{"name":"mcp__ssetest__fail","input":{}}]`
	p.sendUntilEchoed(t, line+"\r", `❯ calls:[{"name":"ToolSearch"`, 8)
	p.waitFor("MCP-FAIL-E2E", 30*time.Second)
	p.exit()
}
