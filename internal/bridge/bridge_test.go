package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// recorded request for shape assertions.
type rec struct {
	Method, Path, Query string
	Headers             http.Header
	Body                map[string]any
}

func testClient(srv *httptest.Server) *Client {
	return &Client{
		BaseURL:       srv.URL,
		RunnerVersion: "2.1.288",
		GetAccessToken: func() string {
			return "oauth-tok"
		},
	}
}

// --- API shape tests -----------------------------------------------------

func TestReconnectSessionShape(t *testing.T) {
	var got rec
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		got = rec{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body}
		w.WriteHeader(200)
	}))
	c := testClient(srv)
	err := c.ReconnectSession(context.Background(), "env_1", "cse_abc")
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != "POST" || got.Path != "/v1/environments/env_1/bridge/reconnect" {
		t.Fatalf("bad request: %s %s", got.Method, got.Path)
	}
	if got.Body["session_id"] != "cse_abc" {
		t.Fatalf("body: %v", got.Body)
	}
	if got.Headers.Get("Authorization") != "Bearer oauth-tok" {
		t.Fatalf("auth header: %v", got.Headers.Get("Authorization"))
	}
	if got.Headers.Get("anthropic-beta") != betaEnvironments {
		t.Fatalf("beta: %v", got.Headers.Get("anthropic-beta"))
	}
	if got.Headers.Get("x-environment-runner-version") != "2.1.288" {
		t.Fatalf("runner version header missing")
	}
	if got.Headers.Get("anthropic-version") != anthropicVersion {
		t.Fatalf("anthropic-version")
	}
}

func TestRegisterBody(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"environment_id": "env_x", "environment_secret": "sec",
			"environment_secret_expires_in": 7200,
		})
	}))
	defer srv.Close()
	c := testClient(srv)
	res, err := c.RegisterBridgeEnvironment(context.Background(), &RegisterParams{
		MachineName: "host1", MachineID: "mid",
		Directory: "/repo", Branch: "main", GitRepoURL: "git@x:y",
		MaxSessions: 4,
		Metadata:    map[string]any{"worker_type": "cli"},
		Advert:      &ToolHostAdvert{Kind: "advertise", CLIVersion: "2.1.288", Platform: "darwin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.EnvironmentID != "env_x" || res.EnvironmentSecret != "sec" || res.EnvironmentSecretExpiresIn != 7200 {
		t.Fatalf("result: %+v", res)
	}
	md := body["metadata"].(map[string]any)
	if md["tool_host"] != "2" || md["tool_host_cc_version"] != "2.1.288" || md["tool_host_platform"] != "darwin" {
		t.Fatalf("advert metadata: %v", md)
	}
	if md["worker_type"] != "cli" || md["machine_id"] != "mid" {
		t.Fatalf("metadata: %v", md)
	}
	if body["machine_name"] != "host1" || body["directory"] != "/repo" || body["branch"] != "main" ||
		body["git_repo_url"] != "git@x:y" || body["max_sessions"] != float64(4) {
		t.Fatalf("body: %v", body)
	}
}

func TestRegister409FolderAlreadyServed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"type": "bridge_folder_already_served", "message": "taken"},
		})
	}))
	defer srv.Close()
	c := testClient(srv)
	_, err := c.RegisterBridgeEnvironment(context.Background(), &RegisterParams{
		Metadata: map[string]any{},
	})
	var fe *FatalError
	if err == nil || !errorAsT(err, &fe) || fe.Status != 409 {
		t.Fatalf("want 409 FatalError, got %v", err)
	}
	if fe.Message != "taken" {
		t.Fatalf("msg: %q", fe.Message)
	}
}

func TestOAuth401RefreshRetry(t *testing.T) {
	var calls int32
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		auth := r.Header.Get("Authorization")
		tokens = append(tokens, auth)
		if n == 1 {
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "expired"}})
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	tok := "old"
	c := &Client{
		BaseURL:        srv.URL,
		RunnerVersion:  "v",
		GetAccessToken: func() string { return tok },
		OnAuth401: func(old string) bool {
			if old != "old" {
				t.Errorf("refresh got %q", old)
			}
			tok = "new"
			return true
		},
	}
	if err := c.ReconnectSession(context.Background(), "e", "cse_s"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
	if tokens[0] != "Bearer old" || tokens[1] != "Bearer new" {
		t.Fatalf("tokens: %v", tokens)
	}
}

func TestNoOAuthRefreshHandler(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(401)
	}))
	defer srv.Close()
	c := testClient(srv)
	err := c.ReconnectSession(context.Background(), "e", "cse_s")
	var fe *FatalError
	if !errorAsT(err, &fe) || fe.Status != 401 {
		t.Fatalf("want 401 FatalError, got %v", err)
	}
	if !strings.Contains(fe.Message, "Authentication failed (401)") ||
		!strings.Contains(fe.Message, ErrNoOAuthToken) {
		t.Fatalf("msg: %q", fe.Message)
	}
	if calls != 1 {
		t.Fatalf("no retry expected, got %d", calls)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
		type_  string
	}{
		{403, `{"error":{"type":"environment_expired","message":"x"}}`, "Remote Control session expired.", "environment_expired"},
		{403, `{"error":{"message":"no"}}`, "Poll: Access denied (403): no. Check your organization permissions.", ""},
		{404, `{}`, "Poll: Not found (404). Remote Control may not be available for this organization.", ""},
		{410, `{}`, "Remote Control session expired.", "environment_expired"},
	}
	for _, tc := range cases {
		err := raise(tc.status, []byte(tc.body), "Poll", http.Header{})
		if err.Error() != tc.want {
			t.Errorf("%d: got %q want %q", tc.status, err.Error(), tc.want)
		}
		var fe *FatalError
		if errorAsT(err, &fe) && fe.ErrorType != tc.type_ {
			t.Errorf("%d: type %q want %q", tc.status, fe.ErrorType, tc.type_)
		}
	}
}

func TestRaise429(t *testing.T) {
	h := http.Header{"Retry-After": {"7"}}
	err := raise(429, nil, "Poll", h)
	var re *RateLimitError
	if !errorAsT2(err, &re) {
		t.Fatalf("want RateLimitError, got %T %v", err, err)
	}
	if re.RetryAfterMs != 7000 {
		t.Fatalf("retryAfterMs=%d", re.RetryAfterMs)
	}
	if re.Message != "Poll: Rate limited (429). Polling too frequently." {
		t.Fatalf("msg %q", re.Message)
	}
	// 429 must NOT classify as fatal
	if _, fatal := fatalStatus(err); fatal {
		t.Fatal("429 wrongly fatal")
	}
}

func TestFatalStatusClassifier(t *testing.T) {
	for _, st := range []int{400, 401, 403, 404, 409, 410} {
		se := &statusError{Status: st}
		if got, _ := fatalStatus(se); got != st {
			t.Fatalf("%d not fatal", st)
		}
	}
	for _, st := range []int{408, 429, 500, 502} {
		se := &statusError{Status: st}
		if _, fatal := fatalStatus(se); fatal {
			t.Fatalf("%d wrongly fatal", st)
		}
	}
}

func TestPollForWorkQuery(t *testing.T) {
	var got rec
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = rec{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery}
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"id": "w1", "secret": "s", "data": map[string]any{"type": "session", "id": "cse_1"},
		})
	}))
	defer srv.Close()
	c := testClient(srv)
	w, err := c.PollForWork(context.Background(), "env9", "envsecret", 5000, 2000)
	if err != nil || w == nil {
		t.Fatalf("err=%v w=%v", err, w)
	}
	if got.Query != "reclaim_older_than_ms=5000&poll_interval_ms=2000" {
		t.Fatalf("query: %s", got.Query)
	}
	var wd WorkData
	json.Unmarshal(w.Data, &wd)
	if wd.Type != "session" || wd.ID != "cse_1" {
		t.Fatalf("data: %+v", wd)
	}
}

func TestPollForWorkEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	defer srv.Close()
	c := testClient(srv)
	w, err := c.PollForWork(context.Background(), "e", "s", 0, 0)
	if err != nil || w != nil {
		t.Fatalf("want nil,nil got %v,%v", w, err)
	}
}

func TestHeartbeatAndArchivePaths(t *testing.T) {
	var paths []string
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		paths = append(paths, r.Method+" "+r.URL.Path)
		bodies = append(bodies, b)
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			json.NewEncoder(w).Encode(map[string]any{"lease_extended": true, "state": "active"})
			return
		}
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer srv.Close()
	c := testClient(srv)
	ctx := context.Background()
	hb, err := c.HeartbeatWork(ctx, "env", "wk", "ingress-tok")
	if err != nil || !hb.LeaseExtended || hb.State != "active" {
		t.Fatalf("hb: %+v err=%v", hb, err)
	}
	if err := c.ArchiveSession(ctx, "cse_9"); err != nil {
		t.Fatal(err)
	}
	if err := c.ArchiveSession(ctx, "session_9"); err != nil { // mc() -> cse_9
		t.Fatal(err)
	}
	want := []string{
		"POST /v1/environments/env/work/wk/heartbeat",
		"POST /v1/code/sessions/cse_9/archive",
		"POST /v1/code/sessions/cse_9/archive",
	}
	if fmt.Sprint(paths) != fmt.Sprint(want) {
		t.Fatalf("paths: %v", paths)
	}
}

func TestV1CompatArchive(t *testing.T) {
	var gotPath, gotOrg, gotBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotOrg = r.Header.Get("x-organization-uuid")
		gotBeta = r.Header.Get("anthropic-beta")
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c := testClient(srv)
	c.OrgUUID = "org-1"
	c.UseCcrV2SessionCrud = func() bool { return false }
	if err := c.ArchiveSession(context.Background(), "cse_zz"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/sessions/session_zz/archive" {
		t.Fatalf("path %s", gotPath)
	}
	if gotOrg != "org-1" || gotBeta != betaCCRBYOC {
		t.Fatalf("headers org=%q beta=%q", gotOrg, gotBeta)
	}
}

func TestCreateCodeSession(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{"id": "cse_new"}})
	}))
	defer srv.Close()
	c := testClient(srv)
	res := c.CreateCodeSession(context.Background(), &CreateSessionParams{
		Title: "t", Cwd: "/d", Model: "m", Tags: []string{"a"}, SessionGrouping: "g1",
	})
	if res == nil || res.SessionID != "cse_new" {
		t.Fatalf("res %+v", res)
	}
	if body["title"] != "t" || body["session_grouping_id"] != "g1" {
		t.Fatalf("body %v", body)
	}
	if _, ok := body["bridge"]; !ok {
		t.Fatal("missing bridge:{} marker")
	}
	cfg := body["config"].(map[string]any)
	if cfg["cwd"] != "/d" || cfg["model"] != "m" {
		t.Fatalf("config %v", cfg)
	}
}

func TestCreateCodeSessionMalformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{"id": "notcse"}})
	}))
	defer srv.Close()
	c := testClient(srv)
	res := c.CreateCodeSession(context.Background(), &CreateSessionParams{})
	if res == nil || res.Reason != "malformed_response" || !res.Terminal {
		t.Fatalf("res %+v", res)
	}
}

func TestMintBridgeCredentials(t *testing.T) {
	var body map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(map[string]any{
			"worker_jwt": "jwt", "expires_in": 300,
			"api_base_url": "https://api.example", "worker_epoch": 7,
		})
	}))
	defer srv.Close()
	c := testClient(srv)
	epoch := int64(6)
	res := c.MintBridgeCredentials(context.Background(), "cse_x", &epoch)
	if res == nil || res.Creds == nil {
		t.Fatalf("res %+v", res)
	}
	if path != "/v1/code/sessions/cse_x/bridge" {
		t.Fatalf("path %s", path)
	}
	if body["expected_epoch"] != float64(6) {
		t.Fatalf("body %v", body)
	}
	if res.Creds.WorkerJWT != "jwt" || res.Creds.WorkerEpoch != 7 || res.Creds.ExpiresIn != 300 {
		t.Fatalf("creds %+v", res.Creds)
	}
}

func TestMintInvalidSessionID(t *testing.T) {
	c := testClient(httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	res := c.MintBridgeCredentials(context.Background(), "bad id!", nil)
	if res == nil || res.Reason != "invalid_session_id" || !res.Terminal {
		t.Fatalf("res %+v", res)
	}
}

func TestRegisterWorker(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/code/sessions/cse_a/worker/register" {
			t.Errorf("path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"worker_epoch": 3})
	}))
	defer srv.Close()
	c := testClient(srv)
	ep, err := c.RegisterWorker(context.Background(), SessionURL(srv.URL, "cse_a"), "ing")
	if err != nil || ep != 3 {
		t.Fatalf("ep=%d err=%v", ep, err)
	}
	if auth != "Bearer ing" {
		t.Fatalf("auth %q", auth)
	}
}

func TestIDHelpers(t *testing.T) {
	if codeSessionID("session_x") != "cse_x" || codeSessionID("cse_x") != "cse_x" {
		t.Fatal("mc")
	}
	if legacySessionID("cse_x") != "session_x" || legacySessionID("session_x") != "session_x" {
		t.Fatal("Zd")
	}
	if bareSessionID("cse_x") != "x" || bareSessionID("session_x") != "x" || bareSessionID("plain") != "plain" {
		t.Fatal("sr")
	}
	if checkID("abc-DEF_123", "x") != nil || checkID("a/b", "x") == nil || checkID("", "x") == nil {
		t.Fatal("Nh")
	}
	if SessionURL("https://b/", "cse_1") != "https://b/v1/code/sessions/cse_1" {
		t.Fatal("mAe")
	}
}

// --- entitlement tests ---------------------------------------------------

func TestGateOrder(t *testing.T) {
	if err := CheckDisabledLocally(true); err == nil ||
		err.Error() != "Error: "+ErrDisabledManaged {
		t.Fatalf("disabled msg %v", err)
	}
	if err := CheckOAuth(false); err == nil || err.Error() != ErrLoginRequired {
		t.Fatalf("login msg %v", err)
	}
	defer func() { OrgPolicyVerdict = func(k, l, v, f string) *string { return nil } }()
	deny := "denied"
	OrgPolicyVerdict = func(k, l, v, f string) *string {
		if k != PolicyAllowRemoteControl || l != "Remote Control" || v != "is" {
			t.Errorf("verdict args %q %q %q", k, l, v)
		}
		return &deny
	}
	if err := CheckOrgPolicy(); err == nil || err.Error() != "Error: denied" {
		t.Fatalf("policy %v", err)
	}
}

func TestMinVersion(t *testing.T) {
	cfg := DefaultREPLConfig()
	if v := CheckMinVersion(cfg, "2.1.288"); v != "" {
		t.Fatalf("default floor should pass: %q", v)
	}
	cfg.MinVersion = "9.0.0"
	v := CheckMinVersion(cfg, "2.1.288")
	want := "Your version of Claude Code (2.1.288) is too old for Remote Control.\n" +
		"Version 9.0.0 or higher is required. Run `claude update` to update."
	if v != want {
		t.Fatalf("got %q want %q", v, want)
	}
	cfg.MinVersion = "2.1.200"
	if v := CheckMinVersion(cfg, "2.1.288"); v != "" {
		t.Fatalf("newer should pass: %q", v)
	}
}

// --- backoff / scheduling math -------------------------------------------

func TestBackoffDoubling(t *testing.T) {
	p := DefaultPacing()
	if d := p.NextBackoff(0); d != 2*time.Second {
		t.Fatalf("f0 %v", d)
	}
	if d := p.NextBackoff(1); d != 4*time.Second {
		t.Fatalf("f1 %v", d)
	}
	if d := p.NextBackoff(2); d != 8*time.Second {
		t.Fatalf("f2 %v", d)
	}
	if d := p.NextBackoff(20); d != 120*time.Second {
		t.Fatalf("cap %v", d)
	}
}

func TestSecretRenewal(t *testing.T) {
	// lifetime < 1h: never proactively renew (real returns undefined)
	if at := SecretRenewalAt(1800, 0, 0.5); at != 0 {
		t.Fatalf("short life %d", at)
	}
	// lifetime 2h: margin = min(max(720s,900s),86400s)=900s*(1+.25r)
	at := SecretRenewalAt(7200, 0, 0)
	if at != 7200000-900000 {
		t.Fatalf("at=%d", at)
	}
	atJitter := SecretRenewalAt(7200, 0, 1.0)
	if atJitter != 7200000-1125000 {
		t.Fatalf("jitter at=%d", atJitter)
	}
	// huge lifetime: margin capped at 24h
	atBig := SecretRenewalAt(30*86400, 0, 0)
	if atBig != int64(30*86400-86400)*1000 {
		t.Fatalf("big at=%d", atBig)
	}
}

// --- work secret ----------------------------------------------------------

func TestDecodeWorkSecret(t *testing.T) {
	good := base64.RawURLEncoding.EncodeToString([]byte(
		`{"version":1,"session_ingress_token":"tok","api_base_url":"https://x"}`))
	s, err := DecodeWorkSecret(good)
	if err != nil || s.SessionIngressToken != "tok" || s.APIBaseURL != "https://x" {
		t.Fatalf("s=%+v err=%v", s, err)
	}
	badJSON := base64.RawURLEncoding.EncodeToString([]byte("{oops"))
	if _, err := DecodeWorkSecret(badJSON); err == nil ||
		err.Error() != "Work secret is not valid JSON" {
		t.Fatalf("bad json %v", err)
	}
	v2 := base64.RawURLEncoding.EncodeToString([]byte(
		`{"version":2,"session_ingress_token":"t","api_base_url":"u"}`))
	if _, err := DecodeWorkSecret(v2); err == nil || err.Error() != "Unsupported work secret version" {
		t.Fatalf("v2 %v", err)
	}
	noTok := base64.RawURLEncoding.EncodeToString([]byte(`{"version":1,"api_base_url":"u"}`))
	if _, err := DecodeWorkSecret(noTok); err == nil ||
		err.Error() != "Invalid work secret: missing or empty session_ingress_token" {
		t.Fatalf("no tok %v", err)
	}
	noURL := base64.RawURLEncoding.EncodeToString([]byte(`{"version":1,"session_ingress_token":"t"}`))
	if _, err := DecodeWorkSecret(noURL); err == nil ||
		err.Error() != "Invalid work secret: missing api_base_url" {
		t.Fatalf("no url %v", err)
	}
}

// --- heartbeat -> reconnect path ------------------------------------------

// heartbeatOnce must call bridge/reconnect for sessions whose token went
// 401/403, with real's "token expired — re-queuing" semantics.
func TestHeartbeatRequeueOn401(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if strings.Contains(r.URL.Path, "/heartbeat") {
			w.WriteHeader(401)
			return
		}
		if strings.Contains(r.URL.Path, "/bridge/reconnect") {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	b := &Bridge{
		API:   testClient(srv),
		envID: "env1", envSecret: "sec",
		sessions: map[string]*WorkSession{
			"cse_s1": {SessionID: "cse_s1", WorkID: "w1", AccessToken: "tok"},
		},
	}
	out := b.heartbeatOnce(context.Background())
	if out != "auth_failed" {
		t.Fatalf("outcome %q", out)
	}
	want := []string{
		"POST /v1/environments/env1/work/w1/heartbeat",
		"POST /v1/environments/env1/bridge/reconnect",
	}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Fatalf("calls %v", calls)
	}
}

func TestPollLoopRegisterThenPoll(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/v1/environments/bridge":
			json.NewEncoder(w).Encode(map[string]any{
				"environment_id": "env1", "environment_secret": "sec",
			})
		case strings.HasSuffix(r.URL.Path, "/work/poll"):
			w.WriteHeader(204) // no work
		default:
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	RemoteConfigSource = func(flag string) (map[string]any, bool) {
		return map[string]any{"multisession_poll_interval_ms_not_at_capacity": 100}, true
	}
	defer func() { RemoteConfigSource = func(string) (map[string]any, bool) { return nil, false } }()
	b := &Bridge{API: testClient(srv), Env: &RegisterParams{
		MachineName: "h", Directory: "/d", MaxSessions: 1,
		Metadata: map[string]any{},
	}}
	_ = b.Run(ctx)
	if len(paths) == 0 || paths[0] != "POST /v1/environments/bridge" {
		t.Fatalf("first call %v", paths)
	}
	sawPoll := false
	for _, p := range paths {
		if p == "GET /v1/environments/env1/work/poll" {
			sawPoll = true
		}
	}
	if !sawPoll {
		t.Fatalf("no poll in %v", paths)
	}
}

// helpers
func errorAsT(err error, t **FatalError) bool {
	for err != nil {
		if fe, ok := err.(*FatalError); ok {
			*t = fe
			return true
		}
		break
	}
	return false
}
func errorAsT2(err error, t **RateLimitError) bool {
	if re, ok := err.(*RateLimitError); ok {
		*t = re
		return true
	}
	return false
}
