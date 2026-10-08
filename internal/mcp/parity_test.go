package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- local scope: projects[cwd].mcpServers ---

func TestLoadConfigsLocalScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	cc := map[string]any{
		"mcpServers": map[string]any{
			"user-scope": map[string]any{"type": "stdio", "command": "u"},
			"shared":     map[string]any{"type": "stdio", "command": "user-wins?"},
		},
		"projects": map[string]any{
			cwd: map[string]any{
				"mcpServers": map[string]any{
					"local-scope": map[string]any{"type": "stdio", "command": "l"},
					"shared":      map[string]any{"type": "stdio", "command": "local-wins"},
				},
			},
		},
	}
	b, _ := json.Marshal(cc)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	cfgs := LoadConfigs(cwd)
	if cfgs["user-scope"].Command != "u" {
		t.Fatalf("user-scope missing: %v", cfgs)
	}
	if cfgs["local-scope"].Command != "l" {
		t.Fatalf("local-scope missing: %v", cfgs)
	}
	// local wins on name conflicts (it's the tighter scope)
	if cfgs["shared"].Command != "local-wins" {
		t.Fatalf("shared = %q", cfgs["shared"].Command)
	}
	// local scope is per-project: another cwd must not see it
	other := t.TempDir()
	cfgs2 := LoadConfigs(other)
	if _, ok := cfgs2["local-scope"]; ok {
		t.Fatal("local-scope leaked into another project")
	}
}

// --- initialize capabilities ---

func TestInitializeCapabilities(t *testing.T) {
	var caps map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		json.Unmarshal(body, &req)
		if req.Method == "initialize" {
			var p struct {
				Capabilities map[string]any `json:"capabilities"`
			}
			json.Unmarshal(req.Params, &p)
			caps = p.Capabilities
		}
		if req.ID == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(rpcHandler(body))
	}))
	defer srv.Close()

	c, err := Connect("cap", ServerConfig{Type: "http", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if caps == nil {
		t.Fatal("server saw no initialize")
	}
	roots, _ := caps["roots"].(map[string]any)
	if lc, _ := roots["listChanged"].(bool); !lc {
		t.Fatalf("roots.listChanged not advertised: %v", caps)
	}
	if _, ok := caps["elicitation"]; !ok {
		t.Fatalf("elicitation not advertised: %v", caps)
	}
	tasks, _ := caps["tasks"].(map[string]any)
	reqs, _ := tasks["requests"].(map[string]any)
	el, _ := reqs["elicitation"].(map[string]any)
	if _, ok := el["create"]; !ok {
		t.Fatalf("tasks.requests.elicitation.create not advertised: %v", caps)
	}
}

// --- url-mode elicitation ---

func TestURLElicitation(t *testing.T) {
	// stdio-like in-memory pipe pair: server side answers elicitation
	// requests; we just exercise the client's url-mode path directly.
	c := &Client{Name: "e", pending: map[string]chan rpcResp{}}
	var sent []byte
	c.tr = &captureTransport{w: &sent}
	got := make(chan json.RawMessage, 1)
	c.ElicitURL = func(url, id string) map[string]any {
		got <- json.RawMessage(fmt.Sprintf(`{"url":%q,"id":%q}`, url, id))
		return map[string]any{"action": "accept"}
	}
	c.handleServerRequest(json.RawMessage(`7`), "elicitation/create",
		json.RawMessage(`{"mode":"url","url":"https://x.test/auth","elicitationId":"e1","message":"open it"}`))
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("ElicitURL not invoked")
	}
	var resp struct {
		Result struct {
			Action string `json:"action"`
		} `json:"result"`
	}
	if err := json.Unmarshal(sent, &resp); err != nil {
		t.Fatalf("response: %s", sent)
	}
	if resp.Result.Action != "accept" {
		t.Fatalf("action = %q", resp.Result.Action)
	}
}

type captureTransport struct {
	w *[]byte
}

func (t *captureTransport) send(_ context.Context, msg []byte) error {
	*t.w = append((*t.w)[:0], msg...)
	return nil
}
func (t *captureTransport) close() error { return nil }

// --- tool timeout ---

func TestMCPToolTimeout(t *testing.T) {
	if got := mcpToolTimeout(0); got != 300*time.Second {
		t.Fatalf("default = %v", got)
	}
	if got := mcpToolTimeout(10); got != time.Second {
		t.Fatalf("clamp min: %v", got)
	}
	if got := mcpToolTimeout(90 * 60 * 1000); got != 30*time.Minute {
		t.Fatalf("clamp max: %v", got)
	}
	t.Setenv("MCP_TOOL_TIMEOUT", "42000")
	if got := mcpToolTimeout(0); got != 42*time.Second {
		t.Fatalf("env = %v", got)
	}
	if got := mcpToolTimeout(5000); got != 5*time.Second {
		t.Fatalf("per-server overrides env: %v", got)
	}
}

// --- ws transport ---

// wsTestServer is a minimal RFC6455 server: accepts the upgrade, then
// answers each text frame with a JSON-RPC result echoing rpcHandler.
func wsTestServer(t *testing.T) (addr string, path string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveWS(conn)
		}
	}()
	return ln.Addr().String(), "/mcp"
}

func serveWS(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	key := req.Header.Get("Sec-WebSocket-Key")
	h := sha1.New()
	h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))
	fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
	for {
		payload, err := readWSClientFrame(br)
		if err != nil {
			return
		}
		var probe struct {
			ID int64 `json:"id"`
		}
		json.Unmarshal(payload, &probe)
		if probe.ID == 0 {
			continue // notification
		}
		out := rpcHandler(payload)
		if writeWSServerFrame(conn, out) != nil {
			return
		}
	}
}

func readWSClientFrame(br *bufio.Reader) ([]byte, error) {
	var msg []byte
	for {
		h := make([]byte, 2)
		if _, err := io.ReadFull(br, h); err != nil {
			return nil, err
		}
		fin := h[0]&0x80 != 0
		op := h[0] & 0x0f
		l := int64(h[1] & 0x7f)
		switch l {
		case 126:
			var b [2]byte
			io.ReadFull(br, b[:])
			l = int64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			io.ReadFull(br, b[:])
			l = int64(binary.BigEndian.Uint64(b[:]))
		}
		var mask [4]byte
		io.ReadFull(br, mask[:])
		payload := make([]byte, l)
		if _, err := io.ReadFull(br, payload); err != nil {
			return nil, err
		}
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
		switch op {
		case 0x8:
			return nil, io.EOF
		case 0x9, 0xA:
			continue
		default:
			msg = append(msg, payload...)
			if fin {
				return msg, nil
			}
		}
	}
}

func writeWSServerFrame(conn net.Conn, payload []byte) error {
	var hdr []byte
	hdr = append(hdr, 0x81)
	l := len(payload)
	switch {
	case l < 126:
		hdr = append(hdr, byte(l))
	case l < 65536:
		hdr = append(hdr, 126)
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(l))
	default:
		hdr = append(hdr, 127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(l))
	}
	if _, err := conn.Write(hdr); err != nil {
		return err
	}
	_, err := conn.Write(payload)
	return err
}

func TestWSTransportConnect(t *testing.T) {
	addr, path := wsTestServer(t)
	c, err := Connect("ws-srv", ServerConfig{
		Type: "ws", URL: "ws://" + addr + path,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if len(c.Tools) != 1 || c.Tools[0].Name != "t1" {
		t.Fatalf("tools = %+v", c.Tools)
	}
	out, isErr, err := c.CallTool(context.Background(), "t1", nil)
	if err != nil || isErr {
		t.Fatalf("CallTool: %v %v", out, err)
	}
	if out != "tool-output" {
		t.Fatalf("out = %q", out)
	}
}

// --- GET stream reconnect ---

func TestGetStreamReconnect(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			atomic.AddInt32(&calls, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return // handler returns → body closes → client must reconnect
		}
		body, _ := io.ReadAll(r.Body)
		var probe struct{ ID int64 }
		json.Unmarshal(body, &probe)
		if probe.ID == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(rpcHandler(body))
	}))
	defer srv.Close()

	c, err := Connect("get-re", ServerConfig{Type: "http", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&calls) < 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if atomic.LoadInt32(&calls) < 2 {
		t.Fatalf("GET stream reconnects = %d, want >=2", calls)
	}
}

// --- OAuth credential store parity ---

func TestCredKeyMatchesReal(t *testing.T) {
	// real: Hl(name,cfg) = name + "|" + sha256(JSON{type,url,headers||{}})[:16]
	var cfg ServerConfig
	if err := json.Unmarshal([]byte(
		`{"type":"http","url":"https://mcp.example.com/x","headers":{"X-Key":"v","Authorization":"Bearer z"}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	// compute expected with exact JSON.stringify byte order
	sum := sha256sum(`{"type":"http","url":"https://mcp.example.com/x","headers":{"X-Key":"v","Authorization":"Bearer z"}}`)
	want := "srv|" + sum[:16]
	if got := cfg.credKey("srv"); got != want {
		t.Fatalf("credKey = %q, want %q", got, want)
	}
	// header order in the source JSON is preserved through the hash
	cfg2 := ServerConfig{Type: "http", URL: "https://mcp.example.com/x",
		Headers: map[string]string{"Authorization": "Bearer z", "X-Key": "v"}}
	// code-built (no raw): headers marshal sorted — differs from raw-ordered
	if cfg2.credKey("srv") == want {
		t.Fatal("expected different hash for differently-ordered headers")
	}
	// stdio config with no url drops the url key (JSON.stringify undefined)
	var std ServerConfig
	json.Unmarshal([]byte(`{"type":"stdio","command":"x"}`), &std)
	wantStd := "s|" + sha256sum(`{"type":"stdio","headers":{}}`)[:16]
	if got := std.credKey("s"); got != wantStd {
		t.Fatalf("stdio credKey = %q, want %q", got, wantStd)
	}
}

func sha256sum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestServerAuthStoreParity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := ServerConfig{Type: "http", URL: "https://mcp.example.com/x"}
	// raw comes from unmarshal in real flow; emulate
	json.Unmarshal([]byte(`{"type":"http","url":"https://mcp.example.com/x"}`), &cfg)

	rec := &serverAuth{AccessToken: "at", RefreshToken: "rt",
		ClientID: "cid", ExpiresAt: 9999999999999}
	if err := SaveServerAuth("srv", cfg, rec); err != nil {
		t.Fatal(err)
	}
	// lands in .credentials.json (shared store), not a separate file
	b, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		t.Fatalf("credentials.json missing: %v", err)
	}
	var top struct {
		McpOAuth map[string]map[string]any `json:"mcpOAuth"`
	}
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	var found map[string]any
	for k, v := range top.McpOAuth {
		if strings.HasPrefix(k, "srv|") {
			found = v
		}
	}
	if found == nil {
		t.Fatalf("no srv|… key in mcpOAuth: %v", top.McpOAuth)
	}
	for _, f := range []string{"accessToken", "refreshToken", "clientId", "serverName", "serverUrl"} {
		if found[f] == nil {
			t.Fatalf("record missing camelCase %s: %v", f, found)
		}
	}
	if found["serverUrl"] != "https://mcp.example.com/x" {
		t.Fatalf("serverUrl = %v", found["serverUrl"])
	}
	// round-trip load
	got := loadServerAuth("srv", cfg)
	if got == nil || got.AccessToken != "at" {
		t.Fatalf("loadServerAuth = %+v", got)
	}
	// legacy file fallback: bare-name records still resolve
	legacy := map[string]any{"mcpOAuth": map[string]any{
		"old": map[string]any{"accessToken": "legacy-tok"},
	}}
	lb, _ := json.Marshal(legacy)
	os.WriteFile(filepath.Join(home, ".claude", ".credentials-mcp.json"), lb, 0o600)
	if s := loadServerAuth("old", ServerConfig{}); s == nil || s.AccessToken != "legacy-tok" {
		t.Fatal("legacy fallback failed")
	}
	if err := DeleteServerAuth("srv", cfg); err != nil {
		t.Fatal(err)
	}
	if s := loadServerAuth("srv", cfg); s != nil {
		t.Fatal("delete failed")
	}
}

// --- mcp serve: stdio JSON-RPC server exposing builtin tools ---

func TestServeHandshakeAndTools(t *testing.T) {
	var in strings.Reader
	var out bytes.Buffer
	in.Reset(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"NoSuchTool","arguments":{}}}
{"jsonrpc":"2.0","id":4,"method":"bogus/method"}
`)
	rc := Serve(context.Background(), &in, &out, nil, ServeOpts{})
	if rc != 0 {
		t.Fatalf("serve exit = %d", rc)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("responses = %d: %s", len(lines), out.String())
	}
	var init struct {
		Result struct {
			ServerInfo struct{ Name string } `json:"serverInfo"`
		} `json:"result"`
	}
	json.Unmarshal([]byte(lines[0]), &init)
	if init.Result.ServerInfo.Name != "claude-go" {
		t.Fatalf("init = %s", lines[0])
	}
	var toolsList struct {
		Result struct {
			Tools []struct{ Name string } `json:"tools"`
		} `json:"result"`
	}
	json.Unmarshal([]byte(lines[1]), &toolsList)
	if len(toolsList.Result.Tools) == 0 {
		t.Fatal("empty tools/list")
	}
	var call struct {
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	json.Unmarshal([]byte(lines[2]), &call)
	if !call.Result.IsError {
		t.Fatal("unknown tool should be isError")
	}
	var bogus struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	json.Unmarshal([]byte(lines[3]), &bogus)
	if bogus.Error.Code != -32601 {
		t.Fatalf("error = %s", lines[3])
	}
}
