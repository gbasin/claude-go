//go:build e2e

// Coverage for second-audit fixes: hooks output semantics, settings env,
// CLAUDE_CONFIG_DIR, additionalDirectories, transcript fidelity.
package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
