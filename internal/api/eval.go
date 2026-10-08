package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"runtime"

	"claude-go/internal/config"
	"time"
)

// evalSDKKey is the GrowthBook client key embedded in the binary
// (captured: /api/eval/sdk-zAZezfDKGoZuXXKe, 2.1.289).
const evalSDKKey = "sdk-zAZezfDKGoZuXXKe"

// EvalAttributes is real's GB eval payload — verbatim field names from a
// live capture. Values come from .claude.json's auth/account blocks.
type EvalAttributes struct {
	ID                    string `json:"id"` // device_id
	SessionID             string `json:"sessionId"`
	DeviceID              string `json:"deviceID"`
	Platform              string `json:"platform"`
	OrganizationUUID      string `json:"organizationUUID,omitempty"`
	AccountUUID           string `json:"accountUUID,omitempty"`
	UserType              string `json:"userType"`
	SubscriptionType      string `json:"subscriptionType,omitempty"`
	RateLimitTier         string `json:"rateLimitTier,omitempty"`
	OrganizationRole      string `json:"organizationRole,omitempty"`
	SubscriptionCreatedAt int64  `json:"subscriptionCreatedAt,omitempty"`
	FirstTokenTime        int64  `json:"firstTokenTime,omitempty"`
	AppVersion            string `json:"appVersion"`
	Entrypoint            string `json:"entrypoint"`
	HasRemoteEnvironment  bool   `json:"hasRemoteEnvironment"`
}

type evalRequest struct {
	Attributes       EvalAttributes `json:"attributes"`
	ForcedVariations map[string]any `json:"forcedVariations"`
	ForcedFeatures   []any          `json:"forcedFeatures"`
	URL              string         `json:"url"`
}

// EvalCheck fires real's startup flag-eval call:
// POST https://api.anthropic.com/api/eval-authed/{sdkKey} (authed lane —
// real's rO apiHost is the hardcoded first-party host, unaffected by
// ANTHROPIC_BASE_URL), falling back to /api/eval/{sdkKey} — UA
// claude-code/<v>, beta oauth-2025-04-20, Accept */* — and merges the
// feature map into .claude.json's cachedGrowthBookFeatures exactly
// like real. Returns the feature map so
// callers can gate flag-dependent wire behavior; err is non-nil when the
// eval can't be performed (real then runs on flag defaults — the
// "eval-failed" header set).
func (c *Client) EvalCheck(ctx context.Context, ua string) (map[string]any, error) {
	if c.Creds == nil || c.Creds.IsAPIKey {
		return nil, fmt.Errorf("eval skipped: no oauth credentials")
	}
	tok, err := c.Creds.Token()
	if err != nil {
		return nil, err
	}
	_, acctUUID, devID := config.Account()
	oa := config.OAuthAccount()
	attrs := EvalAttributes{
		ID:               devID,
		SessionID:        c.SessionID,
		DeviceID:         devID,
		Platform:         runtime.GOOS,
		AccountUUID:      acctUUID,
		UserType:         "external",
		SubscriptionType: c.Creds.SubscriptionType,
		AppVersion:       Version,
		Entrypoint:       string(c.Entrypoint),
		OrganizationUUID: oa.OrganizationUUID,
		OrganizationRole: oa.OrganizationRole,
		RateLimitTier:    oa.OrganizationRateLimitTier,
	}
	if t, err := time.Parse(time.RFC3339Nano, oa.SubscriptionCreatedAt); err == nil {
		attrs.SubscriptionCreatedAt = t.UnixMilli()
	}
	attrs.FirstTokenTime = config.FirstTokenTime()
	body, _ := json.Marshal(evalRequest{
		Attributes:       attrs,
		ForcedVariations: map[string]any{},
		ForcedFeatures:   []any{},
	})
	url := c.evalBase() + "/api/eval-authed/" + evalSDKKey
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-code/"+Version)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "*/*")
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := c.HTTP.Do(req.WithContext(ctx2))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("eval http %d", resp.StatusCode)
	}
	var parsed struct {
		Features map[string]any `json:"features"`
	}
	if json.Unmarshal(raw, &parsed) == nil {
		if len(parsed.Features) > 0 {
			config.MergeCachedGBFeatures(parsed.Features)
			return parsed.Features, nil
		}
		// empty-feature 200: real treats this as eval-failed — the
		// default-on header/beta set appears (MITM-verified)
		return map[string]any{}, nil
	}
	return nil, fmt.Errorf("eval: unparseable body")
}

// evalBase is always the first-party API host — real's GB client does
// NOT honor ANTHROPIC_BASE_URL (verified: eval kept hitting prod while
// messages were redirected, and headers stayed production-shaped).
func (c *Client) evalBase() string {
	return "https://api.anthropic.com"
}

// MarkEvalFailed records the empty-features eval outcome — real switches
// to the fallback header/beta set (MITM-verified).
func (c *Client) MarkEvalFailed(f bool) {
	c.mu.Lock()
	c.evalFailed = f
	c.mu.Unlock()
}

// evalDone closes when the startup eval resolves (ok or failed).
var evalDone = make(chan struct{})
var evalDoneOnce sync.Once

// EvalAwait waits for the startup eval with real's bounded init window.
// The loop calls this before the first request so flag-gated surface is
// deterministic; a still-pending eval after the cap counts as failed.
func (c *Client) EvalAwait() {
	select {
	case <-evalDone:
	case <-time.After(4 * time.Second):
		c.MarkEvalFailed(true)
	}
}

// CloseEvalDone releases EvalAwait — called once the eval outcome has
// been marked (MarkEvalFailed first, so the flag state is visible).
func CloseEvalDone() {
	evalDoneOnce.Do(func() { close(evalDone) })
}

// EvalFailed reports whether the flag eval is known-bad — real drops
// GB-gated wire surface (dangerous-tool-use beta, safeguards) in that
// state.
func (c *Client) EvalFailed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.evalFailed
}
