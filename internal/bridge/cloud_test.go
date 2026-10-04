package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- GetSession (Bx/oF) ----------------------------------------------------

func TestGetSession(t *testing.T) {
	var got rec
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = rec{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), nil}
		json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{
			"id": "cse_abc", "title": "work", "session_status": "running",
			"environment_kind": "cloud",
			"session_context": map[string]any{
				"sources": []map[string]any{
					{"type": "git_repository", "url": "https://github.com/org/repo"}},
				"outcomes": []map[string]any{
					{"type": "git_worktree", "git_branches": []map[string]any{
						{"source": "claude/task-1"}}}},
			},
		}})
	}))
	defer srv.Close()
	s, err := testClient(srv).GetSession(context.Background(), "session_abc")
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != "GET" || got.Path != "/v1/code/sessions/cse_abc" {
		t.Fatalf("req: %s %s", got.Method, got.Path)
	}
	if s.ID != "cse_abc" || s.EnvironmentKind != "cloud" {
		t.Fatalf("session: %+v", s)
	}
	if src := s.SessionContext.Sources[0]; src.URL != "https://github.com/org/repo" {
		t.Fatalf("source: %+v", src)
	}
}

func TestGetSessionErrors(t *testing.T) {
	mk := func(status int, body string) error {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(body))
		}))
		defer srv.Close()
		_, err := testClient(srv).GetSession(context.Background(), "cse_x")
		return err
	}
	if err := mk(404, `{}`); err == nil ||
		err.Error() != "Session not found: cse_x" {
		t.Fatalf("404: %v", err)
	}
	if err := mk(401, `{}`); err == nil ||
		err.Error() != "Session expired. Please run /login to sign in again." {
		t.Fatalf("401: %v", err)
	}
	if err := mk(400, `{"error":{"message":"invalid session ID"}}`); err == nil ||
		err.Error() != "invalid session ID" {
		t.Fatalf("400: %v", err)
	}
	if err := mk(500, `{}`); err == nil || err.Error() != "Failed to fetch session: 500" {
		t.Fatalf("500: %v", err)
	}
}

// --- TeleportEvents (Z3e) --------------------------------------------------

func TestTeleportEventsPagination(t *testing.T) {
	var reqs []rec
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs = append(reqs, rec{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), nil})
		if r.URL.Query().Get("cursor") == "" {
			json.NewEncoder(w).Encode(map[string]any{
				"events": []map[string]any{
					{"payload": map[string]any{"type": "user"}},
					{"payload": map[string]any{"type": "assistant"}},
				},
				"has_more": true, "next_cursor": "c2"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"events": []map[string]any{
				{"type": "user"}, // bare payload (older event shape)
			},
			"has_more": false})
	}))
	defer srv.Close()
	c := testClient(srv)
	c.OrgUUID = "org-9"
	out, err := c.TeleportEvents(context.Background(), "cse_x", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("events: %d", len(out))
	}
	if len(reqs) != 2 || !strings.Contains(reqs[0].Query, "limit=1000") ||
		!strings.Contains(reqs[1].Query, "cursor=c2") {
		t.Fatalf("requests: %+v", reqs)
	}
	if reqs[0].Headers.Get("x-organization-uuid") != "org-9" {
		t.Fatal("missing x-organization-uuid")
	}
}

func TestTeleportEventsErrors(t *testing.T) {
	mk := func(status int) error {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		defer srv.Close()
		_, err := testClient(srv).TeleportEvents(context.Background(), "cse_x", 5)
		return err
	}
	if err := mk(404); err != ErrEventsNotFound {
		t.Fatalf("404: %v", err)
	}
	if err := mk(403); err == nil || err.Error() != "Access denied to session: cse_x" {
		t.Fatalf("403: %v", err)
	}
	if err := mk(500); err == nil || err.Error() != "Failed to get session events: 500" {
		t.Fatalf("500: %v", err)
	}
}

// --- IngressLog + FetchSessionLog (J3e/uss) --------------------------------

func TestFetchSessionLogFilters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"events": []map[string]any{
			{"payload": map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": "hi"}}},
			{"payload": map[string]any{"type": "assistant", "isSidechain": true}},
			{"payload": map[string]any{"type": "system"}},
			{"payload": map[string]any{"type": "assistant"}},
		}})
	}))
	defer srv.Close()
	sess := &CloudSession{ID: "cse_x", EnvironmentKind: "cloud"}
	log, err := testClient(srv).FetchSessionLog(context.Background(), sess)
	if err != nil {
		t.Fatal(err)
	}
	// sidechain + non-message types dropped → user + assistant
	if len(log.Entries) != 2 || log.EnvironmentKind != "cloud" {
		t.Fatalf("log: %d entries, kind %q", len(log.Entries), log.EnvironmentKind)
	}
}

func TestFetchSessionLogIngressFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "teleport-events") {
			w.WriteHeader(404)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/v1/session_ingress/session/cse_x") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"loglines": []any{
			map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": "hi"}},
		}})
	}))
	defer srv.Close()
	log, err := testClient(srv).FetchSessionLog(context.Background(), &CloudSession{ID: "cse_x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(log.Entries) != 1 || log.EnvironmentKind != "unknown" {
		t.Fatalf("fallback: %+v", log)
	}
}

// --- ParseGitRemote (oP) ---------------------------------------------------

func TestParseGitRemote(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/org/repo":     "github.com/org/repo",
		"https://github.com/org/repo.git": "github.com/org/repo",
		"git@github.com:org/repo.git":     "github.com/org/repo",
		"ssh://git@git.internal:22/o/r":   "git.internal/o/r",
		"https://gitlab.com/a/b":          "gitlab.com/a/b",
	} {
		g := ParseGitRemote(in)
		if g == nil || g.Host+"/"+g.slug() != want {
			t.Fatalf("%s -> %+v", in, g)
		}
	}
	for _, bad := range []string{
		"not a url at all",
		// oP requires exactly two path segments
		"https://gitlab.com/a/b/c",
	} {
		if ParseGitRemote(bad) != nil {
			t.Fatalf("%q should not parse", bad)
		}
	}
	// oeo still extracts owner/name from a deep path
	if b := parseOwnerName("https://gitlab.com/a/b/c"); b == nil || b.slug() != "b/c" {
		t.Fatalf("oeo: %+v", b)
	}
}

// --- ValidateSessionRepo (p3n) ---------------------------------------------

func sessionWithRepo(url string) *CloudSession {
	var s CloudSession
	s.SessionContext.Sources = []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}{{Type: "git_repository", URL: url}}
	return &s
}

func TestRepoValidation(t *testing.T) {
	dir := cleanRepo(t)
	git(t, dir, "remote", "set-url", "origin", "https://github.com/org/repo")

	// match
	s := sessionWithRepo("https://github.com/org/repo")
	if st := ValidateSessionRepo(dir, s); st.Status != "match" {
		t.Fatalf("match: %+v", st)
	}
	// case-insensitive slug
	if st := ValidateSessionRepo(dir, sessionWithRepo("https://github.com/ORG/Repo.git")); st.Status != "match" {
		t.Fatalf("case: %+v", st)
	}
	// mismatch: same host, different repo
	if st := ValidateSessionRepo(dir, sessionWithRepo("https://github.com/org/other")); st.Status != "mismatch" ||
		st.SessionRepo != "org/other" || st.CurrentRepo != "org/repo" {
		t.Fatalf("mismatch: %+v", st)
	}
	// no repo on session
	if st := ValidateSessionRepo(dir, &CloudSession{}); st.Status != "no_repo_required" {
		t.Fatalf("no_repo: %+v", st)
	}
	// not in a repo at all
	if st := ValidateSessionRepo(t.TempDir(), s); st.Status != "not_in_repo" {
		t.Fatalf("not_in_repo: %+v", st)
	}
	// mismatch: parseable remote on a different host, same slug
	dir1 := cleanRepo(t)
	git(t, dir1, "remote", "set-url", "origin", "git@git.internal:org/repo.git")
	if st := ValidateSessionRepo(dir1, sessionWithRepo("https://github.com/org/repo")); st.Status != "mismatch" ||
		st.CurrentHost != "git.internal" {
		t.Fatalf("host mismatch: %+v", st)
	}
	// host_unverified: remote whose owner/name extracts (oeo) but whose
	// host doesn't parse (oP needs exactly two path segments)
	dir2 := cleanRepo(t)
	git(t, dir2, "remote", "set-url", "origin", "https://internal.example.com/extra/org/repo")
	st := ValidateSessionRepo(dir2, sessionWithRepo("https://github.com/org/repo"))
	if st.Status != "host_unverified" || st.RawRemoteURL == "" {
		t.Fatalf("host_unverified: %+v", st)
	}
}

func TestRepoStatusError(t *testing.T) {
	err := repoStatusError(RepoStatus{Status: "mismatch",
		SessionRepo: "org/repo", SessionHost: "github.com",
		CurrentRepo: "me/mine", CurrentHost: "github.com"}, "cse_9")
	if err == nil || !strings.Contains(err.Error(),
		"You must run claude --teleport cse_9 from a checkout of org/repo.") ||
		!strings.Contains(err.Error(), "This repo is me/mine.") {
		t.Fatalf("mismatch text: %v", err)
	}
	err = repoStatusError(RepoStatus{Status: "not_in_repo", SessionRepo: "org/repo",
		SessionHost: "git.internal", RawRemoteURL: "weird"}, "cse_9")
	if err == nil || !strings.Contains(err.Error(),
		"from a checkout of git.internal/org/repo.") ||
		!strings.Contains(err.Error(), "Couldn't parse your git remote: weird") {
		t.Fatalf("not_in_repo text: %v", err)
	}
	if err := repoStatusError(RepoStatus{Status: "match"}, "cse_9"); err != nil {
		t.Fatal(err)
	}
}

// --- branch validation + checkout (zT/WNt) ---------------------------------

func TestValidBranchName(t *testing.T) {
	for _, bad := range []string{"", "-x", "/x", "a..b", "a//b", "a/./b", "a b", "a;b"} {
		if ValidBranchName(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
	for _, good := range []string{"main", "claude/task-1", "a+b@c", "x.y_z"} {
		if !ValidBranchName(good) {
			t.Fatalf("rejected %q", good)
		}
	}
}

func TestCheckoutSessionBranch(t *testing.T) {
	dir := cleanRepo(t)
	// add a branch to the remote
	work := filepath.Join(filepath.Dir(dir), "seed")
	git(t, work, "checkout", "-b", "claude/cloud-1")
	git(t, work, "push", "origin", "claude/cloud-1")

	if err := CheckoutSessionBranch(dir, "claude/cloud-1"); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if got := CurrentBranch(dir); got != "claude/cloud-1" {
		t.Fatalf("branch: %q", got)
	}
	if _, out, _ := gitCmd(dir, "rev-parse", "--abbrev-ref", "claude/cloud-1@{upstream}"); strings.TrimSpace(out) == "" {
		t.Fatal("upstream not set")
	}
	// invalid name is refused before any git writes
	git(t, dir, "checkout", "main")
	if err := CheckoutSessionBranch(dir, "bad..name"); err == nil ||
		!strings.Contains(err.Error(), "Invalid branch name from cloud session") {
		t.Fatalf("invalid: %v", err)
	}
	if got := CurrentBranch(dir); got != "main" {
		t.Fatalf("invalid name should not have checked out, on %q", got)
	}
	// missing branch → UserError, current branch retained
	if err := CheckoutSessionBranch(dir, "no/such"); err == nil {
		t.Fatal("expected failure")
	}
	if got := CurrentBranch(dir); got != "main" {
		t.Fatalf("failed checkout left us on %q", got)
	}
	// no .git entry → the BNt refusal
	if err := CheckoutSessionBranch(t.TempDir(), "main"); err == nil ||
		!strings.Contains(err.Error(), "Could not verify this checkout's git repository") {
		t.Fatalf("no .git: %v", err)
	}
	// empty branch is a no-op staying on current
	if err := CheckoutSessionBranch(dir, ""); err != nil || CurrentBranch(dir) != "main" {
		t.Fatalf("empty branch: %v", err)
	}
}

// --- ResumeFromCloud end-to-end --------------------------------------------

func TestResumeFromCloud(t *testing.T) {
	dir := cleanRepo(t)
	git(t, dir, "remote", "set-url", "origin", "https://github.com/org/repo")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/code/sessions/cse_9"):
			json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{
				"id": "cse_9", "environment_kind": "cloud",
				"session_context": map[string]any{
					"sources": []map[string]any{
						{"type": "git_repository", "url": "https://github.com/org/repo"}},
				}}})
		case strings.Contains(r.URL.Path, "teleport-events"):
			json.NewEncoder(w).Encode(map[string]any{
				"events": []map[string]any{
					{"payload": map[string]any{
						"type": "user",
						"message": map[string]any{
							"role":    "user",
							"content": []any{map[string]any{"type": "text", "text": "hello"}},
						},
					}},
				},
				"has_more": false,
			})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	res, err := testClient(srv).ResumeFromCloud(context.Background(), "cse_9",
		&ResumeDeps{CWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	// 1 transcript message + the continuation meta record
	if len(res.Messages) != 2 {
		t.Fatalf("messages: %d", len(res.Messages))
	}
	if res.Messages[0].Role != "user" {
		t.Fatalf("role: %s", res.Messages[0].Role)
	}
	if !strings.Contains(string(res.Messages[1].Content), "continued from another machine") {
		t.Fatalf("meta: %s", res.Messages[1].Content)
	}
	if res.EnvironmentKind != "cloud" {
		t.Fatalf("kind: %s", res.EnvironmentKind)
	}
	if got := ResumeNotice(res.BranchError); got != "Session resumed" {
		t.Fatalf("notice: %q", got)
	}
}

func TestResumeFromCloudRepoRefusal(t *testing.T) {
	dir := cleanRepo(t)
	git(t, dir, "remote", "set-url", "origin", "https://github.com/org/repo")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{
			"id": "cse_9",
			"session_context": map[string]any{
				"sources": []map[string]any{
					{"type": "git_repository", "url": "https://github.com/org/OTHER"}},
			}}})
	}))
	defer srv.Close()
	_, err := testClient(srv).ResumeFromCloud(context.Background(), "cse_9",
		&ResumeDeps{CWD: dir})
	var ue *UserError
	if err == nil || !strings.Contains(err.Error(),
		"from a checkout of org/OTHER") || !errors.As(err, &ue) {
		t.Fatalf("want mismatch UserError, got %v", err)
	}
}

func TestResumeFromCloudDirty(t *testing.T) {
	dir := cleanRepo(t)
	git(t, dir, "remote", "set-url", "origin", "https://github.com/org/repo")
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("dirty"), 0o644)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "teleport-events") {
			json.NewEncoder(w).Encode(map[string]any{"events": []any{}, "has_more": false})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{
			"id": "cse_9",
			"session_context": map[string]any{
				"sources": []map[string]any{
					{"type": "git_repository", "url": "https://github.com/org/repo"}},
				"outcomes": []map[string]any{
					{"type": "git_worktree", "git_branches": []map[string]any{
						{"source": "claude/x"}}}},
			}}})
	}))
	defer srv.Close()
	_, err := testClient(srv).ResumeFromCloud(context.Background(), "cse_9",
		&ResumeDeps{CWD: dir})
	if err == nil || !strings.Contains(err.Error(), "not clean") {
		t.Fatalf("dirty: %v", err)
	}
}

// --- org lane (mod_0963) ---------------------------------------------------

func TestImportGitHubToken(t *testing.T) {
	var got rec
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		got = rec{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body}
		json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer srv.Close()
	c := testClient(srv)
	c.OrgUUID = "org-9"
	res := c.ImportGitHubToken(context.Background(), "ghp_x")
	if !res.OK {
		t.Fatalf("res: %+v", res)
	}
	if got.Method != "POST" || got.Path != "/v1/code/github/import-token" ||
		got.Body["token"] != "ghp_x" {
		t.Fatalf("req: %+v", got)
	}
	if got.Headers.Get("anthropic-beta") != betaCCRBYOC ||
		got.Headers.Get("x-organization-uuid") != "org-9" {
		t.Fatalf("headers: %v", got.Headers)
	}
}

func TestImportGitHubTokenErrors(t *testing.T) {
	mk := func(status int, body string) *ImportTokenResult {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(body))
		}))
		defer srv.Close()
		c := testClient(srv)
		c.OrgUUID = "org-9"
		return c.ImportGitHubToken(context.Background(), "t")
	}
	if r := mk(400, `{"error":{"message":"bad token"}}`); r.Kind != "invalid_token" || r.Message != "bad token" {
		t.Fatalf("400: %+v", r)
	}
	if r := mk(401, `{}`); r.Kind != "not_signed_in" {
		t.Fatalf("401: %+v", r)
	}
	if r := mk(500, `{}`); r.Kind != "server" || r.Status != 500 {
		t.Fatalf("500: %+v", r)
	}
}

func TestGitHubAuthStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/organizations/org-9/sync/github/auth" {
			t.Errorf("path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"is_authenticated": true, "auth_source": "cli_import"})
	}))
	defer srv.Close()
	c := testClient(srv)
	c.OrgUUID = "org-9"
	st := c.GitHubAuthStatus(context.Background())
	if st == nil || !st.IsAuthenticated || st.AuthSource != "cli_import" {
		t.Fatalf("status: %+v", st)
	}
	// unknown auth_source normalizes to ""
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"is_authenticated": false, "auth_source": "weird"})
	}))
	defer srv2.Close()
	c2 := testClient(srv2)
	c2.OrgUUID = "org-9"
	if st := c2.GitHubAuthStatus(context.Background()); st == nil || st.AuthSource != "" {
		t.Fatalf("normalize: %+v", st)
	}
	// missing org → nil without a request
	c3 := testClient(srv)
	c3.OrgUUID = ""
	if c3.GitHubAuthStatus(context.Background()) != nil {
		t.Fatal("no org should short-circuit")
	}
}

// --- design consent (mod_1034) ---------------------------------------------

func TestDesignConsent(t *testing.T) {
	var reqs []rec
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		reqs = append(reqs, rec{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body})
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]bool{"agent_design_projects": true})
	}))
	defer srv.Close()
	c := testClient(srv)
	on, err := c.DesignConsentGet(context.Background())
	if err != nil || !on {
		t.Fatalf("get: %v %v", on, err)
	}
	if err := c.DesignConsentGrant(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.DesignConsentRevoke(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 3 || reqs[0].Method != "GET" || reqs[1].Method != "POST" ||
		reqs[2].Method != "DELETE" {
		t.Fatalf("reqs: %+v", reqs)
	}
	for _, r := range reqs {
		if r.Path != "/v1/design/consent" {
			t.Fatalf("path: %s", r.Path)
		}
	}
	if reqs[1].Body["consent"] != "agent_design_projects" ||
		reqs[2].Body["consent"] != "agent_design_projects" {
		t.Fatalf("bodies: %+v", reqs)
	}
}

// --- usage (fetchUtilization) ----------------------------------------------

func TestFetchUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/usage" {
			t.Errorf("path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"extra_usage": map[string]any{
				"is_enabled": true, "monthly_limit": 50000,
				"used_credits": 27140, "utilization": 54, "currency": "USD"},
			"limits": []map[string]any{
				{"kind": "spend", "group": "monthly", "percent": 54,
					"resets_at": "2026-02-01T00:00:00Z", "severity": "normal", "is_active": true}},
		})
	}))
	defer srv.Close()
	u, err := testClient(srv).FetchUsage(context.Background())
	if err != nil || u == nil || u.ExtraUsage == nil {
		t.Fatalf("usage: %v %+v", err, u)
	}
	if *u.ExtraUsage.UsedCredits != 27140 || *u.ExtraUsage.MonthlyLimit != 50000 {
		t.Fatalf("extra_usage: %+v", u.ExtraUsage)
	}
	if len(u.Limits) != 1 || u.Limits[0].Group != "monthly" {
		t.Fatalf("limits: %+v", u.Limits)
	}
}

func TestFetchUsage404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()
	u, err := testClient(srv).FetchUsage(context.Background())
	if err != nil || u != nil {
		t.Fatalf("404 should yield nil usage: %v %+v", err, u)
	}
}

func TestEnableOverage(t *testing.T) {
	var reqs []rec
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		reqs = append(reqs, rec{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body})
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer srv.Close()
	c := testClient(srv)
	c.OrgUUID = "org-9"
	if err := c.EnableOverage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 2 {
		t.Fatalf("reqs: %d", len(reqs))
	}
	if reqs[0].Method != "POST" ||
		reqs[0].Path != "/api/oauth/organizations/org-9/setup_overage_billing" ||
		reqs[0].Body["org_monthly_spend_limit"] != float64(2000) {
		t.Fatalf("setup: %+v", reqs[0])
	}
	if reqs[1].Method != "PUT" ||
		reqs[1].Path != "/api/oauth/organizations/org-9/overage_spend_limit" ||
		reqs[1].Body["is_enabled"] != true {
		t.Fatalf("limit: %+v", reqs[1])
	}
}
