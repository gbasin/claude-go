// Package auth resolves Claude Code credentials: env vars, macOS Keychain,
// or ~/.claude/.credentials.json, with OAuth refresh.
package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

type Credentials struct {
	AccessToken      string   `json:"accessToken"`
	RefreshToken     string   `json:"refreshToken"`
	ExpiresAt        int64    `json:"expiresAt"` // ms epoch
	Scopes           []string `json:"scopes"`
	SubscriptionType string   `json:"subscriptionType"`
	IsAPIKey         bool     `json:"-"` // sk-ant-api03 style key, not OAuth
}

type credFile struct {
	ClaudeAiOauth *Credentials `json:"claudeAiOauth"`
}

// Load returns credentials. Order:
//  1. ANTHROPIC_API_KEY env (api-key mode)
//  2. CLAUDE_CODE_OAUTH_TOKEN env
//  3. macOS Keychain "Claude Code-credentials"
//  4. ~/.claude/.credentials.json
func Load() (*Credentials, error) {
	if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
		return &Credentials{AccessToken: k, IsAPIKey: true}, nil
	}
	if k := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); k != "" {
		return &Credentials{AccessToken: k}, nil
	}
	if runtime.GOOS == "darwin" {
		if c, err := fromKeychain(); err == nil {
			return c, nil
		}
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(home, ".claude", ".credentials.json"),
	} {
		if c, err := fromFile(p); err == nil {
			return c, nil
		}
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

// Token returns a usable bearer token, refreshing if expired.
func (c *Credentials) Token() (string, error) {
	if c.IsAPIKey || c.RefreshToken == "" {
		return c.AccessToken, nil
	}
	// refresh if expiring within 60s
	if c.ExpiresAt == 0 || time.Now().UnixMilli() < c.ExpiresAt-60_000 {
		return c.AccessToken, nil
	}
	if err := c.Refresh(); err != nil {
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

func (c *Credentials) Refresh() error {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": c.RefreshToken,
		"client_id":     clientID,
	})
	req, err := http.NewRequest("POST", tokenURL, bytes.NewReader(body))
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
	return nil
}
