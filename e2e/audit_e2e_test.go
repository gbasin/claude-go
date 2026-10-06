//go:build e2e

// Coverage for second-audit fixes: hooks output semantics, settings env,
// CLAUDE_CONFIG_DIR, additionalDirectories, transcript fidelity.
package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHookContinueFalse: {"continue":false} (bool) must block like exit 2 —
// regression test for the string-vs-bool parse bug.
func TestHookContinueFalse(t *testing.T) {
	s := newSession(t)
	s.writeFile(".claude/settings.json", `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"echo '{\"continue\":false,\"stopReason\":\"hooked off\"}'"}]}]}}`)
	r := s.run(t, "--mock", "-p", "hello")
	if r.code == 0 && strings.Contains(r.stdout, "Mock response") {
		t.Fatalf("continue:false hook should block the turn; got %q", r.stdout)
	}
}

// TestHookPlainTextContext: non-JSON stdout becomes additional context the
// model sees (UserPromptSubmit) — the mock echoes the prompt back.
func TestHookPlainTextContext(t *testing.T) {
	s := newSession(t)
	s.writeFile(".claude/settings.json", `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"echo 'HOOKCTX-9'"}]}]}}`)
	r := s.run(t, "--mock", "-p", "hi")
	s.mustOK(t, r)
	// the mock echoes the augmented prompt; hook text should appear in it
	if !strings.Contains(r.stdout, "HOOKCTX-9") {
		t.Fatalf("hook stdout should be injected as context: %q", r.stdout)
	}
}

// TestHookExit2FeedsModel: PreToolUse exit 2 blocks the tool and the reason
// reaches the model as a tool_result.
func TestHookExit2FeedsModel(t *testing.T) {
	s := newSession(t)
	s.writeFile(".claude/settings.json", `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo 'no bash today' >&2; exit 2"}]}]}}`)
	r := s.run(t, "--mock", "-p", `tool:Bash {"command":"echo hi"}`)
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "no bash today") {
		t.Fatalf("exit-2 stderr should reach the model: %q", r.stdout)
	}
}

// TestHookPermissionAllow: PreToolUse permissionDecision:"allow" skips the
// ask — tool runs in default mode without a rule.
func TestHookPermissionAllow(t *testing.T) {
	s := newSession(t)
	s.writeFile(".claude/settings.json", `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo '{\"hookSpecificOutput\":{\"permissionDecision\":\"allow\"}}'"}]}]}}`)
	r := s.run(t, "--mock", "-p", `tool:Bash {"command":"echo HOOKOK"}`)
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "HOOKOK") {
		t.Fatalf("permissionDecision allow should run the tool: %q", r.stdout)
	}
}

// TestHookUpdatedInput: PreToolUse can rewrite tool_input.
func TestHookUpdatedInput(t *testing.T) {
	s := newSession(t)
	s.writeFile(".claude/settings.json", `{"permissions":{"allow":["Bash(echo *)"]},"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo '{\"hookSpecificOutput\":{\"updatedInput\":{\"command\":\"echo REWRITTEN\"}}}'"}]}]}}`)
	r := s.run(t, "--mock", "-p", `tool:Bash {"command":"echo ORIGINAL"}`)
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "REWRITTEN") || strings.Contains(r.stdout, "ORIGINAL") {
		t.Fatalf("updatedInput should rewrite the command: %q", r.stdout)
	}
}

// TestHookMatcherCommaList: "Read,Write"-style comma lists match either tool.
func TestHookMatcherCommaList(t *testing.T) {
	s := newSession(t)
	s.writeFile(".claude/settings.json", `{"hooks":{"PreToolUse":[{"matcher":"Bash,Write","hooks":[{"type":"command","command":"echo '{\"decision\":\"block\",\"reason\":\"comma-blocked\"}'"}]}]}}`)
	r := s.run(t, "--mock", "-p", `tool:Bash {"command":"echo hi"}`)
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "comma-blocked") {
		t.Fatalf("comma-list matcher should hit Bash: %q", r.stdout)
	}
}

// TestSettingsEnv: the settings env block is injected into the process
// (Bash children see it).
func TestSettingsEnv(t *testing.T) {
	s := newSession(t)
	s.writeFile(".claude/settings.json", `{"env":{"E2E_MARKER":"env-injected-7"},"permissions":{"allow":["Bash(echo *)"]}}`)
	r := s.run(t, "--mock", "-p", `tool:Bash {"command":"echo $E2E_MARKER"}`)
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "env-injected-7") {
		t.Fatalf("settings env should reach tool children: %q", r.stdout)
	}
}

// TestConfigDirOverride: CLAUDE_CONFIG_DIR moves the config home.
func TestConfigDirOverride(t *testing.T) {
	s := newSession(t)
	conf := filepath.Join(s.home, "confx")
	os.MkdirAll(conf, 0o755)
	os.WriteFile(filepath.Join(conf, "settings.json"),
		[]byte(`{"permissions":{"allow":["Bash(echo *)"]}}`), 0o644)
	r := s.runEx(t, []string{"CLAUDE_CONFIG_DIR=" + conf}, 60e9, "--mock", "-p", `tool:Bash {"command":"echo CFGOK"}`)
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "CFGOK") {
		t.Fatalf("CLAUDE_CONFIG_DIR settings not loaded: %q", r.stdout)
	}
	// transcripts land under the overridden dir
	if _, err := os.Stat(filepath.Join(conf, "projects")); err != nil {
		t.Fatalf("transcripts should live under CLAUDE_CONFIG_DIR: %v", err)
	}
}

// TestAdditionalDirectories: Edit outside cwd is allowed when the dir is in
// permissions.additionalDirectories.
func TestAdditionalDirectories(t *testing.T) {
	s := newSession(t)
	outside := filepath.Join(s.home, "outside")
	os.MkdirAll(outside, 0o755)
	target := filepath.Join(outside, "f.txt")
	os.WriteFile(target, []byte("old"), 0o644)
	s.writeFile(".claude/settings.json",
		`{"permissions":{"additionalDirectories":[`+strconv(outside)+`],"allow":["Read","Edit"]}}`)
	r := s.run(t, "--mock", "-p",
		`calls:[{"name":"Read","input":{"file_path":"`+target+`"}},{"name":"Edit","input":{"file_path":"`+target+`","old_string":"old","new_string":"new"}}]`)
	s.mustOK(t, r)
	b, _ := os.ReadFile(target)
	if string(b) != "new" {
		t.Fatalf("additionalDirectories should permit the edit, file=%q out=%q", b, r.stdout)
	}
}

func strconv(s string) string { b, _ := json.Marshal(s); return string(b) }

// TestTranscriptAssistantShape: assistant records persist the full API
// message (model + stop_reason) like real CC transcripts.
func TestTranscriptAssistantShape(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p", "hello")
	s.mustOK(t, r)
	var found bool
	for _, p := range s.transcriptPaths(t) {
		b, _ := os.ReadFile(p)
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.Contains(line, `"type":"assistant"`) {
				continue
			}
			var rec map[string]any
			if json.Unmarshal([]byte(line), &rec) != nil {
				continue
			}
			msg, _ := rec["message"].(map[string]any)
			if msg["model"] != nil && msg["stop_reason"] != nil {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("no assistant record with model+stop_reason found")
	}
}

// TestPerProjectAllowedTools: ~/.claude.json projects[dir].allowedTools rules
// are honored as allow rules.
func TestPerProjectAllowedTools(t *testing.T) {
	s := newSession(t)
	p := filepath.Join(s.home, ".claude.json")
	b, _ := os.ReadFile(p)
	var cfg map[string]any
	json.Unmarshal(b, &cfg)
	cfg["projects"].(map[string]any)[s.cwd].(map[string]any)["allowedTools"] = []string{"Bash(echo *)"}
	os.WriteFile(p, mustJSON(cfg), 0o644)
	r := s.run(t, "--mock", "-p", `tool:Bash {"command":"echo PROJOK"}`)
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "PROJOK") {
		t.Fatalf("projects[].allowedTools rule should allow the tool: %q", r.stdout)
	}
}

// TestDontAskDenyText: dontAsk mode denials carry the real don't-ask message.
func TestDontAskDenyText(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p", "--permission-mode", "dontAsk",
		`tool:Bash {"command":"echo hi"}`)
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "don't ask mode") {
		t.Fatalf("expected dontAsk denial text, got %q", r.stdout)
	}
}

// TestSessionStartHookOnWire: SessionStart hook output must reach the wire
// the way real sends it — merged into the env composite on request 1, then
// a bare-string system record on request 2+ (verified in 2.1.289 captures).
func TestSessionStartHookOnWire(t *testing.T) {
	s := newSession(t)
	s.writeFile(".claude/settings.json",
		`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo 'SESSHOOK-42'"}]}]}}`)
	recDir := filepath.Join(t.TempDir(), "reqs")
	r := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + recDir}, 60*time.Second,
		"--mock", "-p", `tool:Bash {"command":"echo hi"}`,
		"--permission-mode", "bypassPermissions")
	s.mustOK(t, r)

	ents, err := os.ReadDir(recDir)
	if err != nil || len(ents) < 2 {
		t.Fatalf("expected >=2 recorded requests, got %v err=%v", len(ents), err)
	}
	readReq := func(i int) []map[string]any {
		b, _ := os.ReadFile(filepath.Join(recDir, ents[i].Name()))
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.Unmarshal(b, &req); err != nil {
			t.Fatalf("request %s did not parse: %v", ents[i].Name(), err)
		}
		return req.Messages
	}

	// req1: hook ctx merged into the env composite system block
	req1 := readReq(0)
	var envText string
	for _, m := range req1 {
		if m["role"] == "system" {
			if blocks, ok := m["content"].([]any); ok {
				for _, b := range blocks {
					if bm, ok := b.(map[string]any); ok {
						envText += fmt.Sprint(bm["text"])
					}
				}
			}
		}
	}
	if !strings.Contains(envText, "SESSHOOK-42") {
		t.Fatalf("req1 env composite missing hook ctx\nsystem text: %s", truncStr(envText, 1000))
	}

	// req2: env slot shrinks to a bare-string system record
	req2 := readReq(len(ents) - 1)
	if len(req2) < 2 {
		t.Fatalf("req2 too short: %d messages", len(req2))
	}
	raw, ok := req2[1]["content"].(string)
	if !ok || !strings.Contains(raw, "SESSHOOK-42") {
		t.Fatalf("req2 messages[1] should be bare-string hook ctx, got %v", req2[1]["content"])
	}
}

// TestPluginHooksMerge: an installed+enabled plugin's hooks/hooks.json runs
// as session hooks, with ${CLAUDE_PLUGIN_ROOT} expanded to the install path
// (real resolves it at spawn). The hook's stdout lands as prompt context —
// the mock echoes it back.
func TestPluginHooksMerge(t *testing.T) {
	s := newSession(t)
	installPath := filepath.Join(s.home, "plugcache", "myplug", "1.0")
	s.writeFile(filepath.Join(installPath, "hooks", "hooks.json"),
		`{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"cat ${CLAUDE_PLUGIN_ROOT}/marker.txt"}]}]}}`)
	s.writeFile(filepath.Join(installPath, "marker.txt"), "PLUGINHOOK-7")
	s.writeFile(filepath.Join(s.home, ".claude", "plugins", "installed_plugins.json"),
		fmt.Sprintf(`{"plugins":{"myplug@mkt":[{"installPath":%q,"enabled":true}]}}`, installPath))

	r := s.run(t, "--mock", "-p", "hi")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "PLUGINHOOK-7") {
		t.Fatalf("plugin hook output should reach the model as context: %q", r.stdout)
	}
}

// TestPluginHookDisabled: enabled:false plugins contribute no hooks.
func TestPluginHookDisabled(t *testing.T) {
	s := newSession(t)
	installPath := filepath.Join(s.home, "plugcache", "offplug", "1.0")
	s.writeFile(filepath.Join(installPath, "hooks", "hooks.json"),
		`{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"echo 'SHOULD-NOT-FIRE'"}]}]}}`)
	s.writeFile(filepath.Join(s.home, ".claude", "plugins", "installed_plugins.json"),
		fmt.Sprintf(`{"plugins":{"offplug@mkt":[{"installPath":%q,"enabled":false}]}}`, installPath))

	r := s.run(t, "--mock", "-p", "hi")
	s.mustOK(t, r)
	if strings.Contains(r.stdout, "SHOULD-NOT-FIRE") {
		t.Fatalf("disabled plugin hook fired: %q", r.stdout)
	}
}

// hookMarker returns a settings.json hooks stanza writing a marker file.
func hookMarker(event, matcher, marker string) string {
	m := ""
	if matcher != "" {
		m = `"matcher":"` + matcher + `",`
	}
	return `{"hooks":{"` + event + `":[{` + m +
		`"hooks":[{"type":"command","command":"touch ` + marker + `"}]}]}}`
}

// TestHookPreCompact / PostCompact: /compact fires both lifecycle hooks.
func TestHookPreCompact(t *testing.T) {
	s := newSession(t)
	pre := filepath.Join(s.cwd, "precompact.marker")
	post := filepath.Join(s.cwd, "postcompact.marker")
	s.writeSettings(`{"hooks":{
		"PreCompact":[{"matcher":"manual","hooks":[{"type":"command","command":"touch ` + pre + `"}]}],
		"PostCompact":[{"matcher":"manual","hooks":[{"type":"command","command":"touch ` + post + `"}]}]}}`)

	p := startREPL(t, s)
	p.sendUntilEchoed(t, "seed turn\r", "❯ seed turn", 8)
	p.waitFor("Mock response", 30*time.Second)
	p.sendUntilEchoed(t, "/compact\r", "❯ /compact", 8)
	p.waitFor("ompact", 20*time.Second)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, e1 := os.Stat(pre)
		_, e2 := os.Stat(post)
		if e1 == nil && e2 == nil {
			p.exit()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("PreCompact/PostCompact hooks did not fire (pre=%v post=%v)",
		fileExists(pre), fileExists(post))
}

// TestHookSubagentStop: an Agent finishing fires SubagentStop.
func TestHookSubagentStop(t *testing.T) {
	s := newSession(t)
	mark := filepath.Join(s.cwd, "substop.marker")
	s.writeSettings(hookMarker("SubagentStop", "", mark))
	r := s.run(t, "--mock", "-p",
		`tool:Agent {"description":"e2e","prompt":"sub x","run_in_background":false}`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !fileExists(mark) {
		t.Fatal("SubagentStop hook did not fire")
	}
}

// TestHookCwdChanged: a persisted Bash cd fires the hook (interactive —
// OnCwdChanged is REPL-wired).
func TestHookCwdChanged(t *testing.T) {
	s := newSession(t)
	mark := filepath.Join(s.cwd, "cwd.marker")
	s.writeSettings(hookMarker("CwdChanged", "", mark))
	other := t.TempDir()

	p := startREPL(t, s, "--permission-mode", "bypassPermissions")
	p.sendUntilEchoed(t, `tool:Bash {"command":"cd `+other+`"}`+"\r", "❯ tool:Bash", 8)
	// the cd result is empty — wait on the tool-result receipt, then the marker
	p.waitFor("Tool results received", 30*time.Second)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && !fileExists(mark) {
		time.Sleep(150 * time.Millisecond)
	}
	p.exit()
	if !fileExists(mark) {
		t.Fatal("CwdChanged hook did not fire")
	}
}

// TestHookPermissionRequest: an interactive permission prompt fires
// PermissionRequest + Notification hooks before the dialog.
func TestHookPermissionRequest(t *testing.T) {
	s := newSession(t)
	preM := filepath.Join(s.cwd, "permreq.marker")
	notM := filepath.Join(s.cwd, "notify.marker")
	s.writeSettings(`{"hooks":{
		"PermissionRequest":[{"matcher":"Bash","hooks":[{"type":"command","command":"touch ` + preM + `"}]}],
		"Notification":[{"hooks":[{"type":"command","command":"touch ` + notM + `"}]}]}}`)

	p := startREPL(t, s) // default mode — Bash needs permission
	p.sendUntilEchoed(t, `tool:Bash {"command":"echo hi"}`+"\r", "❯ tool:Bash", 8)
	p.waitFor("1. Yes", 30*time.Second)

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if fileExists(preM) && fileExists(notM) {
			p.exit()
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("permission hooks missing (req=%v notif=%v)", fileExists(preM), fileExists(notM))
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// TestHookPostToolUseFailure: a failing tool fires PostToolUseFailure
// (distinct from PostToolUse).
func TestHookPostToolUseFailure(t *testing.T) {
	s := newSession(t)
	mark := filepath.Join(s.cwd, "fail.marker")
	s.writeSettings(hookMarker("PostToolUseFailure", "Bash", mark))
	r := s.run(t, "--mock", "-p",
		`tool:Bash {"command":"exit 3"}`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !fileExists(mark) {
		t.Fatal("PostToolUseFailure hook did not fire")
	}
}

// TestHookTaskCompleted: a background task exiting fires TaskCompleted.
func TestHookTaskCompleted(t *testing.T) {
	s := newSession(t)
	mark := filepath.Join(s.cwd, "taskdone.marker")
	s.writeSettings(hookMarker("TaskCompleted", "", mark))
	r := s.run(t, "--mock", "-p",
		`tool:Bash {"command":"true","run_in_background":true}`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && !fileExists(mark) {
		time.Sleep(150 * time.Millisecond)
	}
	if !fileExists(mark) {
		t.Fatal("TaskCompleted hook did not fire on bg-task exit")
	}
}

// TestHookCwdChangedPrint: CwdChanged is wired outside the REPL too — a
// Bash cd in -p fires it.
func TestHookCwdChangedPrint(t *testing.T) {
	s := newSession(t)
	mark := filepath.Join(s.cwd, "cwdprint.marker")
	s.writeSettings(hookMarker("CwdChanged", "", mark))
	other := t.TempDir()
	r := s.run(t, "--mock", "-p",
		`tool:Bash {"command":"cd `+other+`"}`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !fileExists(mark) {
		t.Fatal("CwdChanged did not fire in -p")
	}
}

// TestIncludeHookEvents: --include-hook-events emits hook run records in
// stream-json output.
func TestIncludeHookEvents(t *testing.T) {
	s := newSession(t)
	mark := filepath.Join(s.cwd, "x.marker")
	s.writeSettings(hookMarker("PostToolUse", "Bash", mark))
	r := s.run(t, "--mock", "-p",
		`tool:Bash {"command":"echo hooked"}`,
		"--permission-mode", "bypassPermissions",
		"--include-hook-events",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "hook_started") &&
		!strings.Contains(r.stdout, "hook_response") {
		t.Fatalf("hook events missing from stream-json: %s", truncStr(r.stdout, 1500))
	}
}

// TestHookStopFailure: a Stop hook whose command exits nonzero (not 2)
// fires the StopFailure event — a separate handler can observe it.
func TestHookStopFailure(t *testing.T) {
	s := newSession(t)
	mark := filepath.Join(s.cwd, "stopfail.marker")
	s.writeSettings(`{"hooks":{"Stop":[{"hooks":[
		{"type":"command","command":"exit 3"}]}],
		"StopFailure":[{"hooks":[
		{"type":"command","command":"touch ` + mark + `"}]}]}}`)
	// Stop fires on turn end in the interactive REPL
	p := startREPL(t, s)
	p.send("hi\r")
	p.waitFor("Mock response", 30*time.Second)
	p.exit()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && !fileExists(mark) {
		time.Sleep(100 * time.Millisecond)
	}
	if !fileExists(mark) {
		t.Fatal("StopFailure hook did not fire on nonzero Stop hook")
	}
}

// TestPluginHook: an enabled plugin's hooks/hooks.json fires on tool
// calls — ${CLAUDE_PLUGIN_ROOT} expands to the install path.
func TestPluginHook(t *testing.T) {
	s := newSession(t)
	plugDir := filepath.Join(s.home, ".claude", "plugins", "e2eplug")
	os.MkdirAll(filepath.Join(plugDir, "hooks"), 0o755)
	os.WriteFile(filepath.Join(plugDir, "hooks", "hooks.json"), []byte(
		`{"hooks":{"PostToolUse":[{"matcher":"Bash","hooks":[`+
			`{"type":"command","command":"touch ${CLAUDE_PLUGIN_ROOT}/fired.marker"}]}]}}`), 0o644)
	os.WriteFile(filepath.Join(s.home, ".claude", "plugins", "installed_plugins.json"), []byte(
		`{"plugins":{"e2eplug@mkt":[{"installPath":"`+plugDir+`","enabled":true}]}}`), 0o644)

	r := s.run(t, "--mock", "-p",
		`tool:Bash {"command":"echo plug-ok"}`, "--permission-mode", "bypassPermissions")
	s.mustOK(t, r)
	if _, err := os.Stat(filepath.Join(plugDir, "fired.marker")); err != nil {
		t.Fatalf("plugin hook did not fire: %v", err)
	}
}

// TestPluginDisabled: a disabled plugin's hooks do not fire.
func TestPluginDisabled(t *testing.T) {
	s := newSession(t)
	plugDir := filepath.Join(s.home, ".claude", "plugins", "offplug")
	os.MkdirAll(filepath.Join(plugDir, "hooks"), 0o755)
	os.WriteFile(filepath.Join(plugDir, "hooks", "hooks.json"), []byte(
		`{"hooks":{"PostToolUse":[{"matcher":"Bash","hooks":[`+
			`{"type":"command","command":"touch ${CLAUDE_PLUGIN_ROOT}/fired.marker"}]}]}}`), 0o644)
	os.WriteFile(filepath.Join(s.home, ".claude", "plugins", "installed_plugins.json"), []byte(
		`{"plugins":{"offplug@mkt":[{"installPath":"`+plugDir+`","enabled":false}]}}`), 0o644)

	r := s.run(t, "--mock", "-p",
		`tool:Bash {"command":"echo plug-ok"}`, "--permission-mode", "bypassPermissions")
	s.mustOK(t, r)
	if _, err := os.Stat(filepath.Join(plugDir, "fired.marker")); !os.IsNotExist(err) {
		t.Fatal("disabled plugin hook fired")
	}
}
