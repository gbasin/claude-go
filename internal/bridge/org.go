// Org-scoped and account endpoints extracted from Claude Code 2.1.288:
//   - mod_0963.js (Cwr/h): POST /v1/code/github/import-token and
//     GET /api/oauth/organizations/:orgUUID/sync/github/auth — the
//     "teleport-org" lane (OAuth bearer + x-organization-uuid).
//   - mod_1034.js (RAe/XSo): POST + DELETE /v1/design/consent with
//     {consent:"agent_design_projects"} — the /design-consent and
//     /design-revoke backends. GET /v1/design/consent reports the bits.
//   - mod_0278.js (CF/fetchUtilization): GET /api/oauth/usage — the
//     account usage row the /usage-credits UI reads (extra_usage +
//     limits blocks; the gateway-side shape contract is documented in
//     mod_1351.js).
//   - mod_0386.js (cGn/CDt): POST .../setup_overage_billing
//     {org_monthly_spend_limit:2000} then PUT .../overage_spend_limit
//     {is_enabled,...} — the "enable extra usage" write pair.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// teleportOrgHeaders builds headers for the `auth:"teleport-org"` lane:
// the code-session header set plus x-organization-uuid. ErrNoOrgUUID when
// the client has no org id (real requires z2() before issuing the call).
var ErrNoOrgUUID = &UserError{Message: "Unable to get organization UUID for constructing session URL"}

func (c *Client) teleportOrgHeaders(tok string) (http.Header, error) {
	if c.OrgUUID == "" {
		return nil, ErrNoOrgUUID
	}
	h, err := c.codeSessionHeaders(tok)
	if err != nil {
		return nil, err
	}
	h.Set("x-organization-uuid", c.OrgUUID)
	return h, nil
}

// ---------------------------------------------------------------------------
// GitHub token import + auth sync (mod_0963.js).
// ---------------------------------------------------------------------------

// ImportTokenResult mirrors Cwr's outcome union:
// {ok:true,result} | {ok:false,error:{kind:network|not_signed_in|
// invalid_token{message}|server{status}}}.
type ImportTokenResult struct {
	OK      bool
	Kind    string
	Message string // invalid_token: server message ($I(Cb(data)))
	Status  int    // server: http status
}

// ImportGitHubToken: POST /v1/code/github/import-token {token} —
// anthropic-beta: ccr-byoc-2025-07-29 (Tzt.header), teleport-org lane,
// 15s, validateStatus any.
func (c *Client) ImportGitHubToken(ctx context.Context, token string) *ImportTokenResult {
	r, err := c.withOAuthRetry("ImportGitHubToken", func(tok string) (*response, error) {
		h, herr := c.teleportOrgHeaders(tok)
		if herr != nil {
			return nil, herr
		}
		h.Set("anthropic-beta", betaCCRBYOC)
		return c.do(ctx, http.MethodPost,
			c.BaseURL+"/v1/code/github/import-token",
			map[string]any{"token": token}, h, 15*time.Second)
	})
	if err != nil {
		return &ImportTokenResult{Kind: "network", Message: err.Error()}
	}
	switch {
	case r.Status == 200:
		return &ImportTokenResult{OK: true}
	case r.Status == 400:
		return &ImportTokenResult{Kind: "invalid_token", Message: bodyErrorMessage(r.Body)}
	case r.Status == 401:
		return &ImportTokenResult{Kind: "not_signed_in"}
	default:
		c.debugf("[bridge:api] import-token returned %d", r.Status)
		return &ImportTokenResult{Kind: "server", Status: r.Status}
	}
}

// GitHubAuth is h()'s parsed response: {is_authenticated, auth_source}
// where auth_source is "oauth"|"cli_import" (other values → "").
type GitHubAuth struct {
	IsAuthenticated bool   `json:"is_authenticated"`
	AuthSource      string `json:"auth_source"`
}

// GitHubAuthStatus mirrors h(): GET
// /api/oauth/organizations/:orgUUID/sync/github/auth. nil on any failure
// (real's catch returns null).
func (c *Client) GitHubAuthStatus(ctx context.Context) *GitHubAuth {
	if c.OrgUUID == "" {
		return nil
	}
	r, err := c.withOAuthRetry("GitHubAuthStatus", func(tok string) (*response, error) {
		h, herr := c.teleportOrgHeaders(tok)
		if herr != nil {
			return nil, herr
		}
		return c.do(ctx, http.MethodGet,
			c.BaseURL+"/api/oauth/organizations/"+c.OrgUUID+"/sync/github/auth",
			nil, h, 10*time.Second)
	})
	if err != nil || r == nil || r.Status != 200 {
		return nil
	}
	var v GitHubAuth
	if json.Unmarshal(r.Body, &v) != nil {
		return nil
	}
	if v.AuthSource != "oauth" && v.AuthSource != "cli_import" {
		v.AuthSource = ""
	}
	return &v
}

// ---------------------------------------------------------------------------
// Design consent (mod_1034.js RAe/XSo/J3n).
// ---------------------------------------------------------------------------

// DesignConsentBit is the only recognized consent bit in 2.1.288.
const DesignConsentBit = "agent_design_projects"

// DesignConsentGet mirrors J3n(): GET /v1/design/consent — returns whether
// the agent_design_projects bit is present. nil,false on any failure
// (real caches null and proceeds to ask).
func (c *Client) DesignConsentGet(ctx context.Context) (bool, error) {
	r, err := c.withOAuthRetry("DesignConsentGet", func(tok string) (*response, error) {
		h, herr := c.codeSessionHeaders(tok)
		if herr != nil {
			return nil, herr
		}
		return c.do(ctx, http.MethodGet, c.BaseURL+"/v1/design/consent", nil, h, 15*time.Second)
	})
	if err != nil {
		return false, err
	}
	if r.Status != 200 {
		return false, raise(r.Status, r.Body, "DesignConsentGet", r.Headers)
	}
	var v map[string]bool
	if json.Unmarshal(r.Body, &v) != nil {
		return false, fmt.Errorf("DesignConsentGet: malformed response")
	}
	return v[DesignConsentBit], nil
}

// DesignConsentGrant mirrors RAe(): POST /v1/design/consent
// {consent:"agent_design_projects"}.
func (c *Client) DesignConsentGrant(ctx context.Context) error {
	r, err := c.withOAuthRetry("DesignConsentGrant", func(tok string) (*response, error) {
		h, herr := c.codeSessionHeaders(tok)
		if herr != nil {
			return nil, herr
		}
		return c.do(ctx, http.MethodPost, c.BaseURL+"/v1/design/consent",
			map[string]any{"consent": DesignConsentBit}, h, 15*time.Second)
	})
	if err != nil {
		return err
	}
	return raise(r.Status, r.Body, "DesignConsentGrant", r.Headers)
}

// DesignConsentRevoke mirrors XSo(): DELETE /v1/design/consent
// {consent:"agent_design_projects"} — 404 is treated as already revoked.
func (c *Client) DesignConsentRevoke(ctx context.Context) error {
	r, err := c.withOAuthRetry("DesignConsentRevoke", func(tok string) (*response, error) {
		h, herr := c.codeSessionHeaders(tok)
		if herr != nil {
			return nil, herr
		}
		return c.do(ctx, http.MethodDelete, c.BaseURL+"/v1/design/consent",
			map[string]any{"consent": DesignConsentBit}, h, 15*time.Second)
	})
	if err != nil {
		return err
	}
	if r.Status == 404 {
		return nil
	}
	return raise(r.Status, r.Body, "DesignConsentRevoke", r.Headers)
}

// ---------------------------------------------------------------------------
// Usage read (fetchUtilization, mod_0278.js) + overage enable
// (cGn/CDt, mod_0386.js).
// ---------------------------------------------------------------------------

// UsageInfo is the GET /api/oauth/usage body: plan windows plus the
// extra_usage block the /usage-credits UI renders (mod_0082.js schema +
// mod_1351.js contract: amounts in minor units of currency).
type UsageInfo struct {
	ExtraUsage *struct {
		IsEnabled    bool    `json:"is_enabled"`
		MonthlyLimit *int64  `json:"monthly_limit"` // cents; null => unlimited/off
		UsedCredits  *int64  `json:"used_credits"`  // cents
		Utilization  *int64  `json:"utilization"`   // percent
		Currency     *string `json:"currency"`
	} `json:"extra_usage"`
	Limits []struct {
		Kind     string `json:"kind"`
		Group    string `json:"group"`
		Percent  int    `json:"percent"`
		ResetsAt string `json:"resets_at"`
		Severity string `json:"severity"`
		IsActive bool   `json:"is_active"`
	} `json:"limits"`
	// CinderCove is the one-time credit grant block shown alongside.
	CinderCove json.RawMessage `json:"cinder_cove"`
}

// FetchUsage mirrors CF(e,"plain"): GET /api/oauth/usage, OAuth lane, 5s.
// A 404/empty body yields nil,nil like real's M.status===200 gate.
func (c *Client) FetchUsage(ctx context.Context) (*UsageInfo, error) {
	r, err := c.withOAuthRetry("FetchUsage", func(tok string) (*response, error) {
		h, herr := c.codeSessionHeaders(tok)
		if herr != nil {
			return nil, herr
		}
		return c.do(ctx, http.MethodGet, c.BaseURL+"/api/oauth/usage", nil, h, 5*time.Second)
	})
	if err != nil {
		return nil, err
	}
	if r.Status == 404 {
		return nil, nil
	}
	if r.Status != 200 {
		return nil, raise(r.Status, r.Body, "FetchUsage", r.Headers)
	}
	var v UsageInfo
	if json.Unmarshal(r.Body, &v) != nil {
		return nil, fmt.Errorf("FetchUsage: malformed response")
	}
	return &v, nil
}

// defaultOverageLimitCents is cOr (mod_0386.js): the fixed monthly cap the
// client sends when enabling overage billing.
const defaultOverageLimitCents = 2000

// EnableOverage mirrors cGn(): POST setup_overage_billing
// {org_monthly_spend_limit:2000}, then PUT overage_spend_limit
// {is_enabled:true}. Returns nil or the first failure.
func (c *Client) EnableOverage(ctx context.Context) error {
	if c.OrgUUID == "" {
		return ErrNoOrgUUID
	}
	r, err := c.withOAuthRetry("SetupOverageBilling", func(tok string) (*response, error) {
		h, herr := c.teleportOrgHeaders(tok)
		if herr != nil {
			return nil, herr
		}
		return c.do(ctx, http.MethodPost,
			c.BaseURL+"/api/oauth/organizations/"+c.OrgUUID+"/setup_overage_billing",
			map[string]any{"org_monthly_spend_limit": defaultOverageLimitCents}, h, 30*time.Second)
	})
	if err != nil {
		return err
	}
	if r.Status != 200 && r.Status != 201 && r.Status != 204 {
		return raise(r.Status, r.Body, "SetupOverageBilling", r.Headers)
	}
	r2, err := c.withOAuthRetry("OverageSpendLimit", func(tok string) (*response, error) {
		h, herr := c.teleportOrgHeaders(tok)
		if herr != nil {
			return nil, herr
		}
		return c.do(ctx, http.MethodPut,
			c.BaseURL+"/api/oauth/organizations/"+c.OrgUUID+"/overage_spend_limit",
			map[string]any{"is_enabled": true}, h, 15*time.Second)
	})
	if err != nil {
		return err
	}
	if r2.Status != 200 && r2.Status != 201 && r2.Status != 204 {
		return raise(r2.Status, r2.Body, "OverageSpendLimit", r2.Headers)
	}
	return nil
}

// DisableOverage mirrors the is_enabled:false arm of CDt() — PUT
// overage_spend_limit {is_enabled:false}.
func (c *Client) DisableOverage(ctx context.Context) error {
	if c.OrgUUID == "" {
		return ErrNoOrgUUID
	}
	r, err := c.withOAuthRetry("OverageSpendLimit", func(tok string) (*response, error) {
		h, herr := c.teleportOrgHeaders(tok)
		if herr != nil {
			return nil, herr
		}
		return c.do(ctx, http.MethodPut,
			c.BaseURL+"/api/oauth/organizations/"+c.OrgUUID+"/overage_spend_limit",
			map[string]any{"is_enabled": false}, h, 15*time.Second)
	})
	if err != nil {
		return err
	}
	if r.Status != 200 && r.Status != 201 && r.Status != 204 {
		return raise(r.Status, r.Body, "OverageSpendLimit", r.Headers)
	}
	return nil
}
