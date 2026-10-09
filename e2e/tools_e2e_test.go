//go:build e2e

// tools_e2e_test.go — builtin tool + subsystem coverage, all via forced
// tool_use mock directives in print mode:
//   - file tools: Write→Read→Edit in one turn (calls:[...])
//   - image Read → image content block in the transcript
//   - LSP on an unknown file type → graceful error result
//   - CronCreate + CronList
//   - Monitor → background task launch
//   - Workflow → wf_* run id (needs bun; t.Skip otherwise)
//   - PreToolUse hook deny (exit 2) → blocked tool_result
//   - CLAUDE_GO_SANDBOX=1 seatbelt: ls / allowed, ls /etc + master.passwd denied
//   - ANTHROPIC_API_KEY → x-api-key header against a captured endpoint
//   - plugin marketplace add + install + Skill tool resolves installed skill
package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestFileToolsSequence: Write then Read then Edit in a single turn via the
// calls:[] directive (Edit requires a prior Read in the same process).
func TestFileToolsSequence(t *testing.T) {
	s := newSession(t)
	target := filepath.Join(s.cwd, "seq.txt")
	calls := fmt.Sprintf(`calls:[`+
		`{"name":"Write","input":{"file_path":%q,"content":"alpha beta\n"}},`+
		`{"name":"Read","input":{"file_path":%q}},`+
		`{"name":"Edit","input":{"file_path":%q,"old_string":"beta","new_string":"gamma"}}`+
		`]`, target, target, target)
	r := s.run(t, "--mock", "-p", calls,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	events, _ := parseStreamJSON(t, r.stdout)
	trs := toolResultTexts(events)
	if len(trs) != 3 {
		t.Fatalf("expected 3 tool results, got %d:\n%s", len(trs), r.stdout)
	}
	contains(t, trs[1], "alpha beta", "Read result")
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read edited file: %v", err)
	}
	contains(t, string(b), "alpha gamma", "file after Edit")
}

// TestImageRead: Read on a .png produces an image content block; verify via
// the transcript's tool_result payload (mock just echoes text).
func TestImageRead(t *testing.T) {
	s := newSession(t)
	// minimal valid 1x1 PNG
	png := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
		0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00,
		0x0a, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49,
		0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
	}
	imgPath := filepath.Join(s.cwd, "img.png")
	if err := os.WriteFile(imgPath, png, 0o644); err != nil {
		t.Fatal(err)
	}
	sid := newUUID()
	r := s.run(t, "--mock", "-p",
		`tool:Read {"file_path":"`+imgPath+`"}`,
		"--permission-mode", "bypassPermissions",
		"--session-id", sid)
	s.mustOK(t, r)
	raw := mustReadFile(t, s.transcriptPath(t, sid))
	contains(t, raw, `"type":"image"`, "transcript image block")
	contains(t, raw, `"media_type":"image/png"`, "transcript media type")
}

// TestLSPGraceful: LSP on an extension with no server → error result, not a crash.
func TestLSPGraceful(t *testing.T) {
	s := newSession(t)
	s.env = append(s.env, "ENABLE_TOOL_SEARCH=1")
	r := s.run(t, "--mock", "-p",
		`calls:[{"name":"ToolSearch","input":{"query":"select:LSP"}},`+
			`{"name":"LSP","input":{"operation":"diagnostics","filePath":"x.unknownext"}}]`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	events, _ := parseStreamJSON(t, r.stdout)
	trs := toolResultTexts(events)
	if len(trs) == 0 {
		t.Fatalf("no tool_result:\n%s", r.stdout)
	}
	contains(t, trs[len(trs)-1], "No LSP server available", "LSP result")
}

// TestCron: CronCreate registers a session job; CronList shows it.
func TestCron(t *testing.T) {
	s := newSession(t)
	s.env = append(s.env, "ENABLE_TOOL_SEARCH=1")
	r := s.run(t, "--mock", "-p",
		`calls:[{"name":"ToolSearch","input":{"query":"select:CronCreate,CronList"}},`+
			`{"name":"CronCreate","input":{"cron":"*/5 * * * *","prompt":"tick-e2e"}},`+
			`{"name":"CronList","input":{}}]`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	events, _ := parseStreamJSON(t, r.stdout)
	trs := toolResultTexts(events)
	if len(trs) != 3 {
		t.Fatalf("expected 3 tool results, got %d:\n%s", len(trs), r.stdout)
	}
	contains(t, trs[1], "Scheduled recurring job", "CronCreate result")
	contains(t, trs[2], "recurring", "CronList result")
	contains(t, trs[2], "tick-e2e", "CronList prompt")
}

// TestMonitor: Monitor launches a background task.
func TestMonitor(t *testing.T) {
	s := newSession(t)
	s.env = append(s.env, "ENABLE_TOOL_SEARCH=1")
	r := s.run(t, "--mock", "-p",
		`calls:[{"name":"ToolSearch","input":{"query":"select:Monitor"}},`+
			`{"name":"Monitor","input":{"command":"echo hi","description":"e2e monitor"}}]`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	events, _ := parseStreamJSON(t, r.stdout)
	trs := toolResultTexts(events)
	if len(trs) == 0 {
		t.Fatalf("no tool_result:\n%s", r.stdout)
	}
	contains(t, trs[len(trs)-1], "armed", "Monitor result")
}

// TestWorkflow: named workflow resolves from .claude/workflows/ and returns
// a wf_* run id. Requires bun (agent() uses Bun.spawnSync).
func TestWorkflow(t *testing.T) {
	if !hasBinary("bun") {
		t.Skipf("bun not on PATH — Workflow runtime requires it")
	}
	s := newSession(t)
	s.writeFile(".claude/workflows/wf.js",
		"export const meta = {name:\"wf\", description:\"e2e\", phases:[]};\n"+
			"console.log(\"wf-body-marker\");\n")
	r := s.run(t, "--mock", "-p",
		`tool:Workflow {"name":"wf"}`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	events, _ := parseStreamJSON(t, r.stdout)
	trs := toolResultTexts(events)
	if len(trs) == 0 {
		t.Fatalf("no tool_result:\n%s", r.stdout)
	}
	contains(t, trs[0], "run wf_", "Workflow result")
}

// TestHookDeny: a PreToolUse command hook exiting 2 blocks the tool call;
// the block reason lands in the tool_result.
func TestHookDeny(t *testing.T) {
	s := newSession(t)
	s.writeSettings(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[` +
		`{"type":"command","command":"echo hook-blocked-reason >&2; exit 2"}]}]}}`)
	r := s.run(t, "--mock", "-p",
		`tool:Bash {"command":"echo should-not-run"}`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	events, _ := parseStreamJSON(t, r.stdout)
	trs := toolResultTexts(events)
	if len(trs) == 0 {
		t.Fatalf("no tool_result:\n%s", r.stdout)
	}
	contains(t, trs[0], "blocked by hook", "hook-denied result")
	contains(t, trs[0], "hook-blocked-reason", "hook reason text")
	notContains(t, trs[0], "should-not-run", "command must not have run")
}

// TestHookDenyJSON: a PreToolUse hook emitting {"decision":"block"} on
// stdout also blocks.
func TestHookDenyJSON(t *testing.T) {
	s := newSession(t)
	s.writeSettings(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[` +
		`{"type":"command","command":"echo '{\"decision\":\"block\",\"reason\":\"json-deny-reason\"}'"}]}]}}`)
	r := s.run(t, "--mock", "-p",
		`tool:Bash {"command":"echo should-not-run"}`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	events, _ := parseStreamJSON(t, r.stdout)
	trs := toolResultTexts(events)
	if len(trs) == 0 {
		t.Fatalf("no tool_result:\n%s", r.stdout)
	}
	contains(t, trs[0], "json-deny-reason", "json deny reason")
}

// TestSandbox: CLAUDE_GO_SANDBOX=1 runs Bash under seatbelt — `ls /` is
// allowed, `ls /etc` and `cat /etc/master.passwd` are denied (darwin only).
func TestSandbox(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("seatbelt sandbox is darwin-only")
	}
	if !hasBinary("sandbox-exec") {
		t.Skipf("sandbox-exec not found")
	}
	s := newSession(t)
	sandboxEnv := []string{"CLAUDE_GO_SANDBOX=1"}
	// sandboxed Bash auto-allows per sandbox.autoAllowBashIfSandboxed
	r := s.runEx(t, sandboxEnv, 60*time.Second, "--mock", "-p",
		`tool:Bash {"command":"ls /"}`,
		"--output-format", "stream-json")
	s.mustOK(t, r)
	events, _ := parseStreamJSON(t, r.stdout)
	trs := toolResultTexts(events)
	if len(trs) == 0 {
		t.Fatalf("no tool_result for ls /:\n%s", r.stdout)
	}
	contains(t, trs[0], "usr", "sandboxed ls / output")

	r2 := s.runEx(t, sandboxEnv, 60*time.Second, "--mock", "-p",
		`tool:Bash {"command":"ls /etc"}`,
		"--output-format", "stream-json")
	s.mustOK(t, r2)
	ev2, _ := parseStreamJSON(t, r2.stdout)
	trs2 := toolResultTexts(ev2)
	if len(trs2) == 0 {
		t.Fatalf("no tool_result for ls /etc:\n%s", r2.stdout)
	}
	// seatbelt denies file-read* on /etc (world-readable outside the sandbox,
	// so a failure here can only come from the seatbelt profile)
	lower := strings.ToLower(trs2[0])
	if !strings.Contains(lower, "not permitted") && !strings.Contains(lower, "denied") &&
		!strings.Contains(lower, "exit code") {
		t.Fatalf("expected ls /etc to be denied by sandbox, got: %s", trs2[0])
	}

	r3 := s.runEx(t, sandboxEnv, 60*time.Second, "--mock", "-p",
		`tool:Bash {"command":"cat /etc/master.passwd"}`,
		"--output-format", "stream-json")
	s.mustOK(t, r3)
	ev3, _ := parseStreamJSON(t, r3.stdout)
	trs3 := toolResultTexts(ev3)
	if len(trs3) == 0 {
		t.Fatalf("no tool_result for master.passwd:\n%s", r3.stdout)
	}
	lower3 := strings.ToLower(trs3[0])
	if !strings.Contains(lower3, "not permitted") && !strings.Contains(lower3, "denied") {
		t.Fatalf("expected master.passwd read denied, got: %s", trs3[0])
	}
}

// TestAuthAPIKeyHeader: ANTHROPIC_API_KEY auth sends x-api-key on requests.
// Uses a test-owned HTTP endpoint (the internal mock exposes LastBody but
// not headers, so this captures the request directly).
func TestAuthAPIKeyHeader(t *testing.T) {
	s := newSession(t)
	var mu sync.Mutex
	var apiKey, authz string
	gotStream := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var parsed struct {
			Stream bool `json:"stream"`
		}
		json.Unmarshal(body, &parsed)
		mu.Lock()
		if parsed.Stream {
			apiKey = req.Header.Get("x-api-key")
			authz = req.Header.Get("Authorization")
			select {
			case gotStream <- struct{}{}:
			default:
			}
		}
		mu.Unlock()
		if !parsed.Stream {
			// non-stream request (quota probe): plain JSON message
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"m","type":"message","role":"assistant","model":"x",`+
				`"content":[{"type":"text","text":"q"}],"stop_reason":"end_turn",`+
				`"usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"x\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"+
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"auth-flow-ok\"}}\n\n"+
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"+
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	r := s.runEx(t, []string{
		"ANTHROPIC_API_KEY=sk-ant-e2e-fake",
		"ANTHROPIC_BASE_URL=" + srv.URL,
	}, 60*time.Second, "-p", "auth check")
	s.mustOK(t, r)
	select {
	case <-gotStream:
	case <-time.After(5 * time.Second):
		t.Fatal("no streaming request reached test endpoint")
	}
	mu.Lock()
	defer mu.Unlock()
	if apiKey != "sk-ant-e2e-fake" {
		t.Fatalf("x-api-key = %q, want sk-ant-e2e-fake", apiKey)
	}
	if authz != "" {
		t.Fatalf("unexpected Authorization header: %q", authz)
	}
	contains(t, r.stdout, "auth-flow-ok", "endpoint response text")
}

// TestPluginInstallAndSkill: marketplace add → plugin install → the
// installed plugin's skill is resolvable by the Skill tool.
func TestPluginInstallAndSkill(t *testing.T) {
	s := newSession(t)
	// fake local marketplace
	mkt := t.TempDir()
	mkFile := func(rel, content string) {
		p := filepath.Join(mkt, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkFile(".claude-plugin/marketplace.json", `{"name":"e2emkt","plugins":[`+
		`{"name":"demoplug","description":"e2e plugin","source":"./demoplug","version":"1.0.0"}]}`)
	mkFile("demoplug/skills/tskill/SKILL.md",
		"---\ndescription: e2e skill\n---\nSKILL-BODY-MARKER args=$ARGUMENTS\n")

	r := s.run(t, "plugin", "marketplace", "add", mkt)
	s.mustOK(t, r)
	contains(t, r.stdout, "added marketplace", "marketplace add")

	r = s.run(t, "plugin", "install", "demoplug")
	s.mustOK(t, r)
	contains(t, r.stdout, "installed demoplug", "plugin install")

	// installed tree materialized under test HOME
	glob := filepath.Join(s.home, ".claude", "plugins", "synced", "*", "demoplug",
		"skills", "tskill", "SKILL.md")
	if m, _ := filepath.Glob(glob); len(m) == 0 {
		t.Fatalf("installed skill missing, glob %s found nothing", glob)
	}

	// Skill tool resolves it end-to-end
	r = s.run(t, "--mock", "-p",
		`tool:Skill {"skill":"tskill","args":"go"}`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	events, _ := parseStreamJSON(t, r.stdout)
	trs := toolResultTexts(events)
	if len(trs) == 0 {
		t.Fatalf("no tool_result:\n%s", r.stdout)
	}
	contains(t, trs[0], "SKILL-BODY-MARKER", "skill body in result")

	// plugin list shows it enabled
	r = s.run(t, "plugin", "list")
	s.mustOK(t, r)
	contains(t, r.stdout, "demoplug", "plugin list")
}

// TestMarketplaceAddNPM: `plugin marketplace add npm:<pkg>` fetches
// registry metadata → dist.tarball → extracts the package/ contents as the
// marketplace dir (real's npm source). NPM_CONFIG_REGISTRY redirects to a
// local httptest registry.
func TestMarketplaceAddNPM(t *testing.T) {
	s := newSession(t)
	// fake registry: metadata + a tarball carrying a marketplace.json
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	files := map[string]string{
		"package/.claude-plugin/marketplace.json": `{"name":"npm-mkt","plugins":[]}`,
		"package/README.md":                       "npm pkg",
	}
	for name, content := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))})
		tw.Write([]byte(content))
	}
	tw.Close()
	gw.Close()
	tarb := buf.Bytes()

	var reg *httptest.Server
	reg = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".tgz") {
			w.Write(tarb)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"dist-tags":{"latest":"1.2.3"},"versions":{"1.2.3":{"dist":{"tarball":%q}}}}`,
			reg.URL+"/npm-mkt-1.2.3.tgz")
	}))
	defer reg.Close()

	s.env = append(s.env, "NPM_CONFIG_REGISTRY="+reg.URL)
	r := s.run(t, "plugin", "marketplace", "add", "npm:npm-mkt")
	s.mustOK(t, r)
	contains(t, r.stdout, "added marketplace", "npm marketplace add")
	// extracted into the marketplaces dir
	p := filepath.Join(s.home, ".claude", "plugins", "marketplaces",
		"npm-mkt", ".claude-plugin", "marketplace.json")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("npm marketplace not extracted: %v", err)
	}
	r = s.run(t, "plugin", "marketplace", "list")
	contains(t, r.stdout, "npm-mkt", "npm marketplace listed")
}

// TestInstalledPluginsV1Migration: a V1 installed_plugins.json converts to
// V2 on load and writes the migrated shape back (real's _Dr migration).
func TestInstalledPluginsV1Migration(t *testing.T) {
	s := newSession(t)
	pdir := filepath.Join(s.home, ".claude", "plugins")
	os.MkdirAll(pdir, 0o755)
	os.WriteFile(filepath.Join(pdir, "installed_plugins.json"),
		[]byte(`{"version":1,"plugins":{"old@mkt":{"version":"1.0","installedAt":"2026-01-01"}}}`), 0o644)

	r := s.run(t, "plugin", "list")
	s.mustOK(t, r)
	contains(t, r.stdout, "old@mkt", "V1 record listed after migration")
	b, _ := os.ReadFile(filepath.Join(pdir, "installed_plugins.json"))
	var wrap struct {
		Version int `json:"version"`
	}
	json.Unmarshal(b, &wrap)
	if wrap.Version != 2 {
		t.Fatalf("file not migrated to V2: %s", truncStr(string(b), 200))
	}
}

// TestInstalledPluginsUnreadableGuard: a V2 file with a valid plugin id
// whose record this build can't parse (keptRaw — e.g. a future build's
// format) counts as unreadable: no write replaces it (real's b2n/aDr).
func TestInstalledPluginsUnreadableGuard(t *testing.T) {
	s := newSession(t)
	pdir := filepath.Join(s.home, ".claude", "plugins")
	os.MkdirAll(pdir, 0o755)
	bad := `{"version":2,"plugins":{"good@mkt":"not-an-array"}}`
	os.WriteFile(filepath.Join(pdir, "installed_plugins.json"), []byte(bad), 0o644)
	mkt := t.TempDir()
	os.MkdirAll(filepath.Join(mkt, ".claude-plugin"), 0o755)
	os.WriteFile(filepath.Join(mkt, ".claude-plugin", "marketplace.json"),
		[]byte(`{"name":"m","plugins":[{"name":"x","description":"d","source":"./x"}]}`), 0o644)
	os.MkdirAll(filepath.Join(mkt, "x"), 0o755)
	s.run(t, "plugin", "marketplace", "add", mkt)
	r := s.run(t, "plugin", "install", "x@"+filepath.Base(mkt))
	s.mustOK(t, r)
	got, _ := os.ReadFile(filepath.Join(pdir, "installed_plugins.json"))
	if string(got) != bad {
		t.Fatalf("unreadable file was overwritten: %s", truncStr(string(got), 200))
	}
}
