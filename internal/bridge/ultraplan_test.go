package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- create body shape ------------------------------------------------------

func TestCreateCodeSessionUltraplanBody(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{"id": "cse_plan"}})
	}))
	defer srv.Close()
	c := testClient(srv)
	res := c.CreateCodeSession(context.Background(), &CreateSessionParams{
		Title:              "plan it",
		Cwd:                "/d",
		Source:             "ultraplan",
		Ultraplan:          true,
		PermissionMode:     "plan",
		InitialMessage:     "build the feature",
		Tags:               []string{"ultraplan"},
		CustomSystemPrompt: "sp",
		MaxTurns:           40,
		MaxBudgetUSD:       2.5,
		AllowedTools:       []string{"Bash"},
		DisallowedTools:    []string{"WebFetch"},
		BuiltinTools:       []string{"Bash", "Read"},
		EnvVars:            map[string]string{"K": "v"},
	})
	if res == nil || res.SessionID != "cse_plan" {
		t.Fatalf("res %+v", res)
	}
	if body["source"] != "ultraplan" || body["ultraplan"] != true || body["permissionMode"] != "plan" {
		t.Fatalf("markers %v", body)
	}
	if body["environment_variables"].(map[string]any)["K"] != "v" {
		t.Fatalf("env vars %v", body["environment_variables"])
	}
	events, ok := body["events"].([]any)
	if !ok || len(events) != 2 {
		t.Fatalf("events %v", body["events"])
	}
	ev0 := events[0].(map[string]any)
	if ev0["type"] != "control_request" {
		t.Fatalf("ev0 %v", ev0)
	}
	req := ev0["request"].(map[string]any)
	if req["subtype"] != "set_permission_mode" || req["mode"] != "plan" || req["ultraplan"] != true {
		t.Fatalf("request %v", req)
	}
	if rid, _ := ev0["request_id"].(string); !strings.HasPrefix(rid, "set-mode-") {
		t.Fatalf("request_id %v", ev0["request_id"])
	}
	ev1 := events[1].(map[string]any)
	if ev1["type"] != "user" || ev1["session_id"] != "" {
		t.Fatalf("ev1 %v", ev1)
	}
	if _, present := ev1["parent_tool_use_id"]; !present || ev1["parent_tool_use_id"] != nil {
		t.Fatalf("parent_tool_use_id %v", ev1["parent_tool_use_id"])
	}
	uuid, _ := ev1["uuid"].(string)
	if len(uuid) != 36 || strings.Count(uuid, "-") != 4 {
		t.Fatalf("uuid %q", uuid)
	}
	msg := ev1["message"].(map[string]any)
	if msg["role"] != "user" || msg["content"] != "build the feature" {
		t.Fatalf("message %v", msg)
	}
	cfg := body["config"].(map[string]any)
	for k, want := range map[string]any{
		"custom_system_prompt": "sp", "max_turns": float64(40),
		"max_budget_usd": 2.5,
	} {
		if cfg[k] != want {
			t.Fatalf("config[%s]=%v want %v (%v)", k, cfg[k], want, cfg)
		}
	}
	if cfg["allowed_tools"].([]any)[0] != "Bash" ||
		cfg["disallowed_tools"].([]any)[0] != "WebFetch" ||
		len(cfg["builtin_tools"].([]any)) != 2 {
		t.Fatalf("tool config %v", cfg)
	}
}

func TestCreateCodeSessionUltrareviewBody(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{"id": "cse_rev"}})
	}))
	defer srv.Close()
	c := testClient(srv)
	res := c.CreateCodeSession(context.Background(), &CreateSessionParams{
		Title:          "ultrareview: main",
		Cwd:            "/d",
		Source:         "ultrareview",
		InitialMessage: "review prompt",
		Tags:           []string{"ultrareview"},
	})
	if res == nil || res.SessionID != "cse_rev" {
		t.Fatalf("res %+v", res)
	}
	if body["source"] != "ultrareview" || body["tags"].([]any)[0] != "ultrareview" {
		t.Fatalf("body %v", body)
	}
	events := body["events"].([]any)
	if len(events) != 1 { // no permission-mode event without Ultraplan/PermissionMode
		t.Fatalf("events %v", events)
	}
	if events[0].(map[string]any)["type"] != "user" {
		t.Fatalf("ev %v", events[0])
	}
}

// --- policy gates -----------------------------------------------------------

func TestRemoteSessionsPolicyDenial(t *testing.T) {
	defer func() { OrgPolicyVerdict = func(k, l, v, f string) *string { return nil } }()
	if r := RemoteSessionsPolicyDenial(); r != nil {
		t.Fatalf("default should allow, got %q", *r)
	}
	OrgPolicyVerdict = func(k, l, v, f string) *string {
		if k != PolicyAllowRemoteSessions || l != "Cloud sessions" || v != "are" {
			t.Errorf("verdict args %q %q %q", k, l, v)
		}
		return &f
	}
	r := RemoteSessionsPolicyDenial()
	if r == nil || *r != ErrCloudSessionsPolicyDenied {
		t.Fatalf("denial %v", r)
	}
}

func TestRoutinesPolicyDenial(t *testing.T) {
	defer func() { OrgPolicyVerdict = func(k, l, v, f string) *string { return nil } }()
	OrgPolicyVerdict = func(k, l, v, f string) *string {
		if k != PolicyAllowRoutines || l != "Routines" || v != "are" {
			t.Errorf("verdict args %q %q %q", k, l, v)
		}
		return &f
	}
	r := RoutinesPolicyDenial()
	if r == nil || *r != "Routines are disabled by your organization's policy." {
		t.Fatalf("denial %v", r)
	}
}

// --- events fetch -----------------------------------------------------------

func eventPage(events []map[string]any, nextCursor string) map[string]any {
	data := []map[string]any{}
	for i, e := range events {
		data = append(data, map[string]any{
			"sequence_num": i + 1, "payload": e,
		})
	}
	m := map[string]any{"data": data}
	if nextCursor != "" {
		m["next_cursor"] = nextCursor
	}
	return m
}

func assistantExitPlan(id string) map[string]any {
	return map[string]any{
		"type": "assistant", "session_id": "cse_x",
		"message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "name": "ExitPlanMode", "id": id},
		}},
	}
}

func userToolResult(id string, isErr bool, content any) map[string]any {
	return map[string]any{
		"type": "user", "session_id": "cse_x",
		"message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": id,
				"is_error": isErr, "content": content},
		}},
	}
}

// pollServer serves scripted events pages and a session_status.
func pollServer(pages func(call int64) (events []map[string]any, status string)) *httptest.Server {
	var call atomic.Int64
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			n := call.Add(1)
			evs, _ := pages(n)
			json.NewEncoder(w).Encode(eventPage(evs, ""))
			return
		}
		_, status := pages(call.Load())
		json.NewEncoder(w).Encode(map[string]any{"session_status": status})
	}))
}

func TestUltraplanPollPhases(t *testing.T) {
	srv := pollServer(func(n int64) ([]map[string]any, string) {
		switch n {
		case 1:
			return nil, "requires_action" // idle, no events -> needs_input
		case 2:
			return []map[string]any{assistantExitPlan("tu_1")}, "running" // -> plan_ready
		default:
			return []map[string]any{
				userToolResult("tu_1", false, "## Approved Plan:\n\n1. do the thing"),
			}, "idle"
		}
	})
	defer srv.Close()
	c := testClient(srv)
	var phases []string
	out, err := c.PollUltraplan(context.Background(), "cse_x", 30*time.Second, time.Millisecond,
		func(p string) { phases = append(phases, p) }, nil)
	if err != nil {
		t.Fatalf("poll err %v", err)
	}
	// dlr() trims the tail only — the leading newline after the marker stays
	if out.ExecutionTarget != "remote" || out.Plan != "\n1. do the thing" {
		t.Fatalf("outcome %+v", out)
	}
	want := []string{"needs_input", "plan_ready"}
	if fmt.Sprint(phases) != fmt.Sprint(want) {
		t.Fatalf("phases %v", phases)
	}
	if UltraplanPhaseText("running") != "ultraplan" ||
		UltraplanPhaseText("needs_input") != "ultraplan needs your input" ||
		UltraplanPhaseText("plan_ready") != "ultraplan ready" {
		t.Fatal("phase text")
	}
}

func TestUltraplanPollLocalTeleport(t *testing.T) {
	srv := pollServer(func(n int64) ([]map[string]any, string) {
		switch n {
		case 1:
			return []map[string]any{assistantExitPlan("tu_9")}, "running"
		default:
			return []map[string]any{
				userToolResult("tu_9", true, "rejected\n__ULTRAPLAN_TELEPORT_LOCAL__\nlocal plan body"),
			}, "idle"
		}
	})
	defer srv.Close()
	c := testClient(srv)
	out, err := c.PollUltraplan(context.Background(), "cse_x", 30*time.Second, time.Millisecond, nil, nil)
	if err != nil {
		t.Fatalf("poll err %v", err)
	}
	if out.ExecutionTarget != "local" || out.Plan != "local plan body" {
		t.Fatalf("outcome %+v", out)
	}
}

func TestUltraplanPollTerminated(t *testing.T) {
	srv := pollServer(func(n int64) ([]map[string]any, string) {
		return []map[string]any{{
			"type": "result", "session_id": "cse_x", "subtype": "error",
		}}, "idle"
	})
	defer srv.Close()
	c := testClient(srv)
	_, err := c.PollUltraplan(context.Background(), "cse_x", 30*time.Second, time.Millisecond, nil, nil)
	pe, ok := err.(*UltraplanPollError)
	if !ok || pe.Reason != "terminated" ||
		pe.Message != "cloud session ended (error) before plan approval" {
		t.Fatalf("err %v", err)
	}
}

func TestUltraplanPollTimeout(t *testing.T) {
	srv := pollServer(func(n int64) ([]map[string]any, string) {
		return nil, "running"
	})
	defer srv.Close()
	c := testClient(srv)
	_, err := c.PollUltraplan(context.Background(), "cse_x", 50*time.Millisecond, time.Millisecond, nil, nil)
	pe, ok := err.(*UltraplanPollError)
	if !ok || pe.Reason != "timeout_no_plan" ||
		!strings.Contains(pe.Message, "ExitPlanMode never reached") {
		t.Fatalf("err %v", err)
	}
}

func TestFetchSessionEventsSplit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
				{"sequence_num": 7, "payload": map[string]any{
					"type": "control_request", "request": map[string]any{"subtype": "x"}}},
				{"sequence_num": 8, "payload": map[string]any{
					"type": "assistant", "session_id": "cse_x", "message": map[string]any{}}},
				{"sequence_num": 9, "payload": map[string]any{
					"type": "orphan"}}, // no session_id, not a control frame: dropped
			}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"session_status": "idle"})
	}))
	defer srv.Close()
	c := testClient(srv)
	res, err := c.FetchSessionEvents(context.Background(), "cse_x", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.NewEvents) != 1 || len(res.ControlFrames) != 1 {
		t.Fatalf("split %+v", res)
	}
	if res.LastEventID != "9" || res.SessionStatus != "idle" {
		t.Fatalf("res %+v", res)
	}
}

// --- remote agent task -------------------------------------------------------

func TestRemoteAgentTaskKillArchives(t *testing.T) {
	var archived atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/archive") {
			archived.Store(true)
		}
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer srv.Close()
	c := testClient(srv)
	task := c.NewRemoteAgentTask("ultraplan", "cse_x", "t", "cmd")
	if task.Kind != "ultraplan" || task.SessionID != "cse_x" {
		t.Fatalf("task %+v", task)
	}
	if err := task.Kill(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !archived.Load() {
		t.Fatal("kill did not archive")
	}
}

// --- post-review-comment ------------------------------------------------------

func TestPostReviewComment(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/code/github/post-review-comment" {
			t.Errorf("path %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		json.NewEncoder(w).Encode(map[string]any{"comment_url": "https://github.com/o/r/pull/1#c1"})
	}))
	defer srv.Close()
	c := testClient(srv)
	res := c.PostReviewComment(context.Background(), "cse_x", nil)
	if !res.Posted || res.CommentURL != "https://github.com/o/r/pull/1#c1" || res.Kind != "posted" {
		t.Fatalf("res %+v", res)
	}
	if gotBody["session_id"] != "cse_x" {
		t.Fatalf("body %v", gotBody)
	}
}

func TestPostReviewCommentFindingsPending(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.WriteHeader(409)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"details": "findings_pending"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"comment_url": "u", "already_posted": true})
	}))
	defer srv.Close()
	c := testClient(srv)
	res := c.PostReviewComment(context.Background(), "cse_x", &ReviewPostOptions{
		Sleep: func(time.Duration) {},
	})
	if !res.Posted || !res.AlreadyPosted || calls.Load() != 3 {
		t.Fatalf("res %+v calls %d", res, calls.Load())
	}
}

func TestPostReviewCommentRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"resource_type": "github_pull_request"}})
	}))
	defer srv.Close()
	c := testClient(srv)
	res := c.PostReviewComment(context.Background(), "cse_x", nil)
	if res.Posted || res.Kind != "pull_request_not_found" ||
		!strings.Contains(res.Reason, "couldn't find the pull request") {
		t.Fatalf("res %+v", res)
	}
}
