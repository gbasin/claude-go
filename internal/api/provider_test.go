package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"claude-go/internal/auth"
)

func TestDetectProvider(t *testing.T) {
	for _, tt := range []struct {
		env  string
		want Provider
	}{
		{"CLAUDE_CODE_USE_BEDROCK", ProviderBedrock},
		{"CLAUDE_CODE_USE_VERTEX", ProviderVertex},
		{"CLAUDE_CODE_USE_FOUNDRY", ProviderFoundry},
		{"CLAUDE_CODE_USE_GATEWAY", ProviderGateway},
	} {
		t.Setenv(tt.env, "1")
		if got := DetectProvider(); got != tt.want {
			t.Errorf("%s=1: got %q, want %q", tt.env, got, tt.want)
		}
		t.Setenv(tt.env, "")
	}
}

func TestBedrockRewrite(t *testing.T) {
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("AWS_REGION", "us-west-2")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "tok")
	c := NewClient(&auth.Credentials{AccessToken: "x"}, "s", EntrySDKCLI)
	if c.Provider != ProviderBedrock {
		t.Fatalf("provider %q", c.Provider)
	}
	if c.BaseURL != "https://bedrock-runtime.us-west-2.amazonaws.com" {
		t.Fatalf("base %q", c.BaseURL)
	}
	raw, _ := json.Marshal(&Request{Model: "claude-sonnet-5", MaxTokens: 8, Stream: true})
	url, body, err := c.rewriteRequest("/v1/messages?beta=true", raw)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://bedrock-runtime.us-west-2.amazonaws.com/model/claude-sonnet-5/invoke-with-response-stream"
	if url != want {
		t.Fatalf("url %q, want %q", url, want)
	}
	var o map[string]any
	json.Unmarshal(body, &o)
	if o["anthropic_version"] != "bedrock-2023-05-31" {
		t.Fatalf("anthropic_version %v", o["anthropic_version"])
	}
	if _, ok := o["model"]; ok {
		t.Fatal("model should be removed from body")
	}
	if _, ok := o["stream"]; ok {
		t.Fatal("stream should be removed from body")
	}
	betas, ok := o["anthropic_beta"].([]any)
	if !ok || len(betas) == 0 {
		t.Fatalf("anthropic_beta %v", o["anthropic_beta"])
	}
}

func TestBedrockAuthHeader(t *testing.T) {
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "  bearer-tok  ")
	c := NewClient(nil, "s", EntrySDKCLI)
	req, _ := http.NewRequest("POST", c.BaseURL+"/model/m/invoke", nil)
	c.headers(req)
	if err := c.applyAuth(req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer bearer-tok" {
		t.Fatalf("Authorization %q", got)
	}
	if req.Header.Get("anthropic-beta") != "" {
		t.Fatal("anthropic-beta header should be suppressed on bedrock")
	}
}

func TestBedrockNoBearerErrors(t *testing.T) {
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	c := NewClient(nil, "s", EntrySDKCLI)
	req, _ := http.NewRequest("POST", c.BaseURL+"/m", nil)
	if err := c.applyAuth(req); err == nil {
		t.Fatal("expected SigV4-not-implemented error")
	}
}

func TestVertexRewrite(t *testing.T) {
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "1")
	t.Setenv("CLOUD_ML_REGION", "us-east5")
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "proj-1")
	c := NewClient(nil, "s", EntrySDKCLI)
	if c.BaseURL != "https://us-east5-aiplatform.googleapis.com/v1" {
		t.Fatalf("base %q", c.BaseURL)
	}
	raw, _ := json.Marshal(&Request{Model: "claude-sonnet-5", MaxTokens: 8})
	url, body, err := c.rewriteRequest("/v1/messages?beta=true", raw)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://us-east5-aiplatform.googleapis.com/v1/projects/proj-1/" +
		"locations/us-east5/publishers/anthropic/models/claude-sonnet-5:rawPredict"
	if url != want {
		t.Fatalf("url %q, want %q", url, want)
	}
	var o map[string]any
	json.Unmarshal(body, &o)
	if o["anthropic_version"] != "vertex-2023-10-16" {
		t.Fatalf("anthropic_version %v", o["anthropic_version"])
	}
}

func TestRateLimitParse(t *testing.T) {
	h := http.Header{
		"Anthropic-Ratelimit-Unified-Status":                  {"rejected"},
		"Anthropic-Ratelimit-Unified-Claim":                   {"five_hour", "seven_day"},
		"Anthropic-Ratelimit-Unified-Representative-Claim":    {"seven_day"},
		"Anthropic-Ratelimit-Unified-Reset":                   {"1759500000"},
		"Anthropic-Ratelimit-Unified-Overage-Status":          {"rejected"},
		"Anthropic-Ratelimit-Unified-Overage-Disabled-Reason": {"org_level_disabled_until"},
		"Anthropic-Ratelimit-Unified-Slow-Offer":              {"treatment"},
		"Anthropic-Ratelimit-Unified-Slow-Status":             {"active"},
		"Anthropic-Ratelimit-Unified-Slow-Retry-After":        {"12.5"},
		"Anthropic-Ratelimit-Unified-Slow-Max-Wait":           {"120"},
		"Anthropic-Ratelimit-Unified-Slow-Budget-Utilization": {"0.8"},
		"Anthropic-Usage-Limit":                               {"five_hour"},
		"Retry-After":                                         {"30"},
	}
	rl := parseRateLimitHeaders(h)
	if rl == nil {
		t.Fatal("nil RateLimit")
	}
	if rl.Status != "rejected" || rl.RepresentativeClaim != "seven_day" {
		t.Fatalf("status %q claim %q", rl.Status, rl.RepresentativeClaim)
	}
	if len(rl.Claims) != 2 || rl.Claims[0] != "five_hour" {
		t.Fatalf("claims %v", rl.Claims)
	}
	if rl.ResetsAt.Unix() != 1759500000 {
		t.Fatalf("reset %v", rl.ResetsAt)
	}
	if rl.SlowOffer != "treatment" || rl.SlowRetryAfter != 12.5 || rl.SlowMaxWait != 120 {
		t.Fatalf("slow %+v", rl)
	}
	if rl.SlowBudgetUtilization != 0.8 || rl.UsageLimit != "five_hour" || rl.RetryAfter != "30" {
		t.Fatalf("misc %+v", rl)
	}
	if !rl.HasUnifiedHeaders() {
		t.Fatal("expected unified headers")
	}
	if parseRateLimitHeaders(http.Header{}) != nil {
		t.Fatal("empty headers should produce nil")
	}
}

func TestRetryAfterDelay(t *testing.T) {
	h := http.Header{"Retry-After-Ms": {"250"}}
	if d, ok := retryAfterDelay(h, time.Now()); !ok || d != 250*time.Millisecond {
		t.Fatalf("retry-after-ms: %v %v", d, ok)
	}
	h = http.Header{"Retry-After": {"3"}}
	if d, ok := retryAfterDelay(h, time.Now()); !ok || d != 3*time.Second {
		t.Fatalf("retry-after secs: %v %v", d, ok)
	}
	if _, ok := retryAfterDelay(http.Header{}, time.Now()); ok {
		t.Fatal("no headers should be !ok")
	}
}

func TestSanitizable(t *testing.T) {
	cases := []struct {
		msg  string
		want SanitizeHint
	}{
		{"Advisor tool result content could not be processed", SanitizeAdvisor},
		{"found in advisor_tool_result blocks", SanitizeAdvisor},
		{"Invalid `signature` in `thinking` block", SanitizeThinking},
		{"invalid encrypted_content in search_result block", SanitizeEncrypted},
		{"failed to decrypt web search result content", SanitizeEncrypted},
		{"messages.0.content.1.clear_at: Extra inputs are not permitted", SanitizeClearAt},
		{"messages.0: turn-scoped system message not allowed", SanitizeClearAt},
		{"request contains unknown safeguards value", SanitizeSafeguards},
		{"400 invalid beta flag: foo-2026", SanitizeBetaFlag},
		{"credit balance too low", SanitizeNone},
	}
	for _, tt := range cases {
		e := &ErrAPI{E: &APIError{Type: "invalid_request_error", Message: tt.msg}, StatusCode: 400}
		if got := e.Sanitizable(); got != tt.want {
			t.Errorf("%q: got %q, want %q", tt.msg, got, tt.want)
		}
	}
	e := &ErrAPI{E: &APIError{Message: "x"}, StatusCode: 500}
	if e.Sanitizable() != SanitizeNone {
		t.Fatal("500 should never be sanitizable")
	}
}

func TestErrAPITypeMapping(t *testing.T) {
	e := &ErrAPI{E: &APIError{Message: "nope"}, StatusCode: 429}
	if e.Type() != "rate_limit_error" {
		t.Fatalf("type %q", e.Type())
	}
	e = &ErrAPI{E: &APIError{Type: "custom", Message: "x"}, StatusCode: 400}
	if e.Type() != "custom" {
		t.Fatalf("type %q", e.Type())
	}
}
