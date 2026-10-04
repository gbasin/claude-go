package browsermcp

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// bridge.go — the cloud WebSocket transport, mirroring mod_0803.js class _
// (b(e)): wss://bridge.claudeusercontent.com/chrome/<userId> (or the
// localhost/staging overrides), OAuth-authenticated.
//
// Wire shapes (verbatim from the bundle):
//
//	connect:      {type:"connect", client_type:"claude-code",
//	              oauth_token|dev_user_id}
//	server hello: {type:"paired"} | {type:"waiting"}
//	roster:       {type:"list_extensions"} → {type:"extensions_list",
//	              extensions:[{deviceId,name,osPlatform,connectedAt}]}
//	pick:         {type:"pairing_request", request_id, client_type}
//	              → {type:"pairing_response", request_id, device_id, name}
//	call:         {type:"tool_call", tool_use_id, client_type, tool, args,
//	              supports_tool_result_notices:true, target_device_id?,
//	              permission_mode?, allowed_domains?,
//	              handle_permission_prompts?, session_scope?}
//	result:       {type:"tool_result", tool_use_id, result?|error?|
//	              content?, is_error?, notices?}
//	keepalive:    our {type:"ping"} every 30s; server {type:"ping"}→pong;
//	              dead after 90s without a pong
//	routing:      {type:"routing_ack", tool_use_id, routed_to, ...}
//	relay:        {type:"notification", method, params} → forwarded as
//	              MCP notifications; {type:"permission_request", ...}
//
// Not implemented: external_message / external_config relay config push
// (context.getExternalRelayConfig is a host-integration hook, absent here),
// browserActivityObserver hooks, and the proxy wsOptions plumbing
// (getWsOptions → gC()/hC(); gorilla honors HTTPS_PROXY itself via
// http.ProxyFromEnvironment on the dialer).

const (
	bridgeConnectTimeoutMs  = 30000 // S — handshake watchdog
	bridgeEnsureWaitMs      = 10000 // ensureConnected give-up
	bridgeKeepAliveMs       = 30000 // O
	bridgePongDeadMs        = 90000 // C
	bridgeDiscoverWaitMs    = 5000  // Yxr — list_extensions reply bound
	bridgePeerWaitMs        = 10000 // Gsn — peer_connected wait
	bridgePairingPromptMs   = 120000
	bridgeLocalIDsWaitMs    = 3000 // nko — getLocalDeviceIds bound
	bridgeReconnectMaxTries = 100
	bridgeReconnectBaseMs   = 2000
	bridgeReconnectCapMs    = 30000
	externalMessageSlackMs  = 30000 // L
)

// BridgeConfig mirrors context.bridgeConfig (mod_0807.js).
type BridgeConfig struct {
	URL         string
	DevUserID   string // dev_user_local when LOCAL_BRIDGE/USE_LOCAL_OAUTH
	OAuth       OAuthSource
	AccountUUID string // getUserId()'s persisted value
}

// deviceIDRe mirrors mod_0803.js m — /^[A-Za-z0-9-]{1,64}$/.
var deviceIDRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// noticeKindRe / noticeDomainRe mirror mod_0803.js W's sanitizer.
var (
	noticeKindRe   = regexp.MustCompile(`(?i)^[a-z0-9_]{1,64}$`)
	noticeDomainRe = regexp.MustCompile(`(?i)^[a-z0-9.-]+$`)
)

// bridgeNotice is one sanitized {kind, domain?} notice.
type bridgeNotice struct {
	Kind   string `json:"kind"`
	Domain string `json:"domain,omitempty"`
}

// sanitizeBridgeNotices mirrors W(e,onWarn) — cap 4 distinct kind:domain keys.
func sanitizeBridgeNotices(v any, warn func(string)) []bridgeNotice {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	const cap = 4
	seen := map[string]bridgeNotice{}
	var order []string
	put := func(k string, n bridgeNotice) bool {
		if _, has := seen[k]; has {
			seen[k] = n
			return true
		}
		if len(seen) >= cap {
			if warn != nil {
				warn(fmt.Sprintf("sanitizeBridgeNotices: notice cap (%d) reached; dropping entry", cap))
			}
			return false
		}
		seen[k] = n
		order = append(order, k)
		return true
	}
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := m["kind"].(string)
		if kind == "" || !noticeKindRe.MatchString(kind) {
			continue
		}
		kind = lowerASCII(kind)
		domain, hasD := m["domain"]
		if !hasD || domain == nil {
			put(kind+":", bridgeNotice{Kind: kind})
			continue
		}
		ds, ok := domain.(string)
		if !ok || len(ds) > 253 || !noticeDomainRe.MatchString(ds) {
			continue
		}
		ds = strings_TrimSuffix(lowerASCII(ds), ".")
		if ds == "" {
			continue
		}
		put(kind+":"+ds, bridgeNotice{Kind: kind, Domain: ds})
	}
	if len(order) == 0 {
		return nil
	}
	out := make([]bridgeNotice, 0, len(order))
	for _, k := range order {
		out = append(out, seen[k])
	}
	return out
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func strings_TrimSuffix(s, suf string) string {
	for len(s) > 0 && len(s) >= len(suf) && s[len(s)-len(suf):] == suf {
		s = s[:len(s)-len(suf)]
	}
	return s
}

// sanitizeString mirrors mod_0803.js v: strip control chars, cap len.
func sanitizeString(s string, max int) string {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r < 0x20 || r == '\r' || r == '\n' || r == '\t' {
			out = append(out, ' ')
			continue
		}
		out = append(out, string(r)...)
	}
	res := string(out)
	res = trimSpace(res)
	if len(res) > max {
		res = res[:max] + "…"
	}
	return res
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// sanitizeExtension mirrors mod_0803.js U(e).
func sanitizeExtension(m map[string]any) (extensionInfo, bool) {
	id, _ := m["deviceId"].(string)
	if !deviceIDRe.MatchString(id) {
		return extensionInfo{}, false
	}
	var e extensionInfo
	e.DeviceID = id
	e.Name = sanitizeString(strOf(m["name"]), 50)
	e.OSPlatform = sanitizeString(strOf(m["osPlatform"]), 30)
	if v, ok := m["connectedAt"].(float64); ok {
		e.ConnectedAt = int64(v)
	}
	return e, true
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

// pendingBridgeCall mirrors the pendingCalls entry.
type pendingBridgeCall struct {
	resolve            chan map[string]any
	reject             chan error
	timer              *time.Timer
	startTime          int64
	toolName           string
	timeoutMs          int
	routingAckReceived *bool
	routingAckPongAge  *int64
	permissionPaused   bool
}

// bridgeTransport is class _ from mod_0803.js.
type bridgeTransport struct {
	ctx *Context

	mu                   sync.Mutex
	ws                   *websocket.Conn
	connected            bool
	authenticated        bool
	connecting           bool
	reconnectTimer       *time.Timer
	handshakeTimer       *time.Timer
	reconnectAttempts    int
	pendingCalls         map[string]*pendingBridgeCall
	timedOutCalls        map[string]int64
	notify               func(string, json.RawMessage)
	permissionMode       string
	selectedDeviceID     string
	previousSelectedID   string
	discoveryComplete    bool
	multiBrowserPending  bool
	discoveryPending     chan []map[string]any
	pairingInProgress    bool
	pendingPairingReqID  string
	peerWaiters          []chan bool
	keepAlive            *time.Ticker
	lastPong             int64
	pendingSwitchResolve chan any
	stopKeepalive        chan struct{}
}

func newBridgeTransport(c *Context) *bridgeTransport {
	pm := c.InitialPermissionMode
	if pm == "" {
		pm = "ask"
	}
	return &bridgeTransport{
		ctx:            c,
		permissionMode: pm,
		pendingCalls:   map[string]*pendingBridgeCall{},
		timedOutCalls:  map[string]int64{},
	}
}

func (b *bridgeTransport) logf(f string, a ...any) {
	b.ctx.Logger.Infof("["+b.ctx.ServerName+"] "+f, a...)
}
func (b *bridgeTransport) warnf(f string, a ...any) {
	b.ctx.Logger.Warnf("["+b.ctx.ServerName+"] "+f, a...)
}
func (b *bridgeTransport) errf(f string, a ...any) {
	b.ctx.Logger.Errorf("["+b.ctx.ServerName+"] "+f, a...)
}

func (b *bridgeTransport) isConnected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connected && b.authenticated && b.ws != nil
}

func (b *bridgeTransport) setNotificationHandler(h func(string, json.RawMessage)) {
	b.mu.Lock()
	b.notify = h
	b.mu.Unlock()
}

// connect mirrors _'s connect(): resolve userId + token, dial, send
// connect message.
func (b *bridgeTransport) connect() {
	cfg := b.ctx.BridgeConfig
	if cfg == nil {
		b.errf("No bridge config provided")
		return
	}
	b.mu.Lock()
	if b.connecting {
		b.mu.Unlock()
		return
	}
	b.connecting = true
	b.authenticated = false
	b.mu.Unlock()
	b.closeSocket()

	b.handshakeTimer = time.AfterFunc(bridgeConnectTimeoutMs*time.Millisecond, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if !b.connecting {
			return
		}
		var state string
		if b.ws != nil {
			state = "open"
		}
		b.warnf("Bridge connect stuck after %dms (ws_state=%s)", bridgeConnectTimeoutMs, state)
		b.connecting = false
		b.mu.Unlock()
		b.closeSocket()
		b.scheduleReconnect()
		b.mu.Lock()
	})

	var userID, token string
	if cfg.DevUserID != "" {
		userID = cfg.DevUserID
		b.logf("Using dev user ID for bridge connection")
	} else {
		userID = cfg.AccountUUID
		if userID == "" && cfg.OAuth != nil {
			// token-derived account (getUserId): validate → account_uuid
			userID = validateTokenAccountUUID(cfg.OAuth.Token())
		}
		if userID == "" {
			b.errf("No user ID available")
			b.mu.Lock()
			b.connecting = false
			b.mu.Unlock()
			b.scheduleReconnect()
			return
		}
		if cfg.OAuth != nil {
			token = cfg.OAuth.Token()
		}
		if token == "" {
			b.errf("No OAuth token available")
			b.mu.Lock()
			b.connecting = false
			b.mu.Unlock()
			b.scheduleReconnect()
			return
		}
	}
	url := cfg.URL + "/chrome/" + userID
	b.logf("Connecting to bridge: %s", url)
	dialer := websocket.DefaultDialer
	ws, _, err := dialer.Dial(url, nil)
	if err != nil {
		b.errf("Failed to create WebSocket: %v", err)
		b.mu.Lock()
		b.connecting = false
		b.mu.Unlock()
		b.scheduleReconnect()
		return
	}
	b.mu.Lock()
	b.ws = ws
	b.mu.Unlock()

	// on open → send connect
	msg := map[string]any{"type": "connect", "client_type": b.ctx.ClientTypeID}
	if cfg.DevUserID != "" {
		msg["dev_user_id"] = cfg.DevUserID
	} else {
		msg["oauth_token"] = token
	}
	if err := ws.WriteJSON(msg); err != nil {
		b.errf("connect write failed: %v", err)
		ws.Close()
		b.mu.Lock()
		b.connecting = false
		b.mu.Unlock()
		b.scheduleReconnect()
		return
	}
	go b.readPump(ws)
}

func (b *bridgeTransport) readPump(ws *websocket.Conn) {
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			b.mu.Lock()
			if b.ws == ws {
				b.mu.Unlock()
				b.onClose()
			} else {
				b.mu.Unlock()
			}
			return
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			b.errf("Failed to parse bridge message: %v", err)
			continue
		}
		b.handleMessage(m, len(data))
	}
}

func (b *bridgeTransport) onClose() {
	b.mu.Lock()
	wasConn := b.connected
	b.connected = false
	b.authenticated = false
	b.connecting = false
	b.logf("Bridge connection closed")
	b.mu.Unlock()
	b.rejectPending(&SocketConnectionError{Msg: "Bridge connection closed mid-call"})
	b.stopKeepAliveLoop()
	if wasConn {
		b.scheduleReconnect()
	}
}

func (b *bridgeTransport) closeSocket() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopKeepAliveLoop()
	if b.handshakeTimer != nil {
		b.handshakeTimer.Stop()
		b.handshakeTimer = nil
	}
	if b.ws != nil {
		b.ws.Close()
		b.ws = nil
	}
	b.connected = false
	b.authenticated = false
	if b.selectedDeviceID != "" {
		b.previousSelectedID = b.selectedDeviceID
	}
	b.selectedDeviceID = ""
	b.discoveryComplete = false
	b.multiBrowserPending = false
	b.pairingInProgress = false
	b.pendingPairingReqID = ""
}

// handleMessage mirrors _'s handleMessage switch.
func (b *bridgeTransport) handleMessage(m map[string]any, _ int) {
	typ, _ := m["type"].(string)
	switch typ {
	case "paired":
		b.mu.Lock()
		b.connected, b.authenticated = true, true
		b.connecting = false
		b.reconnectAttempts = 0
		b.mu.Unlock()
		b.logf("Paired with Chrome extension")
		b.startKeepAliveLoop()
	case "waiting":
		b.mu.Lock()
		b.connected, b.authenticated = true, true
		b.connecting = false
		b.reconnectAttempts = 0
		b.mu.Unlock()
		b.logf("Waiting for Chrome extension to connect")
		b.startKeepAliveLoop()
	case "peer_connected":
		b.logf("Chrome extension connected to bridge")
		b.mu.Lock()
		if b.selectedDeviceID == "" {
			b.discoveryComplete = false
		}
		if b.previousSelectedID != "" {
			if id, _ := m["deviceId"].(string); id == b.previousSelectedID && b.pendingSwitchResolve == nil {
				b.logf("Previously selected extension reconnected, auto-reselecting")
				b.selectedDeviceID = b.previousSelectedID
				b.previousSelectedID = ""
			}
		}
		waiters := b.peerWaiters
		b.peerWaiters = nil
		b.mu.Unlock()
		for _, w := range waiters {
			w <- true
		}
	case "peer_disconnected":
		id, _ := m["deviceId"].(string)
		b.logf("Chrome extension disconnected from bridge (deviceId=%s, selected=%s)", orNone(id), orNone(b.getSelectedDeviceID()))
		b.mu.Lock()
		if id != "" && id == b.selectedDeviceID {
			b.logf("Selected extension disconnected, clearing selection")
			b.previousSelectedID = b.selectedDeviceID
			b.selectedDeviceID = ""
			b.discoveryComplete = false
			b.mu.Unlock()
			b.rejectPending(&ExtensionDisconnectedMidCallError{Msg: "Chrome extension disconnected mid-call"})
		} else if b.selectedDeviceID == "" && b.multiBrowserPending {
			b.discoveryComplete = false
			b.multiBrowserPending = false
			b.mu.Unlock()
		} else {
			b.mu.Unlock()
		}
	case "routing_ack":
		tid, _ := m["tool_use_id"].(string)
		b.mu.Lock()
		if p := b.pendingCalls[tid]; p != nil {
			t := true
			p.routingAckReceived = &t
			if v, ok := m["target_pong_age_ms"].(float64); ok {
				iv := int64(v)
				p.routingAckPongAge = &iv
			}
		}
		b.mu.Unlock()
	case "extensions_list":
		b.mu.Lock()
		ch := b.discoveryPending
		b.discoveryPending = nil
		b.mu.Unlock()
		exts, _ := m["extensions"].([]any)
		if ch != nil {
			ch <- toMapSlice(exts)
		}
	case "pairing_response":
		reqID, _ := m["request_id"].(string)
		b.mu.Lock()
		if b.pendingPairingReqID != reqID {
			b.mu.Unlock()
			break
		}
		if dismissed, _ := m["dismissed"].(bool); dismissed {
			b.logf("Pairing prompt dismissed in extension")
			b.pendingPairingReqID = ""
			b.pairingInProgress = false
			b.mu.Unlock()
			break
		}
		deviceID, _ := m["device_id"].(string)
		name := sanitizeString(strOf(m["name"]), 50)
		var sw chan any
		if deviceIDRe.MatchString(deviceID) && name != "" {
			b.pendingPairingReqID = ""
			b.pairingInProgress = false
			b.selectedDeviceID = deviceID
			sw = b.pendingSwitchResolve
			b.pendingSwitchResolve = nil
		}
		b.mu.Unlock()
		if deviceIDRe.MatchString(deviceID) && name != "" {
			if b.ctx.OnExtensionPaired != nil {
				b.ctx.OnExtensionPaired(deviceID, name)
			}
			b.logf(`Paired with %q (%s)`, name, deviceID[:minInt(8, len(deviceID))])
			if sw != nil {
				sw <- map[string]string{"deviceId": deviceID, "name": name}
			}
		}
	case "ping":
		b.sendJSON(map[string]any{"type": "pong"})
	case "pong":
		b.mu.Lock()
		b.lastPong = time.Now().UnixMilli()
		b.mu.Unlock()
	case "tool_result":
		b.handleToolResult(m)
	case "permission_request":
		// no host permission callback in the standalone server — forward
		// as an MCP notification (real does the same when no pending call
		// claims it).
		b.forwardAsNotification(m)
	case "notification":
		if method, ok := m["method"].(string); ok {
			b.mu.Lock()
			h := b.notify
			b.mu.Unlock()
			if h != nil {
				var params json.RawMessage
				if p, ok := m["params"]; ok {
					params, _ = json.Marshal(p)
				}
				h(method, params)
			}
		} else {
			b.warnf("Dropped a notification whose method is not a string")
		}
	case "error":
		b.warnf("Bridge error: %v", m["error"])
		if sel := b.getSelectedDeviceID(); sel != "" {
			b.clearSelectionIfGone(sel)
		}
	case "external_message", "external_config", "external_message_result":
		// host relay — not surfaced in the standalone server
	default:
		b.warnf("Unrecognized bridge message type: %s", typ)
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func toMapSlice(arr []any) []map[string]any {
	out := make([]map[string]any, 0, len(arr))
	for _, v := range arr {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// forwardAsNotification wraps an unclaimed bridge message as an MCP
// notification (mirrors the `ot(d)` fall-through on the socket path).
func (b *bridgeTransport) forwardAsNotification(m map[string]any) {
	method, _ := m["method"].(string)
	if method == "" {
		method = "permission_request"
	}
	b.mu.Lock()
	h := b.notify
	b.mu.Unlock()
	if h == nil {
		return
	}
	params, _ := json.Marshal(m)
	h(method, params)
}

// handleToolResult mirrors _'s handleToolResult + normalizeBridgeResponse.
func (b *bridgeTransport) handleToolResult(m map[string]any) {
	tid, _ := m["tool_use_id"].(string)
	if tid == "" {
		b.warnf("Received tool_result without tool_use_id")
		return
	}
	b.mu.Lock()
	p := b.pendingCalls[tid]
	if p == nil {
		if _, wasTimeout := b.timedOutCalls[tid]; wasTimeout {
			delete(b.timedOutCalls, tid)
		}
		b.mu.Unlock()
		return
	}
	delete(b.pendingCalls, tid)
	if p.timer != nil {
		p.timer.Stop()
	}
	b.mu.Unlock()

	c := normalizeBridgeResponse(m)
	isErr := false
	if v, ok := m["is_error"].(bool); ok && v {
		isErr = true
	}
	if _, has := c["error"]; has {
		isErr = true
	}
	if isErr {
		b.warnf("Tool call error: %s", p.toolName)
		b.mu.Lock()
		if b.selectedDeviceID == "" && !b.pairingInProgress {
			b.discoveryComplete = false
			b.mu.Unlock()
			p.reject <- &ExtensionDisconnectedMidCallError{Msg: fmt.Sprintf("[%s] Extension disconnected during tool call: %s", b.ctx.ServerName, p.toolName)}
			return
		}
		b.mu.Unlock()
	}
	p.resolve <- c
}

// normalizeBridgeResponse mirrors _'s normalizeBridgeResponse.
func normalizeBridgeResponse(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if k != "notices" {
			out[k] = v
		}
	}
	notices := sanitizeBridgeNotices(m["notices"], func(msg string) {
		// warn via context logger — needs receiver; caller passes nil
		_ = msg
	})
	addNotices := func(dst map[string]any) {
		if len(notices) > 0 {
			dst["notices"] = notices
		}
	}
	if _, ok := m["result"]; ok {
		addNotices(out)
		return out
	}
	if _, ok := m["error"]; ok {
		addNotices(out)
		return out
	}
	if content, ok := m["content"]; ok {
		if isErr, _ := m["is_error"].(bool); isErr {
			out = map[string]any{"error": map[string]any{"content": content}}
		} else {
			out = map[string]any{"result": map[string]any{"content": content}}
		}
		addNotices(out)
		return out
	}
	addNotices(out)
	return out
}

// rejectPending mirrors rejectPendingCalls.
func (b *bridgeTransport) rejectPending(err error) {
	b.mu.Lock()
	pend := b.pendingCalls
	b.pendingCalls = map[string]*pendingBridgeCall{}
	b.mu.Unlock()
	for _, p := range pend {
		if p.timer != nil {
			p.timer.Stop()
		}
		p.reject <- err
	}
}

// scheduleReconnect mirrors _'s backoff: min(2000*1.5^n, 30000), ≤100.
func (b *bridgeTransport) scheduleReconnect() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reconnectTimer != nil {
		return
	}
	b.reconnectAttempts++
	if b.reconnectAttempts > bridgeReconnectMaxTries {
		b.warnf("Giving up bridge reconnection after 100 attempts")
		b.reconnectAttempts = 0
		return
	}
	delay := float64(bridgeReconnectBaseMs) * pow15(b.reconnectAttempts-1)
	if delay > bridgeReconnectCapMs {
		delay = bridgeReconnectCapMs
	}
	if b.reconnectAttempts <= 10 || b.reconnectAttempts%10 == 0 {
		b.logf("Bridge reconnecting in %dms (attempt %d)", int(delay), b.reconnectAttempts)
	}
	b.reconnectTimer = time.AfterFunc(time.Duration(delay)*time.Millisecond, func() {
		b.mu.Lock()
		b.reconnectTimer = nil
		b.mu.Unlock()
		b.connect()
	})
}

// startKeepAliveLoop mirrors startKeepAlive: ping every 30s, declare dead
// after 90s without pong.
func (b *bridgeTransport) startKeepAliveLoop() {
	b.stopKeepAliveLoop()
	b.mu.Lock()
	b.lastPong = time.Now().UnixMilli()
	b.stopKeepalive = make(chan struct{})
	stop := b.stopKeepalive
	b.mu.Unlock()
	go func() {
		t := time.NewTicker(bridgeKeepAliveMs * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				b.mu.Lock()
				ws := b.ws
				b.mu.Unlock()
				if ws != nil {
					ws.WriteJSON(map[string]any{"type": "ping"})
				}
				b.mu.Lock()
				dead := b.lastPong > 0 && time.Now().UnixMilli()-b.lastPong > bridgePongDeadMs
				b.mu.Unlock()
				if dead {
					b.warnf("No pong received in %dms, closing dead connection", bridgePongDeadMs)
					b.rejectPending(&SocketConnectionError{Msg: "Bridge keepalive timeout — connection dead"})
					b.closeSocket()
					b.scheduleReconnect()
					return
				}
			}
		}
	}()
}

func (b *bridgeTransport) stopKeepAliveLoop() {
	if b.stopKeepalive != nil {
		close(b.stopKeepalive)
		b.stopKeepalive = nil
	}
}

func (b *bridgeTransport) sendJSON(v any) error {
	b.mu.Lock()
	ws := b.ws
	b.mu.Unlock()
	if ws == nil {
		return &SocketConnectionError{Msg: fmt.Sprintf("[%s] Bridge not connected", b.ctx.ServerName)}
	}
	return ws.WriteJSON(v)
}

// ensureConnected mirrors _'s ensureConnected: connect if needed, wait up
// to 10s for connected+authenticated.
func (b *bridgeTransport) ensureConnected() bool {
	b.mu.Lock()
	ok := b.connected && b.authenticated && b.ws != nil
	connecting := b.connecting
	hasTimer := b.reconnectTimer != nil
	b.mu.Unlock()
	if ok {
		return true
	}
	if !connecting && !hasTimer {
		b.connect()
	}
	deadline := time.Now().Add(bridgeEnsureWaitMs * time.Millisecond)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		ok = b.connected && b.authenticated && b.ws != nil
		giveUp := !b.connecting && b.reconnectTimer == nil
		b.mu.Unlock()
		if ok {
			return true
		}
		if giveUp {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
	b.mu.Lock()
	ok = b.connected && b.authenticated && b.ws != nil
	b.mu.Unlock()
	return ok
}

// callTool mirrors _'s callTool: discovery/selection first, then the
// tool_call frame with a per-call timeout.
func (b *bridgeTransport) callTool(tool string, args map[string]any, opts *callOpts) (map[string]any, error) {
	b.mu.Lock()
	wsOpen := b.ws != nil
	b.mu.Unlock()
	if !wsOpen {
		return nil, &SocketConnectionError{Msg: fmt.Sprintf("[%s] Bridge not connected", b.ctx.ServerName)}
	}
	if b.getSelectedDeviceID() == "" {
		b.mu.Lock()
		needDiscovery := !b.discoveryComplete || b.multiBrowserPending
		b.mu.Unlock()
		if needDiscovery {
			b.discoverAndSelectExtension()
		}
	}
	b.mu.Lock()
	sel := b.selectedDeviceID
	done := b.discoveryComplete && sel == "" && !b.pairingInProgress && !b.multiBrowserPending
	b.mu.Unlock()
	if done {
		return nil, &NoExtensionConnectedError{Msg: fmt.Sprintf("[%s] No Chrome extension connected after discovery", b.ctx.ServerName)}
	}

	id := newUUID()
	msg := map[string]any{
		"type":                         "tool_call",
		"tool_use_id":                  id,
		"client_type":                  b.ctx.ClientTypeID,
		"tool":                         tool,
		"args":                         args,
		"supports_tool_result_notices": true,
	}
	b.mu.Lock()
	if b.selectedDeviceID != "" {
		msg["target_device_id"] = b.selectedDeviceID
	}
	b.mu.Unlock()
	// HUn(opts): permission_mode / allowed_domains / handle_permission_prompts / session_scope
	if opts != nil {
		if opts.PermissionMode != "" {
			msg["permission_mode"] = opts.PermissionMode
		}
		if opts.SessionScope != nil {
			msg["session_scope"] = opts.SessionScope
		}
	} else {
		msg["permission_mode"] = b.permissionMode
	}

	p := &pendingBridgeCall{
		resolve:   make(chan map[string]any, 1),
		reject:    make(chan error, 1),
		startTime: time.Now().UnixMilli(),
		toolName:  tool,
		timeoutMs: defaultToolTimeoutMs,
	}
	p.timer = time.AfterFunc(time.Duration(p.timeoutMs)*time.Millisecond, func() {
		b.mu.Lock()
		if _, ok := b.pendingCalls[id]; !ok {
			b.mu.Unlock()
			return
		}
		delete(b.pendingCalls, id)
		b.timedOutCalls[id] = time.Now().UnixMilli()
		if len(b.timedOutCalls) > 200 {
			for k := range b.timedOutCalls {
				delete(b.timedOutCalls, k)
				break
			}
		}
		ack := p.routingAckReceived
		connected := b.connected
		b.mu.Unlock()
		b.warnf("Tool call timeout: %s (%s) pending calls now %d", p.toolName, id[:8], len(b.pendingCalls))
		if connected {
			p.reject <- &ToolCallTimeoutError{Msg: fmt.Sprintf("[%s] Tool call timed out: %s", b.ctx.ServerName, p.toolName), RoutingAckReceived: ack}
		} else {
			p.reject <- &SocketConnectionError{Msg: fmt.Sprintf("[%s] Tool call timed out with bridge disconnected: %s", b.ctx.ServerName, p.toolName)}
		}
	})
	b.mu.Lock()
	b.pendingCalls[id] = p
	b.mu.Unlock()
	b.logf("Sending tool_call: %s (%s)", scrubForLog(tool), id[:8])
	if err := b.sendJSON(msg); err != nil {
		b.mu.Lock()
		delete(b.pendingCalls, id)
		b.mu.Unlock()
		p.timer.Stop()
		return nil, err
	}
	select {
	case res := <-p.resolve:
		return res, nil
	case err := <-p.reject:
		return nil, err
	}
}

// --- discovery & selection (discoverAndSelectExtension / hrn / queryBridgeExtensions) ---

// queryBridgeExtensions mirrors the send+pendingDiscovery + dedupe-by-
// connectedAt part of queryBridgeExtensions.
func (b *bridgeTransport) queryBridgeExtensions() []extensionInfo {
	b.mu.Lock()
	if b.discoveryPending != nil {
		ch := b.discoveryPending
		b.mu.Unlock()
		select {
		case raw := <-ch:
			return sanitizeExtensionList(raw)
		case <-time.After(bridgeDiscoverWaitMs * time.Millisecond):
			return nil
		}
	}
	ch := make(chan []map[string]any, 1)
	b.discoveryPending = ch
	b.mu.Unlock()
	_ = b.sendJSON(map[string]any{"type": "list_extensions"})
	select {
	case raw := <-ch:
		return sanitizeExtensionList(raw)
	case <-time.After(bridgeDiscoverWaitMs * time.Millisecond):
		return nil
	}
}

// sanitizeExtensionList mirrors the dedupe-by-deviceId tail of
// queryBridgeExtensions.
func sanitizeExtensionList(raw []map[string]any) []extensionInfo {
	byID := map[string]extensionInfo{}
	for _, m := range raw {
		e, ok := sanitizeExtension(m)
		if !ok {
			continue
		}
		if prev, has := byID[e.DeviceID]; !has || e.ConnectedAt > prev.ConnectedAt {
			byID[e.DeviceID] = e
		}
	}
	out := make([]extensionInfo, 0, len(byID))
	for _, e := range byID {
		out = append(out, e)
	}
	return out
}

// waitForPeerConnected mirrors waitForPeerConnected(Gsn).
func (b *bridgeTransport) waitForPeerConnected(timeoutMs int) bool {
	ch := make(chan bool, 1)
	b.mu.Lock()
	b.peerWaiters = append(b.peerWaiters, ch)
	b.mu.Unlock()
	select {
	case <-ch:
		return true
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		b.mu.Lock()
		for i, w := range b.peerWaiters {
			if w == ch {
				b.peerWaiters = append(b.peerWaiters[:i], b.peerWaiters[i+1:]...)
			}
		}
		b.mu.Unlock()
		return false
	}
}

// hrnResult mirrors hrn()'s {kind, extension|?, reason, candidates, localCount}.
type hrnResult struct {
	kind       string // "none" | "select" | "ask"
	extension  extensionInfo
	reason     string // "persisted" | "only" | "local"
	candidates []extensionInfo
	localCount int
}

// pickExtension mirrors hrn(e,t,n).
func pickExtension(exts []extensionInfo, persisted string, local map[string]string) hrnResult {
	if len(exts) == 0 {
		return hrnResult{kind: "none"}
	}
	if persisted != "" {
		for _, e := range exts {
			if e.DeviceID == persisted {
				return hrnResult{kind: "select", extension: e, reason: "persisted"}
			}
		}
	}
	if len(exts) == 1 {
		return hrnResult{kind: "select", extension: exts[0], reason: "only"}
	}
	var localSet []extensionInfo
	for _, e := range exts {
		if _, ok := local[e.DeviceID]; ok {
			localSet = append(localSet, e)
		}
	}
	if len(localSet) == 1 && local[localSet[0].DeviceID] == "live" {
		return hrnResult{kind: "select", extension: localSet[0], reason: "local"}
	}
	var rest []extensionInfo
	for _, e := range exts {
		found := false
		for _, l := range localSet {
			if l.DeviceID == e.DeviceID {
				found = true
			}
		}
		if !found {
			rest = append(rest, e)
		}
	}
	return hrnResult{kind: "ask", candidates: append(localSet, rest...), localCount: len(localSet)}
}

// discoverAndSelectExtension mirrors the same-named method.
func (b *bridgeTransport) discoverAndSelectExtension() {
	persisted := ""
	if b.ctx.PersistedDeviceID != nil {
		persisted = b.ctx.PersistedDeviceID()
	}
	exts := b.queryBridgeExtensions()
	if len(exts) == 0 {
		b.logf("No extensions connected, waiting up to %dms for peer_connected", bridgePeerWaitMs)
		if b.waitForPeerConnected(bridgePeerWaitMs) {
			exts = b.queryBridgeExtensions()
		}
	}
	var local map[string]string
	if len(exts) > 1 && b.ctx.LocalDeviceIDs != nil {
		local = b.ctx.LocalDeviceIDs()
		hasPersisted := false
		for _, e := range exts {
			if e.DeviceID == persisted {
				hasPersisted = true
			}
		}
		if hasPersisted {
			local = nil // persisted wins before local check in hrn
		}
	}
	b.mu.Lock()
	b.discoveryComplete = true
	b.mu.Unlock()
	if b.getSelectedDeviceID() != "" {
		return
	}
	pick := pickExtension(exts, persisted, local)
	switch pick.kind {
	case "none":
		b.logf("No extensions found after waiting")
		b.mu.Lock()
		b.multiBrowserPending = false
		b.mu.Unlock()
	case "select":
		b.logf("Auto-connecting to %s extension: %s", pickReasonText(pick.reason), pick.extension.Name)
		b.selectExtension(pick.extension.DeviceID)
	case "ask":
		if b.ctx.AskUserToolName != "" {
			b.mu.Lock()
			b.multiBrowserPending = true
			b.mu.Unlock()
			return
		}
		b.broadcastPairingRequest()
		b.mu.Lock()
		b.pairingInProgress = true
		b.mu.Unlock()
	}
}

func pickReasonText(r string) string {
	switch r {
	case "persisted":
		return "the last-picked"
	case "only":
		return "the only connected"
	case "local":
		return "this computer's"
	}
	return r
}

func (b *bridgeTransport) selectExtension(id string) {
	b.mu.Lock()
	b.selectedDeviceID = id
	b.previousSelectedID = ""
	b.multiBrowserPending = false
	b.mu.Unlock()
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	b.logf("Selected Chrome extension: %s...", short)
}

func (b *bridgeTransport) getSelectedDeviceID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.selectedDeviceID != "" {
		return b.selectedDeviceID
	}
	if b.ctx.PersistedDeviceID != nil {
		return b.ctx.PersistedDeviceID()
	}
	return ""
}

func (b *bridgeTransport) hasActiveSelection() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.selectedDeviceID != ""
}

// clearSelectionIfGone mirrors the same-named method.
func (b *bridgeTransport) clearSelectionIfGone(deviceID string) {
	exts := b.queryBridgeExtensions()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.selectedDeviceID != deviceID {
		return
	}
	for _, e := range exts {
		if e.DeviceID == deviceID {
			return
		}
	}
	b.logf("Selected extension %s absent from bridge roster after error; clearing selection", deviceID[:minInt(8, len(deviceID))])
	b.selectedDeviceID = ""
	b.discoveryComplete = false
}

// broadcastPairingRequest mirrors the same-named method.
func (b *bridgeTransport) broadcastPairingRequest() {
	id := newUUID()
	b.mu.Lock()
	b.pendingPairingReqID = id
	b.mu.Unlock()
	_ = b.sendJSON(map[string]any{
		"type": "pairing_request", "request_id": id, "client_type": b.ctx.ClientTypeID,
	})
}

// listConnectedExtensions mirrors _'s listConnectedExtensions.
func (b *bridgeTransport) listConnectedExtensions() []extensionInfo {
	if !b.ensureConnected() {
		return nil
	}
	exts := b.queryBridgeExtensions()
	var local map[string]string
	if len(exts) > 1 && b.ctx.LocalDeviceIDs != nil {
		local = b.ctx.LocalDeviceIDs()
	}
	out := make([]extensionInfo, len(exts))
	for i, e := range exts {
		e.IsLocal = e.OSPlatform == osNameForPlatform(b.hostPlatform())
		if _, ok := local[e.DeviceID]; ok {
			e.OnThisComputer = true
		}
		out[i] = e
	}
	return out
}

func (b *bridgeTransport) hostPlatform() string {
	if b.ctx.HostPlatform != nil {
		return b.ctx.HostPlatform()
	}
	return "darwin"
}

// osNameForPlatform mirrors mod_0803.js f + isLocalExtension.
func osNameForPlatform(p string) string {
	switch p {
	case "darwin":
		return "macOS"
	case "win32", "windows":
		return "Windows"
	default:
		return "Linux"
	}
}

// switchBrowser mirrors _'s switchBrowser: "no_other_browsers" | nil |
// {deviceId,name} (120s pairing wait).
func (b *bridgeTransport) switchBrowser() (string, any) {
	exts := b.queryBridgeExtensions()
	b.mu.Lock()
	cur := b.selectedDeviceID
	prev := b.previousSelectedID
	b.mu.Unlock()
	n := cur
	if n == "" {
		n = prev
	}
	if len(exts) == 0 || (len(exts) == 1 && (n == "" || exts[0].DeviceID == n)) {
		return "no_other_browsers", nil
	}
	b.mu.Lock()
	b.previousSelectedID = b.selectedDeviceID
	b.selectedDeviceID = ""
	b.discoveryComplete = false
	b.pairingInProgress = false
	reqID := newUUID()
	b.pendingPairingReqID = reqID
	ch := make(chan any, 1)
	b.pendingSwitchResolve = ch
	b.mu.Unlock()
	if err := b.sendJSON(map[string]any{
		"type": "pairing_request", "request_id": reqID, "client_type": b.ctx.ClientTypeID,
	}); err != nil {
		return "", nil
	}
	select {
	case v := <-ch:
		return "", v
	case <-time.After(bridgePairingPromptMs * time.Millisecond):
		b.mu.Lock()
		if b.pendingPairingReqID == reqID {
			b.pendingPairingReqID = ""
		}
		b.pendingSwitchResolve = nil
		b.mu.Unlock()
		return "", nil
	}
}

// selectExtensionByID mirrors _'s selectExtensionById.
func (b *bridgeTransport) selectExtensionByID(deviceID, name string) {
	b.mu.Lock()
	b.discoveryComplete = true
	b.pairingInProgress = false
	b.pendingPairingReqID = ""
	b.mu.Unlock()
	b.selectExtension(deviceID)
	if b.ctx.OnExtensionPaired != nil {
		b.ctx.OnExtensionPaired(deviceID, name)
	}
	b.mu.Lock()
	if b.pendingSwitchResolve != nil {
		b.pendingSwitchResolve <- map[string]string{"deviceId": deviceID, "name": name}
		b.pendingSwitchResolve = nil
	}
	b.mu.Unlock()
}

func (b *bridgeTransport) disconnect() {
	b.mu.Lock()
	if b.reconnectTimer != nil {
		b.reconnectTimer.Stop()
		b.reconnectTimer = nil
	}
	b.mu.Unlock()
	b.closeSocket()
	b.rejectPending(&SocketConnectionError{Msg: "Bridge connection closed"})
}

// ---------------------------------------------------------------------------
// Transport pick — mod_0803.js BAo(e).
// ---------------------------------------------------------------------------

func pickTransport(c *Context) toolTransport {
	if c.BridgeConfig != nil {
		return newBridgeTransport(c)
	}
	if c.SocketPaths != nil {
		return newSocketPool(c)
	}
	return newSocketClient(c, c.SocketPath)
}
