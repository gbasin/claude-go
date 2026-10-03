package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

// RemoteConfigSource answers tengu_* remote-config flags. Real reads
// GrowthBook (getFeatureValue_CACHED_MAY_BE_STALE / qU with a 300s disk
// cache); claude-go has no GrowthBook client, so the default source returns
// no value and the documented defaults apply. Inject a source in tests or
// when a real flag feed exists.
//
// Flag names used:
//   - "tengu_bridge_repl_v2_config"      (mod_1215.js; in-session connect)
//   - "tengu_bridge_poll_interval_config" (mod_1211.js; server poll cadence)
var RemoteConfigSource = func(flag string) (map[string]any, bool) {
	return nil, false
}

// ---------------------------------------------------------------------------
// tengu_bridge_repl_v2_config (mod_1215.js Xi/Xs) — connect/reattach timing.
// ---------------------------------------------------------------------------

type REPLConfig struct {
	InitRetryMaxAttempts     int     `json:"init_retry_max_attempts"`     // 1..10,   def 3
	InitRetryBaseDelayMs     int     `json:"init_retry_base_delay_ms"`    // >=100,   def 500
	InitRetryJitterFraction  float64 `json:"init_retry_jitter_fraction"`  // 0..1,    def 0.25
	InitRetryMaxDelayMs      int     `json:"init_retry_max_delay_ms"`     // >=500,   def 4000
	HTTPTimeoutMs            int     `json:"http_timeout_ms"`             // >=2000,  def 10000
	UUIDDedupBufferSize      int     `json:"uuid_dedup_buffer_size"`      // 100..50k def 2000
	HeartbeatIntervalMs      int     `json:"heartbeat_interval_ms"`       // 5s..30s, def 20000
	HeartbeatJitterFraction  float64 `json:"heartbeat_jitter_fraction"`   // 0..0.5,  def 0.1
	TokenRefreshBufferMs     int     `json:"token_refresh_buffer_ms"`     // 30s..30m def 300000
	TeardownArchiveTimeoutMs int     `json:"teardown_archive_timeout_ms"` // 500..2s  def 1500
	ConnectTimeoutMs         int     `json:"connect_timeout_ms"`          // 5s..60s, def 15000
	OAuthRetryMaxAttempts    int     `json:"oauth_retry_max_attempts"`    // 0..6,    def 3
	OAuthRetryBaseDelayMs    int     `json:"oauth_retry_base_delay_ms"`   // 100..10s def 2000
	MinVersion               string  `json:"min_version"`                 // semver,  def "0.0.0"
}

// DefaultREPLConfig is the Xi defaults table verbatim.
func DefaultREPLConfig() REPLConfig {
	return REPLConfig{
		InitRetryMaxAttempts:     3,
		InitRetryBaseDelayMs:     500,
		InitRetryJitterFraction:  0.25,
		InitRetryMaxDelayMs:      4000,
		HTTPTimeoutMs:            10000,
		UUIDDedupBufferSize:      2000,
		HeartbeatIntervalMs:      20000,
		HeartbeatJitterFraction:  0.1,
		TokenRefreshBufferMs:     300000,
		TeardownArchiveTimeoutMs: 1500,
		ConnectTimeoutMs:         15000,
		OAuthRetryMaxAttempts:    3,
		OAuthRetryBaseDelayMs:    2000,
		MinVersion:               "0.0.0",
	}
}

// LoadREPLConfig overlays the remote flag onto defaults (bn()).
func LoadREPLConfig() REPLConfig {
	cfg := DefaultREPLConfig()
	if m, ok := RemoteConfigSource("tengu_bridge_repl_v2_config"); ok {
		overlayJSON(&cfg, m)
	}
	return cfg
}

// ---------------------------------------------------------------------------
// tengu_bridge_poll_interval_config (mod_1211.js r/l) — server cadence.
// ---------------------------------------------------------------------------

type PollConfig struct {
	// >0 enables heartbeat-mode while at capacity (real default 0 = off).
	NonExclusiveHeartbeatIntervalMs int `json:"non_exclusive_heartbeat_interval_ms"`
	// Poll cadence by occupancy (server-side long poll hint we send back
	// as poll_interval_ms).
	PollIntervalNotAtCapacityMs   int `json:"multisession_poll_interval_ms_not_at_capacity"`  // def 2000
	PollIntervalPartialCapacityMs int `json:"multisession_poll_interval_ms_partial_capacity"` // def 2000
	PollIntervalAtCapacityMs      int `json:"multisession_poll_interval_ms_at_capacity"`      // def 600000
	ReclaimOlderThanMs            int `json:"reclaim_older_than_ms"`                          // def 5000
	SessionKeepaliveIntervalV2Ms  int `json:"session_keepalive_interval_v2_ms"`               // def 120000
	SSEConnectTimeoutMs           int `json:"sse_connect_timeout_ms"`                         // def 30000
}

func DefaultPollConfig() PollConfig {
	return PollConfig{
		NonExclusiveHeartbeatIntervalMs: 0,
		PollIntervalNotAtCapacityMs:     2000,
		PollIntervalPartialCapacityMs:   2000,
		PollIntervalAtCapacityMs:        600000,
		ReclaimOlderThanMs:              5000,
		SessionKeepaliveIntervalV2Ms:    120000,
		SSEConnectTimeoutMs:             30000,
	}
}

// LoadPollConfig overlays the remote flag onto defaults (f5()).
// The real schema refuses the remote value when both at-capacity lanes are
// disabled; we mirror that guard.
func LoadPollConfig() PollConfig {
	cfg := DefaultPollConfig()
	if m, ok := RemoteConfigSource("tengu_bridge_poll_interval_config"); ok {
		candidate := cfg
		overlayJSON(&candidate, m)
		if candidate.NonExclusiveHeartbeatIntervalMs > 0 ||
			candidate.PollIntervalAtCapacityMs > 0 {
			cfg = candidate
		}
	}
	return cfg
}

// pollIntervalFor mirrors the occupancy switch in the work loop.
func (p PollConfig) pollIntervalFor(activeSessions, maxSessions int) int64 {
	switch {
	case maxSessions > 0 && activeSessions >= maxSessions:
		return int64(p.PollIntervalAtCapacityMs)
	case activeSessions > 0:
		return int64(p.PollIntervalPartialCapacityMs)
	default:
		return int64(p.PollIntervalNotAtCapacityMs)
	}
}

func overlayJSON(dst any, m map[string]any) {
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	json.Unmarshal(b, dst)
}

// ---------------------------------------------------------------------------
// Connection pacing (mod_1269.js Fr / jr / Lr / Hr / Er / Ar / Pr) and the
// retry budgets from mod_1258.js (ZJt, eQt) + secret-renewal math (HLn).
// ---------------------------------------------------------------------------

// Pacing is Fr: connect-phase and general retry pacing for the poll loop.
type Pacing struct {
	ConnInitialMs    int // 2000
	ConnCapMs        int // 120000
	ConnGiveUpMs     int // 600000
	GeneralInitialMs int // 500
	GeneralCapMs     int // 30000
	GeneralGiveUpMs  int // 600000
}

func DefaultPacing() Pacing {
	return Pacing{
		ConnInitialMs:    2000,
		ConnCapMs:        120000,
		ConnGiveUpMs:     600000,
		GeneralInitialMs: 500,
		GeneralCapMs:     30000,
		GeneralGiveUpMs:  600000,
	}
}

// giveUpAfter mirrors Pr(e) = connCapMs * 2.
func (p Pacing) giveUpAfter() time.Duration {
	return time.Duration(p.ConnCapMs*2) * time.Millisecond
}

// ReRegisterMaxAttempts = ZJt (3): consecutive re-register attempts before
// the poll loop gives up on the environment.
const ReRegisterMaxAttempts = 3

// RequeueMaxAttempts = eQt (5): per-session reconnectSession retries after
// an in-place re-register before the session is left to token refresh.
const RequeueMaxAttempts = 5

// NextBackoff computes the connect-phase retry delay: real sleeps
// min(max(elapsed-since-last-attempt adjusted, connInitial), connCap) and
// doubles on consecutive failures — i.e. delay = base * 2^failures, floored
// at connInitial, capped at connCap. failures is the count so far (0-based).
func (p Pacing) NextBackoff(failures int) time.Duration {
	d := p.ConnInitialMs
	for i := 0; i < failures && d < p.ConnCapMs; i++ {
		d *= 2
		if d > p.ConnCapMs {
			d = p.ConnCapMs
		}
	}
	if d > p.ConnCapMs {
		d = p.ConnCapMs
	}
	return time.Duration(d) * time.Millisecond
}

// secret-renewal schedule (HLn in mod_1258.js):
//
//	b = min(max(lifetime/10, 15min), 24h) * (1 + 0.25*rand)
//	renewAt = now + lifetime - b   (only when lifetime >= 1h)
const (
	secretRenewalMinFloorMs = 900000   // N  = 15min
	secretRenewalMaxCapMs   = 86400000 // B  = 24h
	secretRenewalJitter     = 0.25     // H
	secretRenewalMinLifeMs  = 3600000  // j  = 1h; shorter-lived secrets
	// never renew proactively
)

// SecretRenewalAt mirrors HLn(environment_secret_expires_in): the timestamp
// (unix ms) at which the environment secret should be proactively renewed,
// or 0 when the lifetime is too short to bother.
func SecretRenewalAt(expiresInSec int, nowMs int64, rand float64) int64 {
	lifeMs := int64(expiresInSec) * 1000
	if lifeMs <= 0 || lifeMs < secretRenewalMinLifeMs {
		return 0
	}
	b := lifeMs / 10
	if b < secretRenewalMinFloorMs {
		b = secretRenewalMinFloorMs
	}
	if b > secretRenewalMaxCapMs {
		b = secretRenewalMaxCapMs
	}
	return nowMs + lifeMs - int64(float64(b)*(1+secretRenewalJitter*rand))
}

// ---------------------------------------------------------------------------
// Work secret decode (Ute in mod_1211.js).
// ---------------------------------------------------------------------------

// WorkSecret is the decoded `secret` field of a work item: base64url JSON
// {version:1, session_ingress_token, api_base_url, claude_code_args?,
//
//	mcp_config?, auto_mode_environment?, worktree?}.
type WorkSecret struct {
	Version             int    `json:"version"`
	SessionIngressToken string `json:"session_ingress_token"`
	APIBaseURL          string `json:"api_base_url"`
	ClaudeCodeArgs      any    `json:"claude_code_args,omitempty"`
	MCPConfig           *struct {
		Content string `json:"content"`
		Path    string `json:"path,omitempty"`
		Mode    int    `json:"mode,omitempty"`
	} `json:"mcp_config,omitempty"`
	AutoModeEnvironment any `json:"auto_mode_environment,omitempty"`
	Worktree            any `json:"worktree,omitempty"`
}

// WorkSecretShapeError mirrors pAe — verdict strings verbatim.
type WorkSecretShapeError struct {
	Verdict string
}

func (e *WorkSecretShapeError) Error() string {
	switch e.Verdict {
	case "secret_did_not_decode":
		return "Work secret is not valid JSON"
	case "unsupported_version":
		return "Unsupported work secret version"
	case "missing_session_ingress_token":
		return "Invalid work secret: missing or empty session_ingress_token"
	case "missing_api_base_url":
		return "Invalid work secret: missing api_base_url"
	}
	return e.Verdict
}

// DecodeWorkSecret mirrors Ute: base64url -> JSON, then the verdict ladder
// (shape -> version -> session_ingress_token -> api_base_url).
func DecodeWorkSecret(secret string) (*WorkSecret, error) {
	raw, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil {
		if raw, err = base64.URLEncoding.DecodeString(secret); err != nil {
			return nil, &WorkSecretShapeError{Verdict: "secret_did_not_decode"}
		}
	}
	var probe struct {
		Version             int    `json:"version"`
		SessionIngressToken string `json:"session_ingress_token"`
		APIBaseURL          string `json:"api_base_url"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, &WorkSecretShapeError{Verdict: "secret_did_not_decode"}
	}
	if probe.Version != 1 {
		return nil, &WorkSecretShapeError{Verdict: "unsupported_version"}
	}
	if probe.SessionIngressToken == "" {
		return nil, &WorkSecretShapeError{Verdict: "missing_session_ingress_token"}
	}
	if probe.APIBaseURL == "" {
		return nil, &WorkSecretShapeError{Verdict: "missing_api_base_url"}
	}
	var out WorkSecret
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &WorkSecretShapeError{Verdict: "secret_did_not_decode"}
	}
	return &out, nil
}

// sleepCtx is ee(): interruptible sleep. Returns false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

var errAborted = errors.New("aborted")
