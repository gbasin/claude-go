package bridge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

func TestUltrareviewPreflightRequestShape(t *testing.T) {
	var gotAuth, gotReqID, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotReqID = r.Header.Get("x-client-request-id")
		w.Write([]byte(`{"action":"proceed","billing_note":"billed as credits","github_login":"octo"}`))
	}))
	defer srv.Close()
	pf := testClient(srv).FetchUltrareviewPreflight(context.Background())
	if gotPath != "/v1/ultrareview/preflight" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer oauth-tok" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotReqID == "" {
		t.Error("missing x-client-request-id")
	}
	if pf == nil || pf.Action != "proceed" || *pf.GithubLogin != "octo" {
		t.Fatalf("pf = %+v", pf)
	}
}

func TestUltrareviewPreflightFixture(t *testing.T) {
	t.Setenv(UltrareviewPreflightFixtureEnv,
		`{"action":"blocked","blocked":{"message":"org says no","action_url":"https://x","reason":"policy"}}`)
	pf := (&Client{}).FetchUltrareviewPreflight(context.Background())
	out := ResolveUltrareviewPreflight(pf, false)
	if out.Kind != PreflightBlocked || out.Message != "org says no" ||
		out.ActionURL != "https://x" || out.Reason != "policy" {
		t.Fatalf("out = %+v", out)
	}
}

func TestResolvePreflightLadder(t *testing.T) {
	if out := ResolveUltrareviewPreflight(nil, false); out.Kind != PreflightProceed || !out.PreflightUnavailable {
		t.Fatalf("nil → %+v", out)
	}
	confirm := &UltrareviewPreflight{Action: "confirm"}
	if out := ResolveUltrareviewPreflight(confirm, false); out.Kind != PreflightNeedsConfirm {
		t.Fatalf("confirm → %+v", out)
	}
	if out := ResolveUltrareviewPreflight(confirm, true); out.Kind != PreflightProceed {
		t.Fatalf("confirmed → %+v", out)
	}
}

func TestPreflightNonFirstParty(t *testing.T) {
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	pf := testClient(httptest.NewServer(http.NotFoundHandler())).FetchUltrareviewPreflight(context.Background())
	if pf == nil || pf.Action != "blocked" || pf.Blocked.Reason != "data_residency" {
		t.Fatalf("pf = %+v", pf)
	}
}

func TestBughunterEnv(t *testing.T) {
	env := (BughunterConfig{}).Env()
	want := map[string]string{
		"BUGHUNTER_DRY_RUN":              "1",
		"AUTOPATCH_ENABLE_ASIMOV":        "1",
		"BUGHUNTER_CODE_QUALITY":         "1",
		"BUGHUNTER_FLEET_SIZE":           "5",
		"BUGHUNTER_MAX_DURATION":         "10",
		"BUGHUNTER_AGENT_TIMEOUT":        "600",
		"BUGHUNTER_TOTAL_WALLCLOCK":      "22",
		"BUGHUNTER_RESULT_RELAY_MAX_SEC": "2520", // (45-3)*60
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q want %q", k, env[k], v)
		}
	}
	// clampN: >cap returns the DEFAULT (real's N, not a clamp)
	c := BughunterConfig{FleetSize: 25, AgentTimeoutSeconds: 90, ClientWaitMinutes: 50}
	e := c.Env()
	if e["BUGHUNTER_FLEET_SIZE"] != "5" {
		t.Errorf("fleet 25 (>cap) → %q, want default 5", e["BUGHUNTER_FLEET_SIZE"])
	}
	if e["BUGHUNTER_AGENT_TIMEOUT"] != "90" {
		t.Errorf("timeout 90 → %q", e["BUGHUNTER_AGENT_TIMEOUT"])
	}
	if e["BUGHUNTER_RESULT_RELAY_MAX_SEC"] != "2820" {
		t.Errorf("relay (50-3)*60 → %q", e["BUGHUNTER_RESULT_RELAY_MAX_SEC"])
	}
	// code_quality === false drops the var entirely
	off := false
	e = (BughunterConfig{CodeQuality: &off}).Env()
	if _, ok := e["BUGHUNTER_CODE_QUALITY"]; ok {
		t.Error("code_quality=false must omit BUGHUNTER_CODE_QUALITY")
	}
	// model + dev bundle passthrough
	t.Setenv("BUGHUNTER_DEV_BUNDLE_B64", "AAAA")
	e = (BughunterConfig{Model: "claude-x"}).Env()
	if e["BUGHUNTER_MODEL"] != "claude-x" || e["BUGHUNTER_DEV_BUNDLE_B64"] != "AAAA" {
		t.Errorf("env = %v", e)
	}
}

func TestBughunterConfigOverlay(t *testing.T) {
	old := RemoteConfigSource
	defer func() { RemoteConfigSource = old }()
	RemoteConfigSource = func(flag string) (map[string]any, bool) {
		if flag == "tengu_review_bughunter_config" {
			return map[string]any{"enabled": false, "fleet_size": 9, "cost_note": "$5"}, true
		}
		return nil, false
	}
	cfg := LoadBughunterConfig()
	if cfg.EnabledGate() {
		t.Error("enabled:false is the kill switch")
	}
	if cfg.CostNote() != "$5" || cfg.Env()["BUGHUNTER_FLEET_SIZE"] != "9" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestUploadGitBundle(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		full := append([]string{"-C", dir}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	run("init", "-b", "main")
	run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "--allow-empty", "-m", "base")
	run("checkout", "-b", "work")
	run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "--allow-empty", "-m", "work")

	var gotPurpose, gotFile bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/files" || r.Method != http.MethodPost {
			w.WriteHeader(404)
			return
		}
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "multipart/form-data") {
			w.WriteHeader(400)
			return
		}
		if err := r.ParseMultipartForm(4 << 20); err != nil {
			w.WriteHeader(400)
			return
		}
		f, hdr, err := r.FormFile("file")
		if err == nil {
			b, _ := io.ReadAll(f)
			f.Close()
			gotFile = hdr.Filename != "" && len(b) > 0
		}
		gotPurpose = r.FormValue("purpose") == "user_data"
		w.Write([]byte(`{"id":"file_bughunter_1"}`))
	}))
	defer srv.Close()
	// merge-base of main..work == first commit; bundle the work commit.
	mb, _ := exec.Command("git", "-C", dir, "merge-base", "main", "HEAD").Output()
	fileID, warn, err := testClient(srv).UploadGitBundle(context.Background(), dir, strings.TrimSpace(string(mb)))
	if err != nil {
		t.Fatal(err)
	}
	if fileID != "file_bughunter_1" {
		t.Fatalf("fileID = %q", fileID)
	}
	if !gotPurpose || !gotFile {
		t.Error("multipart must carry file + purpose=user_data")
	}
	_ = warn // dirty-tree warning is advisory only
}
