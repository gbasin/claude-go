package hooks

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func cfg(ev Event, matcher string, hooks ...Command) Config {
	return Config{string(ev): []Matcher{{Matcher: matcher, Hooks: hooks}}}
}

func payload(kv map[string]any) map[string]any {
	p := map[string]any{"session_id": "s1", "cwd": "/tmp"}
	for k, v := range kv {
		p[k] = v
	}
	return p
}

func TestRunExit2Blocks(t *testing.T) {
	c := cfg(PreToolUse, "Bash", Command{Type: "command", Command: "echo nope >&2; exit 2"})
	r := Run(c, PreToolUse, payload(map[string]any{"tool_name": "Bash"}), "/tmp")
	if !r.Block || !strings.Contains(r.Reason, "nope") {
		t.Fatalf("exit2 should block with stderr reason, got %+v", r)
	}
}

func TestPromptHookGating(t *testing.T) {
	called := 0
	old := Judge
	Judge = func(prompt, ctx string) (bool, string) { called++; return true, "" }
	defer func() { Judge = old }()

	prompt := Command{Type: "prompt", Prompt: "is it ok?"}
	// prompt hooks run on the three tool-decision events
	for _, ev := range []Event{PreToolUse, PostToolUse, PermissionRequest} {
		Run(cfg(ev, "", prompt), ev, payload(map[string]any{"tool_name": "Bash"}), "/tmp")
	}
	if called != 3 {
		t.Fatalf("prompt hook should run on tool events, ran %d times", called)
	}
	// ...and are rejected elsewhere with real's message
	r := Run(cfg(UserPromptSubmit, "", prompt), UserPromptSubmit, payload(nil), "/tmp")
	if called != 3 || !strings.Contains(r.NonBlockErr, "not supported for UserPromptSubmit") {
		t.Fatalf("prompt hook on UserPromptSubmit should be rejected, called=%d err=%q", called, r.NonBlockErr)
	}
	// agent hooks additionally can't serve PermissionRequest
	r = Run(cfg(PermissionRequest, "", Command{Type: "agent", Prompt: "check"}), PermissionRequest,
		payload(map[string]any{"tool_name": "Bash"}), "/tmp")
	if called != 3 || !strings.Contains(r.NonBlockErr, "not supported for PermissionRequest") {
		t.Fatalf("agent hook on PermissionRequest should be rejected, called=%d err=%q", called, r.NonBlockErr)
	}
}

func TestPromptHookVerdict(t *testing.T) {
	old := Judge
	Judge = func(prompt, ctx string) (bool, string) { return false, "denied by judge" }
	defer func() { Judge = old }()
	c := cfg(PreToolUse, "", Command{Type: "prompt", Prompt: "p"})
	if r := Run(c, PreToolUse, payload(map[string]any{"tool_name": "Bash"}), "/tmp"); !r.Block || r.Reason != "denied by judge" {
		t.Fatalf("negative verdict should block, got %+v", r)
	}
	// continueOnBlock demotes the verdict to a message
	c = cfg(PreToolUse, "", Command{Type: "prompt", Prompt: "p", ContinueOnBlock: true})
	if r := Run(c, PreToolUse, payload(map[string]any{"tool_name": "Bash"}), "/tmp"); r.Block {
		t.Fatal("continueOnBlock prompt verdict must not block")
	}
}

func TestIfCondition(t *testing.T) {
	mk := func(cond string) Command {
		return Command{Type: "command", If: cond, Command: "echo hit > " + filepath.Join(t.TempDir(), "x")}
	}
	mark := filepath.Join(t.TempDir(), "ran")
	cmd := Command{Type: "command", If: "Bash(git *)", Command: "echo x > " + mark}
	c := cfg(PreToolUse, "", cmd)
	// tool matches the if-rule
	Run(c, PreToolUse, payload(map[string]any{
		"tool_name": "Bash", "tool_input": json.RawMessage(`{"command":"git status"}`)}), "/tmp")
	if _, err := os.Stat(mark); err != nil {
		t.Fatal("hook with matching if should have run")
	}
	// tool doesn't match
	os.Remove(mark)
	Run(c, PreToolUse, payload(map[string]any{
		"tool_name": "Bash", "tool_input": json.RawMessage(`{"command":"rm x"}`)}), "/tmp")
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("hook with non-matching if must not run")
	}
	// if on a non-tool event skips with the real warning
	r := Run(cfg(UserPromptSubmit, "", Command{Type: "command", If: "Bash", Command: "true"}),
		UserPromptSubmit, payload(nil), "/tmp")
	if !strings.Contains(r.NonBlockErr, "cannot be evaluated for non-tool event") {
		t.Fatalf("expected non-tool if warning, got %q", r.NonBlockErr)
	}
	_ = mk
}

func TestOnceHook(t *testing.T) {
	ResetOnce()
	mark := filepath.Join(t.TempDir(), "n")
	c := cfg(SessionStart, "", Command{Type: "command", Once: true, Command: "echo x >> " + mark})
	Run(c, SessionStart, payload(map[string]any{"source": "startup"}), "/tmp")
	Run(c, SessionStart, payload(map[string]any{"source": "startup"}), "/tmp")
	b, _ := os.ReadFile(mark)
	if strings.Count(string(b), "x") != 1 {
		t.Fatalf("once hook ran %d times, want 1", strings.Count(string(b), "x"))
	}
}

func TestAsyncHook(t *testing.T) {
	mark := filepath.Join(t.TempDir(), "a")
	c := cfg(SessionStart, "", Command{Type: "command", Async: true,
		Command: "sleep 0.3; echo x > " + mark})
	start := time.Now()
	r := Run(c, SessionStart, payload(map[string]any{"source": "startup"}), "/tmp")
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("async hook blocked Run")
	}
	if len(r.Records) != 1 || r.Records[0].Outcome != "async" {
		t.Fatalf("expected async record, got %+v", r.Records)
	}
	time.Sleep(600 * time.Millisecond)
	if _, err := os.Stat(mark); err != nil {
		t.Fatal("async hook never ran")
	}
}

func TestArgsExecForm(t *testing.T) {
	mark := filepath.Join(t.TempDir(), "x")
	c := cfg(SessionStart, "", Command{Type: "command", Command: "/bin/sh",
		Args: []string{"-c", "echo hi > " + mark}})
	Run(c, SessionStart, payload(map[string]any{"source": "s"}), "/tmp")
	if b, _ := os.ReadFile(mark); strings.TrimSpace(string(b)) != "hi" {
		t.Fatal("args exec form did not run without shell")
	}
}

func TestHookEventNameValidation(t *testing.T) {
	out := `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"leak"}}`
	c := cfg(PreToolUse, "", Command{Type: "command", Command: "echo '" + out + "'"})
	r := Run(c, PreToolUse, payload(map[string]any{"tool_name": "Bash"}), "/tmp")
	if r.AdditionalContext != "" || !strings.Contains(r.NonBlockErr, "does not match") {
		t.Fatalf("mismatched hookEventName must discard output, got %+v", r)
	}
}

func TestPermissionRequestDecision(t *testing.T) {
	out := `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow","updatedPermissions":["Bash(ls:*)"],"updatedInput":{"command":"ls"}}}}`
	c := cfg(PermissionRequest, "", Command{Type: "command", Command: "echo '" + out + "'"})
	r := Run(c, PermissionRequest, payload(map[string]any{"tool_name": "Bash"}), "/tmp")
	if r.Permission != "allow" {
		t.Fatalf("nested decision.behavior=allow should allow, got %+v", r)
	}
	if string(r.UpdatedPermissions) == "" || string(r.UpdatedInput) == "" {
		t.Fatalf("decision updatedPermissions/updatedInput not captured: %+v", r)
	}
	// flat permissionDecision form still works
	out = `{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"flat no"}}`
	c = cfg(PermissionRequest, "", Command{Type: "command", Command: "echo '" + out + "'"})
	r = Run(c, PermissionRequest, payload(map[string]any{"tool_name": "Bash"}), "/tmp")
	if r.Permission != "deny" || r.Reason != "flat no" {
		t.Fatalf("flat permissionDecision=deny not applied: %+v", r)
	}
	out = `{"hookSpecificOutput":{"decision":{"behavior":"deny","message":"nope","interrupt":true}}}`
	c = cfg(PermissionRequest, "", Command{Type: "command", Command: "echo '" + out + "'"})
	r = Run(c, PermissionRequest, payload(map[string]any{"tool_name": "Bash"}), "/tmp")
	if r.Permission != "deny" || r.Reason != "nope" || !r.Interrupt {
		t.Fatalf("nested deny decision not applied: %+v", r)
	}
}

func TestHookSpecificFields(t *testing.T) {
	out := `{"hookSpecificOutput":{"sessionTitle":"T","watchPaths":["a","b"],"retry":true,"suppressOriginalPrompt":true,"reloadSkills":true}}`
	c := cfg(SessionStart, "", Command{Type: "command", Command: "echo '" + out + "'"})
	r := Run(c, SessionStart, payload(map[string]any{"source": "s"}), "/tmp")
	if r.SessionTitle != "T" || len(r.WatchPaths) != 2 || !r.Retry || !r.SuppressOriginalPrompt || !r.ReloadSkills {
		t.Fatalf("hookSpecificOutput fields not folded: %+v", r)
	}
}

func TestMatcherCharsets(t *testing.T) {
	// wide-charset events: space/comma/dash matcher is an exact list
	c := cfg(SessionStart, "re sume, startup", Command{Type: "command", Command: "echo ran"})
	r := Run(c, SessionStart, payload(map[string]any{"source": "startup"}), "/tmp")
	if len(r.Records) != 1 || r.Records[0].Outcome != "success" {
		t.Fatal("comma-separated wide matcher should match")
	}
	// strict-charset event (FileChanged not in z3): a space/comma matcher
	// is treated as a regex, not a list
	c = cfg(FileChanged, "a b", Command{Type: "command", Command: "echo ran"})
	r = Run(c, FileChanged, payload(map[string]any{"file_path": "/x/a b"}), "/tmp")
	if len(r.Records) == 0 {
		t.Fatal("regex-able strict matcher should still evaluate")
	}
	c = cfg(FileChanged, "x|y.go", Command{Type: "command", Command: "echo ran"})
	r = Run(c, FileChanged, payload(map[string]any{"file_path": "/d/e/y.go"}), "/tmp")
	if len(r.Records) == 0 {
		t.Fatal("FileChanged matcher applies to the basename")
	}
	c = cfg(FileChanged, "e", Command{Type: "command", Command: "echo ran"})
	r = Run(c, FileChanged, payload(map[string]any{"file_path": "/d/e/y.go"}), "/tmp")
	if len(r.Records) != 0 {
		t.Fatal("FileChanged matcher must not match directory components")
	}
}

func TestModelSwitchMatcherNormalization(t *testing.T) {
	c := cfg(PreModelSwitch, "opus[1m]", Command{Type: "command", Command: "echo ran"})
	r := Run(c, PreModelSwitch, payload(map[string]any{"to_model": "opus"}), "/tmp")
	if len(r.Records) == 0 {
		t.Fatal("[1m] marker in matcher should be stripped")
	}
	// unresolvable to_model → matcher doesn't gate
	c = cfg(PreModelSwitch, "opus", Command{Type: "command", Command: "echo ran"})
	r = Run(c, PreModelSwitch, payload(map[string]any{"to_model": "[1m]"}), "/tmp")
	if len(r.Records) == 0 {
		t.Fatal("empty normalized to_model should run anyway")
	}
}

func TestBareMCPWarn(t *testing.T) {
	bareMCPWarned = sync.Map{}
	c := cfg(PreToolUse, "mcp__github", Command{Type: "command", Command: "echo ran"})
	r := Run(c, PreToolUse, payload(map[string]any{"tool_name": "mcp__github__x"}), "/tmp")
	if !strings.Contains(r.NonBlockErr, "bare mcp__ server name") {
		t.Fatalf("expected bare mcp__ matcher warning, got %q", r.NonBlockErr)
	}
}

func TestHTTPEnvWhitelist(t *testing.T) {
	t.Setenv("SECRET_TOK", "s3cret")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-Auth")
		w.Write([]byte(`{"systemMessage":"ok"}`))
	}))
	defer srv.Close()
	c := cfg(SessionStart, "", Command{Type: "http", URL: srv.URL,
		Headers: map[string]string{"X-Auth": "Bearer $SECRET_TOK"}})
	Run(c, SessionStart, payload(map[string]any{"source": "s"}), "/tmp")
	if strings.Contains(gotAuth, "s3cret") {
		t.Fatalf("non-whitelisted env var must not interpolate, got %q", gotAuth)
	}
	gotAuth = ""
	c = cfg(SessionStart, "", Command{Type: "http", URL: srv.URL,
		Headers:        map[string]string{"X-Auth": "Bearer ${SECRET_TOK}"},
		AllowedEnvVars: []string{"SECRET_TOK"}})
	Run(c, SessionStart, payload(map[string]any{"source": "s"}), "/tmp")
	if gotAuth != "Bearer s3cret" {
		t.Fatalf("whitelisted env var should interpolate, got %q", gotAuth)
	}
}

func TestEnvFileSessionEnv(t *testing.T) {
	// a SessionStart hook may append KEY=value to CLAUDE_ENV_FILE; the var
	// then reaches later hook spawns (real's session-env overlay)
	mark := filepath.Join(t.TempDir(), "env")
	c := Config{
		string(SessionStart): []Matcher{{Hooks: []Command{
			{Type: "command", Command: `echo "export CG_MARK=yes" >> "$CLAUDE_ENV_FILE"`},
		}}},
		string(SessionEnd): []Matcher{{Hooks: []Command{
			{Type: "command", Command: "echo $CG_MARK > " + mark},
		}}},
	}
	r := Run(c, SessionStart, payload(map[string]any{"source": "startup"}), "/tmp")
	if len(r.Records) != 1 {
		t.Fatal("SessionStart hook didn't run")
	}
	Run(c, SessionEnd, payload(map[string]any{"reason": "other"}), "/tmp")
	if b, _ := os.ReadFile(mark); strings.TrimSpace(string(b)) != "yes" {
		t.Fatalf("session env not propagated to later hooks, got %q", b)
	}
	if SessionEnv()["CG_MARK"] != "yes" {
		t.Fatal("SessionEnv() missing emitted var")
	}
}

func TestUnknownKeyWarn(t *testing.T) {
	c := cfg(SessionStart, "", Command{Type: "command",
		Command: `echo '{"systemMessage":"m","typoKey":1}'`})
	r := Run(c, SessionStart, payload(map[string]any{"source": "s"}), "/tmp")
	if !strings.Contains(r.NonBlockErr, "typoKey") {
		t.Fatalf("unknown key should warn, got %q", r.NonBlockErr)
	}
}

func TestRecordsEmitted(t *testing.T) {
	c := cfg(PreToolUse, "", Command{Type: "command", Command: "echo hi; exit 2"})
	r := Run(c, PreToolUse, payload(map[string]any{"tool_name": "Bash"}), "/tmp")
	if len(r.Records) != 1 {
		t.Fatalf("want 1 record, got %+v", r.Records)
	}
	rec := r.Records[0]
	if rec.ExitCode != 2 || rec.Outcome != "blocking" || !strings.Contains(rec.Stdout, "hi") || rec.HookID == "" {
		t.Fatalf("bad record %+v", rec)
	}
}

func TestManagedHooks(t *testing.T) {
	dir := t.TempDir()
	managed := filepath.Join(dir, "managed.json")
	os.WriteFile(managed, []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo m"}]}]}}`), 0o644)
	t.Setenv("CLAUDE_GO_MANAGED_SETTINGS", managed)
	os.MkdirAll(filepath.Join(dir, "cc"), 0o755)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "cc"))
	c := Load("/tmp")
	if len(c[string(SessionStart)]) != 1 {
		t.Fatal("managed hooks not loaded")
	}
	// disableAllHooks in managed → only managed hooks run
	os.WriteFile(managed, []byte(`{"disableAllHooks":true,"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo m"}]}]}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "cc", "settings.json"),
		[]byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo u"}]}]}}`), 0o644)
	c = Load("/tmp")
	if len(c[string(SessionStart)]) != 1 || c[string(SessionStart)][0].Hooks[0].Command != "echo m" {
		t.Fatal("disableAllHooks should drop non-managed hooks")
	}
	// user-tier disableAllHooks also applies (real xxo)
	os.WriteFile(managed, []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(dir, "cc", "settings.json"),
		[]byte(`{"disableAllHooks":true,"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo u"}]}]}}`), 0o644)
	c = Load("/tmp")
	if len(c[string(SessionStart)]) != 0 {
		t.Fatal("user disableAllHooks should empty the config")
	}
}

func TestManagedHooksOnly(t *testing.T) {
	dir := t.TempDir()
	managed := filepath.Join(dir, "managed.json")
	os.WriteFile(managed, []byte(`{"allowManagedHooksOnly":true,"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo m"}]}]}}`), 0o644)
	t.Setenv("CLAUDE_GO_MANAGED_SETTINGS", managed)
	os.MkdirAll(filepath.Join(dir, "cc"), 0o755)
	os.WriteFile(filepath.Join(dir, "cc", "settings.json"),
		[]byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo u"}]}]}}`), 0o644)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "cc"))
	c := Load("/tmp")
	if len(c[string(SessionStart)]) != 1 || c[string(SessionStart)][0].Hooks[0].Command != "echo m" {
		t.Fatal("allowManagedHooksOnly should keep only managed hooks")
	}
}
