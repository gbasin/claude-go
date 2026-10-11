package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gbasin/claude-go/internal/bridge"
)

// remoteTestEnv points RemoteTrigger at an httptest stub and clears the
// policy surfaces that could deny in a dev environment.
func remoteTestEnv(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, e := range []string{
		"CLAUDE_GO_ALLOW_REMOTE_SESSIONS", "CLAUDE_GO_ALLOW_ROUTINES",
		"CLAUDE_CODE_REMOTE",
	} {
		t.Setenv(e, "")
	}
	srv := httptest.NewServer(handler)
	oldBase, oldOrigin := remoteAPIBase, remoteClaudeAIOrigin
	remoteAPIBase = srv.URL
	t.Cleanup(func() { remoteAPIBase, remoteClaudeAIOrigin = oldBase, oldOrigin })
	return srv
}

func remoteCtx(cwd string) *Context {
	return &Context{
		Ctx:        context.Background(),
		CWD:        cwd,
		OAuthToken: func() string { return "oauth-tok" },
	}
}

func TestRemoteTriggerCreateOutputShape(t *testing.T) {
	var gotAuth, gotPath string
	remoteTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.Method + " " + r.URL.Path
		json.NewEncoder(w).Encode(map[string]any{
			"id": "rt_1", "enabled": true,
			"next_run_at":     time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
			"cron_expression": "0 * * * *",
		})
	})
	res := execRemoteTrigger(remoteCtx(t.TempDir()), json.RawMessage(
		`{"action":"create","body":{"cron_expression":"0 * * * *"}}`))
	if res.IsError {
		t.Fatalf("res %+v", res)
	}
	if gotAuth != "Bearer oauth-tok" || gotPath != "POST /v1/code/triggers" {
		t.Fatalf("call %s %s", gotAuth, gotPath)
	}
	if !strings.HasPrefix(res.Content, "HTTP 200\n{") {
		t.Fatalf("shape: %q", res.Content)
	}
	if !strings.Contains(res.Content, `"id": "rt_1"`) {
		t.Fatalf("json missing: %q", res.Content)
	}
	if !strings.Contains(res.Content, "→ Scheduled: next run (cron 0 * * * *) in ") ||
		!strings.Contains(res.Content, " UTC)") {
		t.Fatalf("summary missing schedule line: %q", res.Content)
	}
	if !strings.Contains(res.Content, "→ View/manage: https://claude.ai/code/routines/rt_1") {
		t.Fatalf("summary missing view link: %q", res.Content)
	}
}

func TestRemoteTriggerUpdateSummary(t *testing.T) {
	remoteTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": "rt_9", "enabled": false})
	})
	res := execRemoteTrigger(remoteCtx(t.TempDir()), json.RawMessage(
		`{"action":"update","trigger_id":"rt_9","body":{"enabled":false}}`))
	if res.IsError {
		t.Fatalf("res %+v", res)
	}
	if !strings.HasPrefix(res.Content, "HTTP 200\n{") ||
		!strings.Contains(res.Content, "→ View/manage: https://claude.ai/code/routines/rt_9") {
		t.Fatalf("content %q", res.Content)
	}
}

func TestRemoteTriggerWebhookSummary(t *testing.T) {
	remoteTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	res := execRemoteTrigger(remoteCtx(t.TempDir()), json.RawMessage(
		`{"action":"create_webhook_trigger","body":{"routine_trigger_id":"rt_42","events":["push"]}}`))
	if res.IsError {
		t.Fatalf("res %+v", res)
	}
	if !strings.Contains(res.Content, "→ Fires routine: https://claude.ai/code/routines/rt_42") {
		t.Fatalf("content %q", res.Content)
	}
}

func TestRemoteTriggerRunNoSummary(t *testing.T) {
	remoteTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/triggers/rt_1/run") {
			t.Errorf("path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "rt_1", "enabled": true})
	})
	res := execRemoteTrigger(remoteCtx(t.TempDir()), json.RawMessage(
		`{"action":"run","trigger_id":"rt_1"}`))
	if res.IsError {
		t.Fatalf("res %+v", res)
	}
	if strings.Contains(res.Content, "View/manage") || !strings.HasPrefix(res.Content, "HTTP 200\n") {
		t.Fatalf("content %q", res.Content)
	}
}

func TestRemoteTriggerErrorStatus(t *testing.T) {
	remoteTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "no trigger"}})
	})
	res := execRemoteTrigger(remoteCtx(t.TempDir()), json.RawMessage(
		`{"action":"get","trigger_id":"rt_x"}`))
	// real returns {status,json} data for any HTTP status — not IsError
	if res.IsError || !strings.HasPrefix(res.Content, "HTTP 404\n{") {
		t.Fatalf("content %q", res.Content)
	}
}

func TestRemoteTriggerPolicyGates(t *testing.T) {
	remoteTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("request should not fire under policy denial")
	})
	cwd := t.TempDir()
	defer func() {
		bridge.OrgPolicyVerdict = func(k, l, v, f string) *string { return nil }
	}()

	bridge.OrgPolicyVerdict = func(k, l, v, f string) *string {
		if k == bridge.PolicyAllowRoutines {
			return &f
		}
		return nil
	}
	res := execRemoteTrigger(remoteCtx(cwd), json.RawMessage(`{"action":"list"}`))
	if !res.IsError || res.Content != "Routines are disabled by your organization's policy." {
		t.Fatalf("routines gate %q", res.Content)
	}

	bridge.OrgPolicyVerdict = func(k, l, v, f string) *string {
		if k == bridge.PolicyAllowRemoteSessions {
			return &f
		}
		return nil
	}
	res = execRemoteTrigger(remoteCtx(cwd), json.RawMessage(`{"action":"list"}`))
	if !res.IsError || !strings.Contains(res.Content, "Cloud sessions are disabled by your organization's policy") {
		t.Fatalf("sessions gate %q", res.Content)
	}
}

func TestPolicyCheckManagedDeny(t *testing.T) {
	// policyGate honors CLAUDE_GO_<NAME> env — a managed-deny stand-in.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_GO_ALLOW_ROUTINES", "0")
	r := PolicyCheck(t.TempDir(), bridge.PolicyAllowRoutines)
	if r == nil || *r != bridge.ErrRoutinesPolicyDenied {
		t.Fatalf("deny %v", r)
	}
	t.Setenv("CLAUDE_GO_ALLOW_ROUTINES", "")
	if r := PolicyCheck(t.TempDir(), bridge.PolicyAllowRoutines); r != nil {
		t.Fatalf("allow %v", *r)
	}
}
