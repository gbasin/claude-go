// Package auth resolves Claude Code credentials: env vars, macOS Keychain,
// or ~/.claude/.credentials.json, with OAuth refresh.
package auth

import (
	"bytes"
	"claude-go/internal/config"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type Credentials struct {
	AccessToken      string   `json:"accessToken"`
	RefreshToken     string   `json:"refreshToken"`
	ExpiresAt        int64    `json:"expiresAt"` // ms epoch
	Scopes           []string `json:"scopes"`
	SubscriptionType string   `json:"subscriptionType"`
	// Borrowed marks creds loaded from real's shared stores (keychain /
	// .credentials.json). Refreshing a borrowed refresh token rotates the
	// shared family and can invalidate real's session — we use them
	// read-only until expiry, then require `claude-go login`.
	Borrowed bool `json:"-"`
	// DesignClientID is `clientId` on the designOauth slot — the design
	// OAuth registration the token was minted against (mod_1033.js).
	DesignClientID string `json:"clientId,omitempty"`
	IsAPIKey       bool   `json:"-"` // sk-ant-api03 style key, not OAuth
	// mu serializes token refresh. Token() is called from concurrent lanes
	// (main request, aux traffic, MCP connectors); an expired token without
	// this lock triggers concurrent refreshes that rotate — and lose — the
	// shared refresh token.
	mu sync.Mutex
}

type credFile struct {
	ClaudeAiOauth *Credentials `json:"claudeAiOauth"`
	// DesignOauth is the designOauth slot (xhe/gZt in mod_1033.js):
	// Claude Design's separate OAuth credential, stored beside
	// claudeAiOauth in .credentials.json.
	DesignOauth *Credentials `json:"designOauth"`
}

// Load returns credentials. Order:
//  1. ANTHROPIC_API_KEY env (api-key mode)
//  2. CLAUDE_CODE_OAUTH_TOKEN env
//  3. macOS Keychain "Claude Code-credentials"
//  4. ~/.claude/.credentials.json
func Load() (*Credentials, error) {
	if k := os.Getenv("ANTHROPIC_AUTH_TOKEN"); k != "" {
		return &Credentials{AccessToken: k}, nil
	}
	if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
		return &Credentials{AccessToken: k, IsAPIKey: true}, nil
	}
	if k := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); k != "" {
		return &Credentials{AccessToken: k}, nil
	}
	if k := os.Getenv("CLAUDE_GO_TOKEN"); k != "" {
		return &Credentials{AccessToken: k}, nil
	}
	// fd-supplied credentials (real reads them this way)
	for _, ev := range []string{"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR"} {
		if fds := os.Getenv(ev); fds != "" {
			if b, err := os.ReadFile("/dev/fd/" + fds); err == nil && len(bytes.TrimSpace(b)) > 0 {
				return &Credentials{AccessToken: string(bytes.TrimSpace(b)),
					IsAPIKey: ev == "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR"}, nil
			}
		}
	}
	if k := os.Getenv("CCR_OAUTH_TOKEN_FILE"); k != "" {
		if b, err := os.ReadFile(k); err == nil && len(bytes.TrimSpace(b)) > 0 {
			return &Credentials{AccessToken: string(bytes.TrimSpace(b))}, nil
		}
	}
	// apiKeyHelper: a user-configured command producing credentials
	if helper := apiKeyHelper(); helper != "" {
		return &Credentials{AccessToken: helper}, nil
	}
	// our own creds store first — logins and refreshes live here, never
	// in real's keychain/file
	if c, err := fromFile(ownCredsPath()); err == nil {
		// lazily backfill subscriptionType for stores written before the
		// profile fetch existed — the banner maps it to Claude Max/etc.
		if c.SubscriptionType == "" || c.SubscriptionType == "oauth" {
			if p := FetchProfile(c.AccessToken); p != nil {
				if st := p.SubscriptionType(); st != "" {
					c.SubscriptionType = st
					writeOwnCreds(c) // persist so this only happens once
				}
			}
		}
		return c, nil
	}
	if runtime.GOOS == "darwin" {
		if c, err := fromKeychain(); err == nil {
			c.Borrowed = true
			return c, nil
		}
	}
	for _, p := range []string{
		filepath.Join(config.ClaudeDir(), ".credentials.json"),
	} {
		if c, err := fromFile(p); err == nil {
			c.Borrowed = true
			return c, nil
		}
	}
	// real also stores claudeAiOauth inside ~/.claude.json (config store)
	if c, err := fromFile(filepath.Join(config.Home(), ".claude.json")); err == nil {
		c.Borrowed = true
		return c, nil
	}
	return nil, errors.New("no credentials found: run `claude` once to log in, or set ANTHROPIC_API_KEY")
}

func fromKeychain() (*Credentials, error) {
	out, err := exec.Command("security", "find-generic-password",
		"-s", "Claude Code-credentials", "-w").Output()
	if err != nil {
		return nil, err
	}
	var cf credFile
	if err := json.Unmarshal(bytes.TrimSpace(out), &cf); err != nil {
		return nil, err
	}
	if cf.ClaudeAiOauth == nil {
		return nil, errors.New("keychain entry missing claudeAiOauth")
	}
	return cf.ClaudeAiOauth, nil
}

func fromFile(p string) (*Credentials, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var cf credFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return nil, err
	}
	if cf.ClaudeAiOauth == nil {
		return nil, errors.New("credential file missing claudeAiOauth")
	}
	return cf.ClaudeAiOauth, nil
}

// credentialsFilePath is Gn()'s backing file in claude-go.
func credentialsFilePath() string {
	return filepath.Join(config.ClaudeDir(), ".credentials.json")
}

// LoadDesignOAuth mirrors xhe(): the designOauth slot of the credentials
// file, or nil.
func LoadDesignOAuth() (*Credentials, error) {
	b, err := os.ReadFile(credentialsFilePath())
	if err != nil {
		return nil, err
	}
	var cf credFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return nil, err
	}
	return cf.DesignOauth, nil
}

// SaveDesignOAuth mirrors gZt(): read-modify-write of the credentials file,
// setting only the designOauth slot.
func SaveDesignOAuth(creds *Credentials) error {
	p := credentialsFilePath()
	var cf credFile
	if b, err := os.ReadFile(p); err == nil {
		json.Unmarshal(b, &cf)
	}
	cf.DesignOauth = creds
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(cf)
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// ClearDesignOAuth drops the designOauth slot (T() in mod_1033.js).
func ClearDesignOAuth() error {
	return SaveDesignOAuth(nil)
}

// Token returns a usable bearer token, refreshing if expired.
func (c *Credentials) Token() (string, error) {
	if c.IsAPIKey {
		return c.AccessToken, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.RefreshToken == "" {
		return c.AccessToken, nil
	}
	// refresh if expiring within 60s
	if c.ExpiresAt == 0 || time.Now().UnixMilli() < c.ExpiresAt-60_000 {
		return c.AccessToken, nil
	}
	if c.Borrowed {
		// never rotate a shared family — return the borrowed token until
		// expiry; past that the API's 401 prompts a fresh claude-go login
		return c.AccessToken, nil
	}
	if err := c.refreshLocked(); err != nil {
		// fall back to possibly-still-valid token
		if c.AccessToken != "" {
			return c.AccessToken, nil
		}
		return "", err
	}
	return c.AccessToken, nil
}

const tokenURL = "https://platform.claude.com/v1/oauth/token"
const clientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// ForceRefresh refreshes unconditionally — for the reactive path where the
// server rejected a still-"fresh" token with a 401.
func (c *Credentials) ForceRefresh() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.RefreshToken == "" || c.Borrowed {
		return
	}
	_ = c.refreshLocked()
}

// Refresh rotates the OAuth token. Callers that already hold c.mu use
// refreshLocked instead.
func (c *Credentials) Refresh() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refreshLocked()
}

func (c *Credentials) refreshLocked() error {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": c.RefreshToken,
		"client_id":     clientID,
	})
	u := tokenURL
	if ov := os.Getenv("CLAUDE_GO_TOKEN_URL"); ov != "" {
		u = ov
	}
	req, err := http.NewRequest("POST", u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("token refresh failed: %s", resp.Status)
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if out.AccessToken != "" {
		c.AccessToken = out.AccessToken
	}
	if out.RefreshToken != "" {
		c.RefreshToken = out.RefreshToken
	}
	if out.ExpiresIn > 0 {
		c.ExpiresAt = time.Now().UnixMilli() + out.ExpiresIn*1000
	}
	// persist rotated creds to OUR store only — writing to real's
	// keychain slot is what clobbered the shared session
	if out.RefreshToken != "" {
		writeOwnCreds(c)
	}
	return nil
}

func apiKeyHelper() string {
	helper, _ := config.UserSetting("apiKeyHelper").(string)
	if helper == "" {
		return ""
	}
	out, err := exec.Command("sh", "-c", helper).Output()
	if err == nil && len(out) > 0 {
		return strings.TrimSpace(string(out))
	}
	return ""
}

// ownCredsPath is our private credentials file — defaults to
// ~/.claude-go/.credentials.json, override with CLAUDE_GO_CREDS_FILE.
// Keeping a separate store means our token rotation can never touch the
// family's other consumers.
func ownCredsPath() string {
	if p := os.Getenv("CLAUDE_GO_CREDS_FILE"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude-go", ".credentials.json")
}

func writeOwnCreds(c *Credentials) {
	p := ownCredsPath()
	os.MkdirAll(filepath.Dir(p), 0o700)
	b, _ := json.Marshal(map[string]any{"claudeAiOauth": c})
	os.WriteFile(p, b, 0o600)
}

// SaveOwnCreds persists a freshly-logged-in credential to our store.
func SaveOwnCreds(c *Credentials) {
	c.Borrowed = false
	writeOwnCreds(c)
}
