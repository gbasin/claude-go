package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateAuth redirects every credential/config path at a temp dir and
// clears the env-token lanes Load() checks first.
func isolateAuth(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, ".claude"))
	t.Setenv("CLAUDE_GO_CREDS_FILE", filepath.Join(dir, ".claude-go", ".credentials.json"))
	for _, ev := range []string{
		"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_GO_TOKEN", "CCR_OAUTH_TOKEN_FILE",
		"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR",
	} {
		t.Setenv(ev, "")
	}
	return dir
}

func TestOwnCredsRoundTrip(t *testing.T) {
	isolateAuth(t)
	c := &Credentials{AccessToken: "tok-1", RefreshToken: "rt",
		SubscriptionType: "max", ExpiresAt: 12345}
	SaveOwnCreds(c)
	p := ownCredsPath()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("creds file not written: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("creds file mode = %o, want 0600", st.Mode().Perm())
	}
	if st.Mode().Perm()&0o077 != 0 {
		t.Fatal("creds file readable by group/other")
	}
	got, err := fromFile(p)
	if err != nil {
		t.Fatalf("fromFile: %v", err)
	}
	if got.AccessToken != "tok-1" || got.RefreshToken != "rt" || got.SubscriptionType != "max" {
		t.Fatalf("round trip: %+v", got)
	}
}

func TestLoadPrefersOwnCredsFile(t *testing.T) {
	isolateAuth(t)
	SaveOwnCreds(&Credentials{AccessToken: "mine", SubscriptionType: "pro"})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AccessToken != "mine" || c.Borrowed {
		t.Fatalf("Load returned %+v", c)
	}
}

func TestLoadEnvTokens(t *testing.T) {
	isolateAuth(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	c, err := Load()
	if err != nil || !c.IsAPIKey || c.AccessToken != "sk-ant-test" {
		t.Fatalf("api key lane: %v %+v", err, c)
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-tok")
	c, err = Load()
	if err != nil || c.AccessToken != "oauth-tok" || c.IsAPIKey {
		t.Fatalf("oauth env lane: %v %+v", err, c)
	}
}

func TestLoadNoCreds(t *testing.T) {
	isolateAuth(t)
	if _, err := Load(); err == nil {
		t.Fatal("expected no-credentials error")
	}
}

func TestFromFileErrors(t *testing.T) {
	isolateAuth(t)
	if _, err := fromFile("/nonexistent/x.json"); err == nil {
		t.Fatal("missing file should error")
	}
	p := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(p, []byte("{not json"), 0o644)
	if _, err := fromFile(p); err == nil {
		t.Fatal("bad json should error")
	}
	os.WriteFile(p, []byte(`{"other": 1}`), 0o644)
	if _, err := fromFile(p); err == nil {
		t.Fatal("missing claudeAiOauth should error")
	}
}

func TestTokenAPIKeyPath(t *testing.T) {
	c := &Credentials{AccessToken: "raw-key", IsAPIKey: true}
	tok, err := c.Token()
	if err != nil || tok != "raw-key" {
		t.Fatalf("Token() = %q, %v", tok, err)
	}
}

func TestLoginURLShape(t *testing.T) {
	u, verifier, state := loginURLFor("cid-1", "scope:a scope:b")
	pu, err := url.Parse(u)
	if err != nil {
		t.Fatalf("bad url: %v", err)
	}
	q := pu.Query()
	if q.Get("client_id") != "cid-1" ||
		q.Get("redirect_uri") != oauthRedirect ||
		q.Get("response_type") != "code" ||
		q.Get("state") != state ||
		q.Get("scope") != "scope:a scope:b" ||
		q.Get("code_challenge_method") != "S256" {
		t.Fatalf("query params: %s", u)
	}
	sum := sha256.Sum256([]byte(verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); q.Get("code_challenge") != want {
		t.Fatalf("challenge = %q, want %q", q.Get("code_challenge"), want)
	}
}

func TestRandText(t *testing.T) {
	a, b := randText(64), randText(64)
	if len(a) != 64 || len(b) != 64 {
		t.Fatalf("randText length: %d %d", len(a), len(b))
	}
	if a == b {
		t.Fatal("randText not random")
	}
	for _, r := range a {
		if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", r) {
			t.Fatalf("randText char %q outside base64url", r)
		}
	}
}

func TestProfileSubscriptionType(t *testing.T) {
	mk := func(plan, orgType string, hasMax, hasPro bool) *Profile {
		p := &Profile{}
		p.Organization.PlanDisplayName = plan
		p.Organization.OrganizationType = orgType
		p.Account.HasClaudeMax = hasMax
		p.Account.HasClaudePro = hasPro
		return p
	}
	cases := []struct {
		p    *Profile
		want string
	}{
		{mk("Max", "", false, false), "max"},
		{mk("max", "", false, false), "max"},
		{mk("Team", "", false, false), "team"},
		{mk("Enterprise", "", false, false), "enterprise"},
	}
	for _, tc := range cases {
		if got := tc.p.SubscriptionType(); got != tc.want {
			t.Fatalf("plan %q → %q, want %q", tc.p.Organization.PlanDisplayName, got, tc.want)
		}
	}
}

func TestDesignOAuthConfigured(t *testing.T) {
	t.Setenv("CLAUDE_CODE_DESIGN_OAUTH_CLIENT_ID", "")
	if !DesignOAuthConfigured() {
		t.Fatal("bundled client id should read as configured")
	}
	t.Setenv("CLAUDE_CODE_DESIGN_OAUTH_CLIENT_ID", "00000000-0000-0000-0000-000000000000")
	if DesignOAuthConfigured() {
		t.Fatal("00000000- placeholder should read as unconfigured")
	}
	t.Setenv("CLAUDE_CODE_DESIGN_OAUTH_CLIENT_ID", "abc-123")
	if !DesignOAuthConfigured() {
		t.Fatal("env-provided id should read as configured")
	}
}

func TestSaveDesignOAuthRoundTrip(t *testing.T) {
	isolateAuth(t)
	c := &Credentials{AccessToken: "design-tok"}
	if err := SaveDesignOAuth(c); err != nil {
		t.Fatalf("SaveDesignOAuth: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cf credFile
	if err := json.Unmarshal(b, &cf); err != nil || cf.DesignOauth == nil ||
		cf.DesignOauth.AccessToken != "design-tok" {
		t.Fatalf("credFile: %v %+v", err, cf)
	}
	// file must be private
	st, _ := os.Stat(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".credentials.json"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", st.Mode().Perm())
	}
}
