package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"claude-go/internal/auth"
)

func TestDetectProviderNewEnvs(t *testing.T) {
	for _, tt := range []struct {
		env  string
		want Provider
	}{
		{"CLAUDE_CODE_USE_ANTHROPIC_AWS", ProviderAnthropicAWS},
		{"CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD", ProviderAnthropicGoogleCloud},
		{"CLAUDE_CODE_USE_MANTLE", ProviderMantle},
	} {
		t.Setenv(tt.env, "1")
		if got := DetectProvider(); got != tt.want {
			t.Errorf("%s=1: got %q, want %q", tt.env, got, tt.want)
		}
		t.Setenv(tt.env, "")
	}
}

// Real's provider precedence (mod_0046.js Pe()): gateway > bedrock >
// foundry > anthropicAws > anthropicGoogleCloud > mantle > vertex.
func TestDetectProviderPrecedence(t *testing.T) {
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "1")
	t.Setenv("CLAUDE_CODE_USE_MANTLE", "1")
	if got := DetectProvider(); got != ProviderMantle {
		t.Fatalf("mantle should beat vertex, got %q", got)
	}
	t.Setenv("CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD", "1")
	if got := DetectProvider(); got != ProviderAnthropicGoogleCloud {
		t.Fatalf("gcloud should beat mantle, got %q", got)
	}
	t.Setenv("CLAUDE_CODE_USE_ANTHROPIC_AWS", "1")
	if got := DetectProvider(); got != ProviderAnthropicAWS {
		t.Fatalf("aws should beat gcloud, got %q", got)
	}
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	if got := DetectProvider(); got != ProviderBedrock {
		t.Fatalf("bedrock should beat aws, got %q", got)
	}
}

// Real parses the USE_* envs as strict booleans (M.bool -> Le()): only
// 1/true/yes/on count — "0", "false", "no", and arbitrary strings are off.
func TestDetectProviderStrictBool(t *testing.T) {
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "random")
	if got := DetectProvider(); got == ProviderBedrock {
		t.Fatalf("non-boolean value should not select bedrock")
	}
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "on")
	if got := DetectProvider(); got != ProviderBedrock {
		t.Fatalf("on should select bedrock, got %q", got)
	}
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "yes")
	if got := DetectProvider(); got != ProviderBedrock {
		t.Fatalf("yes should select bedrock, got %q", got)
	}
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "0")
	if got := DetectProvider(); got == ProviderBedrock {
		t.Fatalf("0 should not select bedrock")
	}
}

func TestResolveBaseURLNewProviders(t *testing.T) {
	t.Run("anthropicAws env URL", func(t *testing.T) {
		t.Setenv("ANTHROPIC_AWS_BASE_URL", "https://aws-proxy.example.com/")
		if got := resolveBaseURL(ProviderAnthropicAWS); got != "https://aws-proxy.example.com" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("anthropicAws regional", func(t *testing.T) {
		t.Setenv("AWS_REGION", "eu-west-1")
		if got := resolveBaseURL(ProviderAnthropicAWS); got != "https://aws-external-anthropic.eu-west-1.api.aws" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("anthropicAws no region", func(t *testing.T) {
		t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
		if got := resolveBaseURL(ProviderAnthropicAWS); got != "" {
			t.Fatalf("expected empty base, got %q", got)
		}
	})
	t.Run("anthropicGoogleCloud env URL", func(t *testing.T) {
		t.Setenv("ANTHROPIC_GOOGLE_CLOUD_BASE_URL", "https://gcp-proxy.example.com")
		if got := resolveBaseURL(ProviderAnthropicGoogleCloud); got != "https://gcp-proxy.example.com" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("anthropicGoogleCloud workspace invoke URL", func(t *testing.T) {
		t.Setenv("ANTHROPIC_GOOGLE_CLOUD_PROJECT", "my-proj")
		t.Setenv("ANTHROPIC_GOOGLE_CLOUD_WORKSPACE_ID", "ws1")
		want := "https://claude.googleapis.com/v1alpha/projects/my-proj/locations/global/workspaces/ws1/invoke"
		if got := resolveBaseURL(ProviderAnthropicGoogleCloud); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("anthropicGoogleCloud location", func(t *testing.T) {
		t.Setenv("ANTHROPIC_GOOGLE_CLOUD_PROJECT", "my-proj")
		t.Setenv("ANTHROPIC_GOOGLE_CLOUD_WORKSPACE_ID", "ws1")
		t.Setenv("ANTHROPIC_GOOGLE_CLOUD_LOCATION", "us-east5")
		want := "https://claude.googleapis.com/v1alpha/projects/my-proj/locations/us-east5/workspaces/ws1/invoke"
		if got := resolveBaseURL(ProviderAnthropicGoogleCloud); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("mantle regional", func(t *testing.T) {
		t.Setenv("AWS_DEFAULT_REGION", "us-east-1")
		if got := resolveBaseURL(ProviderMantle); got != "https://bedrock-mantle.us-east-1.api.aws" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("firstParty CLAUDE_CODE_API_BASE_URL fallback", func(t *testing.T) {
		t.Setenv("CLAUDE_CODE_API_BASE_URL", "https://alt.api.example.com/")
		if got := resolveBaseURL(ProviderFirstParty); got != "https://alt.api.example.com" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("ANTHROPIC_BASE_URL wins over CLAUDE_CODE_API_BASE_URL", func(t *testing.T) {
		t.Setenv("ANTHROPIC_BASE_URL", "https://primary.example.com")
		t.Setenv("CLAUDE_CODE_API_BASE_URL", "https://alt.api.example.com")
		if got := resolveBaseURL(ProviderFirstParty); got != "https://primary.example.com" {
			t.Fatalf("got %q", got)
		}
	})
}

func newReq(t *testing.T) *http.Request {
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages",
		strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestAnthropicAWSAuth(t *testing.T) {
	t.Run("workspace-id + api key", func(t *testing.T) {
		t.Setenv("ANTHROPIC_AWS_WORKSPACE_ID", "ws-1")
		t.Setenv("ANTHROPIC_AWS_API_KEY", "ak-1")
		c := &Client{Provider: ProviderAnthropicAWS}
		req := newReq(t)
		if err := c.applyAuth(req); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("anthropic-workspace-id"); got != "ws-1" {
			t.Fatalf("workspace-id %q", got)
		}
		if got := req.Header.Get("x-api-key"); got != "ak-1" {
			t.Fatalf("x-api-key %q", got)
		}
	})
	t.Run("missing workspace errors", func(t *testing.T) {
		t.Setenv("ANTHROPIC_AWS_API_KEY", "ak-1")
		c := &Client{Provider: ProviderAnthropicAWS}
		if err := c.applyAuth(newReq(t)); err == nil ||
			!strings.Contains(err.Error(), "ANTHROPIC_AWS_WORKSPACE_ID") {
			t.Fatalf("expected workspace error, got %v", err)
		}
	})
	t.Run("skip auth allows no workspace", func(t *testing.T) {
		t.Setenv("CLAUDE_CODE_SKIP_ANTHROPIC_AWS_AUTH", "1")
		c := &Client{Provider: ProviderAnthropicAWS}
		if err := c.applyAuth(newReq(t)); err != nil {
			t.Fatal(err)
		}
	})
}

func TestMantleAuth(t *testing.T) {
	t.Run("bearer env", func(t *testing.T) {
		t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "bt-1")
		c := &Client{Provider: ProviderMantle}
		req := newReq(t)
		if err := c.applyAuth(req); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer bt-1" {
			t.Fatalf("auth %q", got)
		}
	})
	t.Run("skip auth", func(t *testing.T) {
		t.Setenv("CLAUDE_CODE_SKIP_MANTLE_AUTH", "true")
		c := &Client{Provider: ProviderMantle}
		if err := c.applyAuth(newReq(t)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("no creds error", func(t *testing.T) {
		t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
		t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
		c := &Client{Provider: ProviderMantle}
		if err := c.applyAuth(newReq(t)); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestAnthropicGoogleCloudAuth(t *testing.T) {
	t.Run("bearer", func(t *testing.T) {
		t.Setenv("GOOGLE_OAUTH_ACCESS_TOKEN", "gt-1")
		t.Setenv("ANTHROPIC_GOOGLE_CLOUD_PROJECT", "p")
		t.Setenv("ANTHROPIC_GOOGLE_CLOUD_WORKSPACE_ID", "w")
		c := &Client{Provider: ProviderAnthropicGoogleCloud}
		req := newReq(t)
		if err := c.applyAuth(req); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer gt-1" {
			t.Fatalf("auth %q", got)
		}
	})
	t.Run("charset gate", func(t *testing.T) {
		t.Setenv("ANTHROPIC_GOOGLE_CLOUD_PROJECT", "bad/proj")
		c := &Client{Provider: ProviderAnthropicGoogleCloud}
		if err := c.applyAuth(newReq(t)); err == nil ||
			!strings.Contains(err.Error(), "ANTHROPIC_GOOGLE_CLOUD_PROJECT") {
			t.Fatalf("expected charset error, got %v", err)
		}
	})
	t.Run("skip auth w/o project+base", func(t *testing.T) {
		t.Setenv("CLAUDE_CODE_SKIP_ANTHROPIC_GOOGLE_CLOUD_AUTH", "1")
		c := &Client{Provider: ProviderAnthropicGoogleCloud}
		if err := c.applyAuth(newReq(t)); err != nil {
			t.Fatal(err)
		}
	})
}

func TestIsFirstPartyBaseURL(t *testing.T) {
	if !IsFirstPartyBaseURL() {
		t.Fatal("unset ANTHROPIC_BASE_URL should be first-party")
	}
	t.Setenv("ANTHROPIC_BASE_URL", "https://api.anthropic.com")
	if !IsFirstPartyBaseURL() {
		t.Fatal("api.anthropic.com should be first-party")
	}
	t.Setenv("ANTHROPIC_BASE_URL", "https://proxy.example.com")
	if IsFirstPartyBaseURL() {
		t.Fatal("custom host should not be first-party")
	}
	t.Setenv("_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL", "1")
	if !IsFirstPartyBaseURL() {
		t.Fatal("assume-first-party should force first-party")
	}
	t.Setenv("_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL", "false")
	if IsFirstPartyBaseURL() {
		t.Fatal("false should not force first-party")
	}
}

func TestGatewayTokenFD(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString("  gw-token-123\n  ")
	f.Sync()
	f.Seek(0, 0) // /dev/fd/N shares the open file's offset
	fd := f.Fd()
	rf, err := os.OpenFile("/dev/fd/"+strconv.Itoa(int(fd)), os.O_RDONLY, 0)
	if err != nil {
		t.Skip("/dev/fd not supported here")
	}
	rf.Close()
	t.Setenv("CLAUDE_CODE_GATEWAY_TOKEN_FILE_DESCRIPTOR", strconv.Itoa(int(fd)))
	if got := readFDToken("CLAUDE_CODE_GATEWAY_TOKEN_FILE_DESCRIPTOR"); got != "gw-token-123" {
		t.Fatalf("got %q", got)
	}
	f.Seek(0, 0) // each read consumes the shared offset
	c := &Client{Provider: ProviderGateway}
	req := newReq(t)
	if err := c.applyAuth(req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer gw-token-123" {
		t.Fatalf("auth %q", got)
	}
}

// ---------- workload identity (mod_0282.js / mod_0115.js) ----------

// tokenSink serves /v1/oauth/token and records the request body + betas.
type tokenSink struct {
	srv   *httptest.Server
	body  map[string]string
	betas string
}

func newTokenSink(t *testing.T) *tokenSink {
	ts := &tokenSink{}
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.betas = r.Header.Get("anthropic-beta")
		_ = json.NewDecoder(r.Body).Decode(&ts.body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "exchanged-tok", "expires_in": 3600,
		})
	}))
	t.Cleanup(ts.srv.Close)
	return ts
}

func TestIdentityEnvQuadExchange(t *testing.T) {
	ts := newTokenSink(t)
	t.Setenv("ANTHROPIC_BASE_URL", ts.srv.URL)
	t.Setenv("ANTHROPIC_FEDERATION_RULE_ID", "rule-1")
	t.Setenv("ANTHROPIC_ORGANIZATION_ID", "org-1")
	t.Setenv("ANTHROPIC_WORKSPACE_ID", "ws-1")
	t.Setenv("ANTHROPIC_SERVICE_ACCOUNT_ID", "sa-1")
	t.Setenv("ANTHROPIC_IDENTITY_TOKEN", "id-jwt")
	t.Setenv("ANTHROPIC_CONFIG_DIR", filepath.Join(t.TempDir(), "cfg"))
	identMu.Lock()
	identCache = map[string]*identityTokenResult{}
	identMu.Unlock()

	c := &Client{Provider: ProviderFirstParty}
	req := newReq(t)
	ok, err := c.applyIdentityAuth(req)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("identity auth not applied")
	}
	if got := req.Header.Get("Authorization"); got != "Bearer exchanged-tok" {
		t.Fatalf("auth %q", got)
	}
	if ts.body["grant_type"] != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		t.Fatalf("grant %q", ts.body["grant_type"])
	}
	if ts.body["assertion"] != "id-jwt" ||
		ts.body["federation_rule_id"] != "rule-1" ||
		ts.body["organization_id"] != "org-1" ||
		ts.body["service_account_id"] != "sa-1" ||
		ts.body["workspace_id"] != "ws-1" {
		t.Fatalf("body %v", ts.body)
	}
	if ts.betas != "oauth-2025-04-20,oidc-federation-2026-04-01" {
		t.Fatalf("betas %q", ts.betas)
	}
	// workspace id goes in the exchange body, not a header, for federation
	if got := req.Header.Get("anthropic-workspace-id"); got != "" {
		t.Fatalf("unexpected workspace header %q", got)
	}
}

func TestIdentityTokenFromFile(t *testing.T) {
	ts := newTokenSink(t)
	dir := t.TempDir()
	tokFile := filepath.Join(dir, "idtoken")
	os.WriteFile(tokFile, []byte(" file-tok \n"), 0o600)
	t.Setenv("ANTHROPIC_BASE_URL", ts.srv.URL)
	t.Setenv("ANTHROPIC_FEDERATION_RULE_ID", "rule-1")
	t.Setenv("ANTHROPIC_ORGANIZATION_ID", "org-1")
	t.Setenv("ANTHROPIC_IDENTITY_TOKEN_FILE", tokFile)
	t.Setenv("ANTHROPIC_CONFIG_DIR", filepath.Join(t.TempDir(), "cfg"))
	identMu.Lock()
	identCache = map[string]*identityTokenResult{}
	identMu.Unlock()

	c := &Client{Provider: ProviderFirstParty}
	ok, err := c.applyIdentityAuth(newReq(t))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if ts.body["assertion"] != "file-tok" {
		t.Fatalf("assertion %q", ts.body["assertion"])
	}
}

func TestIdentityProfileConfig(t *testing.T) {
	ts := newTokenSink(t)
	dir := t.TempDir()
	t.Setenv("ANTHROPIC_CONFIG_DIR", dir)
	t.Setenv("ANTHROPIC_PROFILE", "prod")
	os.MkdirAll(filepath.Join(dir, "configs"), 0o700)
	cfg := `{"organization_id":"org-file","workspace_id":"ws-file",
		"base_url":"` + ts.srv.URL + `",
		"authentication":{"type":"oidc_federation",
			"federation_rule_id":"rule-file",
			"identity_token":{"source":"file","path":"` +
		filepath.Join(dir, "idtok") + `"}}}`
	os.WriteFile(filepath.Join(dir, "configs", "prod.json"), []byte(cfg), 0o600)
	os.WriteFile(filepath.Join(dir, "idtok"), []byte("profile-tok"), 0o600)
	identMu.Lock()
	identCache = map[string]*identityTokenResult{}
	identMu.Unlock()

	c := &Client{Provider: ProviderFirstParty}
	req := newReq(t)
	ok, err := c.applyIdentityAuth(req)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if ts.body["assertion"] != "profile-tok" || ts.body["organization_id"] != "org-file" {
		t.Fatalf("body %v", ts.body)
	}
	// config.base_url overrides the request destination (ANTHROPIC_BASE_URL
	// unset): the httptest host should replace api.anthropic.com.
	if got := req.URL.Host; !strings.Contains(got, "127.0.0.1") && !strings.Contains(got, "localhost") {
		t.Fatalf("base_url override should rewrite request host, got %q", got)
	}
}

// Explicit ANTHROPIC_PROFILE suppresses the env-quad (real Et():
// profile-explicit wins over env-quad).
func TestIdentityExplicitProfileBeatsEnvQuad(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ANTHROPIC_CONFIG_DIR", dir)
	t.Setenv("ANTHROPIC_PROFILE", "prod")
	t.Setenv("ANTHROPIC_FEDERATION_RULE_ID", "rule-1")
	t.Setenv("ANTHROPIC_ORGANIZATION_ID", "org-1")
	os.MkdirAll(filepath.Join(dir, "configs"), 0o700)
	if got := identitySource(); got != "" {
		t.Fatalf("no profile file -> source should be empty, got %q", got)
	}
	os.WriteFile(filepath.Join(dir, "configs", "prod.json"),
		[]byte(`{"authentication":{"type":"user_oauth"}}`), 0o600)
	if got := identitySource(); got != "profile-explicit" {
		t.Fatalf("got %q", got)
	}
}

// Profile name is a path segment — separators/dots are rejected.
func TestIdentityProfileValidation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ANTHROPIC_CONFIG_DIR", dir)
	for _, p := range []string{"../escape", "a/b", "..", "bad name"} {
		if err := validProfile(p); err == nil {
			t.Fatalf("profile %q should be rejected", p)
		}
	}
}

func TestIdentityAPIKeySkips(t *testing.T) {
	t.Setenv("ANTHROPIC_FEDERATION_RULE_ID", "rule-1")
	t.Setenv("ANTHROPIC_ORGANIZATION_ID", "org-1")
	t.Setenv("ANTHROPIC_IDENTITY_TOKEN", "id-jwt")
	t.Setenv("ANTHROPIC_CONFIG_DIR", t.TempDir())
	c := &Client{Provider: ProviderFirstParty,
		Creds: &auth.Credentials{AccessToken: "ak-1", IsAPIKey: true}}
	req := newReq(t)
	if err := c.applyAuth(req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("x-api-key"); got != "ak-1" {
		t.Fatalf("x-api-key %q (identity should not run with an API key)", got)
	}
}

func TestMergeExtraBody(t *testing.T) {
	raw := []byte(`{"model":"m","max_tokens":8}`)
	t.Run("object merge", func(t *testing.T) {
		t.Setenv("CLAUDE_CODE_EXTRA_BODY", `{"tools_extra":"x","betas":"a-1, afk-mode-2026-01-31 ,b-2"}`)
		out := mergeExtraBody(raw)
		var o map[string]any
		json.Unmarshal(out, &o)
		if o["tools_extra"] != "x" {
			t.Fatalf("body %v", o)
		}
		got := o["betas"].([]any)
		if len(got) != 2 || got[0] != "a-1" || got[1] != "b-2" {
			t.Fatalf("betas %v", got)
		}
	})
	t.Run("non-object ignored", func(t *testing.T) {
		t.Setenv("CLAUDE_CODE_EXTRA_BODY", `[1,2]`)
		out := mergeExtraBody(raw)
		var o map[string]any
		json.Unmarshal(out, &o)
		if len(o) != 2 {
			t.Fatalf("body %v", o)
		}
	})
	t.Run("invalid ignored", func(t *testing.T) {
		t.Setenv("CLAUDE_CODE_EXTRA_BODY", `{bad`)
		if string(mergeExtraBody(raw)) != string(raw) {
			t.Fatal("should pass through")
		}
	})
}
