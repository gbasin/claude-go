// Package mcp implements an MCP (Model Context Protocol) client supporting
// the stdio, streamable-HTTP, and legacy SSE transports.
// Servers are configured in ~/.claude.json ("mcpServers") or .mcp.json.
package mcp

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gbasin/claude-go/internal/config"
	"github.com/gbasin/claude-go/internal/plugins"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ProtocolVersion is the MCP revision this client speaks.
const ProtocolVersion = "2025-06-18"

// ServerConfig is one entry of an "mcpServers" map.
type ServerConfig struct {
	Type    string            `json:"type"` // "stdio" | "http" | "sse" | "claudeai-proxy"
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"` // http/sse servers
	Headers map[string]string `json:"headers"`
	// Authorization is a convenience field: sets the Authorization request
	// header (e.g. "Bearer ${MCP_TOKEN}") without listing it in Headers.
	Authorization string `json:"authorization,omitempty"`
	// Timeout is the per-server request timeout in ms — real's config
	// `timeout` field (>=1000, clamped to ~30min). Overrides
	// MCP_TOOL_TIMEOUT for tools/call.
	Timeout int `json:"timeout,omitempty"`
	// Enabled gates auto-connect — real's per-server enable flag
	// (/mcp enable|disable, "enable before reconnect"). nil → enabled.
	Enabled *bool `json:"enabled,omitempty"`
	// OAuth carries the server's client-registration pins
	// (--client-id/--client-secret/--callback-port/--xaa on mcp add).
	OAuth *OAuthConfig `json:"oauth,omitempty"`
	// DisableAutoBackground is real's sdk-server field — prevents the
	// server process from auto-backgrounding on slow startup.
	DisableAutoBackground bool `json:"disableAutoBackground,omitempty"`

	// --- claude.ai connector fields (mod_0386.js SLn) ---
	// Populated by FetchClaudeAIConfigs for Type "claudeai-proxy"; never
	// serialized into user mcpServers config files.
	Scope       string `json:"-"` // "claudeai"
	ID          string `json:"-"` // upstream server id (mcpsrv_…/mcprs_…)
	DisplayName string `json:"-"` // connector display_name as shown on claude.ai
	IconURL     string `json:"-"`
	// Stateless marks connectors whose upstream cannot hold session state;
	// real suppresses the GET notification stream for them (Yt()).
	Stateless bool `json:"-"`
	// Eligible is the control plane's per-connector verdict; a false verdict
	// on a never-connected connector keeps it out of the connect list
	// (ODt in mod_0386.js).
	Eligible         *bool  `json:"-"`
	IneligibleReason string `json:"-"` // eligibility_reason
	// EnterpriseManaged marks org-pushed installs (enterprise_managed).
	EnterpriseManaged bool `json:"-"`
	// ToolPermissions maps upstream tool name → effective_max_permission
	// (compacted JSON or "blocked") — _Ln() in mod_0386.js.
	ToolPermissions map[string]string `json:"-"`
	// CachedInitResponse / CachedDiscoverResponse are the control plane's
	// cached projections used to skip wire initialize / server/discover
	// (Cr()/ro() in mod_1797.js). Kept raw; consumed lazily.
	CachedInitResponse     json.RawMessage `json:"-"`
	DiscoverSupport        string          `json:"-"` // "supported" | "legacy"
	CachedDiscoverResponse json.RawMessage `json:"-"`

	// raw is the verbatim config JSON — the mcpOAuth credential key is a
	// hash over {type,url,headers} that must reproduce real's
	// JSON.stringify byte order, which map round-tripping would lose.
	raw json.RawMessage
}

// OAuthConfig is the optional per-server oauth block (`oauth:` in config).
type OAuthConfig struct {
	ClientID              string `json:"clientId,omitempty"`
	ClientSecret          string `json:"clientSecret,omitempty"`
	CallbackPort          int    `json:"callbackPort,omitempty"`
	AuthServerMetadataURL string `json:"authServerMetadataUrl,omitempty"`
	XAA                   bool   `json:"xaa,omitempty"`
}

// UnmarshalJSON keeps the original bytes in raw.
func (c *ServerConfig) UnmarshalJSON(b []byte) error {
	type plain ServerConfig
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*c = ServerConfig(p)
	c.raw = append(c.raw[:0], b...)
	return nil
}

// credKey is real's Hl(name,cfg): "<name>|<sha256({type,url,headers})[:16]>".
// The hashed object serializes exactly {type,url,headers:headers||{}} in
// that field order; when we still hold the original config bytes the
// headers member is lifted verbatim (real hashes JSON.stringify of the
// config, preserving its key order).
func (c ServerConfig) credKey(name string) string {
	var tj, uj, hj json.RawMessage
	var haveT, haveU bool
	hasRaw := len(c.raw) > 0
	if hasRaw {
		var m map[string]json.RawMessage
		if json.Unmarshal(c.raw, &m) == nil {
			tj, uj, hj = m["type"], m["url"], m["headers"]
			_, haveT = m["type"]
			_, haveU = m["url"]
		}
	} else {
		haveT, haveU = true, true // code-built configs behave as defined
	}
	// JSON.stringify drops undefined fields — type/url only appear when
	// present in the config; headers is `headers||{}` so always emitted.
	var obj []byte
	obj = append(obj, '{')
	sep := []byte(nil)
	emit := func(key string, v json.RawMessage) {
		obj = append(obj, sep...)
		obj = append(obj, key...)
		obj = append(obj, v...)
		sep = []byte(",")
	}
	if len(tj) > 0 {
		emit(`"type":`, tj)
	} else if c.Type != "" && haveT {
		emit(`"type":`, json.RawMessage(strconv.Quote(c.Type)))
	}
	if len(uj) > 0 {
		emit(`"url":`, uj)
	} else if c.URL != "" && haveU {
		emit(`"url":`, json.RawMessage(strconv.Quote(c.URL)))
	}
	if len(hj) == 0 {
		hj, _ = json.Marshal(c.Headers)
		if len(hj) == 0 || string(hj) == "null" {
			hj = []byte("{}")
		}
	}
	emit(`"headers":`, hj)
	obj = append(obj, '}')
	sum := sha256.Sum256(obj)
	return name + "|" + hex.EncodeToString(sum[:])[:16]
}

// LoadConfigs merges mcpServers from ~/.claude.json and <cwd>/.mcp.json.
// expandMCPString expands ${VAR} and ${VAR:-default} against the
// process env plus per-source extras (CLAUDE_PLUGIN_ROOT for plugin
// .mcp.json) — real expands env vars in mcp server configs the same way.
func expandMCPString(s string, extra map[string]string) string {
	return os.Expand(s, func(name string) string {
		def := ""
		if i := strings.Index(name, ":-"); i >= 0 {
			name, def = name[:i], name[i+2:]
		}
		if v, ok := extra[name]; ok {
			return v
		}
		if v := os.Getenv(name); v != "" {
			return v
		}
		return def
	})
}

func expandServerEnv(sc *ServerConfig, extra map[string]string) {
	sc.Command = expandMCPString(sc.Command, extra)
	for i := range sc.Args {
		sc.Args[i] = expandMCPString(sc.Args[i], extra)
	}
	sc.URL = expandMCPString(sc.URL, extra)
	sc.Authorization = expandMCPString(sc.Authorization, extra)
	for k, v := range sc.Env {
		sc.Env[k] = expandMCPString(v, extra)
	}
	for k, v := range sc.Headers {
		sc.Headers[k] = expandMCPString(v, extra)
	}
}

// ~/.claude.json contributes two scopes: top-level mcpServers (user) and
// projects["<cwd>"].mcpServers (local — real's default `mcp add` target;
// local wins on name conflicts). Project (.mcp.json) entries are gated:
// enabledMcpjsonServers allows, disabledMcpjsonServers removes,
// enableAllProjectMcpServers trusts all.
func LoadConfigs(cwd string) map[string]ServerConfig {
	out := map[string]ServerConfig{}
	home, _ := os.UserHomeDir()
	st := config.Load(cwd)
	merge := func(servers map[string]ServerConfig, gated bool) {
		for n, c := range servers {
			expandServerEnv(&c, nil)
			if gated && !st.EnableAllProjectMcpServers {
				if sliceHas(st.DisabledMcpjsonServers, n) {
					continue
				}
				if !sliceHas(st.EnabledMcpjsonServers, n) {
					// untrusted project servers are skipped until approved
					// (real prompts; non-interactive contexts skip)
					if os.Getenv("CLAUDE_GO_APPROVE_MCP") == "" {
						continue
					}
				}
			}
			out[n] = c
		}
	}
	for _, p := range []string{
		filepath.Join(home, ".claude.json"),
		filepath.Join(cwd, ".mcp.json"),
	} {
		// .mcp.json is project scope — --setting-sources without "project" skips it
		if strings.HasSuffix(p, ".mcp.json") && !config.SettingSourceOK(cwd,
			filepath.Join(cwd, ".claude", "settings.json")) {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var top map[string]json.RawMessage
		if json.Unmarshal(b, &top) != nil {
			continue
		}
		var servers map[string]ServerConfig
		if json.Unmarshal(top["mcpServers"], &servers) == nil {
			merge(servers, strings.HasSuffix(p, ".mcp.json"))
		}
		if strings.HasSuffix(p, ".claude.json") {
			// local scope: projects["<cwd>"].mcpServers
			var projects map[string]struct {
				Servers map[string]ServerConfig `json:"mcpServers"`
			}
			if json.Unmarshal(top["projects"], &projects) == nil {
				if proj, ok := projects[cwd]; ok {
					merge(proj.Servers, false)
				}
			}
		}
	}
	// plugin .mcp.json: each enabled plugin dir may declare servers;
	// --plugin-dir-no-mcp entries skip this entirely (real parity)
	var pluginServers []string
	for _, p := range plugins.EnabledPluginEntries() {
		if p.NoMCP {
			continue
		}
		b, err := os.ReadFile(filepath.Join(p.Path, ".mcp.json"))
		if err != nil {
			continue
		}
		var top struct {
			Servers map[string]ServerConfig `json:"mcpServers"`
		}
		if json.Unmarshal(b, &top) != nil {
			continue
		}
		extra := map[string]string{"CLAUDE_PLUGIN_ROOT": p.Path}
		for n, sc := range top.Servers {
			expandServerEnv(&sc, extra)
			if _, dup := out[n]; !dup {
				out[n] = sc
			}
		}
		pluginServers = append(pluginServers, keysOf(top.Servers)...)
	}
	// --channels <csv>: selects which plugin-provided servers run
	// (real's channel allowlist — mod_0056.js). Doesn't gate .mcp.json.
	// --dangerously-load-development-channels exempts dev_* servers.
	if sel := os.Getenv("CLAUDE_GO_CHANNELS"); sel != "" {
		want := map[string]bool{}
		for _, s := range strings.Split(sel, ",") {
			want[strings.TrimSpace(s)] = true
		}
		devOK := os.Getenv("CLAUDE_GO_DANGEROUSLY_LOAD_DEVELOPMENT_CHANNELS") != ""
		for _, n := range pluginServers {
			if devOK && strings.HasPrefix(n, "dev_") {
				continue
			}
			if !want[n] {
				delete(out, n)
			}
		}
	}
	return out
}

func keysOf(m map[string]ServerConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sliceHas(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

// expandConfig interpolates ${VAR} / $VAR environment references in
// url, headers, authorization, and env values.
func expandConfig(cfg ServerConfig) ServerConfig {
	cfg.URL = os.ExpandEnv(cfg.URL)
	cfg.Authorization = os.ExpandEnv(cfg.Authorization)
	for k, v := range cfg.Headers {
		cfg.Headers[k] = os.ExpandEnv(v)
	}
	for k, v := range cfg.Env {
		cfg.Env[k] = os.ExpandEnv(v)
	}
	if cfg.Authorization != "" {
		if cfg.Headers == nil {
			cfg.Headers = map[string]string{}
		}
		if _, ok := cfg.Headers["Authorization"]; !ok {
			cfg.Headers["Authorization"] = cfg.Authorization
		}
	}
	return cfg
}

type rpcReq struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// transport is one JSON-RPC message channel to an MCP server.
// Inbound messages (responses, notifications, server->client requests)
// are delivered to Client.dispatch.
type transport interface {
	// send transmits one marshaled JSON-RPC message. For transports where
	// the response arrives inline (streamable HTTP POST), send dispatches
	// inbound messages before returning.
	send(ctx context.Context, msg []byte) error
	// close shuts the transport down.
	close() error
}

// Client is one MCP server connection (any transport).
type Client struct {
	Name  string
	Tools []Tool
	// Instructions is the server's initialize-result instructions string
	// (injected into the agent's system prompt by internal/agent).
	Instructions string
	// ServerVersion and ServerName come from initialize result serverInfo.
	ServerName    string
	ServerVersion string
	// Capabilities is the raw server capabilities object from initialize.
	Capabilities json.RawMessage
	// Status: "connected", "needs-auth", or "failed".
	Status string
	// AuthURL is populated when the server requires interactive OAuth.
	AuthURL string
	// OnNotification, if set, receives server->client JSON-RPC
	// notifications (notifications/progress, notifications/*/list_changed,
	// notifications/message, ...). Called on a separate goroutine.
	OnNotification func(method string, params json.RawMessage)
	// OnToolsChanged fires after a tools/list_changed refresh lands —
	// the app re-registers the new surface.
	OnToolsChanged func()
	// OnPromptsChanged, if set, fires after prompts/list_changed — callers
	// re-run ListPrompts and rebuild the /mcp__*__ prompt commands.
	OnPromptsChanged func()
	// Elicit answers elicitation/create — returns {"action","content"}
	// (nil → decline). Real renders a form dialog; REPL installs one.
	Elicit func(params json.RawMessage) map[string]any
	// Roots, if set, answers roots/list server requests with the session's
	// workspace roots (project dir + add-dirs).
	Roots func() []map[string]string
	// ElicitURL answers url-mode elicitation/create — should surface
	// params.url to the user (real shows an "open link" dialog). Nil →
	// default handler prints the URL to stderr and accepts.
	ElicitURL func(url, elicitationID string) map[string]any

	tr          transport
	cmd         *exec.Cmd     // stdio only
	toolTimeout time.Duration // per-server `timeout` or MCP_TOOL_TIMEOUT
	seq         atomic.Int64
	mu          sync.Mutex
	pending     map[string]chan rpcResp
	closed      atomic.Bool
	// toolsListErr records a failed tools/list during handshake (v.toolsListError
	// in mod_1797.js); discoveryBearerRejected marks a 401/403 there
	// (v.discoveryBearerRejected). Both gate the ever-connected write-back.
	toolsListErr            error
	discoveryBearerRejected bool
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Connect opens the configured server and performs the MCP handshake.
// cfg.Type may be "stdio" (default), "http" (streamable HTTP), or "sse".
func Connect(name string, cfg ServerConfig) (*Client, error) {
	cfg = expandConfig(cfg)
	typ := cfg.Type
	if typ == "" {
		if cfg.URL != "" {
			typ = "http"
		} else {
			typ = "stdio"
		}
	}
	var c *Client
	var err error
	toolTimeout := mcpToolTimeout(cfg.Timeout)
	switch typ {
	case "stdio":
		c, err = connectStdio(name, cfg)
	case "http":
		c, err = connectHTTP(name, cfg)
	case "sse":
		c, err = connectSSE(name, cfg)
	case "ws":
		c, err = connectWSTransport(name, cfg)
	case "claudeai-proxy":
		c, err = connectClaudeAIProxy(name, cfg)
	default:
		err = fmt.Errorf("mcp transport %q unsupported", cfg.Type)
	}
	if err != nil {
		recordStatus(name, "failed", err.Error(), "")
		return nil, err
	}
	c.toolTimeout = toolTimeout
	if err := c.handshake(); err != nil {
		var ae *AuthError
		if asAuthErr(err, &ae) {
			recordStatus(name, "needs-auth", ae.Error(), ae.AuthURL)
		} else {
			recordStatus(name, "failed", err.Error(), "")
		}
		c.Close()
		return nil, err
	}
	recordStatus(name, "connected", "", "")
	// cdt() in mod_0386.js: a successful claude.ai connector mount (no
	// tools/list failure, no bearer rejection) marks the session set and
	// appends the display name to claudeAiMcpEverConnected in ~/.claude.json.
	if cfg.Type == "claudeai-proxy" && c.toolsListErr == nil && !c.discoveryBearerRejected {
		NoteClaudeAIConnected(name)
	}
	return c, nil
}

func asAuthErr(err error, out **AuthError) bool {
	var ae *AuthError
	if errors.As(err, &ae) {
		*out = ae
		return true
	}
	return false
}

// handshake runs initialize + notifications/initialized + tools/list.
func (c *Client) handshake() error {
	ctx, cancel := context.WithTimeout(context.Background(), mcpTimeout())
	defer cancel()
	var initRes struct {
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		Capabilities json.RawMessage `json:"capabilities"`
		Instructions string          `json:"instructions"`
	}
	// real advertises roots (listChanged), elicitation, and tasks —
	// servers gate roots/list and elicitation/create on these
	err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities": map[string]any{
			"roots":       map[string]any{"listChanged": true},
			"elicitation": map[string]any{},
			"tasks": map[string]any{
				"requests": map[string]any{
					"elicitation": map[string]any{"create": map[string]any{}},
				},
			},
		},
		"clientInfo": map[string]string{"name": "claude-go", "version": "2.1.296"},
	}, &initRes)
	if err != nil {
		return fmt.Errorf("mcp %s initialize: %w", c.Name, err)
	}
	c.ServerName = initRes.ServerInfo.Name
	c.ServerVersion = initRes.ServerInfo.Version
	c.Capabilities = initRes.Capabilities
	c.Instructions = initRes.Instructions
	c.Status = "connected"
	if initRes.Instructions != "" {
		instrMu.Lock()
		Instructions[c.Name] = initRes.Instructions
		instrMu.Unlock()
	}
	c.notify("notifications/initialized", nil)

	var listRes struct {
		Tools []Tool `json:"tools"`
	}
	if err := c.call(ctx, "tools/list", nil, &listRes); err == nil {
		c.Tools = listRes.Tools
	} else {
		c.toolsListErr = err
		var ae *AuthError
		if asAuthErr(err, &ae) {
			c.discoveryBearerRejected = true
		}
	}
	// streamable-http servers may expose a GET stream for server-initiated
	// traffic; open it opportunistically for notifications. claude.ai proxy
	// transports suppress it (the proxy answers GET with a local 405).
	if ht, ok := c.tr.(*httpTransport); ok && !ht.noGetStream {
		ht.openGetStream()
	}
	return nil
}

// dispatch routes one inbound JSON-RPC message.
func (c *Client) dispatch(raw []byte) {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	hasID := len(m.ID) > 0 && string(m.ID) != "null"
	if hasID && m.Method == "" {
		// response to a pending request
		key := string(m.ID)
		c.mu.Lock()
		ch := c.pending[key]
		delete(c.pending, key)
		c.mu.Unlock()
		if ch != nil {
			var resp rpcResp
			json.Unmarshal(raw, &resp)
			ch <- resp
		}
		return
	}
	if m.Method == "" {
		return
	}
	if hasID {
		// server->client request
		c.handleServerRequest(m.ID, m.Method, m.Params)
		return
	}
	c.handleNotification(m.Method, m.Params)
}

// SetNotificationHandler registers the OnNotification hook under the
// client lock (safe to call while connected).
func (c *Client) SetNotificationHandler(fn func(method string, params json.RawMessage)) {
	c.mu.Lock()
	c.OnNotification = fn
	c.mu.Unlock()
}

// handleNotification invokes the OnNotification hook and performs default
// bookkeeping (tool list refresh on tools/list_changed).
// ListTools returns a copy of the current tool list (refreshed on
// notifications/tools/list_changed).
func (c *Client) ListTools() []Tool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Tool, len(c.Tools))
	copy(out, c.Tools)
	return out
}

func (c *Client) handleNotification(method string, params json.RawMessage) {
	c.mu.Lock()
	fn := c.OnNotification
	c.mu.Unlock()
	if fn != nil {
		go fn(method, params)
	}
	switch method {
	case "notifications/prompts/list_changed":
		c.mu.Lock()
		cb := c.OnPromptsChanged
		c.mu.Unlock()
		if cb != nil {
			go cb()
		}
	case "notifications/tools/list_changed":
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var res struct {
				Tools []Tool `json:"tools"`
			}
			if err := c.call(ctx, "tools/list", nil, &res); err == nil {
				c.mu.Lock()
				c.Tools = res.Tools
				oc := c.OnToolsChanged
				c.mu.Unlock()
				if oc != nil {
					oc() // re-register mcp__<server>__<tool> on the new list
				}
			}
		}()
	case "notifications/resources/list_changed":
		// real refreshes resources too — surface via OnNotification
		// (callers already got the notification above)
	}
}

// handleServerRequest answers server->client JSON-RPC requests. Only ping
// is understood; everything else gets -32601 (method not found).
func (c *Client) handleServerRequest(id json.RawMessage, method string, params json.RawMessage) {
	var resp map[string]any
	if method == "logging/setLevel" {
		resp = map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": map[string]any{}}
	} else if method == "roots/list" {
		var roots []map[string]string
		if c.Roots != nil {
			roots = c.Roots()
		}
		resp = map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id),
			"result": map[string]any{"roots": roots}}
	} else if method == "ping" {
		resp = map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": map[string]any{}}
	} else if method == "elicitation/create" {
		// real renders a form (Elicit hook); nil handler → decline.
		// URL mode (params.mode=="url") carries {url, elicitationId}: the
		// user completes the flow in a browser and the server confirms
		// with notifications/elicitation/complete — we surface the URL
		// and acknowledge, matching real's "MCP input: open link" flow.
		var p struct {
			Mode          string `json:"mode"`
			URL           string `json:"url"`
			ElicitationID string `json:"elicitationId"`
		}
		_ = json.Unmarshal(params, &p)
		if p.Mode == "url" && p.URL != "" {
			resp = map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id),
				"result": c.elicitURL(p.URL, p.ElicitationID)}
		} else {
			action := map[string]any{"action": "decline"}
			if c.Elicit != nil {
				action = c.Elicit(params)
			}
			resp = map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": action}
		}
	} else {
		resp = map[string]any{
			"jsonrpc": "2.0", "id": json.RawMessage(id),
			"error": map[string]any{"code": -32601, "message": "method not found: " + method},
		}
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.tr.send(ctx, b)
}

// elicitURL handles a url-mode elicitation: real opens a dialog that links
// the user to params.url; the headless default announces it on stderr and
// accepts (the server confirms via notifications/elicitation/complete).
func (c *Client) elicitURL(url, elicitationID string) map[string]any {
	if c.ElicitURL != nil {
		return c.ElicitURL(url, elicitationID)
	}
	fmt.Fprintf(os.Stderr, "  [mcp %s] server requests you open: %s\n", c.Name, url)
	return map[string]any{"action": "accept"}
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	id := c.seq.Add(1)
	key := strconv.FormatInt(id, 10)
	ch := make(chan rpcResp, 1)
	c.mu.Lock()
	c.pending[key] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}()
	b, _ := json.Marshal(rpcReq{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err := c.tr.send(ctx, b); err != nil {
		return err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return fmt.Errorf("mcp error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		if out != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) notify(method string, params any) {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": method, "params": params,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c.tr.send(ctx, b)
}

type callResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// CallTool invokes a tool and returns concatenated text content.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
	var params map[string]any
	if len(args) > 0 {
		json.Unmarshal(args, &params)
	}
	var res callResult
	to := c.toolTimeout
	if to <= 0 {
		to = 120 * time.Second
	}
	ctx2, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	err := c.call(ctx2, "tools/call", map[string]any{
		"name": name, "arguments": params,
	}, &res)
	if err != nil {
		return "", true, err
	}
	var out string
	for _, ct := range res.Content {
		out += ct.Text
	}
	return out, res.IsError, nil
}

// Ping sends a ping request.
func (c *Client) Ping(ctx context.Context) error {
	return c.call(ctx, "ping", nil, nil)
}

func (c *Client) Close() {
	if c.closed.Swap(true) {
		return
	}
	if c.tr != nil {
		c.tr.close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		c.cmd.Process.Kill()
		c.cmd.Wait()
	}
	// Wake pending callers with an error response. Non-blocking send: the
	// channel is buffered (cap 1) and may already hold a real response.
	c.mu.Lock()
	pend := c.pending
	c.pending = map[string]chan rpcResp{}
	c.mu.Unlock()
	errResp := rpcResp{Error: &struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{Code: -32000, Message: "mcp connection closed"}}
	for _, ch := range pend {
		select {
		case ch <- errResp:
		default:
		}
	}
}

// ListResources calls resources/list and returns a text listing.
func (c *Client) ListResources(ctx context.Context) (string, error) {
	res, _, err := c.ListResourcesTyped(ctx, "")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, r := range res {
		fmt.Fprintf(&b, "%s  %s  %s\n", r.URI, r.Name, r.Description)
	}
	return b.String(), nil
}

// Resource is one entry of resources/list.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MimeType    string `json:"mimeType"`
}

// ListResourcesTyped calls resources/list and returns typed resources plus
// the pagination cursor.
func (c *Client) ListResourcesTyped(ctx context.Context, cursor string) ([]Resource, string, error) {
	var params map[string]any
	if cursor != "" {
		params = map[string]any{"cursor": cursor}
	}
	var res struct {
		Resources  []Resource `json:"resources"`
		NextCursor string     `json:"nextCursor"`
	}
	if err := c.call(ctx, "resources/list", params, &res); err != nil {
		return nil, "", err
	}
	return res.Resources, res.NextCursor, nil
}

// ResourceTemplate is one entry of resources/templates/list.
type ResourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MimeType    string `json:"mimeType"`
}

// ListResourceTemplates calls resources/templates/list.
func (c *Client) ListResourceTemplates(ctx context.Context, cursor string) ([]ResourceTemplate, string, error) {
	var params map[string]any
	if cursor != "" {
		params = map[string]any{"cursor": cursor}
	}
	var res struct {
		ResourceTemplates []ResourceTemplate `json:"resourceTemplates"`
		NextCursor        string             `json:"nextCursor"`
	}
	if err := c.call(ctx, "resources/templates/list", params, &res); err != nil {
		return nil, "", err
	}
	return res.ResourceTemplates, res.NextCursor, nil
}

// ReadResource calls resources/read.
func (c *Client) ReadResource(ctx context.Context, uri string) (string, error) {
	var res struct {
		Contents []struct {
			URI      string `json:"uri"`
			MimeType string `json:"mimeType"`
			Text     string `json:"text"`
			Blob     string `json:"blob"`
		} `json:"contents"`
	}
	if err := c.call(ctx, "resources/read", map[string]any{"uri": uri}, &res); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, ct := range res.Contents {
		if ct.Text != "" {
			b.WriteString(ct.Text)
		} else if ct.Blob != "" {
			b.WriteString("[base64 blob " + ct.MimeType + "]")
		}
	}
	return b.String(), nil
}

// --- prompts capability ---

// Prompt is one entry of prompts/list.
type Prompt struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

// PromptArgument describes one argument slot of a prompt.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// ListPrompts calls prompts/list; returns prompts and the next cursor.
func (c *Client) ListPrompts(ctx context.Context, cursor string) ([]Prompt, string, error) {
	var params map[string]any
	if cursor != "" {
		params = map[string]any{"cursor": cursor}
	}
	var res struct {
		Prompts    []Prompt `json:"prompts"`
		NextCursor string   `json:"nextCursor"`
	}
	if err := c.call(ctx, "prompts/list", params, &res); err != nil {
		return nil, "", err
	}
	return res.Prompts, res.NextCursor, nil
}

// PromptMessage is one message of a prompts/get result.
type PromptMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// GetPromptResult is the result of prompts/get.
type GetPromptResult struct {
	Description string          `json:"description"`
	Messages    []PromptMessage `json:"messages"`
}

// GetPrompt calls prompts/get with optional string arguments.
func (c *Client) GetPrompt(ctx context.Context, name string, args map[string]string) (*GetPromptResult, error) {
	var res GetPromptResult
	err := c.call(ctx, "prompts/get", map[string]any{
		"name": name, "arguments": args,
	}, &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// --- status registry (for `mcp` status display) ---

// ServerStatus records the last Connect outcome for one server.
type ServerStatus struct {
	Name    string
	Status  string // "connected" | "failed" | "needs-auth"
	Err     string
	AuthURL string
}

var statusReg sync.Map // name -> ServerStatus

func recordStatus(name, status, errStr, authURL string) {
	statusReg.Store(name, ServerStatus{Name: name, Status: status, Err: errStr, AuthURL: authURL})
}

// ServerStatuses returns all recorded connection outcomes.
func ServerStatuses() []ServerStatus {
	var out []ServerStatus
	statusReg.Range(func(_, v any) bool {
		out = append(out, v.(ServerStatus))
		return true
	})
	return out
}

// Status reports the recorded outcome for one server.
func Status(name string) (ServerStatus, bool) {
	v, ok := statusReg.Load(name)
	if !ok {
		return ServerStatus{}, false
	}
	return v.(ServerStatus), true
}

// --- stdio transport ---

type stdioTransport struct {
	stdin io.WriteCloser
}

func (t *stdioTransport) send(_ context.Context, msg []byte) error {
	_, err := t.stdin.Write(append(msg, '\n'))
	return err
}

func (t *stdioTransport) close() error { return nil }

// connectStdio spawns the server subprocess.
func connectStdio(name string, cfg ServerConfig) (*Client, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	env := os.Environ()
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &Client{
		Name:    name,
		cmd:     cmd,
		tr:      &stdioTransport{stdin: stdin},
		pending: map[string]chan rpcResp{},
	}
	go c.readLoop(stdout)
	return c, nil
}

func (c *Client) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		cp := make([]byte, len(line))
		copy(cp, line)
		c.dispatch(cp)
	}
	// transport EOF — the server died (spawn failure, crash). Wake every
	// pending call now; they would otherwise hang to the handshake
	// timeout with an opaque "context deadline exceeded"
	c.mu.Lock()
	pend := c.pending
	c.pending = map[string]chan rpcResp{}
	c.mu.Unlock()
	errResp := rpcResp{Error: &struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{Code: -32000, Message: "mcp server exited"}}
	for _, ch := range pend {
		select {
		case ch <- errResp:
		default:
		}
	}
}

// Instructions captured from initialize results (injected into system
// prompt). Keyed by server name. Guarded by instrMu.
var (
	Instructions = map[string]string{}
	instrMu      sync.RWMutex
)

// Call issues a raw JSON-RPC request and returns the result payload — used by
// extension callers (e.g. resources/directory/read).
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.call(ctx, method, params, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ReadResourceDir calls resources/directory/read (non-standard extension used
// by Claude Code's ReadMcpResourceDirTool).
func (c *Client) ReadResourceDir(ctx context.Context, uri string) (string, error) {
	out, err := c.Call(ctx, "resources/directory/read", map[string]any{"uri": uri})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ListResourcesJSON returns resources as raw JSON for structured listing.
func (c *Client) ListResourcesJSON(ctx context.Context) (json.RawMessage, error) {
	return c.Call(ctx, "resources/list", map[string]any{})
}

// MCP_TIMEOUT (ms) bounds requests; MCP_CONNECT_TIMEOUT_MS bounds handshakes.
func mcpTimeout() time.Duration {
	return mcpEnvMS("MCP_TIMEOUT", 30_000)
}
func mcpConnectTimeout() time.Duration {
	return mcpEnvMS("MCP_CONNECT_TIMEOUT_MS", 5_000)
}

// MCP_TOOL_TIMEOUT (ms) bounds tools/call — real's default is ~5min and a
// per-server `timeout` config field (>=1000ms, clamped to ~30min) overrides
// the env. cfgMs<=0 falls through to the env/default chain.
func mcpToolTimeout(cfgMs int) time.Duration {
	const minMS, maxMS = 1000, 30 * 60 * 1000
	if cfgMs > 0 {
		if cfgMs < minMS {
			cfgMs = minMS
		}
		if cfgMs > maxMS {
			cfgMs = maxMS
		}
		return time.Duration(cfgMs) * time.Millisecond
	}
	return mcpEnvMS("MCP_TOOL_TIMEOUT", 300_000)
}

func mcpEnvMS(name string, def int) time.Duration {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 2147483647 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return time.Duration(def) * time.Millisecond
}

// PendingProjectServers returns .mcp.json servers awaiting trust —
// approved (enabledMcpjsonServers) and disabled entries are excluded.
func PendingProjectServers(cwd string) []string {
	var out []string
	// --setting-sources without "project" suppresses .mcp.json entirely —
	// the same gate the server loader uses
	if !config.SettingSourceOK(cwd, filepath.Join(cwd, ".claude", "settings.json")) {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(cwd, ".mcp.json"))
	if err != nil {
		return nil
	}
	var top struct {
		Servers map[string]ServerConfig `json:"mcpServers"`
	}
	if json.Unmarshal(b, &top) != nil {
		return nil
	}
	st := config.Load(cwd)
	for n := range top.Servers {
		if st.EnableAllProjectMcpServers ||
			sliceHas(st.EnabledMcpjsonServers, n) ||
			sliceHas(st.DisabledMcpjsonServers, n) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// ApproveProjectServer writes the name into local settings'
// enabledMcpjsonServers — real's approve-path (mod_1441).
func ApproveProjectServer(cwd, name string) {
	p := filepath.Join(cwd, ".claude", "settings.local.json")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	b, _ := os.ReadFile(p)
	var m map[string]any
	json.Unmarshal(b, &m)
	if m == nil {
		m = map[string]any{}
	}
	var en []string
	if arr, ok := m["enabledMcpjsonServers"].([]any); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok {
				en = append(en, s)
			}
		}
	}
	en = append(en, name)
	m["enabledMcpjsonServers"] = en
	nb, _ := json.MarshalIndent(m, "", "  ")
	_ = os.WriteFile(p, nb, 0o644)
}
