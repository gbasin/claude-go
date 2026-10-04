package browsermcp

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// transport.go — the tool-call transports, mirroring mod_0802.js class ft
// (single socket client `MUn`) and mod_0803.js class I (socket pool `T`):
//
//   ft: dial unix socket / named pipe → uint32-LE length-prefixed JSON
//       frames. Requests: {method:"execute_tool",
//       params:{client_id, tool, args, tool_use_id}}. Responses arrive as
//       {result|error, tool_use_id?}; {method,params} frames are
//       extension→client notifications (e.g. permission_request).
//   I:  scans getSocketPaths() for every live native-host socket, keeps
//       one ft per path, routes calls by remembered tabId routes, and
//       fans tabs_context_mcp out to all sockets.
//
// Deliberately not implemented: localPairing (the JWS-ES256 pairing +
// sealed-channel mode real uses for desktop-local pairing — context
//.localPairing is never set on this path), and the inference seam
// (answerInferenceRequest).

const (
	defaultToolTimeoutMs = 60000 // mHt (mod_0801.js)
	connectWaitMs        = 5000  // H (mod_0802.js)
	pairingConnectWaitMs = 20000 // ct — unused without localPairing
	reconnectBaseMs      = 1000  // reconnectDelay
	maxReconnectMs       = 30000
	giveUpAfter          = 100     // reconnectAttempts > 100 → stop
	maxFrameBytes        = 1048576 // w (mod_0788.js) — 1 MiB cap
)

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ---------------------------------------------------------------------------
// socketClient — one uint32-LE framed connection to a native host socket.
// ---------------------------------------------------------------------------

type pendingCall struct {
	toolUseID string
	resolve   chan map[string]any
	err       chan error
	timer     *time.Timer
	timeoutMs int
	tool      string
}

type socketClient struct {
	ctx  *Context
	path string

	mu                   sync.Mutex
	conn                 net.Conn
	connected            bool
	connecting           bool
	pending              *pendingCall
	reconnectN           int
	reconnTimer          *time.Timer
	connTimer            *time.Timer
	disableAutoReconnect bool
	onRedialWanted       func(reason string) // "dead" | "peer_dropped" | "transport"
	notify               func(string, json.RawMessage)
}

func newSocketClient(c *Context, path string) *socketClient {
	return &socketClient{ctx: c, path: path}
}

func (sc *socketClient) logf(f string, a ...any) {
	sc.ctx.Logger.Infof("["+sc.ctx.ServerName+"] "+f, a...)
}

// dialSocket connects to a unix socket path (or the Windows named pipe —
// unsupported on unix; honest error).
func dialSocket(path string) (net.Conn, error) {
	if runtime.GOOS == "windows" {
		return net.Dial("unix", path) // named pipes need npipe; report failure honestly
	}
	return net.Dial("unix", path)
}

// validateSocketSecurity mirrors ft.validateSocketSecurity: refuse symlink
// dirs / non-0700 dirs / sockets not owned by us.
func (sc *socketClient) validateSocketSecurity(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir := filepath.Dir(path)
	base := filepath.Base(dir)
	if strings.HasPrefix(base, "claude-mcp-browser-bridge-") {
		fi, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("[%s] Socket directory is a symlink: %s. Directory may have been tampered with.", sc.ctx.ServerName, dir)
		}
		if fi.IsDir() {
			if perm := fi.Mode().Perm(); perm != 0o700 {
				return fmt.Errorf("[%s] Insecure socket directory permissions: %o (expected 0700). Directory may have been tampered with.", sc.ctx.ServerName, perm)
			}
		}
	}
	return nil
}

func (sc *socketClient) connect() {
	sc.mu.Lock()
	if sc.connecting {
		sc.mu.Unlock()
		sc.logf("Already connecting, skipping duplicate attempt")
		return
	}
	sc.closeSocketLocked()
	sc.connecting = true
	sc.mu.Unlock()

	if err := sc.validateSocketSecurity(sc.path); err != nil {
		sc.mu.Lock()
		sc.connecting = false
		sc.mu.Unlock()
		sc.logf("Security validation failed: %v", err)
		return
	}
	sc.logf("Attempting to connect to: %s", sc.path)
	conn, err := dialSocket(sc.path)
	if err != nil {
		sc.mu.Lock()
		sc.connecting = false
		sc.mu.Unlock()
		sc.logf("Socket connect failed: %v", err)
		// dt(u.code): ENOENT/ECONNREFUSED-ish → "dead" cadence (native
		// host not running); anything else is "transport" — on unix both
		// read as poll-for-the-host.
		sc.scheduleReconnect("dead")
		return
	}
	sc.mu.Lock()
	sc.conn = conn
	sc.mu.Unlock()
	sc.connTimer = time.AfterFunc(connectWaitMs*time.Millisecond, func() {
		sc.mu.Lock()
		defer sc.mu.Unlock()
		if !sc.connected {
			sc.logf("Connection attempt timed out after %dms", connectWaitMs)
			sc.closeSocketLocked()
		}
		go sc.scheduleReconnect("transport")
	})
	sc.mu.Lock()
	sc.connected = true
	sc.connecting = false
	sc.reconnectN = 0
	sc.mu.Unlock()
	if sc.connTimer != nil {
		sc.connTimer.Stop()
	}
	sc.logf("Successfully connected to bridge server")
	go sc.readLoop(conn)
}

// readLoop mirrors the socket "data" handler: accumulate, then peel off
// uint32-LE frames.
func (sc *socketClient) readLoop(conn net.Conn) {
	var buf []byte
	tmp := make([]byte, 64<<10)
	for {
		n, err := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for len(buf) >= 4 {
				l := int(binary.LittleEndian.Uint32(buf))
				if len(buf) < 4+l {
					break
				}
				frame := buf[4 : 4+l]
				buf = buf[4+l:]
				sc.handleFrame(frame)
			}
		}
		if err != nil {
			sc.mu.Lock()
			sc.closeSocketLocked()
			sc.mu.Unlock()
			// real: close event → scheduleReconnect() ("transport" cadence)
			sc.scheduleReconnect("transport")
			return
		}
	}
}

// handleFrame mirrors the dispatch in ft's data handler:
// {method:string} → notification; {result|error} → tool response.
func (sc *socketClient) handleFrame(frame []byte) {
	var m map[string]any
	if err := json.Unmarshal(frame, &m); err != nil {
		sc.logf("Failed to parse message: %v", err)
		return
	}
	if method, ok := m["method"].(string); ok {
		sc.logf("Received notification: %s", scrubForLog(method))
		sc.mu.Lock()
		h := sc.notify
		sc.mu.Unlock()
		if h != nil {
			var params json.RawMessage
			if p, ok := m["params"]; ok {
				params, _ = json.Marshal(p)
			}
			h(method, params)
		}
		return
	}
	if _, hasResult := m["result"]; hasResult {
		sc.deliverResponse(m)
		return
	}
	if _, hasErr := m["error"]; hasErr {
		sc.deliverResponse(m)
		return
	}
	sc.logf("Received a message of unknown shape")
}

// deliverResponse mirrors handleResponse: matches the single pending call
// (by tool_use_id when present; untagged frames are accepted).
func (sc *socketClient) deliverResponse(m map[string]any) {
	sc.mu.Lock()
	p := sc.pending
	if p == nil {
		sc.mu.Unlock()
		return
	}
	if tid, ok := m["tool_use_id"].(string); ok && tid != p.toolUseID {
		sc.mu.Unlock()
		return
	}
	sc.pending = nil
	if p.timer != nil {
		p.timer.Stop()
	}
	sc.mu.Unlock()
	sc.logf("Received tool response for %s", scrubForLog(p.toolUseID))
	p.resolve <- m
}

func (sc *socketClient) failPending(err error) {
	sc.mu.Lock()
	p := sc.pending
	sc.pending = nil
	sc.mu.Unlock()
	if p != nil {
		if p.timer != nil {
			p.timer.Stop()
		}
		p.err <- err
	}
}

func (sc *socketClient) closeSocketLocked() {
	if sc.connTimer != nil {
		sc.connTimer.Stop()
		sc.connTimer = nil
	}
	if sc.conn != nil {
		sc.conn.Close()
		sc.conn = nil
	}
	sc.connected = false
	sc.connecting = false
	sc.failPending(&ExtensionDisconnectedMidCallError{
		Msg: fmt.Sprintf("[%s] Native host connection closed before the tool responded", sc.ctx.ServerName)})
}

func (sc *socketClient) scheduleReconnect(reason string) {
	if sc.disableAutoReconnect {
		if sc.onRedialWanted != nil {
			sc.onRedialWanted(reason)
		}
		return
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.reconnTimer != nil {
		return
	}
	sc.reconnectN++
	if sc.reconnectN > giveUpAfter {
		sc.logf("Giving up after %d attempts. Will retry on next tool call.", giveUpAfter)
		sc.reconnectN = 0
		return
	}
	delay := float64(reconnectBaseMs) * pow15(sc.reconnectN-1)
	if delay > maxReconnectMs {
		delay = maxReconnectMs
	}
	if sc.reconnectN <= 10 {
		sc.logf("Reconnecting in %dms (attempt %d)", int(delay), sc.reconnectN)
	} else if sc.reconnectN%10 == 0 {
		sc.logf("Still polling for native host (attempt %d)", sc.reconnectN)
	}
	sc.reconnTimer = time.AfterFunc(time.Duration(delay)*time.Millisecond, func() {
		sc.mu.Lock()
		sc.reconnTimer = nil
		sc.mu.Unlock()
		sc.connect()
	})
}

func pow15(n int) float64 {
	v := 1.0
	for i := 0; i < n; i++ {
		v *= 1.5
	}
	return v
}

func (sc *socketClient) isConnected() bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.connected
}

// ensureConnected mirrors ft.ensureConnected: wait up to connectWaitMs,
// resolving false early when no dial is in flight and none is scheduled.
func (sc *socketClient) ensureConnected() bool {
	sc.mu.Lock()
	if sc.connected && sc.conn != nil {
		sc.mu.Unlock()
		return true
	}
	if sc.conn == nil && !sc.connecting {
		sc.mu.Unlock()
		sc.connect()
	} else {
		sc.mu.Unlock()
	}
	deadline := time.Now().Add(connectWaitMs * time.Millisecond)
	for {
		sc.mu.Lock()
		switch {
		case sc.connected:
			sc.mu.Unlock()
			return true
		case sc.conn == nil && !sc.connecting && sc.reconnTimer == nil:
			sc.mu.Unlock()
			return false
		}
		sc.mu.Unlock()
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// writeRaw mirrors writeRaw: uint32-LE length prefix + JSON bytes.
func (sc *socketClient) writeRaw(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	sc.mu.Lock()
	conn := sc.conn
	sc.mu.Unlock()
	if conn == nil {
		return &SocketConnectionError{Msg: fmt.Sprintf("[%s] Cannot send request: not connected", sc.ctx.ServerName)}
	}
	frame := make([]byte, 4+len(b))
	binary.LittleEndian.PutUint32(frame, uint32(len(b)))
	copy(frame[4:], b)
	_, err = conn.Write(frame)
	return err
}

// callTool mirrors ft.callTool + sendRequest + sendRequestWithRetry:
// {method:"execute_tool", params:{client_id, tool, args, tool_use_id}}.
func (sc *socketClient) callTool(tool string, args map[string]any, opts *callOpts) (map[string]any, error) {
	params := map[string]any{
		"client_id":   sc.ctx.ClientTypeID,
		"tool":        tool,
		"args":        args,
		"tool_use_id": newUUID(),
	}
	if opts != nil && opts.PermissionMode != "" {
		params["permission_mode"] = opts.PermissionMode
	}
	if opts != nil && opts.SessionScope != nil {
		params["session_scope"] = opts.SessionScope
	}
	req := map[string]any{"method": "execute_tool", "params": params}
	timeoutMs := defaultToolTimeoutMs

	res, err := sc.sendRequest(req, timeoutMs, tool, params["tool_use_id"].(string))
	if err != nil {
		var sce *SocketConnectionError
		if !isConnErr(err, &sce) {
			return nil, err
		}
		// sendRequestWithRetry: force reconnect, retry once.
		sc.logf("Connection error, forcing reconnect and retrying: %s", err.Error())
		sc.mu.Lock()
		sc.closeSocketLocked()
		sc.mu.Unlock()
		if !sc.ensureConnected() {
			return nil, &SocketConnectionError{Msg: fmt.Sprintf("[%s] Cannot send request: not connected", sc.ctx.ServerName)}
		}
		return sc.sendRequest(req, timeoutMs, tool, params["tool_use_id"].(string))
	}
	return res, nil
}

func isConnErr(err error, out **SocketConnectionError) bool {
	var e *SocketConnectionError
	if ok := errorAs(err, &e); ok {
		*out = e
		return true
	}
	return false
}

func errorAs(err error, target any) bool {
	switch t := target.(type) {
	case **SocketConnectionError:
		if e, ok := err.(*SocketConnectionError); ok {
			*t = e
			return true
		}
	}
	return false
}

// sendRequest mirrors ft.sendRequest: single in-flight call (new calls
// supersede), armed timeout closes the socket.
func (sc *socketClient) sendRequest(req map[string]any, timeoutMs int, tool, toolUseID string) (map[string]any, error) {
	sc.mu.Lock()
	if sc.conn == nil {
		sc.mu.Unlock()
		return nil, &SocketConnectionError{Msg: fmt.Sprintf("[%s] Cannot send request: not connected", sc.ctx.ServerName)}
	}
	p := &pendingCall{
		toolUseID: toolUseID,
		resolve:   make(chan map[string]any, 1),
		err:       make(chan error, 1),
		timeoutMs: timeoutMs,
		tool:      tool,
	}
	// supersede the previous in-flight call (failPending(W))
	old := sc.pending
	sc.pending = p
	sc.mu.Unlock()
	if old != nil {
		if old.timer != nil {
			old.timer.Stop()
		}
		old.err <- &ToolCallSupersededError{Msg: fmt.Sprintf("[%s] Superseded by a newer tool request on the same connection", sc.ctx.ServerName)}
	}
	p.timer = time.AfterFunc(time.Duration(timeoutMs)*time.Millisecond, func() {
		sc.mu.Lock()
		if sc.pending != p {
			sc.mu.Unlock()
			return
		}
		sc.pending = nil
		sc.mu.Unlock()
		p.err <- &ToolCallTimeoutError{Msg: fmt.Sprintf("[%s] Tool request timed out after %dms", sc.ctx.ServerName, timeoutMs)}
		sc.mu.Lock()
		sc.closeSocketLocked()
		sc.mu.Unlock()
		sc.scheduleReconnect("transport")
	})
	if err := sc.writeRaw(req); err != nil {
		sc.mu.Lock()
		if sc.pending == p {
			sc.pending = nil
		}
		sc.mu.Unlock()
		p.timer.Stop()
		return nil, err
	}
	select {
	case m := <-p.resolve:
		return m, nil
	case e := <-p.err:
		return nil, e
	}
}

func (sc *socketClient) setNotificationHandler(h func(string, json.RawMessage)) {
	sc.mu.Lock()
	sc.notify = h
	sc.mu.Unlock()
}

func (sc *socketClient) disconnect() {
	sc.mu.Lock()
	if sc.reconnTimer != nil {
		sc.reconnTimer.Stop()
		sc.reconnTimer = nil
	}
	sc.mu.Unlock()
	sc.failPending(&ExtensionDisconnectedMidCallError{Msg: fmt.Sprintf("[%s] Socket client disconnected", sc.ctx.ServerName)})
	sc.mu.Lock()
	sc.closeSocketLocked()
	sc.reconnectN = 0
	sc.mu.Unlock()
}

// bridge-only capabilities a socket client doesn't have (mod_0803.js Kn/
// Hn/Gn gate on e.bridgeConfig / t.listConnectedExtensions presence).
func (sc *socketClient) listConnectedExtensions() []extensionInfo { return nil }
func (sc *socketClient) selectExtensionByID(_, _ string)          {}
func (sc *socketClient) switchBrowser() (string, any)             { return "", nil }
func (sc *socketClient) hasActiveSelection() bool                 { return false }
func (sc *socketClient) getSelectedDeviceID() string              { return "" }

// ---------------------------------------------------------------------------
// socketPool — mod_0803.js class I (T(e)): a client per socket in the dir.
// ---------------------------------------------------------------------------

type socketPool struct {
	ctx     *Context
	mu      sync.Mutex
	clients map[string]*socketClient
	routes  map[float64]string // tabId → socket path
	notify  func(string, json.RawMessage)
}

func newSocketPool(c *Context) *socketPool {
	return &socketPool{ctx: c, clients: map[string]*socketClient{}, routes: map[float64]string{}}
}

// refreshClients mirrors doRefreshClients: add a socketClient for every
// path from SocketPaths(), drop vanished ones.
func (p *socketPool) refreshClients() {
	var paths []string
	if p.ctx.SocketPaths != nil {
		paths = p.ctx.SocketPaths()
	}
	sort.Strings(paths)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, path := range paths {
		if _, ok := p.clients[path]; ok {
			continue
		}
		p.ctx.Logger.Infof("[%s] Adding socket to pool: %s", p.ctx.ServerName, path)
		cl := newSocketClient(p.ctx, path)
		cl.disableAutoReconnect = true
		cl.onRedialWanted = func(reason string) {
			if reason == "dead" {
				p.mu.Lock()
				delete(p.clients, path)
				for t, sp := range p.routes {
					if sp == path {
						delete(p.routes, t)
					}
				}
				p.mu.Unlock()
				return
			}
			cl.connect()
		}
		if p.notify != nil {
			cl.setNotificationHandler(p.notify)
		}
		p.clients[path] = cl
	}
	for path, cl := range p.clients {
		found := false
		for _, q := range paths {
			if q == path {
				found = true
			}
		}
		if !found && !cl.isConnected() {
			p.ctx.Logger.Infof("[%s] Removing stale socket from pool: %s", p.ctx.ServerName, path)
			cl.disconnect()
			delete(p.clients, path)
			for t, sp := range p.routes {
				if sp == path {
					delete(p.routes, t)
				}
			}
		}
	}
}

func (p *socketPool) ensureConnected() bool {
	p.refreshClients()
	p.mu.Lock()
	clients := make([]*socketClient, 0, len(p.clients))
	for _, c := range p.clients {
		clients = append(clients, c)
	}
	p.mu.Unlock()
	any := false
	for _, c := range clients {
		if c.ensureConnected() {
			any = true
		}
	}
	return any
}

func (p *socketPool) isConnected() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.clients {
		if c.isConnected() {
			return true
		}
	}
	return false
}

func (p *socketPool) connected() []*socketClient {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*socketClient
	for _, c := range p.clients {
		if c.isConnected() {
			out = append(out, c)
		}
	}
	return out
}

func (p *socketPool) pathFor(c *socketClient) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for path, cl := range p.clients {
		if cl == c {
			return path
		}
	}
	return ""
}

// updateTabRoutes mirrors updateTabRoutes: learn tabId→socket from
// tabs_context_mcp results.
func (p *socketPool) updateTabRoutes(res map[string]any, path string) {
	for _, t := range extractTabIDs(res) {
		p.mu.Lock()
		p.routes[t] = path
		p.mu.Unlock()
	}
}

func extractTabIDs(res map[string]any) []float64 {
	result, _ := res["result"].(map[string]any)
	content, _ := result["content"].([]any)
	var ids []float64
	for _, it := range content {
		m, _ := it.(map[string]any)
		if m == nil || m["type"] != "text" {
			continue
		}
		txt, _ := m["text"].(string)
		var parsed map[string]any
		if json.Unmarshal([]byte(txt), &parsed) != nil {
			var arr []any
			if json.Unmarshal([]byte(txt), &arr) != nil {
				continue
			}
			parsed = map[string]any{"availableTabs": arr}
		}
		tabs, _ := parsed["availableTabs"].([]any)
		for _, t := range tabs {
			if tm, ok := t.(map[string]any); ok {
				if id, ok := tm["tabId"].(float64); ok {
					ids = append(ids, id)
				}
			}
		}
	}
	return ids
}

// callTool mirrors I.callTool: route by tabId when known, else first
// connected client; tabs_context_mcp fans out and merges.
func (p *socketPool) callTool(tool string, args map[string]any, opts *callOpts) (map[string]any, error) {
	if tool == "tabs_context_mcp" {
		return p.callTabsContext(args, opts)
	}
	var preferred *socketClient
	if tid, ok := args["tabId"].(float64); ok {
		p.mu.Lock()
		if path, ok := p.routes[tid]; ok {
			preferred = p.clients[path]
		}
		p.mu.Unlock()
	}
	conns := p.connected()
	if preferred != nil {
		for _, c := range conns {
			if c == preferred {
				return c.callTool(tool, args, opts)
			}
		}
	}
	if len(conns) == 0 {
		return nil, &SocketConnectionError{Msg: fmt.Sprintf("[%s] No connected sockets available", p.ctx.ServerName)}
	}
	return conns[0].callTool(tool, args, opts)
}

// callTabsContext mirrors callTabsContext: single socket → direct; many →
// fan out, merge availableTabs + first tabGroupId.
func (p *socketPool) callTabsContext(args map[string]any, opts *callOpts) (map[string]any, error) {
	conns := p.connected()
	if len(conns) == 0 {
		return nil, &SocketConnectionError{Msg: fmt.Sprintf("[%s] No connected sockets available", p.ctx.ServerName)}
	}
	if len(conns) == 1 {
		res, err := conns[0].callTool("tabs_context_mcp", args, opts)
		if err == nil {
			p.updateTabRoutes(res, p.pathFor(conns[0]))
		}
		return res, err
	}
	type res struct {
		m    map[string]any
		path string
		err  error
	}
	ch := make(chan res, len(conns))
	for _, c := range conns {
		go func(c *socketClient) {
			m, err := c.callTool("tabs_context_mcp", args, opts)
			ch <- res{m, p.pathFor(c), err}
		}(c)
	}
	var merged []any
	var groupID *float64
	var first map[string]any
	p.mu.Lock()
	p.routes = map[float64]string{}
	p.mu.Unlock()
	for range conns {
		r := <-ch
		if r.err != nil {
			p.ctx.Logger.Infof("[%s] tabs_context_mcp failed on one socket: %v", p.ctx.ServerName, r.err)
			continue
		}
		p.updateTabRoutes(r.m, r.path)
		result, _ := r.m["result"].(map[string]any)
		content, _ := result["content"].([]any)
		for _, it := range content {
			m, _ := it.(map[string]any)
			if m == nil || m["type"] != "text" {
				continue
			}
			txt, _ := m["text"].(string)
			var parsed map[string]any
			if json.Unmarshal([]byte(txt), &parsed) != nil {
				var arr []any
				if json.Unmarshal([]byte(txt), &arr) == nil {
					merged = append(merged, arr...)
				}
				continue
			}
			if tabs, ok := parsed["availableTabs"].([]any); ok {
				merged = append(merged, tabs...)
			}
			if groupID == nil {
				if g, ok := parsed["tabGroupId"].(float64); ok {
					groupID = &g
				}
			}
		}
		if first == nil {
			first = r.m
		}
	}
	if len(merged) > 0 {
		out := map[string]any{"availableTabs": merged}
		if groupID != nil {
			out["tabGroupId"] = *groupID
		}
		b, _ := json.Marshal(out)
		return map[string]any{"result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": string(b)}}}}, nil
	}
	if first != nil {
		return first, nil
	}
	return nil, &SocketConnectionError{Msg: fmt.Sprintf("[%s] All sockets failed for tabs_context_mcp", p.ctx.ServerName)}
}

func (p *socketPool) disconnect() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.clients {
		c.disconnect()
	}
	p.clients = map[string]*socketClient{}
	p.routes = map[float64]string{}
}

func (p *socketPool) setNotificationHandler(h func(string, json.RawMessage)) {
	p.mu.Lock()
	p.notify = h
	for _, c := range p.clients {
		c.setNotificationHandler(h)
	}
	p.mu.Unlock()
}

func (p *socketPool) listConnectedExtensions() []extensionInfo { return nil }
func (p *socketPool) selectExtensionByID(_, _ string)          {}
func (p *socketPool) switchBrowser() (string, any)             { return "", nil }
func (p *socketPool) hasActiveSelection() bool                 { return false }
func (p *socketPool) getSelectedDeviceID() string              { return "" }
