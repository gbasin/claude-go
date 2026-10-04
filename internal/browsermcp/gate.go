package browsermcp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// gate.go — the startup entitlement check, mirroring W()/R()/T()/Y() in
// mod_0807.js and the policy fetch in mod_0450.js y3r:
//
//	R(): CLAUDE_CODE_CHROME_MCP_ORG_DENIED → denied("parent_org_policy");
//	     non-first-party provider → unverified("third_party_provider");
//	     custom ANTHROPIC_BASE_URL / -local-oauth → unverified("custom_base_url")
//	W(): refresh creds → accessToken; none → unverified("no_oauth_token");
//	     else T(token)
//	T(): GET {BASE_API_URL}/api/claude_code/policy_limits
//	     Authorization: Bearer <token>, anthropic-beta: oauth-2025-04-20,
//	     timeout 3000ms; unreadable → unverified("policy_unverified");
//	     restrictions.allow_claude_browser_extension.allowed decides.
//	Y(verified): bearerGate — memoized per-token re-check for tokens the
//	     bridge later presents.
//
// On denied the real server writes the disabled message to stderr and
// exits 1 (chrome_mcp_server_start telemetry + flush elided).

// PolicyAllowClaudeBrowserExtension is the policy_limits key (QWt arg).
const PolicyAllowClaudeBrowserExtension = "allow_claude_browser_extension"

// ErrOrgDenied is mod_0807.js L.
const ErrOrgDenied = "Claude in Chrome is disabled by your organization policy."

// gateTimeoutMs is z in mod_0807.js.
const gateTimeoutMs = 3000

// oauthValidateBeta is fp in mod_0450.js — the anthropic-beta flag on the
// policy_limits bearer read.
const oauthValidateBeta = "oauth-2025-04-20"

// GateResult mirrors {denied, cause, unverified, verified:{bearer}}.
type GateResult struct {
	Denied         bool
	Cause          string // "parent_org_policy" | "org_policy"
	Unverified     string // "" | "third_party_provider" | "custom_base_url" | "no_oauth_token" | "policy_unverified"
	VerifiedBearer string
}

// UnverifiedMessage mirrors B(r) — the stderr warning for each
// unverified cause.
func (g GateResult) UnverifiedMessage() string {
	switch g.Unverified {
	case "third_party_provider", "custom_base_url":
		return "Organization Claude in Chrome policy not read: third-party provider or custom ANTHROPIC_BASE_URL. Continuing; the extension enforces the org setting."
	case "no_oauth_token":
		return "Organization Claude in Chrome policy not read: no claude.ai sign-in. Continuing; the extension enforces the org setting."
	case "policy_unverified":
		return "Couldn't read your organization's Claude in Chrome policy (network or server error). Continuing; the extension enforces the org setting."
	}
	return ""
}

// GateDeps are injectable for tests; nil members fall back to real env.
type GateDeps struct {
	Provider     string // "firstParty" | other — api.DetectProvider() output
	BaseURL      string // resolved ANTHROPIC base URL ("" → default)
	OAuthToken   func() string
	HTTPClient   *http.Client
	PolicyFetch  func(token string, timeoutMs int) (*policyLimits, error)
	IsLocalOAuth bool // _V()==="-local-oauth" equivalent
}

// policyLimits mirrors XWt's parsed shape: {restrictions:{key:{allowed}},
// compliance_taints:[]}.
type policyLimits struct {
	Restrictions map[string]struct {
		Allowed bool `json:"allowed"`
	} `json:"restrictions"`
	ComplianceTaints []string `json:"compliance_taints"`
}

// policyAllowed mirrors the QWt slice we can verify: explicit restriction
// wins; absent key → allowed (the taint/hint tables real consults are
// bundled elsewhere; absent-taints default is allow).
func (pl *policyLimits) policyAllowed(key string) bool {
	if r, ok := pl.Restrictions[key]; ok {
		return r.Allowed
	}
	return true
}

// earlyGate mirrors R().
func earlyGate(d GateDeps) *GateResult {
	if os.Getenv("CLAUDE_CODE_CHROME_MCP_ORG_DENIED") != "" {
		return &GateResult{Denied: true, Cause: "parent_org_policy"}
	}
	if d.Provider != "" && d.Provider != "firstParty" {
		return &GateResult{Denied: false, Unverified: "third_party_provider"}
	}
	if isCustomBaseURL(d.BaseURL) || d.IsLocalOAuth {
		return &GateResult{Denied: false, Unverified: "custom_base_url"}
	}
	return nil
}

// isCustomBaseURL mirrors !ig(): any ANTHROPIC_BASE_URL that isn't the
// first-party default disables the policy read (the extension enforces).
func isCustomBaseURL(base string) bool {
	if base == "" {
		return false
	}
	u := strings.TrimRight(base, "/")
	return u != "https://api.anthropic.com"
}

// Gate mirrors W(): env/provider early-outs, token, then the policy read.
func Gate(d GateDeps) GateResult {
	if r := earlyGate(d); r != nil {
		return *r
	}
	token := ""
	if d.OAuthToken != nil {
		token = d.OAuthToken()
	}
	if token == "" {
		return GateResult{Unverified: "no_oauth_token"}
	}
	return checkPolicy(d, token)
}

// checkPolicy mirrors T().
func checkPolicy(d GateDeps, token string) GateResult {
	if r := earlyGate(d); r != nil {
		return *r
	}
	fetch := d.PolicyFetch
	if fetch == nil {
		fetch = func(t string, timeoutMs int) (*policyLimits, error) {
			return fetchPolicyLimits(d, t, timeoutMs)
		}
	}
	pl, err := fetch(token, gateTimeoutMs)
	if err != nil || pl == nil {
		return GateResult{Unverified: "policy_unverified"}
	}
	if pl.policyAllowed(PolicyAllowClaudeBrowserExtension) {
		return GateResult{VerifiedBearer: token}
	}
	return GateResult{Denied: true, Cause: "org_policy"}
}

// fetchPolicyLimits mirrors y3r's GET: {base}/api/claude_code/policy_limits
// with Bearer + anthropic-beta header, acceptAny2xx, 304 → nil.
func fetchPolicyLimits(d GateDeps, token string, timeoutMs int) (*policyLimits, error) {
	base := strings.TrimRight(d.BaseURL, "/")
	if base == "" {
		base = "https://api.anthropic.com"
	}
	req, err := http.NewRequest(http.MethodGet, base+"/api/claude_code/policy_limits", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", oauthValidateBeta)
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	hc := d.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: time.Duration(timeoutMs) * time.Millisecond}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 304 {
		return nil, nil // not_modified → real treats as no usable verdict
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("policy_limits http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var pl policyLimits
	if err := json.Unmarshal(body, &pl); err != nil {
		return nil, fmt.Errorf("policy_limits: invalid response format")
	}
	return &pl, nil
}

// BearerGate mirrors Y(r): the callback deciding whether a bearer may be
// handed to the bridge socket. The startup-verified token passes; other
// tokens re-run the policy check (denials memoized).
type BearerGate struct {
	verifiedBearer string
	deny           map[string]bool
	deps           GateDeps
	log            Logger
	mu             sync.Mutex
}

func NewBearerGate(verified string, d GateDeps, log Logger) *BearerGate {
	return &BearerGate{verifiedBearer: verified, deny: map[string]bool{}, deps: d, log: log}
}

// Allow mirrors the Y() closure.
func (g *BearerGate) Allow(token string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if token == g.verifiedBearer {
		return true
	}
	if g.deny[token] {
		return false
	}
	res := checkPolicy(g.deps, token)
	if res.Denied {
		g.deny[token] = true
		g.log.Warnf("%s (bridge token withheld)", ErrOrgDenied)
		return false
	}
	if res.VerifiedBearer != "" {
		g.verifiedBearer = res.VerifiedBearer
	} else if res.Unverified != "" {
		g.log.Warnf("%s", GateResult{Unverified: res.Unverified}.UnverifiedMessage())
	}
	return true
}

// validateTokenAccountUUID mirrors the token-derived branch of getUserId:
// POST {base}/api/oauth/validate → account_uuid.
func validateTokenAccountUUID(token string) string {
	if token == "" {
		return ""
	}
	base := strings.TrimRight(os.Getenv("ANTHROPIC_BASE_URL"), "/")
	if base == "" {
		base = "https://api.anthropic.com"
	}
	req, err := http.NewRequest(http.MethodPost, base+"/api/oauth/validate", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 10 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	var body struct {
		AccountUUID string `json:"account_uuid"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return ""
	}
	return strings.ToLower(body.AccountUUID)
}
