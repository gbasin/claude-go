package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Header values extracted from the bundle:
//   - anthropic-version pinned at 2023-06-01 (mod_1258.js, mod_0829.js)
//   - bridge env API carries anthropic-beta: environments-2025-11-01
//     (O8r = w("environments","environments-2025-11-01"), mod_0082.js)
//   - v1-compat session CRUD carries anthropic-beta: ccr-byoc-2025-07-29
//     (Tzt) plus x-organization-uuid
//   - User-Agent is ko() = `claude-code/{VERSION}` (mod_0005.js)
//   - anthropic-client-platform is Fh() = "claude_code_cli" for the CLI
//     entrypoint (mod_0005.js)
const (
	anthropicVersion      = "2023-06-01"
	betaEnvironments      = "environments-2025-11-01"
	betaCCRBYOC           = "ccr-byoc-2025-07-29"
	headerRunnerVersion   = "x-environment-runner-version"
	headerTrustedDevice   = "X-Trusted-Device-Token"
	headerClientPlatform  = "anthropic-client-platform"
	defaultClientPlatform = "claude_code_cli"
)

// Client is the Remote Control HTTP surface (ySr / the code-session helpers
// in mod_0829.js). Two authentication lanes exist and they are not
// interchangeable:
//
//   - OAuth lane (withOAuthRetry, real's `w`): environment register /
//     deregister, bridge/reconnect, session archive. Bearer = claude.ai
//     OAuth token; a 401 triggers one OnAuth401 refresh + retry.
//   - Environment-secret lane: work poll/ack/stop/heartbeat and
//     markEnvironmentOffline. Bearer = the environment_secret issued by
//     registration (or a per-work session ingress token on ack).
//
// GetAccessToken / OnAuth401 are only invoked on the OAuth lane.
type Client struct {
	BaseURL       string // e.g. https://api.anthropic.com (no trailing slash)
	RunnerVersion string // sent as x-environment-runner-version
	UserAgent     string // default: claude-code/{RunnerVersion}

	// OAuth lane.
	GetAccessToken func() string              // required for OAuth-lane calls
	OnAuth401      func(oldToken string) bool // refresh; true => retry once

	// Optional trusted-device proof (X-Trusted-Device-Token).
	GetTrustedDeviceToken func() (string, error)

	// Session CRUD flavor: v2 (/v1/code/sessions, cse_ ids) is the default;
	// set false + OrgUUID for the v1-compat path (/v1/sessions, session_
	// ids, x-organization-uuid + ccr-byoc beta).
	UseCcrV2SessionCrud func() bool
	OrgUUID             string

	HTTP    *http.Client
	OnDebug func(string)
}

func (c *Client) debugf(format string, args ...any) {
	if c.OnDebug != nil {
		c.OnDebug(fmt.Sprintf(format, args...))
	}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) userAgent() string {
	if c.UserAgent != "" {
		return c.UserAgent
	}
	return "claude-code/" + c.RunnerVersion
}

// oauthToken mirrors real's b(): a missing token is the "no_token" error.
func (c *Client) oauthToken() (string, error) {
	if c.GetAccessToken == nil {
		return "", ErrNoToken
	}
	t := c.GetAccessToken()
	if t == "" {
		return "", ErrNoToken
	}
	return t, nil
}

// envHeaders builds the u() header set for the environment-bridge API.
func (c *Client) envHeaders(bearer string) (http.Header, error) {
	h := http.Header{
		"Authorization":             {"Bearer " + bearer},
		"Content-Type":              {"application/json"},
		"anthropic-version":         {anthropicVersion},
		"anthropic-beta":            {betaEnvironments},
		headerRunnerVersion:         {c.RunnerVersion},
		"User-Agent":                {c.userAgent()},
		"anthropic-client-platform": {defaultClientPlatform},
	}
	if c.GetTrustedDeviceToken != nil {
		if t, err := c.GetTrustedDeviceToken(); err == nil && t != "" {
			h.Set(headerTrustedDevice, t)
		}
	}
	return h, nil
}

// codeSessionHeaders builds the c_t() header set for /v1/code/sessions.
func (c *Client) codeSessionHeaders(bearer string) (http.Header, error) {
	h := http.Header{
		"Authorization":      {"Bearer " + bearer},
		"Content-Type":       {"application/json"},
		"anthropic-version":  {anthropicVersion},
		"User-Agent":         {c.userAgent()},
		headerClientPlatform: {defaultClientPlatform},
	}
	if c.GetTrustedDeviceToken != nil {
		if t, err := c.GetTrustedDeviceToken(); err == nil && t != "" {
			h.Set(headerTrustedDevice, t)
		}
	}
	return h, nil
}

func (c *Client) useV2() bool {
	if c.UseCcrV2SessionCrud == nil {
		return true
	}
	return c.UseCcrV2SessionCrud()
}

type response struct {
	Status  int
	Body    []byte
	Headers http.Header
}

// do issues one request; validateStatus accepts anything < 500 like real's
// axios validateStatus, so >=500 lands here as an error-free 5xx response
// that raise() maps to a (transient) statusError.
func (c *Client) do(ctx context.Context, method, url string, body any, h http.Header, timeout time.Duration) (*response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	for k, vs := range h {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	return &response{Status: resp.StatusCode, Body: b, Headers: resp.Header}, nil
}

// withOAuthRetry mirrors w(): run fn with the OAuth token; on 401 invoke
// OnAuth401 once and retry with the refreshed token.
func (c *Client) withOAuthRetry(label string, fn func(token string) (*response, error)) (*response, error) {
	tok, err := c.oauthToken()
	if err != nil {
		return nil, err
	}
	r, err := fn(tok)
	if err != nil || r.Status != 401 {
		return r, err
	}
	if c.OnAuth401 == nil {
		c.debugf("[bridge:api] %s: 401 received, no refresh handler", label)
		return r, nil
	}
	c.debugf("[bridge:api] %s: 401 received, attempting token refresh", label)
	if !c.OnAuth401(tok) {
		c.debugf("[bridge:api] %s: Token refresh failed", label)
		return r, nil
	}
	c.debugf("[bridge:api] %s: Token refreshed, retrying request", label)
	tok2, err := c.oauthToken()
	if err != nil {
		return nil, err
	}
	r2, err := fn(tok2)
	if err != nil || r2.Status != 401 {
		return r2, err
	}
	c.debugf("[bridge:api] %s: Retry after refresh also got 401", label)
	return r, nil // real returns the original response
}

// bodyErrorMessage mirrors Cb(): dig error.message out of a response body.
func bodyErrorMessage(b []byte) string {
	var v struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &v) == nil {
		return v.Error.Message
	}
	return ""
}

// bodyErrorType mirrors M(): dig error.type out of a response body.
func bodyErrorType(b []byte) string {
	var v struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &v) == nil {
		return v.Error.Type
	}
	return ""
}

// raise mirrors A(): 200/204 pass; mapped statuses become FatalError;
// 429 becomes RateLimitError; everything else is a transient statusError.
func raise(status int, body []byte, label string, headers http.Header) error {
	if status == 200 || status == 204 {
		return nil
	}
	msg := bodyErrorMessage(body)
	etype := bodyErrorType(body)
	suffix := ""
	if msg != "" {
		suffix = ": " + msg
	}
	switch status {
	case 401:
		return &FatalError{Status: status, ErrorType: etype,
			Message: fmt.Sprintf("%s: Authentication failed (401)%s. %s", label, suffix, ErrNoOAuthToken)}
	case 403:
		m := fmt.Sprintf("%s: Access denied (403)%s. Check your organization permissions.", label, suffix)
		if isExpiredType(etype) {
			m = "Remote Control session expired."
		}
		return &FatalError{Status: status, ErrorType: etype, Message: m}
	case 404:
		m := msg
		if m == "" {
			m = fmt.Sprintf("%s: Not found (404). Remote Control may not be available for this organization.", label)
		}
		return &FatalError{Status: status, ErrorType: etype, Message: m}
	case 410:
		m := msg
		if m == "" {
			m = "Remote Control session expired."
		}
		if etype == "" {
			etype = "environment_expired"
		}
		return &FatalError{Status: status, ErrorType: etype, Message: m}
	case 429:
		var retryAfter int64
		if ra := headers.Get("retry-after"); ra != "" {
			if secs, err := strconv.ParseFloat(ra, 64); err == nil {
				retryAfter = int64(secs * 1000)
			}
		}
		return &RateLimitError{Status: status, RetryAfterMs: retryAfter,
			Message: fmt.Sprintf("%s: Rate limited (429). Polling too frequently.", label)}
	default:
		return &statusError{Status: status,
			Message: fmt.Sprintf("%s: Failed with status %d%s", label, status, suffix)}
	}
}

// ---------------------------------------------------------------------------
// Environment-bridge API (mod_1258.js).
// ---------------------------------------------------------------------------

// ToolHostAdvert is the getToolHostAdvert() tri-state real sends inside the
// registration metadata: "2" advertises this host can run tool_use work
// items, "" withdraws, absent means unknown.
type ToolHostAdvert struct {
	Kind       string // "advertise" | "withdraw" | "unknown"
	CLIVersion string
	Platform   string
}

// RegisterParams is the POST /v1/environments/bridge body.
type RegisterParams struct {
	MachineName        string          `json:"machine_name"`
	MachineID          string          `json:"machine_id,omitempty"`
	Directory          string          `json:"directory"`
	Branch             string          `json:"branch"`
	GitRepoURL         string          `json:"git_repo_url"`
	MaxSessions        int             `json:"max_sessions"`
	Metadata           map[string]any  `json:"metadata"`
	Host               any             `json:"host,omitempty"`
	ReuseEnvironmentID string          `json:"environment_id,omitempty"`
	Advert             *ToolHostAdvert `json:"-"`
}

// BuildBody mirrors the payload assembly in registerBridgeEnvironment:
// tool_host fields live under metadata and come from the advert tri-state.
func (p *RegisterParams) BuildBody() map[string]any {
	metadata := map[string]any{}
	for k, v := range p.Metadata {
		metadata[k] = v
	}
	if p.MachineID != "" {
		metadata["machine_id"] = p.MachineID
	}
	switch {
	case p.Advert == nil || p.Advert.Kind == "unknown":
		// unknown: no tool_host keys at all
	case p.Advert.Kind == "advertise":
		metadata["tool_host"] = "2"
		metadata["tool_host_cc_version"] = p.Advert.CLIVersion
		metadata["tool_host_platform"] = p.Advert.Platform
	case p.Advert.Kind == "withdraw" && p.ReuseEnvironmentID != "":
		metadata["tool_host"] = ""
		metadata["tool_host_cc_version"] = ""
		metadata["tool_host_platform"] = ""
		metadata["tool_host_blocker"] = ""
	}
	body := map[string]any{
		"machine_name": p.MachineName,
		"directory":    p.Directory,
		"branch":       p.Branch,
		"git_repo_url": p.GitRepoURL,
		"max_sessions": p.MaxSessions,
		"metadata":     metadata,
	}
	if p.MachineID != "" {
		body["machine_id"] = p.MachineID
	}
	if p.Host != nil {
		body["host"] = p.Host
	}
	if p.ReuseEnvironmentID != "" {
		body["environment_id"] = p.ReuseEnvironmentID
	}
	return body
}

// RegisterResult is the 200 body: {environment_id, environment_secret,
// environment_secret_expires_in?}.
type RegisterResult struct {
	EnvironmentID              string `json:"environment_id"`
	EnvironmentSecret          string `json:"environment_secret"`
	EnvironmentSecretExpiresIn int    `json:"environment_secret_expires_in,omitempty"`
}

// RegisterBridgeEnvironment: POST /v1/environments/bridge (OAuth lane,
// retries once on 401). A 409 with error.type bridge_folder_already_served
// gets real's dedicated message.
func (c *Client) RegisterBridgeEnvironment(ctx context.Context, p *RegisterParams) (*RegisterResult, error) {
	c.debugf("[bridge:api] POST /v1/environments/bridge")
	r, err := c.withOAuthRetry("Registration", func(tok string) (*response, error) {
		h, err := c.envHeaders(tok)
		if err != nil {
			return nil, err
		}
		return c.do(ctx, http.MethodPost, c.BaseURL+"/v1/environments/bridge",
			p.BuildBody(), h, 15*time.Second)
	})
	if err != nil {
		return nil, err
	}
	if r.Status == 409 && bodyErrorType(r.Body) == ErrTypeFolderAlreadyServed {
		m := bodyErrorMessage(r.Body)
		if m == "" {
			m = "This folder is already served by another Claude Code on this device. Stop it first."
		}
		return nil, &FatalError{Status: 409, ErrorType: ErrTypeFolderAlreadyServed, Message: m}
	}
	if err := raise(r.Status, r.Body, "Registration", r.Headers); err != nil {
		return nil, err
	}
	var out RegisterResult
	if err := json.Unmarshal(r.Body, &out); err != nil {
		return nil, &FatalError{Status: 200, Message: "Registration: malformed response"}
	}
	c.debugf("[bridge:api] POST /v1/environments/bridge -> %d environment_id=%s", r.Status, out.EnvironmentID)
	return &out, nil
}

// WorkItem is one row from GET .../work/poll. Secret is the base64url blob
// decoded by DecodeWorkSecret; Data carries {type:"session", id, mode,...}.
type WorkItem struct {
	ID     string          `json:"id"`
	State  string          `json:"state,omitempty"`
	Secret string          `json:"secret,omitempty"`
	Data   json.RawMessage `json:"data"`
}

// WorkData is the work payload; mode=="tool_use" routes to the tool-host
// handler in real, anything else is a session spawn.
type WorkData struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Mode string `json:"mode,omitempty"`
}

// PollForWork: GET /v1/environments/{env}/work/poll on the environment-
// secret lane. reclaimOlderThanMs / pollIntervalMs become query params
// only when set (real only sends defined values).
func (c *Client) PollForWork(ctx context.Context, envID, envSecret string, reclaimOlderThanMs, pollIntervalMs int64) (*WorkItem, error) {
	if err := checkID(envID, "environmentId"); err != nil {
		return nil, err
	}
	u := c.BaseURL + "/v1/environments/" + envID + "/work/poll"
	sep := "?"
	if reclaimOlderThanMs > 0 {
		u += fmt.Sprintf("%sreclaim_older_than_ms=%d", sep, reclaimOlderThanMs)
		sep = "&"
	}
	if pollIntervalMs > 0 {
		u += fmt.Sprintf("%spoll_interval_ms=%d", sep, pollIntervalMs)
	}
	h, err := c.envHeaders(envSecret)
	if err != nil {
		return nil, err
	}
	r, err := c.do(ctx, http.MethodGet, u, nil, h, 10*time.Second)
	if err != nil {
		return nil, err
	}
	if err := raise(r.Status, r.Body, "Poll", r.Headers); err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(r.Body)) == 0 {
		return nil, nil
	}
	var w WorkItem
	if err := json.Unmarshal(r.Body, &w); err != nil || w.ID == "" {
		return nil, nil // real: falsy data => "no work"
	}
	c.debugf("[bridge:api] GET .../work/poll -> %d workId=%s", r.Status, w.ID)
	return &w, nil
}

// AcknowledgeWork: POST .../work/{work}/ack {} — env-secret lane; real
// passes the work secret's session_ingress_token as the bearer.
func (c *Client) AcknowledgeWork(ctx context.Context, envID, workID, bearer string) error {
	if err := checkID(envID, "environmentId"); err != nil {
		return err
	}
	if err := checkID(workID, "workId"); err != nil {
		return err
	}
	c.debugf("[bridge:api] POST .../work/%s/ack", workID)
	h, err := c.envHeaders(bearer)
	if err != nil {
		return err
	}
	r, err := c.do(ctx, http.MethodPost,
		c.BaseURL+"/v1/environments/"+envID+"/work/"+workID+"/ack",
		map[string]any{}, h, 10*time.Second)
	if err != nil {
		return err
	}
	return raise(r.Status, r.Body, "Acknowledge", r.Headers)
}

// StopWork: POST .../work/{work}/stop {force} — env-secret lane.
func (c *Client) StopWork(ctx context.Context, envID, workID, bearer string, force bool) error {
	if err := checkID(envID, "environmentId"); err != nil {
		return err
	}
	if err := checkID(workID, "workId"); err != nil {
		return err
	}
	c.debugf("[bridge:api] POST .../work/%s/stop force=%v", workID, force)
	h, err := c.envHeaders(bearer)
	if err != nil {
		return err
	}
	r, err := c.do(ctx, http.MethodPost,
		c.BaseURL+"/v1/environments/"+envID+"/work/"+workID+"/stop",
		map[string]any{"force": force}, h, 10*time.Second)
	if err != nil {
		return err
	}
	return raise(r.Status, r.Body, "StopWork", r.Headers)
}

// HeartbeatResult is the /work/{id}/heartbeat response.
type HeartbeatResult struct {
	LeaseExtended bool   `json:"lease_extended"`
	State         string `json:"state"`
}

// HeartbeatWork: POST .../work/{work}/heartbeat {} — env-secret lane;
// real sends each session's access token (the work's ingress token).
func (c *Client) HeartbeatWork(ctx context.Context, envID, workID, bearer string) (*HeartbeatResult, error) {
	if err := checkID(envID, "environmentId"); err != nil {
		return nil, err
	}
	if err := checkID(workID, "workId"); err != nil {
		return nil, err
	}
	c.debugf("[bridge:api] POST .../work/%s/heartbeat", workID)
	h, err := c.envHeaders(bearer)
	if err != nil {
		return nil, err
	}
	r, err := c.do(ctx, http.MethodPost,
		c.BaseURL+"/v1/environments/"+envID+"/work/"+workID+"/heartbeat",
		map[string]any{}, h, 10*time.Second)
	if err != nil {
		return nil, err
	}
	if err := raise(r.Status, r.Body, "Heartbeat", r.Headers); err != nil {
		return nil, err
	}
	var out HeartbeatResult
	json.Unmarshal(r.Body, &out)
	c.debugf("[bridge:api] POST .../work/%s/heartbeat -> %d lease_extended=%v state=%s",
		workID, r.Status, out.LeaseExtended, out.State)
	return &out, nil
}

// DeregisterEnvironment: DELETE /v1/environments/bridge/{env} — OAuth lane.
func (c *Client) DeregisterEnvironment(ctx context.Context, envID string) error {
	if err := checkID(envID, "environmentId"); err != nil {
		return err
	}
	c.debugf("[bridge:api] DELETE /v1/environments/bridge/%s", envID)
	r, err := c.withOAuthRetry("Deregister", func(tok string) (*response, error) {
		h, err := c.envHeaders(tok)
		if err != nil {
			return nil, err
		}
		return c.do(ctx, http.MethodDelete,
			c.BaseURL+"/v1/environments/bridge/"+envID, nil, h, 10*time.Second)
	})
	if err != nil {
		return err
	}
	return raise(r.Status, r.Body, "Deregister", r.Headers)
}

// MarkEnvironmentOffline: POST /v1/environments/bridge/{env}/offline —
// env-secret lane, 5s timeout (best-effort shutdown signal).
func (c *Client) MarkEnvironmentOffline(ctx context.Context, envID, envSecret string) error {
	if err := checkID(envID, "environmentId"); err != nil {
		return err
	}
	path := "/v1/environments/bridge/" + envID + "/offline"
	c.debugf("[bridge:api] POST %s", path)
	h, err := c.envHeaders(envSecret)
	if err != nil {
		return err
	}
	r, err := c.do(ctx, http.MethodPost, c.BaseURL+path, nil, h, 5*time.Second)
	if err != nil {
		return err
	}
	return raise(r.Status, r.Body, "MarkOffline", r.Headers)
}

// ReconnectSession: POST /v1/environments/{env}/bridge/reconnect
// {session_id} — OAuth lane. Asks the server to re-queue an orphaned
// session onto this environment (the "token expired — re-queuing" path).
func (c *Client) ReconnectSession(ctx context.Context, envID, sessionID string) error {
	if err := checkID(envID, "environmentId"); err != nil {
		return err
	}
	if err := checkID(sessionID, "sessionId"); err != nil {
		return err
	}
	c.debugf("[bridge:api] POST /v1/environments/%s/bridge/reconnect session_id=%s", envID, sessionID)
	r, err := c.withOAuthRetry("ReconnectSession", func(tok string) (*response, error) {
		h, err := c.envHeaders(tok)
		if err != nil {
			return nil, err
		}
		return c.do(ctx, http.MethodPost,
			c.BaseURL+"/v1/environments/"+envID+"/bridge/reconnect",
			map[string]any{"session_id": sessionID}, h, 10*time.Second)
	})
	if err != nil {
		return err
	}
	return raise(r.Status, r.Body, "ReconnectSession", r.Headers)
}

// sessionPath returns the v2 (/v1/code/sessions/cse_) or v1-compat
// (/v1/sessions/session_) path for a session id — mod_0829.js x().
func (c *Client) sessionPath(sessionID, suffix string) (string, http.Header, error) {
	if c.useV2() {
		return "/v1/code/sessions/" + codeSessionID(sessionID) + suffix, nil, nil
	}
	if c.OrgUUID == "" {
		return "", nil, fmt.Errorf("v1 compat path needs an org UUID")
	}
	return "/v1/sessions/" + legacySessionID(sessionID) + suffix, nil, nil
}

// ArchiveSession: POST .../archive {} — OAuth lane. A 409 means the session
// is already archived and is treated as success (real returns early).
func (c *Client) ArchiveSession(ctx context.Context, sessionID string) error {
	if err := checkID(sessionID, "sessionId"); err != nil {
		return err
	}
	path, _, err := c.sessionPath(sessionID, "/archive")
	if err != nil {
		return err
	}
	c.debugf("[bridge:api] POST %s", path)
	r, err := c.withOAuthRetry("ArchiveSession", func(tok string) (*response, error) {
		h, err := c.codeSessionHeaders(tok)
		if err != nil {
			return nil, err
		}
		if !c.useV2() {
			h.Set("anthropic-beta", betaCCRBYOC)
			h.Set("x-organization-uuid", c.OrgUUID)
		}
		return c.do(ctx, http.MethodPost, c.BaseURL+path, map[string]any{}, h, 10*time.Second)
	})
	if err != nil {
		return err
	}
	if r.Status == 409 {
		c.debugf("[bridge:api] POST %s -> 409 (already archived)", path)
		return nil
	}
	return raise(r.Status, r.Body, "ArchiveSession", r.Headers)
}

// UnarchiveSession: POST .../unarchive {} — OAuth lane. Real's resume path
// (mod_0829.js U("Unarchive",...)): returns the raw status so the caller can
// apply its verdict mapping (untrusted_device/elevated_auth/session_stale_-
// relogin are collapsed to the status + error body for honesty).
func (c *Client) UnarchiveSession(ctx context.Context, sessionID string) (int, error) {
	if err := checkID(sessionID, "sessionId"); err != nil {
		return 0, err
	}
	path, _, err := c.sessionPath(sessionID, "/unarchive")
	if err != nil {
		return 0, err
	}
	c.debugf("[bridge:api] POST %s", path)
	r, err := c.withOAuthRetry("UnarchiveSession", func(tok string) (*response, error) {
		h, err := c.codeSessionHeaders(tok)
		if err != nil {
			return nil, err
		}
		if !c.useV2() {
			h.Set("anthropic-beta", betaCCRBYOC)
			h.Set("x-organization-uuid", c.OrgUUID)
		}
		return c.do(ctx, http.MethodPost, c.BaseURL+path, map[string]any{}, h, 10*time.Second)
	})
	if err != nil {
		return 0, err
	}
	if r.Status == 403 {
		// Real surfaces untrusted_device / session_stale_relogin refusals
		// here; the caller maps them. Return status + parsed error verbatim.
		return 403, &FatalError{Status: 403, ErrorType: bodyErrorType(r.Body),
			Message: bodyErrorMessage(r.Body)}
	}
	return r.Status, raise(r.Status, r.Body, "UnarchiveSession", r.Headers)
}

// ---------------------------------------------------------------------------
// Code-session API (mod_0829.js) — the v2 session-mint lane used by the
// in-REPL /remote-control connect flow.
// ---------------------------------------------------------------------------

// CreateSessionParams is the POST /v1/code/sessions body:
// {title, bridge:{}, tags?, session_grouping_id?, config:{cwd, model?}}.
// sources/outcomes/reuse_outcome_branches are added when a gitRepoUrl is
// attached (buildGitSessionContext); we accept them verbatim.
type CreateSessionParams struct {
	Title                string
	Cwd                  string
	Model                string
	Tags                 []string
	SessionGrouping      string
	Sources              []any
	Outcomes             []any
	ReuseOutcomeBranches bool
}

// CreateResult reasons mirror j(): oauth_rejected (401), transient
// (408/429/5xx), rejected (other 4xx), grouping_rejected (not_found_error
// session_grouping / public_grouping_hosted_only / feature_disabled),
// malformed_response.
type CreateResult struct {
	Terminal  bool
	Reason    string // "" on success; see above
	Status    int
	Detail    string
	SessionID string // cse_* on success
}

// groupingRejected mirrors T(): not_found_error+session_grouping or a
// public_grouping_hosted_only / feature_disabled reason means the grouping
// feature isn't available to this org — terminal.
func groupingRejected(body []byte) bool {
	var v struct {
		Error struct {
			Type         string `json:"type"`
			ResourceType string `json:"resource_type"`
			Reason       string `json:"reason"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &v) != nil {
		return false
	}
	return (v.Error.Type == "not_found_error" && v.Error.ResourceType == "session_grouping") ||
		v.Error.Reason == "public_grouping_hosted_only" ||
		v.Error.Reason == "feature_disabled"
}

// classifyCreateStatus mirrors j().
func classifyCreateStatus(status int) string {
	switch {
	case status == 401:
		return "oauth_rejected"
	case status == 408 || status == 429 || status >= 500:
		return "transient"
	default:
		return "rejected"
	}
}

// CreateCodeSession: POST {base}/v1/code/sessions — OAuth bearer, no
// retry wrapper (the caller drives the OAuth retry loop itself, matching
// real's kn() wrapper in mod_1215.js).
func (c *Client) CreateCodeSession(ctx context.Context, p *CreateSessionParams) *CreateResult {
	tok, err := c.oauthToken()
	if err != nil {
		return &CreateResult{Reason: "oauth_rejected"}
	}
	cfg := map[string]any{"cwd": p.Cwd}
	if p.Model != "" {
		cfg["model"] = p.Model
	}
	if len(p.Sources) > 0 || len(p.Outcomes) > 0 {
		cfg["sources"] = p.Sources
		cfg["outcomes"] = p.Outcomes
		cfg["reuse_outcome_branches"] = true
	}
	body := map[string]any{
		"title":  p.Title,
		"bridge": map[string]any{},
		"config": cfg,
	}
	if len(p.Tags) > 0 {
		body["tags"] = p.Tags
	}
	if p.SessionGrouping != "" {
		body["session_grouping_id"] = p.SessionGrouping
	}
	h, err := c.codeSessionHeaders(tok)
	if err != nil {
		return &CreateResult{Terminal: true, Reason: "request_rejected", Detail: err.Error()}
	}
	r, err := c.do(ctx, http.MethodPost, c.BaseURL+"/v1/code/sessions", body, h, 10*time.Second)
	if err != nil {
		c.debugf("[code-session] Session create request failed: %v", err)
		return nil
	}
	if r.Status != 200 && r.Status != 201 {
		detail := bodyErrorMessage(r.Body)
		c.debugf("[code-session] Session create failed %d", r.Status)
		switch classifyCreateStatus(r.Status) {
		case "oauth_rejected":
			return &CreateResult{Terminal: false, Reason: "oauth_rejected"}
		case "transient":
			return nil
		default:
			if groupingRejected(r.Body) {
				return &CreateResult{Terminal: true, Reason: "grouping_rejected", Status: r.Status, Detail: detail}
			}
			return &CreateResult{Terminal: true, Reason: "request_rejected", Status: r.Status, Detail: detail}
		}
	}
	var v struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	if json.Unmarshal(r.Body, &v) != nil || len(v.Session.ID) < 4 || v.Session.ID[:4] != "cse_" {
		return &CreateResult{Terminal: true, Reason: "malformed_response", Status: r.Status}
	}
	c.debugf("[remote-bridge] Created session %s", v.Session.ID)
	return &CreateResult{SessionID: v.Session.ID}
}

// BridgeCredentials is the POST /v1/code/sessions/{id}/bridge response —
// the worker credential mint (DQn in mod_0829.js).
type BridgeCredentials struct {
	WorkerJWT   string `json:"worker_jwt"`
	ExpiresIn   int    `json:"expires_in"`
	APIBaseURL  string `json:"api_base_url"`
	WorkerEpoch int64  `json:"worker_epoch"`
	MCPConfig   *struct {
		Content string `json:"content"`
	} `json:"mcp_config,omitempty"`
}

// MintResult mirrors DQn's outcome union: nil = transient, Terminal with a
// Reason (oauth_rejected|request_rejected|invalid_session_id|
// malformed_response|untrusted_device|session_stale_relogin), or creds.
type MintResult struct {
	Terminal bool
	Reason   string
	Status   int
	Source   string // non-origin 403 hint ("cf","proxy",...) when derivable
	Creds    *BridgeCredentials
}

// MintBridgeCredentials: POST /v1/code/sessions/{id}/bridge. Body is {}
// or {expected_epoch} when fencing a reattach. OAuth lane.
func (c *Client) MintBridgeCredentials(ctx context.Context, sessionID string, expectedEpoch *int64) *MintResult {
	if err := checkID(sessionID, "sessionId"); err != nil {
		return &MintResult{Terminal: true, Reason: "invalid_session_id"}
	}
	tok, err := c.oauthToken()
	if err != nil {
		return &MintResult{Reason: "oauth_rejected"}
	}
	h, err := c.codeSessionHeaders(tok)
	if err != nil {
		return &MintResult{Terminal: true, Reason: "request_rejected"}
	}
	body := map[string]any{}
	if expectedEpoch != nil {
		body["expected_epoch"] = *expectedEpoch
	}
	r, err := c.do(ctx, http.MethodPost,
		c.BaseURL+"/v1/code/sessions/"+codeSessionID(sessionID)+"/bridge",
		body, h, 10*time.Second)
	if err != nil {
		c.debugf("[code-session] /bridge request failed: %v", err)
		return nil
	}
	if r.Status != 200 {
		c.debugf("[code-session] /bridge failed %d: %s", r.Status, bodyErrorMessage(r.Body))
		if r.Status == 403 {
			// sZ(): error.resource carries the refusal class.
			var v struct {
				Error struct {
					Resource  string `json:"resource"`
					SubReason string `json:"sub_reason"`
				} `json:"error"`
			}
			if json.Unmarshal(r.Body, &v) == nil &&
				(v.Error.Resource == "untrusted_device" || v.Error.Resource == "session_stale_relogin") {
				return &MintResult{Terminal: true, Reason: v.Error.Resource, Status: 403}
			}
		}
		switch classifyCreateStatus(r.Status) {
		case "oauth_rejected":
			return &MintResult{Terminal: false, Reason: "oauth_rejected"}
		case "transient":
			return nil
		default:
			return &MintResult{Terminal: true, Reason: "request_rejected", Status: r.Status}
		}
	}
	var creds BridgeCredentials
	if err := json.Unmarshal(r.Body, &creds); err != nil ||
		creds.WorkerJWT == "" || creds.APIBaseURL == "" {
		return &MintResult{Terminal: true, Reason: "malformed_response", Status: 200}
	}
	return &MintResult{Creds: &creds}
}

// ---------------------------------------------------------------------------
// Worker endpoints (mod_1211.js Wqe + mod_1214.js request paths) — these are
// the honest HTTP handshake of the data plane; see transport.go for the
// SSE read stream.
// ---------------------------------------------------------------------------

// RegisterWorker mirrors Wqe(): POST {sessionBase}/worker/register {}
// with the session ingress token; returns worker_epoch.
func (c *Client) RegisterWorker(ctx context.Context, sessionBaseURL, ingressToken string) (int64, error) {
	h := http.Header{
		"Authorization":     {"Bearer " + ingressToken},
		"Content-Type":      {"application/json"},
		"anthropic-version": {anthropicVersion},
		"User-Agent":        {c.userAgent()},
	}
	r, err := c.do(ctx, http.MethodPost, sessionBaseURL+"/worker/register",
		map[string]any{}, h, 10*time.Second)
	if err != nil {
		return 0, err
	}
	if r.Status != 200 {
		return 0, raise(r.Status, r.Body, "registerWorker", r.Headers)
	}
	var v struct {
		WorkerEpoch json.Number `json:"worker_epoch"`
	}
	if err := json.Unmarshal(r.Body, &v); err != nil {
		return 0, fmt.Errorf("registerWorker: invalid worker_epoch in response")
	}
	epoch, err := v.WorkerEpoch.Int64()
	if err != nil {
		return 0, fmt.Errorf("registerWorker: invalid worker_epoch in response")
	}
	return epoch, nil
}
