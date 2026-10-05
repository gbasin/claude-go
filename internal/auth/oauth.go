package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const (
	oauthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	oauthAuthURL  = "https://claude.ai/oauth/authorize"
	oauthTokenURL = "https://platform.claude.com/v1/oauth/token"
	oauthRedirect = "https://platform.claude.com/oauth/code/callback"
	oauthScope    = "user:inference user:profile"
)

// Design OAuth constants (mod_1033.js): the separate Claude Design client
// registration and its scopes. CLAUDE_CODE_DESIGN_OAUTH_CLIENT_ID overrides
// the bundled id (hZt()); a 00000000-… prefix means unconfigured (xAe()).
const DesignClientID = "59637612-477b-4836-a601-b0589eda7704"

var DesignScopes = []string{"user:design:read", "user:design:write"}

// DesignOAuthClientID mirrors hZt(): env override, else the bundled id.
func DesignOAuthClientID() string {
	if v := os.Getenv("CLAUDE_CODE_DESIGN_OAUTH_CLIENT_ID"); v != "" {
		return v
	}
	return DesignClientID
}

// DesignOAuthConfigured mirrors xAe(): a 00000000-* client id is the
// unconfigured placeholder.
func DesignOAuthConfigured() bool {
	return !strings.HasPrefix(DesignOAuthClientID(), "00000000-")
}

// LoginURL builds the OAuth authorize URL and returns (url, verifier, state).
func LoginURL() (loginURL, verifier, state string) {
	return loginURLFor(oauthClientID, oauthScope)
}

// DesignLoginURL is LoginURL for the Claude Design client (yZt's
// oauthClient override — same authorize endpoint, different client/scopes).
func DesignLoginURL() (loginURL, verifier, state string) {
	return loginURLFor(DesignOAuthClientID(), strings.Join(DesignScopes, " "))
}

func loginURLFor(clientID, scope string) (loginURL, verifier, state string) {
	v := randText(64)
	s := randText(32)
	sum := sha256.Sum256([]byte(v))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	q := url.Values{
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"client_id":             {clientID},
		"redirect_uri":          {oauthRedirect},
		"response_type":         {"code"},
		"state":                 {s},
		"scope":                 {scope},
	}
	return oauthAuthURL + "?" + q.Encode(), v, s
}

func randText(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)[:n]
}

// ExchangeCode trades the pasted "code#state" for tokens.
func ExchangeCode(codeAndState, verifier, expectState string) (*Credentials, error) {
	return exchangeCode(codeAndState, verifier, expectState, oauthClientID)
}

// ExchangeDesignCode is ExchangeCode for the Claude Design client (the
// token exchange inside yZt's OAuth flow, mod_1033.js).
func ExchangeDesignCode(codeAndState, verifier, expectState string) (*Credentials, error) {
	c, err := exchangeCode(codeAndState, verifier, expectState, DesignOAuthClientID())
	if err != nil {
		return nil, err
	}
	c.DesignClientID = DesignOAuthClientID()
	return c, nil
}

func exchangeCode(codeAndState, verifier, expectState, clientID string) (*Credentials, error) {
	code, state, _ := strings.Cut(codeAndState, "#")
	if expectState != "" && state != expectState {
		return nil, fmt.Errorf("state mismatch")
	}
	body := map[string]any{
		"grant_type":    "authorization_code",
		"code":          code,
		"state":         state,
		"redirect_uri":  oauthRedirect,
		"client_id":     clientID,
		"code_verifier": verifier,
	}
	req, _ := http.NewRequest("POST", oauthTokenURL, strings.NewReader(mustJSON(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		Error        string `json:"error"`
	}
	if json.NewDecoder(resp.Body).Decode(&tr) != nil || tr.AccessToken == "" {
		return nil, fmt.Errorf("token exchange failed (status %d)", resp.StatusCode)
	}
	expMs := int64(0)
	if tr.ExpiresIn > 0 {
		expMs = time.Now().UnixMilli() + tr.ExpiresIn*1000
	}
	creds := &Credentials{
		AccessToken:      tr.AccessToken,
		RefreshToken:     tr.RefreshToken,
		ExpiresAt:        expMs,
		Scopes:           strings.Split(tr.Scope, " "),
		SubscriptionType: "oauth",
	}
	// real's banner maps subscriptionType→"Claude Max/Pro/Team/API"; the
	// exchange response doesn't carry it — fetch the profile for the real
	// tier (organizationType/planDisplayName). Best-effort.
	if p := FetchProfile(tr.AccessToken); p != nil {
		if st := p.SubscriptionType(); st != "" {
			creds.SubscriptionType = st
		}
	}
	return creds, nil
}

// Profile is the subset of /api/oauth/profile used for the plan label —
// the response nests under account/organization (verified live).
type Profile struct {
	Account struct {
		DisplayName  string `json:"display_name"`
		HasClaudeMax bool   `json:"has_claude_max"`
		HasClaudePro bool   `json:"has_claude_pro"`
	} `json:"account"`
	Organization struct {
		OrganizationType string `json:"organization_type"` // claude_max/pro/team/enterprise
		BillingType      string `json:"billing_type"`
		PlanDisplayName  string `json:"plan_display_name"`
	} `json:"organization"`
}

// SubscriptionType maps the profile to the subscriptionType stored on
// credentials ("max"/"pro"/"team"/"enterprise") — real's banner switch.
func (p *Profile) SubscriptionType() string {
	// planDisplayName like "Max" wins when present (real's sA() path)
	switch strings.ToLower(p.Organization.PlanDisplayName) {
	case "max", "pro", "team", "enterprise":
		return strings.ToLower(p.Organization.PlanDisplayName)
	}
	switch p.Organization.OrganizationType {
	case "claude_max":
		return "max"
	case "claude_pro":
		return "pro"
	case "claude_team":
		return "team"
	case "claude_enterprise", "enterprise":
		return "enterprise"
	}
	if p.Account.HasClaudeMax {
		return "max"
	}
	if p.Account.HasClaudePro {
		return "pro"
	}
	return ""
}

// FetchProfile GETs /api/oauth/profile with the access token.
func FetchProfile(token string) *Profile {
	req, _ := http.NewRequest("GET", "https://api.anthropic.com/api/oauth/profile", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	var p Profile
	if json.NewDecoder(resp.Body).Decode(&p) != nil {
		return nil
	}
	return &p
}

// WriteKeychain stores credentials the same way real claude does.
func WriteKeychain(creds *Credentials) error {
	inner := map[string]any{"claudeAiOauth": creds}
	b, _ := json.Marshal(inner)
	if runtime.GOOS == "darwin" {
		user := os.Getenv("USER")
		// delete existing then add
		exec.Command("security", "delete-generic-password",
			"-s", "Claude Code-credentials", "-a", user).Run()
		return exec.Command("security", "add-generic-password",
			"-s", "Claude Code-credentials", "-a", user,
			"-w", string(b), "-U").Run()
	}
	return fmt.Errorf("keychain only on macOS")
}

// OpenBrowser opens the login URL.
func OpenBrowser(url string) {
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", url).Start()
	case "linux":
		exec.Command("xdg-open", url).Start()
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
