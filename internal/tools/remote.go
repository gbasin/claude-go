package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gbasin/claude-go/internal/api"
	"github.com/gbasin/claude-go/internal/auth"
	"github.com/gbasin/claude-go/internal/bridge"
)

// RemoteTrigger — real client for claude.ai's trigger service.
// Actions map to /v1/code/* endpoints per the extracted implementation:
//   list|get|create|update|run -> /v1/code/triggers[/id][/run]
//   create_webhook_trigger     -> /v1/code/webhook-triggers
//   list_runs                  -> /v1/code/sessions?trigger_id=…
//   get_run_log                -> /v1/code/sessions/{id}/events
//
// Auth uses the OAuth access token (never an API key) — matching real
// behavior where these calls go out with claude.ai creds.

// remoteAPIBase is pn().BASE_API_URL; remoteClaudeAIOrigin is
// pn().CLAUDE_AI_ORIGIN — both vars so httptest can point at a stub.
var remoteAPIBase = "https://api.claude.ai"
var remoteClaudeAIOrigin = "https://claude.ai"

func init() {
	Register(&Tool{Def: loadSchema("RemoteTrigger"), Exec: execRemoteTrigger})
}

// remoteTriggerEnabled mirrors the real isEnabled gate (mod_1605.js):
//
//	Jn() && ft() && !CLAUDE_CODE_REMOTE && sn("allow_remote_sessions") && sn("allow_routines")
//
// Locally checkable pieces: first-party provider, a claude.ai OAuth login
// (not an API key), the not-remote env, and the two policy gates.
func remoteTriggerEnabled() bool {
	cwd, _ := os.Getwd()
	return api.DetectProvider() == api.ProviderFirstParty &&
		claudeAIOAuthOK() &&
		!envTruthy(os.Getenv("CLAUDE_CODE_REMOTE")) &&
		policyGate(cwd, bridge.PolicyAllowRemoteSessions) &&
		policyGate(cwd, bridge.PolicyAllowRoutines)
}

// RoutinesEnabled reports whether the /schedule (routines) skill is usable —
// same gate minus allow_remote_sessions (real: Jn&&ft&&!remote&&sn(JX)).
func RoutinesEnabled() bool {
	cwd, _ := os.Getwd()
	return api.DetectProvider() == api.ProviderFirstParty &&
		claudeAIOAuthOK() &&
		!envTruthy(os.Getenv("CLAUDE_CODE_REMOTE")) &&
		policyGate(cwd, bridge.PolicyAllowRoutines)
}

// PolicyAllows exposes policyGate for command-level gating (e.g. /schedule's
// policyGate: iQ → "Routines are disabled by your organization's policy.").
func PolicyAllows(cwd, name string) bool { return policyGate(cwd, name) }

// HasClaudeAIOAuth exposes the ft() login check for command gating.
func HasClaudeAIOAuth() bool { return claudeAIOAuthOK() }

// claudeAIOAuthOK mirrors ft(): a claude.ai OAuth credential is present
// (API-key sessions don't qualify). Cached — auth.Load may hit keychain.
var oauthCache = struct {
	mu sync.Mutex
	ok bool
	at time.Time
}{}

func claudeAIOAuthOK() bool {
	oauthCache.mu.Lock()
	defer oauthCache.mu.Unlock()
	if time.Since(oauthCache.at) < 60*time.Second {
		return oauthCache.ok
	}
	creds, err := auth.Load()
	oauthCache.ok = err == nil && creds != nil && !creds.IsAPIKey && creds.AccessToken != ""
	oauthCache.at = time.Now()
	return oauthCache.ok
}

func execRemoteTrigger(c *Context, raw json.RawMessage) Result {
	var in struct {
		Action    string          `json:"action"`
		TriggerID string          `json:"trigger_id"`
		SessionID string          `json:"session_id"`
		Cursor    string          `json:"cursor"`
		Body      json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	// isEnabled's sn() pair, surfaced at call time: allow_remote_sessions
	// first, then allow_routines (real hides the tool; when it does run the
	// gate fires in this order).
	cwd := c.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if r := PolicyCheck(cwd, bridge.PolicyAllowRemoteSessions); r != nil {
		return Result{Content: *r, IsError: true}
	}
	if r := PolicyCheck(cwd, bridge.PolicyAllowRoutines); r != nil {
		return Result{Content: *r, IsError: true}
	}
	if c.OAuthToken == nil || c.OAuthToken() == "" {
		return Result{Content: "Not authenticated with a claude.ai account. Run /login and try again.", IsError: true}
	}
	var method, path string
	var body any
	switch in.Action {
	case "list":
		method, path = "GET", "/v1/code/triggers"
	case "get":
		if in.TriggerID == "" {
			return Result{Content: "get requires trigger_id", IsError: true}
		}
		method, path = "GET", "/v1/code/triggers/"+in.TriggerID
	case "create":
		if in.Body == nil {
			return Result{Content: "create requires body", IsError: true}
		}
		method, path = "POST", "/v1/code/triggers"
		body = in.Body
	case "update":
		if in.TriggerID == "" {
			return Result{Content: "update requires trigger_id", IsError: true}
		}
		if in.Body == nil {
			return Result{Content: "update requires body", IsError: true}
		}
		method, path = "POST", "/v1/code/triggers/"+in.TriggerID
		body = in.Body
	case "create_webhook_trigger":
		if in.Body == nil {
			return Result{Content: "create_webhook_trigger requires body", IsError: true}
		}
		method, path = "POST", "/v1/code/webhook-triggers"
		body = in.Body
	case "list_runs":
		if in.TriggerID == "" {
			return Result{Content: "list_runs requires trigger_id", IsError: true}
		}
		q := url.Values{"trigger_id": {in.TriggerID}, "limit": {"20"}}
		if in.Cursor != "" {
			q.Set("cursor", in.Cursor)
		}
		method, path = "GET", "/v1/code/sessions?"+q.Encode()
	case "get_run_log":
		if in.SessionID == "" {
			return Result{Content: "get_run_log requires session_id", IsError: true}
		}
		q := url.Values{"limit": {"100"}, "sort_order": {"desc"}}
		if in.Cursor != "" {
			q.Set("cursor", in.Cursor)
		}
		method, path = "GET", "/v1/code/sessions/"+url.PathEscape(in.SessionID)+"/events?"+q.Encode()
	case "run":
		if in.TriggerID == "" {
			return Result{Content: "run requires trigger_id", IsError: true}
		}
		method, path = "POST", "/v1/code/triggers/"+in.TriggerID+"/run"
		if in.Body != nil {
			var m map[string]any
			json.Unmarshal(in.Body, &m)
			delete(m, "trigger_id")
			body = m
		}
	default:
		return Result{Content: "unknown action: " + in.Action, IsError: true}
	}
	return remoteCall(c, in.Action, in.Body, method, path, body)
}

// remoteTriggerOut mirrors real's {status, json, summary?} data object
// (mod_1605.js): status is the HTTP code, json is S(response), summary is
// appended for created/updated triggers and webhook triggers.
type remoteTriggerOut struct {
	Status  int    `json:"status"`
	JSON    string `json:"json"`
	Summary string `json:"summary,omitempty"`
}

// routineIDRe is real's /^[\w-]+$/ guard on routine_trigger_id.
var routineIDRe = regexp.MustCompile(`^[\w-]+$`)

func remoteCall(c *Context, action string, reqBody json.RawMessage, method, path string, body any) Result {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, remoteAPIBase+path, rdr)
	if err != nil {
		return Result{Content: err.Error(), IsError: true}
	}
	req.Header.Set("Authorization", "Bearer "+c.OAuthToken())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", "cc-code-2026-01-01")
	cl := &http.Client{Timeout: 20 * time.Second}
	resp, err := cl.Do(req.WithContext(c.Ctx))
	if err != nil {
		return Result{Content: "Remote triggers unavailable: " + err.Error(), IsError: true}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 100*1024))

	// {status, json, summary} — real returns data for any status
	// (validateStatus:()=>!0); the rendered block is "HTTP <status>\n<json>"
	// plus "\n\n<summary>" when present.
	out := remoteTriggerOut{Status: resp.StatusCode, JSON: truncStr(string(b), 100000)}
	var v any
	if json.Unmarshal(b, &v) == nil {
		pb, _ := json.MarshalIndent(v, "", "  ")
		out.JSON = truncStr(string(pb), 100000)
	}
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	if ok && (action == "create" || action == "update") {
		if m, isMap := v.(map[string]any); isMap {
			out.Summary = routineSummary(m, time.Now())
		}
	}
	if ok && action == "create_webhook_trigger" {
		var p map[string]any
		if json.Unmarshal(reqBody, &p) == nil {
			if id, _ := p["routine_trigger_id"].(string); routineIDRe.MatchString(id) {
				out.Summary = "→ Fires routine: " + remoteClaudeAIOrigin + "/code/routines/" + id
			}
		}
	}
	content := fmt.Sprintf("HTTP %d\n%s", out.Status, out.JSON)
	if out.Summary != "" {
		content += "\n\n" + out.Summary
	}
	return Result{Content: content}
}

// routineSummary is $e(trigger): the "→ Scheduled: …" / "→ Disabled (…)"
// line plus "→ View/manage: <CLAUDE_AI_ORIGIN>/code/routines/<id>".
func routineSummary(trig map[string]any, now time.Time) string {
	enabled := true
	if e, ok := trig["enabled"].(bool); ok {
		enabled = e
	}
	var lines []string
	if s, ok := trig["next_run_at"].(string); ok && s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			rel := relativeRunTime(t, now)
			iso := t.UTC().Format("2006-01-02T15:04:05Z")
			what := "next run"
			if runOnce, _ := trig["run_once_at"].(string); runOnce != "" {
				what = "runs once"
			} else if cron, _ := trig["cron_expression"].(string); cron != "" {
				what = fmt.Sprintf("next run (cron %s)", cron)
			}
			if enabled {
				lines = append(lines, fmt.Sprintf("→ Scheduled: %s %s (%s UTC)", what, rel, iso))
				if runOnce, _ := trig["run_once_at"].(string); runOnce != "" && t.Before(now) {
					lines = append(lines, "⚠ next_run_at is in the past — confirm the date/timezone is intended.")
				}
			} else {
				lines = append(lines, fmt.Sprintf("→ Disabled (next run would be %s, %s UTC)", rel, iso))
			}
		}
	}
	if id, _ := trig["id"].(string); id != "" {
		lines = append(lines, fmt.Sprintf("→ View/manage: %s/code/routines/%s", remoteClaudeAIOrigin, id))
	}
	return strings.Join(lines, "\n")
}

// relativeRunTime approximates y4(next_run_at, {now}): "in 2 hours" /
// "3 days ago" style relative text (rounds to nearest unit, singular-aware).
func relativeRunTime(t, now time.Time) string {
	d := t.Sub(now)
	past := d < 0
	if past {
		d = -d
	}
	plural := func(n int64, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	var s string
	switch {
	case d < 45*time.Second:
		s = plural(int64(math.Round(d.Seconds())), "second")
	case d < 90*time.Minute:
		s = plural(int64(math.Round(d.Minutes())), "minute")
	case d < 36*time.Hour:
		s = plural(int64(math.Round(d.Hours())), "hour")
	default:
		s = plural(int64(math.Round(d.Hours()/24)), "day")
	}
	if past {
		return s + " ago"
	}
	return "in " + s
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…[truncated]"
}

var _ = strings.TrimSpace
