package browsermcp

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// testContext builds a Context without env/credential plumbing.
func testContext() *Context {
	c := &Context{
		ServerName:    ServerDisplayName,
		Logger:        StdLogger{},
		ClientTypeID:  "claude-code",
		ExcludedTools: map[string]bool{},
		HostTools:     func() []Tool { return nil },
	}
	c.OnToolCallDisconnected = c.disconnectedMessage
	return c
}

// rpcConn is a line-delimited JSON-RPC pipe pair for driving server.
type rpcConn struct {
	reqW *io.PipeWriter
	resR *bufio.Reader
	mu   sync.Mutex
}

func newRPCConn() (*rpcConn, *io.PipeReader, *io.PipeWriter) {
	reqR, reqW := io.Pipe()
	resR, resW := io.Pipe()
	return &rpcConn{reqW: reqW, resR: bufio.NewReader(resR)}, reqR, resW
}

func (c *rpcConn) send(t *testing.T, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.reqW.Write(append(b, '\n')); err != nil {
		t.Fatalf("write request: %v", err)
	}
}

func (c *rpcConn) read(t *testing.T) map[string]any {
	t.Helper()
	line, err := c.resR.ReadString('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("response not JSON: %s", line)
	}
	return m
}

// fakeTransport stubs the tool transport (no socket, no bridge).
type fakeTransport struct {
	connected bool
	calls     []map[string]any
	respond   map[string]any
	err       error
	notify    func(string, json.RawMessage)
}

func (f *fakeTransport) ensureConnected() bool { return f.connected }
func (f *fakeTransport) isConnected() bool     { return f.connected }
func (f *fakeTransport) disconnect()           { f.connected = false }
func (f *fakeTransport) setNotificationHandler(h func(string, json.RawMessage)) {
	f.notify = h
}
func (f *fakeTransport) callTool(tool string, args map[string]any, _ *callOpts) (map[string]any, error) {
	f.calls = append(f.calls, map[string]any{"tool": tool, "args": args})
	if f.err != nil {
		return nil, f.err
	}
	return f.respond, nil
}
func (f *fakeTransport) listConnectedExtensions() []extensionInfo { return nil }
func (f *fakeTransport) selectExtensionByID(_, _ string)          {}
func (f *fakeTransport) switchBrowser() (string, any)             { return "", nil }
func (f *fakeTransport) hasActiveSelection() bool                 { return false }
func (f *fakeTransport) getSelectedDeviceID() string              { return "" }

// TestHandshakeToolsList exercises initialize → initialized → tools/list.
func TestHandshakeToolsList(t *testing.T) {
	conn, reqR, resW := newRPCConn()
	ctx := testContext()
	ctx.BridgeConfig = &BridgeConfig{URL: "ws://x"} // bridge transport → all 22 tools
	srv := newServer(ctx, &fakeTransport{}, reqR, resW)
	go srv.serve()

	conn.send(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]string{"name": "claude-go", "version": "2.1.288"},
		},
	})
	init := conn.read(t)
	res := init["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocolVersion = %v", res["protocolVersion"])
	}
	si := res["serverInfo"].(map[string]any)
	if si["name"] != "Claude in Chrome" || si["version"] != "1.0.0" {
		t.Fatalf("serverInfo = %v", si)
	}
	caps := res["capabilities"].(map[string]any)
	if caps["tools"].(map[string]any)["listChanged"] != true {
		t.Fatalf("capabilities = %v", caps)
	}
	if _, ok := caps["logging"]; !ok {
		t.Fatal("missing logging capability")
	}

	// notifications must not produce output
	conn.send(t, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})

	conn.send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	list := conn.read(t)
	tools := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) != len(toolNamesOrdered) {
		t.Fatalf("tools count = %d, want %d", len(tools), len(toolNamesOrdered))
	}
	for i, name := range toolNamesOrdered {
		got := tools[i].(map[string]any)["name"]
		if got != name {
			t.Fatalf("tool[%d] = %v, want %s", i, got, name)
		}
		if _, ok := tools[i].(map[string]any)["inputSchema"]; !ok {
			t.Fatalf("tool %s missing inputSchema", name)
		}
	}
	// _meta preserved on entries that have it
	var rp map[string]any
	for _, tt := range tools {
		m := tt.(map[string]any)
		if m["name"] == "read_page" {
			rp = m
		}
	}
	if rp == nil {
		t.Fatal("read_page missing")
	}
	meta, ok := rp["_meta"].(map[string]any)
	if !ok || meta["anthropic/maxResultSizeChars"] != float64(60000) {
		t.Fatalf("read_page _meta = %v", rp["_meta"])
	}

	// ping
	conn.send(t, map[string]any{"jsonrpc": "2.0", "id": 3, "method": "ping"})
	pong := conn.read(t)
	if _, ok := pong["result"]; !ok {
		t.Fatalf("ping result = %v", pong)
	}
	// unknown method → -32601
	conn.send(t, map[string]any{"jsonrpc": "2.0", "id": 4, "method": "resources/list"})
	e := conn.read(t)
	errObj := e["error"].(map[string]any)
	if errObj["code"] != float64(-32601) {
		t.Fatalf("unknown method error = %v", errObj)
	}
}

// TestToolsListSocketDropsBridgeTools mirrors D(e): without bridgeConfig
// the oko set is absent.
func TestToolsListSocketDropsBridgeTools(t *testing.T) {
	c := testContext() // no BridgeConfig → socket semantics
	got := map[string]bool{}
	for _, tt := range c.listTools() {
		got[tt.Name] = true
	}
	for _, name := range []string{"switch_browser", "list_connected_browsers", "select_browser"} {
		if got[name] {
			t.Fatalf("%s must not be listed on socket transport", name)
		}
	}
	if !got["navigate"] {
		t.Fatal("navigate missing on socket transport")
	}
	c.BridgeConfig = &BridgeConfig{URL: "ws://x"}
	got = map[string]bool{}
	for _, tt := range c.listTools() {
		got[tt.Name] = true
	}
	if !got["select_browser"] {
		t.Fatal("select_browser missing on bridge transport")
	}
}

// TestToolsCallDisconnected: transport never connects → L() content (no
// isError), faithful to real.
func TestToolsCallDisconnected(t *testing.T) {
	conn, reqR, resW := newRPCConn()
	ctx := testContext()
	srv := newServer(ctx, &fakeTransport{connected: false}, reqR, resW)
	go srv.serve()
	conn.send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "navigate", "arguments": map[string]any{"url": "https://x", "tabId": 1}}})
	res := conn.read(t)["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("disconnected result must not be isError (real returns plain content)")
	}
	txt := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.HasPrefix(txt, "Browser extension is not connected") {
		t.Fatalf("unexpected disconnected text: %q", txt[:60])
	}
}

// TestToolsCallResultMapping: transport {result:{content}} → MCP content.
func TestToolsCallResultMapping(t *testing.T) {
	ft := &fakeTransport{
		connected: true,
		respond: map[string]any{"result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "hello"}}}},
	}
	conn, reqR, resW := newRPCConn()
	ctx := testContext()
	ctx.BridgeConfig = &BridgeConfig{URL: "ws://x"}
	srv := newServer(ctx, ft, reqR, resW)
	go srv.serve()
	conn.send(t, map[string]any{"jsonrpc": "2.0", "id": 9, "method": "tools/call",
		"params": map[string]any{"name": "get_page_text", "arguments": map[string]any{"tabId": 1}}})
	res := conn.read(t)["result"].(map[string]any)
	txt := res["content"].([]any)[0].(map[string]any)["text"]
	if txt != "hello" {
		t.Fatalf("content = %v", txt)
	}
	if ft.calls[0]["tool"] != "get_page_text" {
		t.Fatalf("forwarded tool = %v", ft.calls[0])
	}
}

// TestSocketFramingRoundTrip drives socketClient against a fake native
// host on net.Pipe — uint32-LE framed execute_tool → response.
func TestSocketFramingRoundTrip(t *testing.T) {
	client, host := net.Pipe()
	ctx := testContext()
	sc := &socketClient{ctx: ctx, path: "test"}
	sc.mu.Lock()
	sc.conn = client
	sc.connected = true
	sc.mu.Unlock()
	go sc.readLoop(client)

	go func() {
		var buf []byte
		tmp := make([]byte, 4096)
		for {
			n, err := host.Read(tmp)
			if err != nil {
				return
			}
			buf = append(buf, tmp[:n]...)
			for len(buf) >= 4 {
				l := int(binary.LittleEndian.Uint32(buf))
				if len(buf) < 4+l {
					break
				}
				frame := buf[4 : 4+l]
				buf = buf[4+l:]
				var req map[string]any
				json.Unmarshal(frame, &req)
				params := req["params"].(map[string]any)
				resp, _ := json.Marshal(map[string]any{
					"tool_use_id": params["tool_use_id"],
					"result": map[string]any{
						"content": []any{map[string]any{"type": "text", "text": "pong"}},
					},
				})
				out := make([]byte, 4+len(resp))
				binary.LittleEndian.PutUint32(out, uint32(len(resp)))
				copy(out[4:], resp)
				host.Write(out)
			}
		}
	}()

	res, err := sc.callTool("navigate", map[string]any{"url": "https://x"}, nil)
	if err != nil {
		t.Fatalf("callTool: %v", err)
	}
	result := res["result"].(map[string]any)
	if result["content"].([]any)[0].(map[string]any)["text"] != "pong" {
		t.Fatalf("res = %v", res)
	}
}

// TestSocketRequestShape verifies the execute_tool frame's JSON body.
func TestSocketRequestShape(t *testing.T) {
	client, host := net.Pipe()
	defer client.Close()
	defer host.Close()
	ctx := testContext()
	sc := &socketClient{ctx: ctx, path: "test"}
	sc.mu.Lock()
	sc.conn = client
	sc.connected = true
	sc.mu.Unlock()
	go sc.readLoop(client)

	done := make(chan map[string]any, 1)
	go func() {
		var buf []byte
		tmp := make([]byte, 4096)
		for {
			n, err := host.Read(tmp)
			if err != nil {
				return
			}
			buf = append(buf, tmp[:n]...)
			if len(buf) >= 4 {
				l := int(binary.LittleEndian.Uint32(buf))
				if len(buf) >= 4+l {
					var req map[string]any
					json.Unmarshal(buf[4:4+l], &req)
					done <- req
					return
				}
			}
		}
	}()
	go func() {
		sc.callTool("read_page", map[string]any{"tabId": float64(7)}, nil)
	}()
	select {
	case req := <-done:
		if req["method"] != "execute_tool" {
			t.Fatalf("method = %v", req["method"])
		}
		p := req["params"].(map[string]any)
		if p["client_id"] != "claude-code" || p["tool"] != "read_page" || p["tool_use_id"] == "" {
			t.Fatalf("params = %v", p)
		}
		args := p["args"].(map[string]any)
		if args["tabId"] != float64(7) {
			t.Fatalf("args = %v", args)
		}
		if _, has := p["permission_mode"]; has {
			t.Fatal("socket must not send permission_mode without session scope")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame received")
	}
}

// TestNativeHostRoundTrip: ping → pong over the uint32-LE stdio framing;
// MCP client socket → tool_request forwarded to "Chrome".
func TestNativeHostPing(t *testing.T) {
	outR, outW := io.Pipe()
	h := &nativeHost{
		mcpClients: map[int]net.Conn{},
		out:        outW,
		computer:   func() string { return "testhost" },
	}
	go h.handleMessage(`{"type":"ping"}`)
	var hdr [4]byte
	if _, err := io.ReadFull(outR, hdr[:]); err != nil {
		t.Fatal(err)
	}
	l := binary.LittleEndian.Uint32(hdr[:])
	buf := make([]byte, l)
	io.ReadFull(outR, buf)
	var m map[string]any
	json.Unmarshal(buf, &m)
	if m["type"] != "pong" {
		t.Fatalf("pong = %v", m)
	}
}

func TestNativeHostGetStatus(t *testing.T) {
	outR, outW := io.Pipe()
	h := &nativeHost{mcpClients: map[int]net.Conn{}, out: outW,
		computer: func() string { return "My Mac.local" }}
	go h.handleMessage(`{"type":"get_status"}`)
	var hdr [4]byte
	io.ReadFull(outR, hdr[:])
	buf := make([]byte, binary.LittleEndian.Uint32(hdr[:]))
	io.ReadFull(outR, buf)
	var m map[string]any
	json.Unmarshal(buf, &m)
	if m["type"] != "status_response" || m["native_host_version"] != "1.0.0" || m["computer_name"] != "My Mac.local" {
		t.Fatalf("status = %v", m)
	}
}

// TestNativeHostToolRequest: an MCP client socket frame becomes a
// tool_request to Chrome; tool_response comes back on the socket.
func TestNativeHostToolRequest(t *testing.T) {
	outR, outW := io.Pipe()
	h := &nativeHost{mcpClients: map[int]net.Conn{}, out: outW,
		computer: func() string { return "h" }}
	sock, mine := net.Pipe()
	defer mine.Close()
	go h.handleMCPClient(sock)

	// drain the mcp_connected message
	readFrame := func() map[string]any {
		var hdr [4]byte
		io.ReadFull(outR, hdr[:])
		buf := make([]byte, binary.LittleEndian.Uint32(hdr[:]))
		io.ReadFull(outR, buf)
		var m map[string]any
		json.Unmarshal(buf, &m)
		return m
	}
	deadline := time.Now().Add(2 * time.Second)
	var connMsg map[string]any
	for time.Now().Before(deadline) {
		h.mu.Lock()
		ready := len(h.mcpClients) > 0
		h.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	connMsg = readFrame()
	if connMsg["type"] != "mcp_connected" {
		t.Fatalf("expected mcp_connected, got %v", connMsg)
	}

	// write a framed execute_tool from the MCP client side
	body, _ := json.Marshal(map[string]any{
		"method": "execute_tool",
		"params": map[string]any{"client_id": "claude-code", "tool": "navigate",
			"args": map[string]any{"url": "x"}, "tool_use_id": "u1"},
	})
	frame := make([]byte, 4+len(body))
	binary.LittleEndian.PutUint32(frame, uint32(len(body)))
	copy(frame[4:], body)
	mine.Write(frame)

	req := readFrame()
	if req["type"] != "tool_request" || req["method"] != "execute_tool" {
		t.Fatalf("tool_request = %v", req)
	}
	params := req["params"].(map[string]any)
	if params["tool"] != "navigate" || params["tool_use_id"] != "u1" {
		t.Fatalf("params = %v", params)
	}

	// Chrome → host tool_response → forwarded to socket client
	go h.handleMessage(`{"type":"tool_response","tool_use_id":"u1","result":{"content":[{"type":"text","text":"done"}]}}`)
	var rhdr [4]byte
	io.ReadFull(mine, rhdr[:])
	rbuf := make([]byte, binary.LittleEndian.Uint32(rhdr[:]))
	io.ReadFull(mine, rbuf)
	var resp map[string]any
	json.Unmarshal(rbuf, &resp)
	if resp["tool_use_id"] != "u1" {
		t.Fatalf("socket response = %v", resp)
	}
	result := resp["result"].(map[string]any)
	if result["content"].([]any)[0].(map[string]any)["text"] != "done" {
		t.Fatalf("resp = %v", resp)
	}
	if _, has := resp["type"]; has {
		t.Fatal("type field must be stripped before forwarding")
	}
}

// TestGate mirrors W()/R(): env deny, provider, no-token, policy verdicts.
func TestGate(t *testing.T) {
	t.Setenv("CLAUDE_CODE_CHROME_MCP_ORG_DENIED", "1")
	r := Gate(GateDeps{})
	if !r.Denied || r.Cause != "parent_org_policy" {
		t.Fatalf("env deny = %+v", r)
	}
	t.Setenv("CLAUDE_CODE_CHROME_MCP_ORG_DENIED", "")

	r = Gate(GateDeps{Provider: "bedrock"})
	if r.Denied || r.Unverified != "third_party_provider" {
		t.Fatalf("provider = %+v", r)
	}
	r = Gate(GateDeps{BaseURL: "https://custom.example.com"})
	if r.Unverified != "custom_base_url" {
		t.Fatalf("custom base = %+v", r)
	}
	r = Gate(GateDeps{OAuthToken: func() string { return "" }})
	if r.Unverified != "no_oauth_token" {
		t.Fatalf("no token = %+v", r)
	}
	r = Gate(GateDeps{
		OAuthToken: func() string { return "tok" },
		PolicyFetch: func(string, int) (*policyLimits, error) {
			return &policyLimits{Restrictions: map[string]struct {
				Allowed bool `json:"allowed"`
			}{"allow_claude_browser_extension": {Allowed: true}}}, nil
		},
	})
	if r.Denied || r.VerifiedBearer != "tok" {
		t.Fatalf("verified = %+v", r)
	}
	r = Gate(GateDeps{
		OAuthToken: func() string { return "tok" },
		PolicyFetch: func(string, int) (*policyLimits, error) {
			return &policyLimits{Restrictions: map[string]struct {
				Allowed bool `json:"allowed"`
			}{"allow_claude_browser_extension": {Allowed: false}}}, nil
		},
	})
	if !r.Denied || r.Cause != "org_policy" {
		t.Fatalf("denied = %+v", r)
	}
	r = Gate(GateDeps{
		OAuthToken:  func() string { return "tok" },
		PolicyFetch: func(string, int) (*policyLimits, error) { return nil, io.EOF },
	})
	if r.Unverified != "policy_unverified" {
		t.Fatalf("unverified = %+v", r)
	}
	// absent restriction key → allowed (real's no-verdict default)
	r = Gate(GateDeps{
		OAuthToken: func() string { return "tok" },
		PolicyFetch: func(string, int) (*policyLimits, error) {
			return &policyLimits{Restrictions: map[string]struct {
				Allowed bool `json:"allowed"`
			}{}}, nil
		},
	})
	if r.Denied || r.VerifiedBearer != "tok" {
		t.Fatalf("absent key = %+v", r)
	}
}

// TestBearerGateMemoizesDeny: denied tokens are cached and withheld.
func TestBearerGateMemoizesDeny(t *testing.T) {
	calls := 0
	d := GateDeps{
		PolicyFetch: func(string, int) (*policyLimits, error) {
			calls++
			return &policyLimits{Restrictions: map[string]struct {
				Allowed bool `json:"allowed"`
			}{"allow_claude_browser_extension": {Allowed: false}}}, nil
		},
	}
	g := NewBearerGate("good-tok", d, StdLogger{})
	if !g.Allow("good-tok") {
		t.Fatal("verified bearer should pass")
	}
	if g.Allow("bad-tok") {
		t.Fatal("denied token should be withheld")
	}
	if g.Allow("bad-tok") {
		t.Fatal("denied token should stay withheld")
	}
	if calls != 1 {
		t.Fatalf("deny memoization: %d fetches", calls)
	}
}

// TestExcludedAndBatchValidation mirrors Mn().
func TestExcludedAndBatchValidation(t *testing.T) {
	c := testContext()
	c.ExcludedTools = map[string]bool{"gif_creator": true}
	if v := validateCall(c, "gif_creator", nil); v != "Unknown tool: gif_creator" {
		t.Fatalf("excluded = %q", v)
	}
	if v := validateCall(c, "browser_batch", map[string]any{}); v != "browser_batch: actions must be an array" {
		t.Fatalf("batch non-array = %q", v)
	}
	if v := validateCall(c, "browser_batch", map[string]any{"actions": []any{
		map[string]any{"name": "browser_batch"}}}); v == "" {
		t.Fatal("nested batch should be refused")
	}
	if v := validateCall(c, "browser_batch", map[string]any{"actions": []any{
		map[string]any{"name": "gif_creator"}}}); v != "Unknown tool: gif_creator" {
		t.Fatalf("batch excluded = %q", v)
	}
}

// TestStripSaveToDisk mirrors Dn().
func TestStripSaveToDisk(t *testing.T) {
	args, wants := stripSaveToDisk("computer", map[string]any{
		"action": "screenshot", "save_to_disk": true, "tabId": 1.0})
	if !wants {
		t.Fatal("wantsSave not set")
	}
	if _, has := args["save_to_disk"]; has {
		t.Fatal("save_to_disk not stripped")
	}
	args, wants = stripSaveToDisk("browser_batch", map[string]any{
		"actions": []any{
			map[string]any{"name": "computer", "input": map[string]any{"action": "screenshot", "save_to_disk": true}},
			map[string]any{"name": "navigate", "input": map[string]any{"url": "x"}},
		}})
	if !wants {
		t.Fatal("batch wantsSave not detected")
	}
	in := args["actions"].([]any)[0].(map[string]any)["input"].(map[string]any)
	if _, has := in["save_to_disk"]; has {
		t.Fatal("action input save_to_disk not stripped")
	}
}

// TestNavigateFrontLoadBackForward mirrors the qn early-error.
func TestNavigateFrontLoadBackForward(t *testing.T) {
	c := testContext()
	fl := c.frontLoadTabContext(&fakeTransport{connected: true}, "navigate",
		map[string]any{"url": "back"}, nil, "sess")
	if fl.err == nil {
		t.Fatal("expected front-load error for url:back")
	}
	if fl.err.Meta != nil {
		t.Fatal("unexpected meta")
	}
	txt := fl.err.Content[0].(map[string]any)["text"].(string)
	if !strings.Contains(txt, "tabId is required") {
		t.Fatalf("text = %q", txt)
	}
	// non-navigate passes through untouched
	fl = c.frontLoadTabContext(&fakeTransport{connected: true}, "read_page",
		map[string]any{"tabId": 1.0}, nil, "")
	if fl.err != nil {
		t.Fatal("non-navigate must not front-load")
	}
}

// TestTimeoutErrorMapping covers Ln's ToolCallTimeoutError branches.
func TestTimeoutErrorMapping(t *testing.T) {
	c := testContext()
	ack := false
	r := mapCallError(&ToolCallTimeoutError{Msg: "x", RoutingAckReceived: &ack}, "navigate", c)
	if !strings.Contains(r.Content[0].(map[string]any)["text"].(string), "never delivered to the Chrome extension") {
		t.Fatalf("ack=false text: %v", r.Content)
	}
	if r.Meta["isBridgeTimeout"] != true {
		t.Fatal("isBridgeTimeout meta missing")
	}
	r = mapCallError(&ToolCallTimeoutError{Msg: "x"}, "navigate", c)
	if !strings.Contains(r.Content[0].(map[string]any)["text"].(string), "did not respond in time") {
		t.Fatalf("ack=nil text: %v", r.Content)
	}
	r = mapCallError(&SocketConnectionError{Msg: "x"}, "navigate", c)
	if r.IsError {
		t.Fatal("SocketConnectionError maps to disconnected content, not isError")
	}
	r = mapCallError(&ExtensionDisconnectedMidCallError{Msg: "x"}, "navigate", c)
	if !strings.Contains(r.Content[0].(map[string]any)["text"].(string), "disconnected mid-operation") {
		t.Fatalf("ice text: %v", r.Content)
	}
}

// TestNotificationsForwarded: transport notification → stdout MCP
// notification.
func TestNotificationsForwarded(t *testing.T) {
	conn, reqR, resW := newRPCConn()
	ft := &fakeTransport{connected: true}
	ctx := testContext()
	srv := newServer(ctx, ft, reqR, resW)
	go srv.serve()
	if ft.notify == nil {
		t.Fatal("notification handler not registered")
	}
	params, _ := json.Marshal(map[string]any{"x": 1})
	go ft.notify("permission_request", params)
	m := conn.read(t)
	if m["method"] != "permission_request" {
		t.Fatalf("notification = %v", m)
	}
	if _, has := m["id"]; has {
		t.Fatal("notifications must not carry id")
	}
}
