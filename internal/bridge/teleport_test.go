package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- git fixture ----------------------------------------------------------

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// cleanRepo builds <dir> as a git repo with one commit, cloned from a bare
// remote so an upstream exists.
func cleanRepo(t *testing.T) (dir string) {
	t.Helper()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	work := filepath.Join(tmp, "seed")
	os.Mkdir(remote, 0o755)
	os.Mkdir(work, 0o755)
	git(t, tmp, "init", "--bare", "-b", "main", remote)
	git(t, work, "init", "-b", "main")
	git(t, work, "config", "user.email", "t@t")
	git(t, work, "config", "user.name", "t")
	os.WriteFile(filepath.Join(work, "f.txt"), []byte("x"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "-m", "init")
	git(t, work, "remote", "add", "origin", remote)
	git(t, work, "push", "-u", "origin", "main")
	// clone so the checked-out branch tracks origin/main
	dir = filepath.Join(tmp, "clone")
	git(t, tmp, "clone", remote, dir)
	git(t, dir, "config", "user.email", "t@t")
	git(t, dir, "config", "user.name", "t")
	return dir
}

// --- CheckWorktree (function Y in mod_2173.js) -----------------------------

func TestWorktreeNoGit(t *testing.T) {
	st := CheckWorktree(t.TempDir(), "cse_x")
	if st.Kind != WtNoGit {
		t.Fatalf("kind: %s", st.Kind)
	}
}

func TestWorktreeDirty(t *testing.T) {
	dir := cleanRepo(t)
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("dirty"), 0o644)
	st := CheckWorktree(dir, "cse_x")
	if st.Kind != WtDirty || st.Branch != "main" {
		t.Fatalf("got %+v", st)
	}
	if msg := MsgTeleportDirty(st.Branch); !strings.HasPrefix(msg, "You have uncommitted changes on main.") {
		t.Fatalf("dirty text: %q", msg)
	}
	// untracked files don't count ({ignoreUntracked:!0})
	dir2 := cleanRepo(t)
	os.WriteFile(filepath.Join(dir2, "untracked.txt"), []byte("u"), 0o644)
	if st := CheckWorktree(dir2, "cse_x"); st.Kind != WtReady {
		t.Fatalf("untracked should stay ready, got %+v", st)
	}
}

func TestWorktreeUnpushed(t *testing.T) {
	dir := cleanRepo(t)
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("more"), 0o644)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "wip")
	st := CheckWorktree(dir, "cse_x")
	if st.Kind != WtUnpushed || st.Branch != "main" {
		t.Fatalf("got %+v", st)
	}
	if msg := MsgTeleportUnpushed(st.Branch); !strings.HasPrefix(msg, "main has commits that haven’t been pushed.") {
		t.Fatalf("unpushed text: %q", msg)
	}
	// a repo with no upstream at all is also "unpushed" (!Qir)
	tmp := t.TempDir()
	git(t, tmp, "init", "-b", "main")
	git(t, tmp, "config", "user.email", "t@t")
	git(t, tmp, "config", "user.name", "t")
	os.WriteFile(filepath.Join(tmp, "f"), []byte("x"), 0o644)
	git(t, tmp, "add", ".")
	git(t, tmp, "commit", "-m", "init")
	if st := CheckWorktree(tmp, "cse_x"); st.Kind != WtUnpushed {
		t.Fatalf("no-upstream should be unpushed, got %+v", st)
	}
}

func TestWorktreeReadyBranchName(t *testing.T) {
	dir := cleanRepo(t)
	id := "cse_abcdef1234567890"
	st := CheckWorktree(dir, id)
	if st.Kind != WtReady {
		t.Fatalf("got %+v", st)
	}
	// real: `claude/teleport-${sessionId.slice(-8)}` — last 8 of the
	// bridge session id, not a sha.
	if st.OutcomeBranch != "claude/teleport-34567890" {
		t.Fatalf("branch: %q", st.OutcomeBranch)
	}
}

// --- MoveToCloud (function L in mod_2173.js) -------------------------------

func TestMoveToCloudShape(t *testing.T) {
	var got rec
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		got = rec{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body}
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{
			"environment_kind": "cloud", "environment_id": "env_1"}})
	}))
	defer srv.Close()
	res := testClient(srv).MoveToCloud(context.Background(), &MoveToCloudParams{
		SessionID: "cse_abc", EnvironmentID: "env_1", OutcomeBranch: "claude/teleport-abc",
	})
	if !res.OK || res.EnvironmentKind != "cloud" || res.EnvironmentID != "env_1" {
		t.Fatalf("res: %+v", res)
	}
	if got.Method != "POST" || got.Path != "/v1/code/sessions/cse_abc/move-to-cloud" {
		t.Fatalf("req: %s %s", got.Method, got.Path)
	}
	if got.Body["environment_id"] != "env_1" || got.Body["outcome_branch"] != "claude/teleport-abc" {
		t.Fatalf("body: %v", got.Body)
	}
}

func TestMoveToCloudServerErrors(t *testing.T) {
	mk := func(status int, body string) *MoveToCloudResult {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(body))
		}))
		defer srv.Close()
		return testClient(srv).MoveToCloud(context.Background(), &MoveToCloudParams{
			SessionID: "cse_abc", EnvironmentID: "env_1"})
	}
	if r := mk(500, `{}`); r.OK || !r.MayHaveCommitted ||
		!strings.Contains(r.Message, "The server responded with 500. The move may still have gone through") {
		t.Fatalf("500: %+v", r)
	}
	if r := mk(400, `{"error":{"message":"not a bridge session","type":"invalid_request_error"}}`); r.OK ||
		!r.MayHaveCommitted || r.Message != "This session is already on cloud — refresh the web UI to see it." {
		t.Fatalf("400 bridge: %+v", r)
	}
	if r := mk(403, `{"error":{"message":"nope","type":"permission_error"}}`); r.OK ||
		r.MayHaveCommitted || r.Message != "nope" || r.Reason != "permission_error" {
		t.Fatalf("403: %+v", r)
	}
	if r := mk(400, `{"error":{"message":"The request was invalid.","type":"invalid_request_error"}}`); r.OK ||
		!strings.Contains(r.Message, "The server couldn’t move this session (HTTP 400)") {
		t.Fatalf("invalid-req collapse: %+v", r)
	}
}

func TestMoveToCloudUnreachable(t *testing.T) {
	// closed server: every request errors at the transport layer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	c := &Client{BaseURL: url, GetAccessToken: func() string { return "t" }}
	res := c.MoveToCloud(context.Background(), &MoveToCloudParams{SessionID: "cse_abc"})
	if res.OK {
		t.Fatal("expected failure")
	}
	if !strings.HasPrefix(res.Message, "Couldn’t reach the server — ") {
		t.Fatalf("msg: %q", res.Message)
	}
}

// --- TeleportToCloud orchestration (j + W) ---------------------------------

// guardedCur is a mutex-safe shared string for the bridge-session id —
// TeleportToCloud reads/writes it from a goroutine while the test polls.
type guardedCur struct {
	mu sync.Mutex
	v  string
}

func (g *guardedCur) get() string  { g.mu.Lock(); defer g.mu.Unlock(); return g.v }
func (g *guardedCur) set(v string) { g.mu.Lock(); defer g.mu.Unlock(); g.v = v }

func readyRepoDeps(t *testing.T, id string) (*TeleportDeps, *guardedCur) {
	t.Helper()
	cur := &guardedCur{v: id}
	return &TeleportDeps{
		CWD:             cleanRepo(t),
		BridgeSessionID: func() string { return cur.get() },
		StampBridgeSession: func(v string) {
			cur.set(v)
		},
		Environment:   func(context.Context) (string, error) { return "env_1", nil },
		ReconnectWait: time.Millisecond,
	}, cur
}

func TestTeleportPreconditions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no HTTP should leave preconditions")
	}))
	defer srv.Close()
	c := testClient(srv)

	// restricted first ($T) — beats a missing bridge session
	d, _ := readyRepoDeps(t, "")
	d.Restricted = func() bool { return true }
	if got := TeleportToCloud(context.Background(), c, d); got != MsgTeleportRestricted {
		t.Fatalf("restricted: %q", got)
	}
	d.Restricted = func() bool { return false }

	// no bridge session
	if got := TeleportToCloud(context.Background(), c, d); got != MsgTeleportNoBridge {
		t.Fatalf("no_bridge: %q", got)
	}

	// no auth
	d.BridgeSessionID = func() string { return "cse_abc" }
	noAuth := &Client{BaseURL: srv.URL, GetAccessToken: func() string { return "" }}
	if got := TeleportToCloud(context.Background(), noAuth, d); got != MsgTeleportNoAuth {
		t.Fatalf("no_auth: %q", got)
	}

	// dirty worktree refuses before any HTTP
	d.CWD = cleanRepo(t)
	os.WriteFile(filepath.Join(d.CWD, "f.txt"), []byte("dirty"), 0o644)
	if got := TeleportToCloud(context.Background(), c, d); !strings.HasPrefix(got, "You have uncommitted changes on") {
		t.Fatalf("dirty: %q", got)
	}
}

func TestTeleportSuccessDisconnected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"session":{"environment_kind":"cloud","environment_id":"env_1"}}`))
	}))
	defer srv.Close()
	d, cur := readyRepoDeps(t, "cse_abcdef1234567890")
	got := TeleportToCloud(context.Background(), testClient(srv), d)
	want := "Session now in the cloud: https://claude.ai/code/cse_abcdef1234567890?from=cli\n" + MsgRCInactive
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if cur.get() != "" {
		t.Fatalf("bridge claim should be cleared after move, got %q", cur.get())
	}
}

func TestTeleportReconnected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"session":{}}`))
	}))
	defer srv.Close()
	d, cur := readyRepoDeps(t, "cse_old000001")
	d.ReconnectWait = 2 * time.Second
	d.PollInterval = 5 * time.Millisecond
	// write the fresh stamp only after the move clears the claim —
	// precondition worktree checks take variable time, so a fixed sleep
	// raced stamp("") and could be cleared before the poll started
	go func() {
		for cur.get() != "" {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(10 * time.Millisecond)
		cur.set("cse_new000002") // remote-control loop stamped a fresh session
	}()
	got := TeleportToCloud(context.Background(), testClient(srv), d)
	if !strings.Contains(got, "Session now in the cloud: https://claude.ai/code/cse_old000001?from=cli") ||
		!strings.Contains(got, "New /remote-control session is active · Continue here, on your phone, or at https://claude.ai/code/cse_new000002?from=cli") {
		t.Fatalf("reconnected text: %q", got)
	}
}

func TestTeleportServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	d, _ := readyRepoDeps(t, "cse_abc")
	got := TeleportToCloud(context.Background(), testClient(srv), d)
	if !strings.HasPrefix(got, "Couldn’t teleport: The server responded with 500.") ||
		!strings.HasSuffix(got, MsgRCInactive) {
		t.Fatalf("mayHaveCommitted text: %q", got)
	}
}

func TestTeleportCleanFailureRestoresClaim(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"error":{"message":"denied","type":"permission_error"}}`))
	}))
	defer srv.Close()
	d, cur := readyRepoDeps(t, "cse_abc")
	got := TeleportToCloud(context.Background(), testClient(srv), d)
	if got != "Couldn’t teleport: denied" {
		t.Fatalf("failure text: %q", got)
	}
	if cur.get() != "cse_abc" {
		t.Fatalf("non-committed failure must restore the claim, got %q", cur.get())
	}
}

// --- session web URL (lc/Nmt) -----------------------------------------------

func TestSessionWebURL(t *testing.T) {
	if u := SessionWebURL("session_abc", ""); u != "https://claude.ai/code/cse_abc?from=cli" {
		t.Fatalf("prod: %q", u)
	}
	if u := SessionWebURL("cse_x_local_y", ""); u != "http://localhost:4000/code/cse_x_local_y?from=cli" {
		t.Fatalf("local: %q", u)
	}
	if u := SessionWebURL("cse_x", "https://api.staging.example"); u != "https://claude-ai.staging.ant.dev/code/cse_x?from=cli" {
		t.Fatalf("staging: %q", u)
	}
}
