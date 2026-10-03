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
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"claude-go/internal/auth"
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

// serverAuth is the persisted per-server OAuth record.
type serverAuth struct {
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"` // unix ms
	TokenType    string `json:"token_type,omitempty"`
	Scope        string `json:"scope,omitempty"`

	// dynamic client registration results
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`

	// discovered endpoints, cached for refresh
	TokenEndpoint         string `json:"token_endpoint,omitempty"`
	RegistrationEndpoint  string `json:"registration_endpoint,omitempty"`
	AuthorizationEndpoint string `json:"authorization_endpoint,omitempty"`
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
	if s == nil || s.RefreshToken == "" || s.TokenEndpoint == "" {
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
	req, err := http.NewRequestWithContext(ctx, "POST", s.TokenEndpoint,
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

func credentialsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", ".credentials-mcp.json")
}

var credMu sync.Mutex

// LoadMCPCredentials reads ~/.claude/.credentials-mcp.json.
func LoadMCPCredentials() map[string]*serverAuth {
	credMu.Lock()
	defer credMu.Unlock()
	return loadCredFile()
}

func loadCredFile() map[string]*serverAuth {
	out := map[string]*serverAuth{}
	b, err := os.ReadFile(credentialsPath())
	if err != nil {
		return out
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(b, &top) != nil {
		return out
	}
	// accept both {"mcpOAuth": {...}} and a bare {name: {...}} map
	for _, key := range []string{"mcpOAuth"} {
		if raw, ok := top[key]; ok {
			var m map[string]*serverAuth
			if json.Unmarshal(raw, &m) == nil && m != nil {
				return m
			}
		}
	}
	var m map[string]*serverAuth
	if json.Unmarshal(b, &m) == nil {
		for k, v := range m {
			if v != nil {
				out[k] = v
			}
		}
	}
	return out
}

func loadServerAuth(name string) *serverAuth {
	credMu.Lock()
	defer credMu.Unlock()
	return loadCredFile()[name]
}

// SaveServerAuth upserts one server's OAuth record.
func SaveServerAuth(name string, s *serverAuth) error {
	credMu.Lock()
	defer credMu.Unlock()
	m := loadCredFile()
	m[name] = s
	out, err := json.MarshalIndent(map[string]any{"mcpOAuth": m}, "", "  ")
	if err != nil {
		return err
	}
	p := credentialsPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, out, 0o600)
}

// DeleteServerAuth removes one server's stored tokens.
func DeleteServerAuth(name string) error {
	credMu.Lock()
	defer credMu.Unlock()
	m := loadCredFile()
	delete(m, name)
	out, _ := json.MarshalIndent(map[string]any{"mcpOAuth": m}, "", "  ")
	return os.WriteFile(credentialsPath(), out, 0o600)
}

// --- discovery ---

// OAuthMetadata is the subset of RFC 8414 authorization-server metadata we
// need.
type OAuthMetadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
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
// → token exchange → persist to ~/.claude/.credentials-mcp.json.
// It blocks until the user completes authorization or ctx is cancelled.
func RunOAuthFlow(ctx context.Context, serverName, serverURL string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	md, err := DiscoverOAuthMetadata(ctx, serverURL)
	if err != nil {
		return fmt.Errorf("mcp %s oauth discovery: %w", serverName, err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)

	stored := loadServerAuth(serverName)
	clientID, clientSecret := "", ""
	if stored != nil {
		clientID, clientSecret = stored.ClientID, stored.ClientSecret
	}
	if clientID == "" {
		clientID, clientSecret, err = registerClient(ctx, md, redirectURI)
		if err != nil {
			return fmt.Errorf("mcp %s client registration: %w", serverName, err)
		}
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
		AccessToken:           tr.AccessToken,
		RefreshToken:          tr.RefreshToken,
		TokenType:             tr.TokenType,
		Scope:                 tr.Scope,
		ClientID:              clientID,
		ClientSecret:          clientSecret,
		TokenEndpoint:         md.TokenEndpoint,
		RegistrationEndpoint:  md.RegistrationEndpoint,
		AuthorizationEndpoint: md.AuthorizationEndpoint,
	}
	if tr.ExpiresIn > 0 {
		rec.ExpiresAt = time.Now().UnixMilli() + tr.ExpiresIn*1000
	}
	if err := SaveServerAuth(serverName, rec); err != nil {
		return err
	}
	recordStatus(serverName, "connected", "", "")
	return nil
}
