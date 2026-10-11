package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gbasin/claude-go/internal/auth"
)

func connTestCreds() *auth.Credentials {
	return &auth.Credentials{
		AccessToken: "sess-tok",
		Scopes:      []string{"user:inference", "user:mcp_servers"},
	}
}

// resetConnectorEndpoints points the (package-var) endpoints at test
// servers and restores them afterwards.
func resetConnectorEndpoints(t *testing.T, baseAPI, proxy string) {
	t.Helper()
	oldB, oldP, oldO := ClaudeAIBaseAPIURL, ClaudeAIProxyURL, ClaudeAIOrigin
	ClaudeAIBaseAPIURL, ClaudeAIProxyURL = baseAPI, proxy
	t.Cleanup(func() {
		ClaudeAIBaseAPIURL, ClaudeAIProxyURL, ClaudeAIOrigin = oldB, oldP, oldO
		ResetClaudeAICache()
		SetClaudeAICredentials(nil)
	})
	ResetClaudeAICache()
}

func TestNormalizeServerName(t *testing.T) {
	cases := map[string]string{
		"claude.ai Claude Docs":     "claude_ai_Claude_Docs",
		"claude.ai Gmail":           "claude_ai_Gmail",
		"claude.ai Slack beta":      "claude_ai_Slack_beta",
		"claude.ai Intuit TurboTax": "claude_ai_Intuit_TurboTax",
		"claude.ai  Weird  Name ":   "claude_ai_Weird_Name",
		"my-server":                 "my-server",
		"server.with.dots":          "server_with_dots",
		"no collapse outside":       "no_collapse_outside",
	}
	for in, want := range cases {
		if got := NormalizeServerName(in); got != want {
			t.Errorf("NormalizeServerName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConnectorEligibilityGates(t *testing.T) {
	if got := connectorEligibility(connTestCreds(), t.TempDir()); got != "" {
		t.Fatalf("eligible creds gated: %q", got)
	}
	if got := connectorEligibility(&auth.Credentials{AccessToken: "k", IsAPIKey: true,
		Scopes: []string{"user:mcp_servers"}}, t.TempDir()); got != "api_key_precedence" {
		t.Fatalf("api key gate: %q", got)
	}
	if got := connectorEligibility(&auth.Credentials{AccessToken: "k"},
		t.TempDir()); got != "missing_scope" {
		t.Fatalf("scope gate: %q", got)
	}
	if got := connectorEligibility(nil, t.TempDir()); got != "no_oauth_token" {
		t.Fatalf("nil creds gate: %q", got)
	}
	t.Setenv("ENABLE_CLAUDEAI_MCP_SERVERS", "false")
	if got := connectorEligibility(connTestCreds(), t.TempDir()); got != "disabled_env_var" {
		t.Fatalf("env gate: %q", got)
	}
}

func TestFetchClaudeAIConfigsParsesAndNames(t *testing.T) {
	var sawAuth, sawBeta, sawVersion, sawPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth.Store(r.Header.Get("Authorization"))
		sawBeta.Store(r.Header.Get("anthropic-beta"))
		sawVersion.Store(r.Header.Get("anthropic-version"))
		sawPath.Store(r.URL.String())
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{
					"id": "mcpsrv_1", "url": "https://mcp.example.com/a/",
					"display_name": "Claude Docs", "icon_url": "https://x/i.png",
					"stateless": true, "enterprise_managed": false,
					"tools": []map[string]any{
						{"name": "query", "effective_max_permission": "allow"},
						{"name": "batch"},
					},
				},
				{
					// duplicate upstream URL (no trailing slash) — dropped
					"id": "mcpsrv_dup", "url": "https://mcp.example.com/a",
					"display_name": "Dup",
				},
				{
					"id": "mcpsrv_2", "url": "https://mcp.example.com/b",
					"display_name": "Claude Docs", // display-name collision
				},
				{
					"id": "mcpsrv_3", "url": "https://mcp.example.com/c",
					"display_name": "Old", "additional_install": true,
					"created_at": "2024-01-01T00:00:00Z",
				},
			},
		})
	}))
	defer srv.Close()
	resetConnectorEndpoints(t, srv.URL, "http://127.0.0.1:0")
	SetClaudeAICredentials(connTestCreds())

	cfgs := FetchClaudeAIConfigs(context.Background(), connTestCreds(), t.TempDir())
	if len(cfgs) != 3 {
		t.Fatalf("got %d configs, want 3: %v", len(cfgs), cfgs)
	}
	c1, ok := cfgs["claude.ai Claude Docs"]
	if !ok {
		t.Fatalf("missing primary key; got %v", cfgs)
	}
	if c1.Type != "claudeai-proxy" || c1.Scope != "claudeai" || c1.ID != "mcpsrv_1" ||
		c1.DisplayName != "Claude Docs" || c1.IconURL != "https://x/i.png" || !c1.Stateless {
		t.Fatalf("bad config shape: %+v", c1)
	}
	if c1.ToolPermissions["query"] != `"allow"` {
		t.Fatalf("toolPermissions: %v", c1.ToolPermissions)
	}
	if _, ok := cfgs["claude.ai Claude Docs (2)"]; !ok {
		t.Fatalf("collision suffix missing; got %v", cfgs)
	}
	if _, ok := cfgs["claude.ai Old"]; !ok {
		t.Fatalf("additional install missing; got %v", cfgs)
	}
	if sawAuth.Load() != "Bearer sess-tok" {
		t.Fatalf("auth header: %v", sawAuth.Load())
	}
	if sawBeta.Load() != "mcp-servers-2025-12-04" || sawVersion.Load() != "2023-06-01" {
		t.Fatalf("api headers: %v %v", sawBeta.Load(), sawVersion.Load())
	}
	if !strings.Contains(sawPath.Load().(string), "/v1/mcp_servers?limit=1000") {
		t.Fatalf("discovery path: %v", sawPath.Load())
	}
}

func TestFetchClaudeAIConfigsIneligibleReturnsNil(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(500)
	}))
	defer srv.Close()
	resetConnectorEndpoints(t, srv.URL, "http://127.0.0.1:0")

	// API-key creds → eligibility gate stops before any HTTP call.
	cfgs := FetchClaudeAIConfigs(context.Background(),
		&auth.Credentials{AccessToken: "k", IsAPIKey: true}, t.TempDir())
	if cfgs != nil || called {
		t.Fatalf("expected nil without request; got %v called=%v", cfgs, called)
	}
}

func TestConnectorCacheFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// discovery endpoint always fails
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	resetConnectorEndpoints(t, srv.URL, "http://127.0.0.1:0")
	SetClaudeAICredentials(connTestCreds())

	// seed the cache the way a prior successful fetch would have
	cfg, p := claudeJSONForTest(t)
	cfg["claudeAiConnectorCache"] = []map[string]any{{
		"id": "mcpsrv_cached", "url": "https://mcp.example.com/x",
		"display_name": "Cached Conn",
	}}
	writeClaudeJSONForTest(t, p, cfg)

	cfgs := FetchClaudeAIConfigs(context.Background(), connTestCreds(), t.TempDir())
	if _, ok := cfgs["claude.ai Cached Conn"]; !ok {
		t.Fatalf("cache fallback missing; got %v", cfgs)
	}
}

func TestConnectorMountThroughProxy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	var gotPath, gotAuth atomic.Value
	gotGet := atomic.Bool{}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			gotGet.Store(true) // GET stream must be suppressed
			w.WriteHeader(405)
			return
		}
		gotPath.Store(r.URL.Path)
		gotAuth.Store(r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write(rpcHandler(body))
	}))
	defer proxy.Close()
	resetConnectorEndpoints(t, "http://127.0.0.1:0", proxy.URL)
	SetClaudeAICredentials(connTestCreds())

	cfg := ServerConfig{Type: "claudeai-proxy", ID: "mcpsrv_42",
		DisplayName: "Gmail", Scope: "claudeai"}
	c, err := Connect("claude.ai Gmail", cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	if gotPath.Load() != "/v1/mcp/mcpsrv_42" {
		t.Fatalf("proxy path: %v", gotPath.Load())
	}
	if gotAuth.Load() != "Bearer sess-tok" {
		t.Fatalf("bearer: %v", gotAuth.Load())
	}
	if len(c.Tools) != 1 || c.Tools[0].Name != "t1" {
		t.Fatalf("tools: %+v", c.Tools)
	}
	if gotGet.Load() {
		t.Fatal("GET stream was opened for claudeai-proxy")
	}
	// ever-connected write-back (cdt)
	if !ClaudeAIConnectedThisSession("claude.ai Gmail") {
		t.Fatal("session set not updated")
	}
	m, _ := readClaudeJSONForTest(t)
	var ever []string
	if raw, ok := m["claudeAiMcpEverConnected"]; ok {
		b, _ := json.Marshal(raw)
		json.Unmarshal(b, &ever)
	}
	if len(ever) != 1 || ever[0] != "claude.ai Gmail" {
		t.Fatalf("claudeAiMcpEverConnected: %v", ever)
	}
	// idempotent
	NoteClaudeAIConnected("claude.ai Gmail")
	m, _ = readClaudeJSONForTest(t)
	ever = ever[:0]
	b, _ := json.Marshal(m["claudeAiMcpEverConnected"])
	json.Unmarshal(b, &ever)
	if len(ever) != 1 {
		t.Fatalf("dup written: %v", ever)
	}
}

func TestConnectorNeedsAuthURL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeJSONForTest(t, filepath.Join(home, ".claude.json"), map[string]any{
		"oauthAccount": map[string]any{"organizationUuid": "org-9"},
	})
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer proxy.Close()
	resetConnectorEndpoints(t, "http://127.0.0.1:0", proxy.URL)
	SetClaudeAICredentials(connTestCreds())

	cfg := ServerConfig{Type: "claudeai-proxy", ID: "mcprs_legacy7",
		DisplayName: "Figma", Scope: "claudeai"}
	_, err := Connect("claude.ai Figma", cfg)
	var ae *AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("want AuthError, got %v", err)
	}
	want := ClaudeAIOrigin + "/api/organizations/org-9/mcp/start-auth/mcpsrv_legacy7?product_surface=cli"
	if ae.AuthURL != want {
		t.Fatalf("authURL %q, want %q", ae.AuthURL, want)
	}
	st, _ := Status("claude.ai Figma")
	if st.Status != "needs-auth" {
		t.Fatalf("status %q", st.Status)
	}
	if ClaudeAIConnectedThisSession("claude.ai Figma") {
		t.Fatal("ever-connected written despite auth failure")
	}
}

// --- .claude.json helpers (HOME redirected by the test) ---

func claudeJSONForTest(t *testing.T) (map[string]any, string) {
	t.Helper()
	m, p := readClaudeJSONForTest(t)
	return m, p
}

func readClaudeJSONForTest(t *testing.T) (map[string]any, string) {
	t.Helper()
	p := filepath.Join(os.Getenv("HOME"), ".claude.json")
	m := map[string]any{}
	if b, err := os.ReadFile(p); err == nil {
		json.Unmarshal(b, &m)
	}
	return m, p
}

func writeClaudeJSONForTest(t *testing.T, p string, m map[string]any) {
	t.Helper()
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
