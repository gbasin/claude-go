package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
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

const remoteAPIBase = "https://api.claude.ai"

func init() {
	Register(&Tool{Def: loadSchema("RemoteTrigger"), Exec: execRemoteTrigger})
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
	return remoteCall(c, method, path, body)
}

func remoteCall(c *Context, method, path string, body any) Result {
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
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return Result{Content: "Not authenticated with a claude.ai account. Run /login and try again.", IsError: true}
	}
	if resp.StatusCode >= 400 {
		return Result{Content: fmt.Sprintf("Remote triggers unavailable: %s — %s", resp.Status, truncStr(string(b), 500)), IsError: true}
	}
	// pretty-print JSON
	var v any
	if json.Unmarshal(b, &v) == nil {
		pb, _ := json.MarshalIndent(v, "", "  ")
		if len(pb) > 100000 {
			pb = append(pb[:100000], []byte("…[truncated]")...)
		}
		return Result{Content: string(pb)}
	}
	return Result{Content: truncStr(string(b), 100000)}
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…[truncated]"
}

var _ = strings.TrimSpace
