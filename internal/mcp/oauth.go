package mcp

// OAuth support for remote MCP servers.
//
// Full interactive authorization (dynamic client registration +
// authorization-code + PKCE + local redirect listener) is implemented in
// RunOAuthFlow. Transports automatically consume stored tokens and refresh
// expired ones; when a server answers 401 the connection is recorded as
// "needs-auth" (see ServerStatus) for `mcp` status display.
//
// Tokens persist in ~/.claude/.credentials-mcp.json.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/gbasin/claude-go/internal/config"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gbasin/claude-go/internal/auth"
)

// AuthError is returned when a server answers 401/403 and no usable token
// is available. WWWAuthenticate carries the response header verbatim.
type AuthError struct {
	Server          string
	URL             string
	WWWAuthenticate string
	AuthURL         string // discovered authorization endpoint, if any
}

func (e *AuthError) Error() string {
	if e.AuthURL != "" {
		return fmt.Sprintf("mcp %s: authorization required (401) — authorize at %s", e.Server, e.AuthURL)
	}
	return fmt.Sprintf("mcp %s: authorization required (401)", e.Server)
}

// serverAuth is the persisted per-server OAuth record — real's
// mcpOAuth[<name>|<hash>] entry shape (camelCase, discoveryState carries
// the resolved endpoints, stepUpScope survives a scope-elevation login).
type serverAuth struct {
	ServerName   string `json:"serverName,omitempty"`
	ServerURL    string `json:"serverUrl,omitempty"`
	AccessToken  string `json:"accessToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
	ExpiresAt    int64  `json:"expiresAt,omitempty"` // unix ms
	TokenType    string `json:"tokenType,omitempty"`
	Scope        string `json:"scope,omitempty"`

	// dynamic client registration results (or --client-id/--client-secret)
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
	RedirectURI  string `json:"redirectUri,omitempty"`

	DiscoveryState *discoveryState `json:"discoveryState,omitempty"`
	StepUpScope    string          `json:"stepUpScope,omitempty"`
}

// discoveryState caches the resolved authorization-server metadata inside
// the credential record — real's discoveryState field.
type discoveryState struct {
	AuthorizationServerURL string `json:"authorizationServerUrl,omitempty"`
	ResourceMetadataURL    string `json:"resourceMetadataUrl,omitempty"`
	Issuer                 string `json:"issuer,omitempty"`
	TokenEndpoint          string `json:"tokenEndpoint,omitempty"`
	RegistrationEndpoint   string `json:"registrationEndpoint,omitempty"`
	AuthorizationEndpoint  string `json:"authorizationEndpoint,omitempty"`
	RevocationEndpoint     string `json:"revocationEndpoint,omitempty"`
}

func (s *serverAuth) tokenEndpoint() string {
	if s != nil && s.DiscoveryState != nil {
		return s.DiscoveryState.TokenEndpoint
	}
	return ""
}

// accessToken returns the bearer token if present and not expired.
func (s *serverAuth) accessToken() string {
	if s == nil || s.AccessToken == "" {
		return ""
	}
	if s.ExpiresAt > 0 && time.Now().UnixMilli() > s.ExpiresAt-60_000 {
		return ""
	}
	return s.AccessToken
}

// refresh trades the refresh token for a new access token at the stored
// token endpoint.
func (s *serverAuth) refresh(ctx context.Context) error {
	if s == nil || s.RefreshToken == "" || s.tokenEndpoint() == "" {
		return fmt.Errorf("no refresh token")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {s.RefreshToken},
		"client_id":     {s.ClientID},
	}
	if s.ClientSecret != "" {
		form.Set("client_secret", s.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", s.tokenEndpoint(),
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
	}
	if json.NewDecoder(resp.Body).Decode(&tr) != nil || tr.AccessToken == "" {
		return fmt.Errorf("token refresh failed (status %d)", resp.StatusCode)
	}
	s.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		s.RefreshToken = tr.RefreshToken
	}
	s.TokenType = tr.TokenType
	s.Scope = tr.Scope
	if tr.ExpiresIn > 0 {
		s.ExpiresAt = time.Now().UnixMilli() + tr.ExpiresIn*1000
	}
	return nil
}

// --- credential store ---
//
// Real keeps MCP OAuth tokens inside the shared credential store (the
// storageV5 blob that also holds claudeAiOauth — keychain on macOS,
// ~/.claude/.credentials.json elsewhere), under
// mcpOAuth["<name>|<sha256({type,url,headers})[:16]>"] (Hl() in the
// decompile). We use the plaintext file arm of that store so real and
// claude-go interoperate on the same layout; the legacy
// .credentials-mcp.json is still read as a fallback so existing logins
// keep working (and are migrated on the next save).

func credentialsPath() string {
	return filepath.Join(config.ClaudeDir(), ".credentials.json")
}

func legacyCredentialsPath() string {
	return filepath.Join(config.ClaudeDir(), ".credentials-mcp.json")
}

var credMu sync.Mutex

// readCredStore returns the parsed credentials file.
func readCredStore() map[string]any {
	m := map[string]any{}
	if b, err := os.ReadFile(credentialsPath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func writeCredStore(m map[string]any) error {
	p := credentialsPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func credMap(m map[string]any) map[string]*serverAuth {
	out := map[string]*serverAuth{}
	raw, ok := m["mcpOAuth"]
	if !ok {
		return out
	}
	b, _ := json.Marshal(raw)
	var mm map[string]*serverAuth
	if json.Unmarshal(b, &mm) == nil {
		for k, v := range mm {
			if v != nil {
				out[k] = v
			}
		}
	}
	return out
}

// loadCredFile merges the credential store's mcpOAuth map with the legacy
// standalone file (keyed by bare server name — a migration fallback).
func loadCredFile() map[string]*serverAuth {
	out := credMap(readCredStore())
	if b, err := os.ReadFile(legacyCredentialsPath()); err == nil {
		var top map[string]json.RawMessage
		if json.Unmarshal(b, &top) == nil {
			var m map[string]*serverAuth
			if raw, ok := top["mcpOAuth"]; ok && json.Unmarshal(raw, &m) == nil {
				for k, v := range m {
					if v != nil {
						out[k] = v
					}
				}
			} else if json.Unmarshal(b, &m) == nil {
				for k, v := range m {
					if v != nil {
						out[k] = v
					}
				}
			}
		}
	}
	return out
}

func loadServerAuth(name string, cfg ServerConfig) *serverAuth {
	credMu.Lock()
	defer credMu.Unlock()
	m := loadCredFile()
	if s := m[cfg.credKey(name)]; s != nil {
		return s
	}
	// bare-name fallbacks: unmigrated legacy entries, and same-name
	// records whose config hash drifted (headers/url edited)
	var legacy *serverAuth
	for k, v := range m {
		if k == name {
			return v
		}
		if strings.HasPrefix(k, name+"|") {
			legacy = v
		}
	}
	return legacy
}

// SaveServerAuth upserts one server's OAuth record under its Hl() key.
func SaveServerAuth(name string, cfg ServerConfig, s *serverAuth) error {
	credMu.Lock()
	defer credMu.Unlock()
	s.ServerName, s.ServerURL = name, cfg.URL
	store := readCredStore()
	om := credMap(store)
	key := cfg.credKey(name)
	om[key] = s
	// drop the bare-name legacy entry once the keyed record exists
	if _, ok := om[name]; ok && name != key {
		delete(om, name)
	}
	raw, _ := json.Marshal(om)
	var anyMap map[string]any
	_ = json.Unmarshal(raw, &anyMap)
	store["mcpOAuth"] = anyMap
	return writeCredStore(store)
}

// DeleteServerAuth removes one server's stored tokens (and client
// registration record).
func DeleteServerAuth(name string, cfg ServerConfig) error {
	credMu.Lock()
	defer credMu.Unlock()
	store := readCredStore()
	om := credMap(store)
	delete(om, cfg.credKey(name))
	delete(om, name) // legacy bare-name key
	raw, _ := json.Marshal(om)
	var anyMap map[string]any
	_ = json.Unmarshal(raw, &anyMap)
	store["mcpOAuth"] = anyMap
	return writeCredStore(store)
}

// SaveClientSecret stores a --client-secret under real's
// mcpOAuthClientConfig[Hl(name,cfg)] entry in the credential store.
func SaveClientSecret(name string, cfg ServerConfig, secret string) error {
	credMu.Lock()
	defer credMu.Unlock()
	store := readCredStore()
	cc, _ := store["mcpOAuthClientConfig"].(map[string]any)
	if cc == nil {
		cc = map[string]any{}
	}
	cc[cfg.credKey(name)] = map[string]any{"clientSecret": secret}
	store["mcpOAuthClientConfig"] = cc
	return writeCredStore(store)
}

// loadClientSecret reads mcpOAuthClientConfig[Hl] — where real puts the
// --client-secret (never in the plaintext server config).
func loadClientSecret(name string, cfg ServerConfig) string {
	credMu.Lock()
	defer credMu.Unlock()
	cc, _ := readCredStore()["mcpOAuthClientConfig"].(map[string]any)
	if e, ok := cc[cfg.credKey(name)].(map[string]any); ok {
		s, _ := e["clientSecret"].(string)
		return s
	}
	return ""
}

// RevokeServerAuth performs RFC 7009 revocation of the stored tokens at
// the server's revocation_endpoint (real's mcp_oauth_revoke on logout),
// then deletes the record. Revocation failure is reported but the local
// record is still removed — logout means "forget it" either way.
func RevokeServerAuth(ctx context.Context, name string, cfg ServerConfig) error {
	s := loadServerAuth(name, cfg)
	var revokeErr error
	if s != nil && (s.AccessToken != "" || s.RefreshToken != "") {
		revokeErr = revokeTokens(ctx, name, cfg, s)
	}
	if err := DeleteServerAuth(name, cfg); err != nil {
		return err
	}
	return revokeErr
}

// revokeTokens POSTs each held token to the revocation endpoint with the
// registered client credentials (Basic or post body per server support).
func revokeTokens(ctx context.Context, name string, cfg ServerConfig, s *serverAuth) error {
	ep := ""
	if s.DiscoveryState != nil && s.DiscoveryState.RevocationEndpoint != "" {
		ep = s.DiscoveryState.RevocationEndpoint
	} else if md, err := DiscoverOAuthMetadata(ctx, cfg.URL); err == nil {
		ep = md.RevocationEndpoint
	}
	if ep == "" {
		return nil // server doesn't advertise revocation
	}
	post := func(token, hint string) error {
		form := url.Values{"token": {token}, "token_type_hint": {hint}}
		req, err := http.NewRequestWithContext(ctx, "POST", ep, strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if s.ClientID != "" && s.ClientSecret != "" {
			req.SetBasicAuth(url.QueryEscape(s.ClientID), url.QueryEscape(s.ClientSecret))
		} else if s.ClientID != "" {
			form.Set("client_id", s.ClientID)
			req, _ = http.NewRequestWithContext(ctx, "POST", ep, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		// 401 → retry once with the access token as Bearer (real's Ie())
		if resp.StatusCode == http.StatusUnauthorized && s.AccessToken != "" {
			req, _ = http.NewRequestWithContext(ctx, "POST", ep,
				strings.NewReader(url.Values{"token": {token}, "token_type_hint": {hint}}.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Authorization", "Bearer "+s.AccessToken)
			r2, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			r2.Body.Close()
			if r2.StatusCode >= 400 {
				return fmt.Errorf("revoke %s: http %d", hint, r2.StatusCode)
			}
			return nil
		}
		if resp.StatusCode >= 400 {
			return fmt.Errorf("revoke %s: http %d", hint, resp.StatusCode)
		}
		return nil
	}
	var firstErr error
	if s.RefreshToken != "" {
		firstErr = post(s.RefreshToken, "refresh_token")
	}
	if s.AccessToken != "" {
		if err := post(s.AccessToken, "access_token"); firstErr == nil {
			firstErr = err
		}
	}
	_ = name
	return firstErr
}

// --- discovery ---

// OAuthMetadata is the subset of RFC 8414 authorization-server metadata we
// need.
type OAuthMetadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
	RevocationEndpoint    string `json:"revocation_endpoint"`
}

func getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: http %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// DiscoverOAuthMetadata resolves the authorization-server metadata for a
// remote MCP server per RFC 9728 (protected-resource metadata) with an
// RFC 8414 fallback on the server origin.
func DiscoverOAuthMetadata(ctx context.Context, serverURL string) (*OAuthMetadata, error) {
	origin := originOf(serverURL)
	if origin == "" {
		return nil, fmt.Errorf("bad server url %q", serverURL)
	}
	issuers := []string{origin}
	// RFC 9728: protected resource metadata may point elsewhere
	var prm struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := getJSON(ctx, origin+"/.well-known/oauth-protected-resource", &prm); err == nil {
		issuers = append(prm.AuthorizationServers, issuers...)
	}
	var lastErr error
	for _, iss := range issuers {
		iss = strings.TrimRight(iss, "/")
		for _, wk := range []string{
			"/.well-known/oauth-authorization-server",
			"/.well-known/openid-configuration",
		} {
			var md OAuthMetadata
			if err := getJSON(ctx, iss+wk, &md); err != nil {
				lastErr = err
				continue
			}
			if md.AuthorizationEndpoint != "" && md.TokenEndpoint != "" {
				return &md, nil
			}
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no OAuth authorization server found for %s", serverURL)
}

// DiscoverAuthURL returns the authorization endpoint for a server, using
// the WWW-Authenticate resource_metadata hint when present. Used to point
// the user at a login page on auth failure.
func DiscoverAuthURL(ctx context.Context, serverURL, wwwAuthenticate string) (string, error) {
	if rm := wwwAuthParam(wwwAuthenticate, "resource_metadata"); rm != "" {
		var prm struct {
			AuthorizationServers []string `json:"authorization_servers"`
		}
		if err := getJSON(ctx, rm, &prm); err == nil && len(prm.AuthorizationServers) > 0 {
			iss := strings.TrimRight(prm.AuthorizationServers[0], "/")
			var md OAuthMetadata
			if err := getJSON(ctx, iss+"/.well-known/oauth-authorization-server", &md); err == nil && md.AuthorizationEndpoint != "" {
				return md.AuthorizationEndpoint, nil
			}
		}
	}
	md, err := DiscoverOAuthMetadata(ctx, serverURL)
	if err != nil {
		return "", err
	}
	return md.AuthorizationEndpoint, nil
}

// wwwAuthParam extracts a quoted param from a WWW-Authenticate header.
func wwwAuthParam(hdr, key string) string {
	for _, part := range strings.Split(hdr, ",") {
		part = strings.TrimSpace(part)
		k, v, ok := strings.Cut(part, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), key) {
			continue
		}
		return strings.Trim(strings.TrimSpace(v), `"`)
	}
	return ""
}

// --- dynamic client registration (RFC 7591) ---

func registerClient(ctx context.Context, md *OAuthMetadata, redirectURI string) (clientID, clientSecret string, err error) {
	if md.RegistrationEndpoint == "" {
		return "", "", fmt.Errorf("authorization server has no registration endpoint")
	}
	body, _ := json.Marshal(map[string]any{
		"client_name":                "claude-go",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	req, err := http.NewRequestWithContext(ctx, "POST", md.RegistrationEndpoint, strings.NewReader(string(body)))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var rr struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if json.NewDecoder(resp.Body).Decode(&rr) != nil || rr.ClientID == "" {
		return "", "", fmt.Errorf("client registration failed (status %d)", resp.StatusCode)
	}
	return rr.ClientID, rr.ClientSecret, nil
}

// --- interactive flow ---

func pkcePair() (verifier, challenge string) {
	b := make([]byte, 64)
	rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}

func randState() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// RunOAuthFlow performs the full interactive MCP OAuth flow:
// discovery → dynamic client registration → open browser → local callback
// → token exchange → persist to the credential store's mcpOAuth map.
// cfg.OAuth supplies the real mcp-add pins: --client-id/--client-secret
// skip dynamic registration, --callback-port fixes the loopback port.
// It blocks until the user completes authorization or ctx is cancelled.
func RunOAuthFlow(ctx context.Context, serverName string, cfg ServerConfig) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	md, err := discoverMetadata(ctx, serverName, cfg)
	if err != nil {
		return fmt.Errorf("mcp %s oauth discovery: %w", serverName, err)
	}

	listen := "127.0.0.1:0"
	if cfg.OAuth != nil && cfg.OAuth.CallbackPort > 0 {
		listen = fmt.Sprintf("127.0.0.1:%d", cfg.OAuth.CallbackPort)
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	defer ln.Close()
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)

	stored := loadServerAuth(serverName, cfg)
	clientID, clientSecret := "", ""
	if cfg.OAuth != nil {
		clientID = cfg.OAuth.ClientID
		clientSecret = cfg.OAuth.ClientSecret
	}
	if clientSecret == "" {
		clientSecret = os.Getenv("MCP_CLIENT_SECRET") // real's env fallback
	}
	if clientSecret == "" {
		clientSecret = loadClientSecret(serverName, cfg)
	}
	if clientID == "" && stored != nil {
		clientID, clientSecret = stored.ClientID, stored.ClientSecret
		if clientSecret == "" {
			clientSecret = loadClientSecret(serverName, cfg)
		}
	}
	if clientID == "" {
		clientID, clientSecret, err = registerClient(ctx, md, redirectURI)
		if err != nil {
			return fmt.Errorf("mcp %s client registration: %w", serverName, err)
		}
		// real persists the dynamic client under the credential store's
		// mcpOAuthClientConfig[Hl(name,cfg)] (secret) — .claude.json keeps
		// the non-secret registration for older flows
		saveClientConfig(serverName, clientID, clientSecret)
		_ = SaveClientSecret(serverName, cfg, clientSecret)
	}

	verifier, challenge := pkcePair()
	state := randState()
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	authURL := md.AuthorizationEndpoint + "?" + q.Encode()

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("state") != state {
			errCh <- fmt.Errorf("oauth state mismatch")
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			errCh <- fmt.Errorf("oauth callback missing code: %s", r.URL.Query().Get("error"))
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, "Authorization complete — return to claude-go.")
		codeCh <- code
	})}
	go srv.Serve(ln)
	defer srv.Close()

	auth.OpenBrowser(authURL)

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return fmt.Errorf("mcp %s oauth: timed out waiting for authorization (open %s)", serverName, authURL)
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	}
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", md.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
	}
	if json.NewDecoder(resp.Body).Decode(&tr) != nil || tr.AccessToken == "" {
		return fmt.Errorf("mcp %s token exchange failed (status %d)", serverName, resp.StatusCode)
	}
	rec := &serverAuth{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		TokenType:    tr.TokenType,
		Scope:        tr.Scope,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURI:  redirectURI,
		DiscoveryState: &discoveryState{
			AuthorizationServerURL: md.Issuer,
			Issuer:                 md.Issuer,
			TokenEndpoint:          md.TokenEndpoint,
			RegistrationEndpoint:   md.RegistrationEndpoint,
			AuthorizationEndpoint:  md.AuthorizationEndpoint,
			RevocationEndpoint:     md.RevocationEndpoint,
		},
	}
	if tr.ExpiresIn > 0 {
		rec.ExpiresAt = time.Now().UnixMilli() + tr.ExpiresIn*1000
	}
	if err := SaveServerAuth(serverName, cfg, rec); err != nil {
		return err
	}
	recordStatus(serverName, "connected", "", "")
	return nil
}

// discoverMetadata resolves the AS metadata, honoring a configured
// authServerMetadataUrl override (real's configuredMetadataUrl — https
// only).
func discoverMetadata(ctx context.Context, serverName string, cfg ServerConfig) (*OAuthMetadata, error) {
	if cfg.OAuth != nil && cfg.OAuth.AuthServerMetadataURL != "" {
		u := cfg.OAuth.AuthServerMetadataURL
		if !strings.HasPrefix(u, "https://") {
			return nil, fmt.Errorf("authServerMetadataUrl must use https:// (got %s)", u)
		}
		var md OAuthMetadata
		if err := getJSON(ctx, u, &md); err != nil {
			return nil, fmt.Errorf("http fetch of configured auth server metadata: %w", err)
		}
		return &md, nil
	}
	return DiscoverOAuthMetadata(ctx, serverURL(cfg))
}

func serverURL(cfg ServerConfig) string { return cfg.URL }

// saveClientConfig writes the dynamic client registration into ~/.claude.json
// mcpOAuthClientConfig[server] — where real CC keeps it.
func saveClientConfig(server, id, secret string) {
	cfg, p := config.ReadClaudeJSON()
	cc, _ := cfg["mcpOAuthClientConfig"].(map[string]any)
	if cc == nil {
		cc = map[string]any{}
	}
	cc[server] = map[string]any{"clientId": id, "clientSecret": secret}
	cfg["mcpOAuthClientConfig"] = cc
	config.WriteClaudeJSON(p, cfg)
}
