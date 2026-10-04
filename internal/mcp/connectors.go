// claude.ai account connectors.
//
// Real CC auto-mounts the user's claude.ai connector directory as MCP
// servers of scope "claudeai" and type "claudeai-proxy"; their tools appear
// as mcp__claude_ai_<Connector>__<tool> (mod_0386.js, mod_1797.js,
// mod_0643.js).
//
// Discovery (SLn/yke in mod_0386.js):
//
//	GET {BASE_API_URL}/v1/mcp_servers?limit=1000&include_additional_installs=true
//	Authorization: Bearer <session OAuth access token>
//	anthropic-beta: mcp-servers-2025-12-04   (Azt.header, mod_0082.js)
//	anthropic-version: 2023-06-01
//
// Mount (mod_1797.js): each connector is a streamable-http MCP endpoint at
// {MCP_PROXY_URL}/v1/mcp/{server_id} (prod https://mcp-proxy.anthropic.com)
// with the *session* OAuth bearer injected per request (Li()). Per-connector
// upstream OAuth is brokered by claude.ai itself — needs-auth connectors
// point at {CLAUDE_AI_ORIGIN}/api/organizations/{org}/mcp/start-auth/{id}
// (Yye()), not at a local OAuth flow; mcpOAuthClientConfig is only for
// regular http/sse servers' dynamic client registration.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"claude-go/internal/api"
	"claude-go/internal/auth"
	"claude-go/internal/config"
)

// Prod endpoints — pn() in mod_0032.js. Package vars so tests (and exotic
// deployments) can point them at a harness.
var (
	ClaudeAIBaseAPIURL = "https://api.anthropic.com"       // BASE_API_URL
	ClaudeAIOrigin     = "https://claude.ai"               // CLAUDE_AI_ORIGIN
	ClaudeAIProxyURL   = "https://mcp-proxy.anthropic.com" // MCP_PROXY_URL
)

const (
	// claudeAIProxyPath is MCP_PROXY_PATH; {server_id} takes the raw API id.
	claudeAIProxyPath = "/v1/mcp/{server_id}"
	// claudeAIServersPath is the connector directory listing endpoint.
	claudeAIServersPath = "/v1/mcp_servers?limit=1000"
	// claudeAIServersBeta is Azt.header (mod_0082.js: w("mcp_servers", ...)).
	claudeAIServersBeta = "mcp-servers-2025-12-04"
	// claudeAIScope is the config scope real stamps on connector entries.
	claudeAIScope = "claudeai"
	// claudeAIRequiredScope gates the whole feature (SLn eligibility).
	claudeAIRequiredScope = "user:mcp_servers"
	// connectorDisplayPrefix: server keys are `claude.ai <display_name>`.
	connectorDisplayPrefix = "claude.ai "
	// reservedClaudeAIName seeds the display-name dedup set (wWe,
	// mod_0109.js) — the built-in memory server owns "claude_ai".
	reservedClaudeAIName = "claude_ai"
)

// SLn retry/budget constants (mod_0386.js): gst/hst/mLn/gLn/hLn.
const (
	connectorReqTimeout = 5 * time.Second
	connectorMaxTries   = 3
	connectorBackoff    = 500 * time.Millisecond
	connectorBackoffFac = 3
	connectorBudget     = 12 * time.Second
)

// connectorRecord is one entry of the /v1/mcp_servers `data` array.
type connectorRecord struct {
	ID                     string          `json:"id"`
	URL                    string          `json:"url"`
	DisplayName            string          `json:"display_name"`
	IconURL                string          `json:"icon_url"`
	CreatedAt              string          `json:"created_at"`
	AdditionalInstall      bool            `json:"additional_install"`
	Stateless              bool            `json:"stateless"`
	Eligible               *bool           `json:"eligible"`
	EligibilityReason      string          `json:"eligibility_reason"`
	EnterpriseManaged      bool            `json:"enterprise_managed"`
	CachedInitResponse     json.RawMessage `json:"cached_init_response"`
	DiscoverSupport        string          `json:"discover_support"`
	CachedDiscoverResponse json.RawMessage `json:"cached_discover_response"`
	Tools                  []struct {
		Name                   string          `json:"name"`
		EffectiveMaxPermission json.RawMessage `json:"effective_max_permission"`
	} `json:"tools"`
}

// session credential for claudeai-proxy transports — SetClaudeAICredentials
// wires the logged-in OAuth record; per-request bearer injection (Li()).
var (
	claudeAICredsMu sync.Mutex
	claudeAICreds   *auth.Credentials
)

// SetClaudeAICredentials installs the session OAuth credentials used to
// authenticate both discovery and every claudeai-proxy request.
func SetClaudeAICredentials(c *auth.Credentials) {
	claudeAICredsMu.Lock()
	claudeAICreds = c
	claudeAICredsMu.Unlock()
}

func claudeAICredentials() *auth.Credentials {
	claudeAICredsMu.Lock()
	defer claudeAICredsMu.Unlock()
	return claudeAICreds
}

// claudeAIBearer mirrors Li() (mod_1797.js): refresh, then return the
// session access token for the Authorization header.
func claudeAIBearer() string {
	c := claudeAICredentials()
	if c == nil {
		return ""
	}
	tok, err := c.Token() // Token() refreshes when expiring
	if err != nil || tok == "" {
		return c.AccessToken
	}
	return tok
}

// --- eligibility (SLn gate order) ---

// envDisabled mirrors _s() (chunk-kprv19eg): "0"/"false"/"no"/"off" mean off.
func envDisabled(name string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	switch v {
	case "0", "false", "no", "off":
		return true
	}
	return false
}

func hasScope(creds *auth.Credentials, scope string) bool {
	for _, s := range creds.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// connectorEligibility returns "" when connector discovery may run, else
// the telemetry state name real would report (tengu_claudeai_mcp_eligibility).
func connectorEligibility(creds *auth.Credentials, cwd string) string {
	if envDisabled("ENABLE_CLAUDEAI_MCP_SERVERS") {
		return "disabled_env_var"
	}
	if config.Load(cwd).DisableClaudeAiConnectors {
		return "disabled_setting"
	}
	// Jn(): connectors are first-party-only (mod_0386.js).
	if api.DetectProvider() != api.ProviderFirstParty {
		return "third_party_provider"
	}
	if creds == nil {
		return "no_oauth_token"
	}
	// ft(): an API-key auth source takes precedence over the claude.ai login.
	if creds.IsAPIKey || os.Getenv("ANTHROPIC_API_KEY") != "" {
		return "api_key_precedence"
	}
	if tok, err := creds.Token(); err != nil || tok == "" {
		if creds.AccessToken == "" {
			return "no_oauth_token"
		}
	}
	if !hasScope(creds, claudeAIRequiredScope) {
		return "missing_scope"
	}
	return ""
}

// --- discovery ---

// fetchState memoizes the connector listing for the session, like
// Kt().claudeAiConfigsFetch (yke()). Reset with ResetClaudeAICache (Uan()).
var (
	fetchMu      sync.Mutex
	fetchDone    bool
	fetchResult  map[string]ServerConfig
	fetchRecords []connectorRecord
)

// ResetClaudeAICache clears the memoized discovery result (Uan(),
// mod_0386.js) — mainly for tests.
func ResetClaudeAICache() {
	fetchMu.Lock()
	fetchDone, fetchResult, fetchRecords = false, nil, nil
	fetchMu.Unlock()
}

// FetchClaudeAIConfigs returns the user's claude.ai connectors as MCP server
// configs keyed by `claude.ai <display_name>` (SLn(), mod_0386.js). Returns
// nil (not an error) when the feature is ineligible or the fetch fails — a
// persisted cache (claudeAiConnectorCache in ~/.claude.json, a claude-go
// addition) is used as fallback so tools still mount after a transient
// failure.
func FetchClaudeAIConfigs(ctx context.Context, creds *auth.Credentials, cwd string) map[string]ServerConfig {
	fetchMu.Lock()
	if fetchDone {
		defer fetchMu.Unlock()
		return fetchResult
	}
	fetchMu.Unlock()

	if reason := connectorEligibility(creds, cwd); reason != "" {
		fetchMu.Lock()
		fetchDone, fetchResult = true, nil
		fetchMu.Unlock()
		return nil
	}
	recs, err := fetchConnectorRecords(ctx)
	if err != nil {
		recs = loadConnectorCache()
	}
	if len(recs) == 0 {
		fetchMu.Lock()
		fetchDone, fetchResult = true, nil
		fetchMu.Unlock()
		return nil
	}
	if err == nil {
		saveConnectorCache(recs)
	}
	out := connectorConfigs(recs)
	fetchMu.Lock()
	fetchDone, fetchResult, fetchRecords = true, out, recs
	fetchMu.Unlock()
	return out
}

// ConnectorRecords exposes the raw discovery payload (for diagnostics).
func ConnectorRecords() []connectorRecord {
	fetchMu.Lock()
	defer fetchMu.Unlock()
	return append([]connectorRecord(nil), fetchRecords...)
}

// fetchConnectorRecords performs the GET with SLn's retry/budget policy:
// ≤3 attempts, 5s per-request timeout, 500ms×3^i backoff, 12s total budget.
// On a non-401/429 failure the request is retried once without
// include_additional_installs (older backends reject the param).
func fetchConnectorRecords(ctx context.Context) ([]connectorRecord, error) {
	base := strings.TrimRight(ClaudeAIBaseAPIURL, "/") + claudeAIServersPath
	withInstalls := base + "&include_additional_installs=true"
	endpoint := withInstalls

	var lastErr error
	start := time.Now()
	for attempt := 1; attempt <= connectorMaxTries; attempt++ {
		recs, status, err := getConnectorList(ctx, endpoint)
		if err == nil {
			return recs, nil
		}
		lastErr = err
		// Pe() fallback: drop include_additional_installs once, only when
		// the failure carried an HTTP status that isn't 401/429 — network
		// errors (status 0) retry the same URL.
		if endpoint != base && status != 0 && status != 401 && status != 429 {
			endpoint = base
		}
		if attempt == connectorMaxTries {
			break
		}
		backoff := connectorBackoff
		for i := 1; i < attempt; i++ {
			backoff *= connectorBackoffFac
		}
		if time.Since(start)+backoff+connectorReqTimeout >= connectorBudget {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
	return nil, lastErr
}

// getConnectorList issues one discovery GET; returns (records, httpStatus, err).
func getConnectorList(ctx context.Context, endpoint string) ([]connectorRecord, int, error) {
	ctx, cancel := context.WithTimeout(ctx, connectorReqTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "claude-cli/"+api.Version+" (external, cli)")
	if tok := claudeAIBearer(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", claudeAIServersBeta)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode,
			fmt.Errorf("mcp_servers: http %d", resp.StatusCode)
	}
	var body struct {
		Data []connectorRecord `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, resp.StatusCode, err
	}
	return body.Data, resp.StatusCode, nil
}

// connectorConfigs transforms the listing into ServerConfigs — the tail of
// SLn(): primaries deduped by normalized URL (first wins), additional
// installs sorted by created_at then id and appended last, display-name
// collisions get " (N)" suffixes on normalized-name comparison.
func connectorConfigs(recs []connectorRecord) map[string]ServerConfig {
	var primaries, additions []connectorRecord
	seenURL := map[string]string{}
	for _, r := range recs {
		if r.AdditionalInstall {
			additions = append(additions, r)
			continue
		}
		nu := normalizeConnURL(r.URL)
		if keep, dup := seenURL[nu]; dup {
			_ = keep // first wins; later duplicate upstream ids dropped
			continue
		}
		seenURL[nu] = r.ID
		primaries = append(primaries, r)
	}
	sort.SliceStable(additions, func(i, j int) bool {
		a, b := additions[i], additions[j]
		at, bt := parseTime(a.CreatedAt), parseTime(b.CreatedAt)
		if !at.Equal(bt) {
			return at.Before(bt)
		}
		return a.ID < b.ID
	})

	out := map[string]ServerConfig{}
	seenNames := map[string]bool{reservedClaudeAIName: true}
	for _, r := range append(primaries, additions...) {
		name := connectorDisplayPrefix + r.DisplayName
		cand, n := name, 1
		for seenNames[NormalizeServerName(cand)] {
			n++
			cand = fmt.Sprintf("%s (%d)", name, n)
		}
		seenNames[NormalizeServerName(cand)] = true
		out[cand] = ServerConfig{
			Type:                   "claudeai-proxy",
			URL:                    r.URL,
			ID:                     r.ID,
			DisplayName:            r.DisplayName,
			IconURL:                r.IconURL,
			Scope:                  claudeAIScope,
			ToolPermissions:        connectorToolPermissions(r),
			Stateless:              r.Stateless,
			CachedInitResponse:     r.CachedInitResponse,
			DiscoverSupport:        r.DiscoverSupport,
			CachedDiscoverResponse: r.CachedDiscoverResponse,
			Eligible:               r.Eligible,
			IneligibleReason:       r.EligibilityReason,
			EnterpriseManaged:      r.EnterpriseManaged,
		}
	}
	return out
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// normalizeConnURL is kLn(): canonicalize then strip trailing slashes.
func normalizeConnURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return strings.TrimRight(u.String(), "/")
}

// connectorToolPermissions is _Ln(): name → effective_max_permission as
// compacted JSON (real parses it through a schema; "blocked" on the wire is
// kept verbatim).
func connectorToolPermissions(r connectorRecord) map[string]string {
	var out map[string]string
	for _, t := range r.Tools {
		if len(t.EffectiveMaxPermission) == 0 {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[t.Name] = strings.TrimSpace(string(t.EffectiveMaxPermission))
	}
	return out
}

// --- connector cache in ~/.claude.json (claude-go addition; real keeps the
// fetch in-memory only). Key mirrors what fetch returns. ---

func loadConnectorCache() []connectorRecord {
	cfg, _ := config.ReadClaudeJSON()
	raw, ok := cfg["claudeAiConnectorCache"]
	if !ok {
		return nil
	}
	var recs []connectorRecord
	b, err := json.Marshal(raw)
	if err != nil || json.Unmarshal(b, &recs) != nil {
		return nil
	}
	return recs
}

func saveConnectorCache(recs []connectorRecord) {
	cfg, p := config.ReadClaudeJSON()
	cfg["claudeAiConnectorCache"] = recs
	config.WriteClaudeJSON(p, cfg)
}

// --- ever-connected tracking (cdt/nDt/rDt/oDt, mod_0386.js) ---

var (
	sessionMu          sync.Mutex
	connectedThisSessn = map[string]bool{}
)

// NoteClaudeAIConnected is cdt(): record name in the session set and append
// it to claudeAiMcpEverConnected in ~/.claude.json.
func NoteClaudeAIConnected(name string) {
	sessionMu.Lock()
	connectedThisSessn[name] = true
	sessionMu.Unlock()

	cfg, p := config.ReadClaudeJSON()
	var list []string
	if raw, ok := cfg["claudeAiMcpEverConnected"]; ok {
		if b, err := json.Marshal(raw); err == nil {
			json.Unmarshal(b, &list)
		}
	}
	for _, v := range list {
		if v == name {
			return
		}
	}
	cfg["claudeAiMcpEverConnected"] = append(list, name)
	config.WriteClaudeJSON(p, cfg)
}

// ClaudeAIConnectedThisSession is nDt().
func ClaudeAIConnectedThisSession(name string) bool {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	return connectedThisSessn[name]
}

// ClaudeAIEverConnected is rDt(): name ∈ claudeAiMcpEverConnected.
func ClaudeAIEverConnected(name string) bool {
	cfg, _ := config.ReadClaudeJSON()
	raw, ok := cfg["claudeAiMcpEverConnected"]
	if !ok {
		return false
	}
	var list []string
	if b, err := json.Marshal(raw); err == nil {
		json.Unmarshal(b, &list)
	}
	for _, v := range list {
		if v == name {
			return true
		}
	}
	return false
}

// ConnectorAuthURL is Yye(): the claude.ai start-auth link for a connector
// that needs upstream OAuth. "" when no org id is available.
func ConnectorAuthURL(cfg ServerConfig) string {
	org := config.OrganizationUUID()
	if org == "" || cfg.ID == "" {
		return ""
	}
	surface := os.Getenv("CLAUDE_CODE_ENTRYPOINT")
	if surface == "" {
		surface = "cli"
	}
	return fmt.Sprintf("%s/api/organizations/%s/mcp/start-auth/%s?product_surface=%s",
		strings.TrimRight(ClaudeAIOrigin, "/"), org,
		connectorServerID(cfg.ID), url.QueryEscape(surface))
}

// connectorServerID is H6n(): normalize legacy `mcprs…` ids to `mcpsrv…`.
func connectorServerID(id string) string {
	if strings.HasPrefix(id, "mcprs") {
		return "mcpsrv" + id[len("mcprs"):]
	}
	return id
}

// ConnectorsURL is Kce(): where users manage connectors on claude.ai.
func ConnectorsURL() string {
	return strings.TrimRight(ClaudeAIOrigin, "/") + "/customize/connectors"
}

// --- mount ---

// connectClaudeAIProxy builds the claudeai-proxy transport (mod_1797.js):
// streamable-http POSTs to {MCP_PROXY_URL}/v1/mcp/{server_id} with the
// session OAuth bearer injected per request; GET stream suppressed (the
// proxy answers it locally with 405).
func connectClaudeAIProxy(name string, cfg ServerConfig) (*Client, error) {
	if cfg.ID == "" {
		return nil, fmt.Errorf("mcp %s: claudeai-proxy requires an id", name)
	}
	u := strings.TrimRight(ClaudeAIProxyURL, "/") +
		strings.ReplaceAll(claudeAIProxyPath, "{server_id}", cfg.ID)
	c := &Client{Name: name, pending: map[string]chan rpcResp{}}
	t := &httpTransport{
		url:         u,
		client:      &http.Client{},
		c:           c,
		bearerFn:    claudeAIBearer,
		noGetStream: true,
		authURL:     ConnectorAuthURL(cfg),
		done:        make(chan struct{}),
	}
	c.tr = t
	return c, nil
}

// --- naming (vn(), chunk-*/mod_0047.js) ---

// NormalizeServerName maps an MCP server config name to its tool-prefix
// segment — `mcp__${NormalizeServerName(name)}__<tool>`. Non
// [a-zA-Z0-9_-] bytes become '_'; for `claude.ai …` names underscores are
// additionally collapsed and trimmed, so "claude.ai Claude Docs" →
// "claude_ai_Claude_Docs".
func NormalizeServerName(s string) string {
	t := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-':
			return r
		}
		return '_'
	}, s)
	if strings.HasPrefix(s, connectorDisplayPrefix) {
		t = collapseUnderscores(t)
		t = strings.Trim(t, "_")
	}
	return t
}

func collapseUnderscores(s string) string {
	var b strings.Builder
	prev := false
	for _, r := range s {
		if r == '_' {
			if prev {
				continue
			}
			prev = true
		} else {
			prev = false
		}
		b.WriteRune(r)
	}
	return b.String()
}
