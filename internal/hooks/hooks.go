// Package hooks executes Claude Code settings.json hooks.
// Shape: {"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"..."}]}],...}}
package hooks

import (
	"bytes"
	"claude-go/internal/config"
	"claude-go/internal/perms"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Event string

const (
	PreToolUse          Event = "PreToolUse"
	PostToolUse         Event = "PostToolUse"
	PostToolUseFailure  Event = "PostToolUseFailure"
	PostToolBatch       Event = "PostToolBatch"
	PermissionDenied    Event = "PermissionDenied"
	PermissionRequest   Event = "PermissionRequest"
	Notification        Event = "Notification"
	UserPromptSubmit    Event = "UserPromptSubmit"
	UserPromptExpansion Event = "UserPromptExpansion"
	SessionStart        Event = "SessionStart"
	SessionEnd          Event = "SessionEnd"
	Stop                Event = "Stop"
	StopFailure         Event = "StopFailure"
	SubagentStart       Event = "SubagentStart"
	SubagentStop        Event = "SubagentStop"
	PreCompact          Event = "PreCompact"
	PostCompact         Event = "PostCompact"
	PreModelSwitch      Event = "PreModelSwitch"
	PostModelSwitch     Event = "PostModelSwitch"
	Setup               Event = "Setup"
	TeammateIdle        Event = "TeammateIdle"
	TaskCreated         Event = "TaskCreated"
	TaskCompleted       Event = "TaskCompleted"
	Elicitation         Event = "Elicitation"
	ElicitationResult   Event = "ElicitationResult"
	ConfigChange        Event = "ConfigChange"
	InstructionsLoaded  Event = "InstructionsLoaded"
	WorktreeCreate      Event = "WorktreeCreate"
	WorktreeRemove      Event = "WorktreeRemove"
	CwdChanged          Event = "CwdChanged"
	FileChanged         Event = "FileChanged"
	DirectoryAdded      Event = "DirectoryAdded"
	MessageDisplay      Event = "MessageDisplay"
)

// matcherField maps an event to the payload field its matcher applies to
// (real matcherMetadata.fieldToMatch). Events NOT listed ignore matchers
// entirely — a configured matcher must not prevent them from running.
// FileChanged's match value is basename(file_path) (real's OVe).
var matcherField = map[Event]string{
	PreToolUse: "tool_name", PostToolUse: "tool_name",
	PostToolUseFailure: "tool_name",
	PermissionDenied:   "tool_name", PermissionRequest: "tool_name",
	UserPromptExpansion: "command_name",
	SessionStart:        "source", SessionEnd: "reason",
	PreCompact: "trigger", PostCompact: "trigger",
	PreModelSwitch: "to_model", PostModelSwitch: "to_model",
	Notification: "notification_type", StopFailure: "error",
	SubagentStart: "agent_type", SubagentStop: "agent_type",
	Setup:        "trigger",
	ConfigChange: "source", InstructionsLoaded: "load_reason",
	DirectoryAdded: "source",
	Elicitation:    "mcp_server_name", ElicitationResult: "mcp_server_name",
	FileChanged: "file_path",
	// no matcher: UserPromptSubmit, Stop, PostToolBatch, TeammateIdle,
	// TaskCreated, TaskCompleted, WorktreeCreate/Remove, CwdChanged,
	// MessageDisplay.
}

// wideCharsetEvents is real's z3 — events whose "simple" matcher charset
// admits spaces, commas and dashes (and splits alternatives on | or ,).
// Other matcher events use the strict [a-zA-Z0-9_|] charset split on |.
var wideCharsetEvents = map[Event]bool{
	PreToolUse: true, PostToolUse: true, PostToolUseFailure: true,
	PermissionRequest: true, PermissionDenied: true,
	UserPromptExpansion: true, SessionStart: true, SessionEnd: true,
	Setup: true, PreCompact: true, PostCompact: true,
	PreModelSwitch: true, PostModelSwitch: true,
	Notification: true, SubagentStart: true, SubagentStop: true,
	Elicitation: true, ElicitationResult: true,
	ConfigChange: true, InstructionsLoaded: true, DirectoryAdded: true,
}

// toolEvents is real's iSn — events carrying a tool call.
var toolEvents = map[Event]bool{
	PreToolUse: true, PostToolUse: true, PostToolUseFailure: true,
	PermissionRequest: true, PermissionDenied: true,
}

// envFileEvents get CLAUDE_ENV_FILE — real lets these hooks emit session
// env vars by appending `export KEY=value` lines to the file.
var envFileEvents = map[Event]bool{
	SessionStart: true, Setup: true, CwdChanged: true, FileChanged: true,
}

// Command is one hook entry (real's Bee input schema).
type Command struct {
	Type    string `json:"type"` // "command" | "prompt" | "agent" | "http" | "mcp_tool" | "script" | "callback" | "function"
	Command string `json:"command"`
	// Args execs Command directly without a shell when non-empty.
	Args    []string          `json:"args,omitempty"`
	Prompt  string            `json:"prompt"` // type=prompt|agent
	URL     string            `json:"url"`    // type=http
	Headers map[string]string `json:"headers,omitempty"`
	// AllowedEnvVars whitelists the env names headers may interpolate
	// (real: only listed vars expand in header values).
	AllowedEnvVars []string `json:"allowedEnvVars,omitempty"`
	Timeout        int      `json:"timeout"` // seconds
	Shell          string   `json:"shell,omitempty"`
	// Async hooks run detached; their output arrives out-of-band (real's
	// rewake pipeline) instead of folding into this Run's Result.
	Async         bool   `json:"async,omitempty"`
	AsyncRewake   bool   `json:"asyncRewake,omitempty"`
	AsyncTimeout  int    `json:"asyncTimeout,omitempty"` // ms; default 15000
	RewakeMessage string `json:"rewakeMessage,omitempty"`
	RewakeSummary string `json:"rewakeSummary,omitempty"`
	// Once runs the hook at most once per session.
	Once bool `json:"once,omitempty"`
	// If is a permission-rule precondition ("Bash(git *)") evaluated
	// against tool events before spawning.
	If            string `json:"if,omitempty"`
	StatusMessage string `json:"statusMessage,omitempty"`
	Model         string `json:"model,omitempty"` // model override for prompt/agent hooks
	// ContinueOnBlock: a prompt hook's not-met verdict doesn't prevent
	// continuation (real's preventContinuation flag).
	ContinueOnBlock bool   `json:"continueOnBlock,omitempty"`
	Server          string `json:"server,omitempty"` // mcp_tool
	Tool            string `json:"tool,omitempty"`   // mcp_tool
	File            string `json:"file,omitempty"`   // script
	Script          string `json:"script,omitempty"` // script
	CallbackID      string `json:"callbackId,omitempty"`
}

// Judge evaluates prompt-type hooks. Set by the agent loop (Answerer).
var Judge func(prompt, contextJSON string) (allow bool, reason string)

type Matcher struct {
	Matcher string    `json:"matcher"`
	Hooks   []Command `json:"hooks"`
}

type Config map[string][]Matcher

// managedSettingsPath mirrors config's managed-settings lookup (kept
// local so policy hooks load without exporting config internals).
func managedSettingsPath() string {
	if p := os.Getenv("CLAUDE_GO_MANAGED_SETTINGS"); p != "" {
		return p
	}
	if runtime.GOOS == "darwin" {
		return "/Library/Application Support/ClaudeCode/managed-settings.json"
	}
	return "/etc/claude-code/managed-settings.json"
}

// Load reads hooks from policy + user + project settings files.
// Managed (policySettings) hooks run first — real's source ordering puts
// policySettings/flagSettings below user < project < local < session <
// plugin. Gates (real's xxo, mod_1338.js):
//   - disableAllHooks in managed OR user settings → only managed hooks run
//   - allowManagedHooksOnly (managed only) → only managed hooks run
func Load(cwd string) Config {
	if os.Getenv("CLAUDE_CODE_SIMPLE") != "" {
		return Config{} // --bare: no hooks
	}
	out := Config{}
	type fileHooks struct {
		hooks           Config
		disableAllHooks bool
	}
	read := func(p string) fileHooks {
		var fh fileHooks
		if !config.SettingSourceOK(cwd, p) {
			return fh
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return fh
		}
		var top struct {
			Hooks           Config `json:"hooks"`
			DisableAllHooks bool   `json:"disableAllHooks"`
		}
		if json.Unmarshal(b, &top) != nil {
			return fh
		}
		fh.hooks, fh.disableAllHooks = top.Hooks, top.DisableAllHooks
		return fh
	}
	managed := struct {
		hooks                 Config
		disableAllHooks       bool
		allowManagedHooksOnly bool
	}{}
	mp := managedSettingsPath()
	if config.SettingSourceOK(cwd, mp) {
		if b, err := os.ReadFile(mp); err == nil {
			var top struct {
				Hooks                 Config `json:"hooks"`
				DisableAllHooks       bool   `json:"disableAllHooks"`
				AllowManagedHooksOnly bool   `json:"allowManagedHooksOnly"`
			}
			if json.Unmarshal(b, &top) == nil {
				managed.hooks = top.Hooks
				managed.disableAllHooks = top.DisableAllHooks
				managed.allowManagedHooksOnly = top.AllowManagedHooksOnly
			}
		}
	}
	merge := func(src Config) {
		for ev, ms := range src {
			out[ev] = append(out[ev], ms...)
		}
	}
	merge(managed.hooks)
	user := read(filepath.Join(config.ClaudeDir(), "settings.json"))
	if managed.disableAllHooks || user.disableAllHooks {
		return out // disableAllHooks: only managed hooks still run
	}
	if managed.allowManagedHooksOnly {
		return out // allowManagedHooksOnly: non-managed tiers don't load
	}
	merge(user.hooks)
	merge(read(filepath.Join(cwd, ".claude", "settings.json")).hooks)
	merge(read(filepath.Join(cwd, ".claude", "settings.local.json")).hooks)
	// plugin hooks: each enabled plugin's hooks/hooks.json merges in —
	// real resolves ${CLAUDE_PLUGIN_ROOT} in commands to the install path
	for _, p := range pluginHooks() {
		for ev, ms := range p.cfg {
			out[ev] = append(out[ev], ms...)
		}
	}
	return out
}

// pluginHooks loads hooks/hooks.json from each installed+enabled plugin's
// cache dir. ${CLAUDE_PLUGIN_ROOT} in command strings expands to the
// plugin's install path (real does this at spawn time).
func pluginHooks() []struct {
	root string
	cfg  Config
} {
	var out []struct {
		root string
		cfg  Config
	}
	b, _ := os.ReadFile(filepath.Join(config.ClaudeDir(), "plugins", "installed_plugins.json"))
	var wrap struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
			Enabled     *bool  `json:"enabled"`
		} `json:"plugins"`
	}
	json.Unmarshal(b, &wrap)
	for _, vers := range wrap.Plugins {
		for _, v := range vers {
			if v.Enabled != nil && !*v.Enabled {
				continue
			}
			hb, err := os.ReadFile(filepath.Join(v.InstallPath, "hooks", "hooks.json"))
			if err != nil {
				continue
			}
			var top struct {
				Hooks Config `json:"hooks"`
			}
			if json.Unmarshal(hb, &top) != nil {
				continue
			}
			// expand CLAUDE_PLUGIN_ROOT in every command
			for ev, ms := range top.Hooks {
				for mi := range ms {
					for hi := range ms[mi].Hooks {
						ms[mi].Hooks[hi].Command = strings.ReplaceAll(
							ms[mi].Hooks[hi].Command, "${CLAUDE_PLUGIN_ROOT}", v.InstallPath)
					}
				}
				top.Hooks[ev] = ms
			}
			out = append(out, struct {
				root string
				cfg  Config
			}{v.InstallPath, top.Hooks})
		}
	}
	// --plugin-dir: load a dev plugin directory directly (hooks/hooks.json)
	// without requiring installed_plugins.json registration
	// filepath.SplitList like the plugin loader — a path list, not
	// whitespace-separated words
	for _, dir := range filepath.SplitList(os.Getenv("CLAUDE_GO_PLUGIN_DIR")) {
		if dir == "" {
			continue
		}
		hb, err := os.ReadFile(filepath.Join(dir, "hooks", "hooks.json"))
		if err != nil {
			continue
		}
		var top struct {
			Hooks Config `json:"hooks"`
		}
		if json.Unmarshal(hb, &top) != nil {
			continue
		}
		for ev, ms := range top.Hooks {
			for mi := range ms {
				for hi := range ms[mi].Hooks {
					ms[mi].Hooks[hi].Command = strings.ReplaceAll(
						ms[mi].Hooks[hi].Command, "${CLAUDE_PLUGIN_ROOT}", dir)
				}
			}
			top.Hooks[ev] = ms
		}
		out = append(out, struct {
			root string
			cfg  Config
		}{dir, top.Hooks})
	}
	return out
}

// Output is a hook's parsed JSON stdout (real a4() surface).
type Output struct {
	Continue           json.RawMessage `json:"continue"` // bool or "false"
	Decision           string          `json:"decision"` // "block" | "approve"
	Reason             string          `json:"reason"`
	SystemMessage      string          `json:"systemMessage"`
	StopReason         string          `json:"stopReason"`
	SuppressOutput     bool            `json:"suppressOutput"`
	UpdatedInput       json.RawMessage `json:"updatedInput"`
	HookSpecificOutput *struct {
		HookEventName            string          `json:"hookEventName"`
		PermissionDecision       string          `json:"permissionDecision"` // allow|deny|ask|defer
		PermissionDecisionReason string          `json:"permissionDecisionReason"`
		AdditionalContext        string          `json:"additionalContext"`
		UpdatedInput             json.RawMessage `json:"updatedInput"`
		// Decision is the nested PermissionRequest verdict:
		// {behavior:allow,updatedInput?,updatedPermissions?} or
		// {behavior:deny,message?,interrupt?}
		Decision *struct {
			Behavior           string          `json:"behavior"`
			Message            string          `json:"message"`
			Interrupt          bool            `json:"interrupt"`
			UpdatedInput       json.RawMessage `json:"updatedInput"`
			UpdatedPermissions json.RawMessage `json:"updatedPermissions"`
		} `json:"decision"`
		UpdatedPermissions     json.RawMessage `json:"updatedPermissions"`
		ClassifierContext      string          `json:"classifierContext"`
		UpdatedToolOutput      json.RawMessage `json:"updatedToolOutput"`
		UpdatedMCPToolOutput   json.RawMessage `json:"updatedMCPToolOutput"`
		InitialUserMessage     string          `json:"initialUserMessage"`
		SessionTitle           string          `json:"sessionTitle"`
		SuppressOriginalPrompt bool            `json:"suppressOriginalPrompt"`
		WatchPaths             []string        `json:"watchPaths"`
		ReloadSkills           bool            `json:"reloadSkills"`
		Retry                  bool            `json:"retry"`
		DisplayContent         string          `json:"displayContent"` // MessageDisplay: replaces the rendered delta
		WorktreePath           string          `json:"worktreePath"`
		TerminalSequence       string          `json:"terminalSequence"`
		Action                 string          `json:"action"`  // Elicitation answer
		Content                json.RawMessage `json:"content"` // Elicitation answer
		Metrics                json.RawMessage `json:"metrics"`
	} `json:"hookSpecificOutput"`
}

// continueFalse reports whether `continue` was explicitly set false
// (bool false or string "false" both appear in the wild).
func (o *Output) continueFalse() bool {
	if len(o.Continue) == 0 {
		return false
	}
	s := strings.TrimSpace(string(o.Continue))
	return s == "false" || s == `"false"`
}

// Record is one hook execution — emitted as hook_started/hook_response
// stream records under --include-hook-events (real's ji() fields).
type Record struct {
	HookID     string `json:"hook_id"`
	Event      string `json:"hook_event"`
	Command    string `json:"hook_name"`
	ExitCode   int    `json:"exit_code,omitempty"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	Outcome    string `json:"outcome"` // success|blocking|non_blocking_error|async|skipped|error
	DurationMs int64  `json:"duration_ms,omitempty"`
}

type Result struct {
	Block             bool
	Reason            string
	AdditionalContext string
	SystemMessage     string
	Permission        string          // allow|deny|ask|""
	UpdatedInput      json.RawMessage // PreToolUse can rewrite tool_input
	DisplayContent    string          // MessageDisplay: screen-only rewrite of the delta
	PlainText         string          // non-JSON stdout → context for some events
	NonBlockErr       string          // hook failed non-blocking — shown to user
	// Per-event outputs surfaced for callers (real's hookSpecificOutput
	// surface). Consumers that live outside the hooks package read these.
	SuppressOutput         bool
	SessionTitle           string
	InitialUserMessage     string
	SuppressOriginalPrompt bool
	WatchPaths             []string
	ReloadSkills           bool
	UpdatedToolOutput      json.RawMessage
	UpdatedMCPToolOutput   json.RawMessage
	ClassifierContext      string
	Retry                  bool   // PermissionDenied: re-prompt
	Interrupt              bool   // PermissionRequest deny interrupt
	ElicitAction           string // Elicitation: hook's answer action
	ElicitContent          json.RawMessage
	WorktreePath           string // WorktreeCreate: provisioned path
	TerminalSequence       string
	Metrics                json.RawMessage
	UpdatedPermissions     json.RawMessage // PermissionRequest: persisted rules
	Records                []Record        // per-hook executions (stream-json)
}

// Run executes all matching hooks for an event. Input is the JSON payload
// CC sends on stdin (session_id, transcript_path, cwd, tool_name, tool_input...).
// Progress counts hook commands done/total for the current Run — the
// REPL's "(running X hook… 0/2)" status.
var Progress atomic.Int64 // hi 32 bits = total, lo = done
var lastTotal atomic.Int64

func Run(c Config, ev Event, payload map[string]any, cwd string) Result {
	var res Result
	matchers := c[string(ev)]
	if len(matchers) == 0 {
		return res
	}
	field := matcherField[ev]
	var matchVal string
	haveField := false
	if field != "" {
		if v, ok := payload[field]; ok {
			if s, ok := v.(string); ok {
				matchVal, haveField = s, true
				if ev == FileChanged {
					matchVal = filepath.Base(matchVal) // real: OVe basename
				}
				if ev == PreModelSwitch || ev == PostModelSwitch {
					matchVal = normalizeModelName(matchVal)
					haveField = matchVal != "" // unresolvable → no gate
				}
			}
		}
	}
	stdin, _ := json.Marshal(payload)
	total := 0
	for _, m := range matchers {
		total += len(m.Hooks)
	}
	Progress.Store(int64(total) << 32)
	defer Progress.Store(0)
	for _, m := range matchers {
		if m.Matcher != "" && m.Matcher != "*" {
			switch {
			case field == "", !haveField:
				// no matcher field on this event (or the field is absent/
				// unresolvable): real ignores matchers and runs anyway
			default:
				warnBareMCP(&res, ev, m.Matcher)
				if !matcherMatch(ev, m.Matcher, matchVal, &res) {
					continue
				}
			}
		}
		for _, h := range m.Hooks {
			res.runOne(c, ev, m, h, payload, stdin, cwd)
		}
	}
	return res
}

// runOne executes a single hook entry and folds it into res.
func (res *Result) runOne(c Config, ev Event, m Matcher, h Command, payload map[string]any, stdin []byte, cwd string) {
	if h.Once && !claimOnce(ev, m.Matcher, h) {
		return
	}
	// `if` precondition — real evaluates it (permission-rule syntax) for
	// command/prompt/agent/http/mcp_tool/script hooks on tool events
	// only; on other events the hook is skipped with a warning.
	if h.If != "" && ifCheckableType(h.Type) {
		if !toolEvents[ev] {
			res.NonBlockErr += fmt.Sprintf("Hook if condition %q cannot be evaluated for non-tool event %s\n", h.If, ev)
			res.Records = append(res.Records, Record{HookID: newHookID(), Event: string(ev), Command: hookLabel(h), ExitCode: -1, Outcome: "skipped"})
			return
		}
		if !evalIf(h.If, payload, cwd) {
			res.Records = append(res.Records, Record{HookID: newHookID(), Event: string(ev), Command: hookLabel(h), ExitCode: -1, Outcome: "skipped"})
			return
		}
	}
	if h.Async && (h.Type == "command" || h.Type == "") {
		to := h.AsyncTimeout
		if to <= 0 {
			to = h.Timeout * 1000
		}
		if to <= 0 {
			to = 15000 // real's asyncTimeout default
		}
		go runCmd(h, stdin, cwd, ev, time.Duration(to)*time.Millisecond+5*time.Second)
		res.Records = append(res.Records, Record{HookID: newHookID(), Event: string(ev), Command: hookLabel(h), ExitCode: -1, Outcome: "async"})
		Progress.Add(1)
		return
	}
	if h.Type == "prompt" || h.Type == "agent" {
		res.runLLMHook(ev, h, stdin)
		return
	}
	if h.Type == "http" && h.URL != "" {
		start := time.Now()
		o := runHTTPHook(h, stdin)
		outcome := "success"
		if o == nil {
			outcome = "non_blocking_error"
		}
		res.Records = append(res.Records, Record{HookID: newHookID(), Event: string(ev), Command: hookLabel(h), ExitCode: -1, Outcome: outcome, DurationMs: time.Since(start).Milliseconds()})
		if o != nil {
			applyOutput(res, o, ev)
		}
		return
	}
	if h.Type == "script" {
		// real's script hooks execute a bundled script — treat the
		// file/script field as the command path
		target := h.File
		if target == "" {
			target = h.Script
		}
		if target == "" {
			res.NonBlockErr += "script-type hook has no file\n"
			return
		}
		h = Command{Type: "command", Command: target}
	}
	if h.Type != "command" && h.Type != "" {
		// mcp_tool / callback / function need host machinery the hooks
		// package doesn't own — warn rather than silently skip.
		res.NonBlockErr += fmt.Sprintf("%s-type hooks are not supported; use a command-type hook\n", h.Type)
		res.Records = append(res.Records, Record{HookID: newHookID(), Event: string(ev), Command: hookLabel(h), ExitCode: -1, Outcome: "skipped"})
		return
	}
	start := time.Now()
	stdout, stderr, code := runCmd(h, stdin, cwd, ev, hookTimeout(h, ev))
	dur := time.Since(start)
	outcome := "success"
	if code == 2 {
		outcome = "blocking"
	} else if code != 0 {
		outcome = "non_blocking_error"
	}
	res.Records = append(res.Records, Record{
		HookID: newHookID(), Event: string(ev), Command: hookLabel(h),
		ExitCode: code, Stdout: string(stdout), Stderr: string(stderr),
		Outcome: outcome, DurationMs: dur.Milliseconds(),
	})
	Progress.Add(1)
	mergeEnvFile(ev, payload)
	var o Output
	rawOK := false
	var rawKeys map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimSpace(stdout), &o) == nil && json.Unmarshal(bytes.TrimSpace(stdout), &rawKeys) == nil && rawKeys != nil {
		rawOK = o.Decision != "" || o.Reason != "" || o.SystemMessage != "" ||
			o.StopReason != "" || o.HookSpecificOutput != nil ||
			len(o.Continue) > 0 || len(o.UpdatedInput) > 0 || o.SuppressOutput
	}
	if rawOK {
		warnUnknownKeys(res, rawKeys)
		applyOutput(res, &o, ev)
	} else if code == 2 {
		// exit 2 = block; stderr (separate stream) is the reason
		if !res.Block {
			res.Block = true
		}
		res.Reason = firstNonEmpty(string(bytes.TrimSpace(stderr)), res.Reason)
	} else if len(bytes.TrimSpace(stdout)) > 0 {
		// plain-text stdout → context payload (UserPromptSubmit/
		// SessionStart show it to Claude; PreCompact = instructions)
		res.PlainText += string(bytes.TrimSpace(stdout)) + "\n"
	}
	if code != 0 && code != 2 && code != -1 && len(bytes.TrimSpace(stderr)) > 0 {
		res.NonBlockErr += string(bytes.TrimSpace(stderr)) + "\n"
	}
	// a Stop hook that errors (nonzero, non-block exit) fires the
	// StopFailure event — real lets a separate handler observe it
	if ev == Stop && code != 0 && code != 2 && code != -1 {
		fp := map[string]any{}
		for k, v := range payload {
			fp[k] = v
		}
		fp["error"] = strings.TrimSpace(string(stderr))
		if fr := Run(c, StopFailure, fp, cwd); fr.Block {
			res.Block = true
			res.Reason = firstNonEmpty(fr.Reason, res.Reason)
		}
	}
}

// runLLMHook handles type=prompt|agent. Real gates them to tool events
// with conversation context (PreToolUse/PostToolUse; PermissionRequest
// accepts prompt but NOT agent — it can't return allow/deny).
func (res *Result) runLLMHook(ev Event, h Command, stdin []byte) {
	switch {
	case h.Type == "agent" && ev == PermissionRequest:
		res.NonBlockErr += "agent-type hooks are not supported for PermissionRequest events (an agent hook answers ok or not ok, and cannot return the allow / deny decision a permission request needs). Use a command- or http-type hook instead.\n"
		res.Records = append(res.Records, Record{HookID: newHookID(), Event: string(ev), Command: hookLabel(h), ExitCode: -1, Outcome: "non_blocking_error"})
		return
	case ev != PreToolUse && ev != PostToolUse && ev != PermissionRequest:
		res.NonBlockErr += fmt.Sprintf("%s-type hooks are not supported for %s events (no conversation context is available). Use a command-type hook instead.\n", h.Type, ev)
		res.Records = append(res.Records, Record{HookID: newHookID(), Event: string(ev), Command: hookLabel(h), ExitCode: -1, Outcome: "non_blocking_error"})
		return
	}
	if Judge == nil {
		return
	}
	allow, reason := Judge(h.Prompt+"\n\nContext:\n"+string(stdin), "")
	res.Records = append(res.Records, Record{HookID: newHookID(), Event: string(ev), Command: hookLabel(h), ExitCode: -1, Outcome: "success"})
	// on PermissionRequest the ok/not-ok verdict IS the permission
	// answer; elsewhere a negative verdict is a blocking error
	if ev == PermissionRequest {
		if allow {
			res.Permission = "allow"
		} else {
			res.Permission = "deny"
			res.Reason = firstNonEmpty(reason, res.Reason)
		}
		return
	}
	if !allow {
		if h.ContinueOnBlock {
			res.SystemMessage += reason
			return
		}
		if !res.Block {
			res.Block = true
			res.Reason = reason
		}
	}
}

// ifCheckableType: real evaluates `if` for command/prompt/agent/http/
// mcp_tool/script hooks only.
func ifCheckableType(t string) bool {
	switch t {
	case "command", "", "prompt", "agent", "http", "mcp_tool", "script":
		return true
	}
	return false
}

// evalIf evaluates the `if` permission-rule precondition against the
// tool call in the payload (real's evaluateCondition → permission matcher).
func evalIf(cond string, payload map[string]any, cwd string) bool {
	toolName, _ := payload["tool_name"].(string)
	var input json.RawMessage
	switch ti := payload["tool_input"].(type) {
	case json.RawMessage:
		input = ti
	default:
		input, _ = json.Marshal(ti)
	}
	return perms.MatchToolCall(cond, toolName, input, cwd)
}

// onceKeys dedupes `once` hooks per session.
var onceKeys sync.Map

func claimOnce(ev Event, matcher string, h Command) bool {
	key := fmt.Sprintf("%s|%s|%s|%s|%s|%s", ev, matcher, h.Type, h.Command, h.Prompt, h.URL)
	_, loaded := onceKeys.LoadOrStore(key, true)
	return !loaded
}

// ResetOnce clears the once-hook session set (tests).
func ResetOnce() { onceKeys = sync.Map{} }

// warnBareMCP warns once when a tool-event matcher carries a bare
// mcp__server alternative — real's Wmr: it compares as an exact string
// and matches no tool.
var bareMCPWarned sync.Map

func warnBareMCP(res *Result, ev Event, matcher string) {
	if !toolEvents[ev] {
		return
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_|, -]+$`).MatchString(matcher) {
		return
	}
	for _, alt := range strings.FieldsFunc(matcher, func(r rune) bool { return r == '|' || r == ',' }) {
		a := strings.TrimSpace(alt)
		if strings.HasPrefix(a, "mcp__") && !strings.Contains(a[len("mcp__"):], "__") {
			if _, dup := bareMCPWarned.LoadOrStore(matcher, true); !dup {
				res.NonBlockErr += fmt.Sprintf("Hook matcher %q contains %q — a bare mcp__ server name matches no tool (it is compared as an exact string); use %q to match all tools on that server\n", matcher, a, a+"__*")
			}
			return
		}
	}
}

var modelMarkerRe = regexp.MustCompile(`\s*\[[12]m\]`)

// normalizeModelName approximates real's ZVe — strips [1m]/[2m] markers
// from a to_model value so switch matchers see the bare model.
func normalizeModelName(s string) string {
	return strings.TrimSpace(modelMarkerRe.ReplaceAllString(s, ""))
}

// matcherMatch: real's G3 semantics — for "wide charset" events a matcher
// of [a-zA-Z0-9_|, -] is an exact/|/,/-separated list; other matcher
// events use [a-zA-Z0-9_|] split on | only. Anything else is an
// UNANCHORED regex test. Model-switch matchers first lose [1m]/[2m]
// markers (real's LVe).
func matcherMatch(ev Event, matcher, val string, res *Result) bool {
	if ev == PreModelSwitch || ev == PostModelSwitch {
		if stripped := stripMatcherMarkers(matcher); strings.TrimSpace(stripped) != "" {
			matcher = stripped
		}
	}
	wide := wideCharsetEvents[ev]
	charset := `^[a-zA-Z0-9_|]+$`
	if wide {
		charset = `^[a-zA-Z0-9_|, -]+$`
	}
	if regexp.MustCompile(charset).MatchString(matcher) {
		split := func(r rune) bool { return r == '|' }
		if wide {
			split = func(r rune) bool { return r == '|' || r == ',' }
		}
		for _, part := range strings.FieldsFunc(matcher, split) {
			if strings.TrimSpace(part) == val {
				return true
			}
		}
		return false
	}
	re, err := regexp.Compile(matcher)
	if err != nil {
		res.NonBlockErr += fmt.Sprintf("Invalid regex pattern in hook matcher %q\n", matcher)
		return false
	}
	return re.MatchString(val) // unanchored, like JS test()
}

// real: /\[[12]m\](?=\s*(?:[|,]|$))/gi — RE2 has no lookahead, so the
// delimiter is captured and re-emitted.
var matcherMarkerRe = regexp.MustCompile(`(?i)\[[12]m\](\s*[|,]|\s*$)`)

func stripMatcherMarkers(matcher string) string {
	return matcherMarkerRe.ReplaceAllString(matcher, "$1")
}

// applyOutput folds one parsed hook Output into the aggregate Result.
// First blocked wins for the reported reason; fields accumulate.
func applyOutput(res *Result, o *Output, ev Event) {
	// real's yH validation: hookSpecificOutput.hookEventName must match
	// the event the hook ran for — a mismatched payload is discarded.
	if hs := o.HookSpecificOutput; hs != nil && hs.HookEventName != "" && hs.HookEventName != string(ev) {
		res.NonBlockErr += fmt.Sprintf("hookSpecificOutput.hookEventName %q does not match %s — output discarded\n", hs.HookEventName, ev)
		return
	}
	if o.Decision == "block" || o.continueFalse() {
		if !res.Block {
			res.Block = true
			res.Reason = firstNonEmpty(o.Reason, o.SystemMessage, o.StopReason)
		}
	}
	// decision:"approve" maps to permissionDecision allow
	if o.Decision == "approve" && res.Permission == "" {
		res.Permission = "allow"
	}
	if o.SuppressOutput {
		res.SuppressOutput = true
	}
	if hs := o.HookSpecificOutput; hs != nil {
		if hs.PermissionDecision != "" {
			res.Permission = hs.PermissionDecision
		}
		if hs.PermissionDecisionReason != "" && res.Reason == "" {
			res.Reason = hs.PermissionDecisionReason
		}
		res.AdditionalContext += hs.AdditionalContext
		if len(hs.UpdatedInput) > 0 {
			res.UpdatedInput = hs.UpdatedInput
		}
		if d := hs.Decision; d != nil {
			switch d.Behavior {
			case "allow":
				res.Permission = "allow"
				if len(d.UpdatedInput) > 0 {
					res.UpdatedInput = d.UpdatedInput
				}
				if len(d.UpdatedPermissions) > 0 {
					res.UpdatedPermissions = d.UpdatedPermissions
				}
			case "deny":
				res.Permission = "deny"
				res.Reason = firstNonEmpty(d.Message, res.Reason)
				res.Interrupt = d.Interrupt
			}
		}
		if len(hs.UpdatedPermissions) > 0 {
			res.UpdatedPermissions = hs.UpdatedPermissions
		}
		res.ClassifierContext += hs.ClassifierContext
		if len(hs.UpdatedToolOutput) > 0 {
			res.UpdatedToolOutput = hs.UpdatedToolOutput
		}
		if len(hs.UpdatedMCPToolOutput) > 0 {
			res.UpdatedMCPToolOutput = hs.UpdatedMCPToolOutput
		}
		res.InitialUserMessage = firstNonEmpty(hs.InitialUserMessage, res.InitialUserMessage)
		res.SessionTitle = firstNonEmpty(hs.SessionTitle, res.SessionTitle)
		res.SuppressOriginalPrompt = res.SuppressOriginalPrompt || hs.SuppressOriginalPrompt
		res.WatchPaths = append(res.WatchPaths, hs.WatchPaths...)
		res.ReloadSkills = res.ReloadSkills || hs.ReloadSkills
		res.Retry = res.Retry || hs.Retry
		res.DisplayContent = firstNonEmpty(hs.DisplayContent, res.DisplayContent)
		res.WorktreePath = firstNonEmpty(hs.WorktreePath, res.WorktreePath)
		res.TerminalSequence += hs.TerminalSequence
		res.ElicitAction = firstNonEmpty(hs.Action, res.ElicitAction)
		if len(hs.Content) > 0 {
			res.ElicitContent = hs.Content
		}
		if len(hs.Metrics) > 0 {
			res.Metrics = hs.Metrics
		}
		if o.HookSpecificOutput.DisplayContent != "" {
			res.DisplayContent = o.HookSpecificOutput.DisplayContent
		}
	}
	if len(o.UpdatedInput) > 0 {
		res.UpdatedInput = o.UpdatedInput
	}
	if o.SystemMessage != "" {
		res.SystemMessage += o.SystemMessage
	}
	if o.StopReason != "" && res.Reason == "" {
		res.Reason = o.StopReason
	}
}

// knownTopLevel is real's Bee output surface — anything else warns.
var knownTopLevel = map[string]bool{
	"continue": true, "decision": true, "reason": true,
	"systemMessage": true, "stopReason": true, "suppressOutput": true,
	"updatedInput": true, "hookSpecificOutput": true, "asyncResponse": true,
}

func warnUnknownKeys(res *Result, raw map[string]json.RawMessage) {
	for k := range raw {
		if !knownTopLevel[k] {
			res.NonBlockErr += fmt.Sprintf("unknown hook output key %q — did you mean hookSpecificOutput.%s?\n", k, k)
		}
	}
}

// runCmd executes a command hook with real semantics: separate stdout/stderr
// (64KB caps each), per-event timeout +5s grace, CLAUDE_PROJECT_DIR (and
// CLAUDE_ENV_FILE for env-capable events) env, dangerous env vars stripped,
// session env merged, whole process group killed on timeout.
func runCmd(h Command, stdin []byte, cwd string, ev Event, timeout time.Duration) (stdout, stderr []byte, code int) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var cmd *exec.Cmd
	if len(h.Args) > 0 {
		cmd = exec.CommandContext(ctx, h.Command, h.Args...) // exec form — no shell
	} else {
		shell := h.Shell
		if shell == "" {
			shell = os.Getenv("SHELL")
		}
		if shell == "" {
			shell = "/bin/bash"
		}
		cmd = exec.CommandContext(ctx, shell, "-c", h.Command)
	}
	cmd.Dir = cwd
	cmd.Stdin = bytes.NewReader(stdin)
	var ob, eb cappedBuf
	ob.cap, eb.cap = 64*1024, 64*1024
	cmd.Stdout = &ob
	cmd.Stderr = &eb
	cmd.Env = hookEnv(cwd, ev)
	cmd.SysProcAttr = sysProcAttrDetached()
	err := cmd.Start()
	if err != nil {
		return nil, []byte(err.Error()), -1
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case err = <-wait:
	case <-ctx.Done():
		killProcessGroup(cmd.Process.Pid) // detached: kill the whole group
		err = <-wait
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	return ob.Bytes(), eb.Bytes(), code
}

// hookTimeout is the per-event default (or configured) timeout +5s grace.
func hookTimeout(h Command, ev Event) time.Duration {
	to := h.Timeout
	if to <= 0 {
		to = defaultTimeout(ev)
	}
	return time.Duration(to)*time.Second + 5*time.Second
}

// hookEnv builds the scrubbed env for a hook child.
func hookEnv(cwd string, ev Event) []string {
	env := os.Environ()
	var kept []string
	for _, kv := range env {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if hookEnvDeny(k) {
			continue
		}
		kept = append(kept, kv)
	}
	kept = append(kept, "CLAUDE_PROJECT_DIR="+cwd)
	if envFileEvents[ev] {
		if p := envFileFor(ev); p != "" {
			kept = append(kept, "CLAUDE_ENV_FILE="+p)
		}
	}
	// session env vars emitted by earlier env-file hooks apply to
	// subsequent hook spawns (real sources the env file per spawn)
	sessionEnvMu.Lock()
	for k, v := range sessionEnv {
		kept = append(kept, k+"="+v)
	}
	sessionEnvMu.Unlock()
	return kept
}

// envFileFor returns (creating) the per-event CLAUDE_ENV_FILE path —
// real's <tmp>/session-env/<session>/<event>-hook-<session>.sh.
var envFileOnce sync.Once
var envFileDir string

func envFileFor(ev Event) string {
	envFileOnce.Do(func() {
		sid := os.Getenv("CLAUDE_GO_SESSION_ID")
		if sid == "" {
			sid = fmt.Sprintf("%d", os.Getpid())
		}
		envFileDir = filepath.Join(os.TempDir(),
			fmt.Sprintf("claude-%d", os.Getuid()), "session-env", sid)
	})
	if err := os.MkdirAll(envFileDir, 0o700); err != nil {
		return ""
	}
	return filepath.Join(envFileDir, strings.ToLower(string(ev))+"-hook.sh")
}

// sessionEnv holds vars collected from CLAUDE_ENV_FILE writes.
var (
	sessionEnvMu sync.Mutex
	sessionEnv   = map[string]string{}
)

// mergeEnvFile re-reads the event's env file after a hook and folds
// `KEY=value` / `export KEY=value` lines into the session env overlay.
func mergeEnvFile(ev Event, payload map[string]any) {
	if !envFileEvents[ev] {
		return
	}
	p := envFileFor(ev)
	if p == "" {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	sessionEnvMu.Lock()
	defer sessionEnvMu.Unlock()
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			continue
		}
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		sessionEnv[k] = v
	}
}

// SessionEnv returns env vars emitted by hooks via CLAUDE_ENV_FILE —
// callers that maintain a session environment overlay read this.
func SessionEnv() map[string]string {
	sessionEnvMu.Lock()
	defer sessionEnvMu.Unlock()
	out := make(map[string]string, len(sessionEnv))
	for k, v := range sessionEnv {
		out[k] = v
	}
	return out
}

// cappedBuf drops output past cap (real hooks cap stdout/stderr at 64KB).
type cappedBuf struct {
	bytes.Buffer
	cap int
}

func (b *cappedBuf) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.cap {
		p = p[:max(0, b.cap-b.Len())]
	}
	return b.Buffer.Write(p)
}

// hookEnvDeny mirrors real's dangerous-env scrub for hook children.
func hookEnvDeny(k string) bool {
	for _, pre := range []string{"LD_", "DYLD_", "PYTHON", "BASH_FUNC_", "GIT_CONFIG"} {
		if strings.HasPrefix(k, pre) {
			return true
		}
	}
	switch k {
	case "GCONV_PATH", "NLSPATH", "DEVELOPER_DIR", "BASH_ENV", "ENV", "SUDO_ASKPASS":
		return true
	}
	return false
}

// per-event default timeouts (seconds), real table rOn.
func defaultTimeout(ev Event) int {
	switch ev {
	case Stop, StopFailure, SubagentStop:
		return 120
	case UserPromptSubmit, UserPromptExpansion:
		return 30
	default:
		return 15
	}
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func newHookID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// hookLabel is the display name used in hook records (real's ED labels).
func hookLabel(h Command) string {
	switch {
	case h.Command != "":
		return h.Command
	case h.Prompt != "":
		return h.Prompt
	case h.URL != "":
		return h.URL
	case h.Type != "":
		return h.Type
	}
	return "hook"
}

var envVarRe = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)\}|\$([A-Z_][A-Z0-9_]*)`)

// expandHeaderEnv interpolates $VAR / ${VAR} only for names in the
// hook's allowedEnvVars whitelist — real refuses to expand anything
// else (it substitutes an empty string and warns). Control chars are
// stripped (real's Vpr).
func expandHeaderEnv(v string, allowed map[string]bool) string {
	out := envVarRe.ReplaceAllStringFunc(v, func(tok string) string {
		name := strings.TrimPrefix(strings.TrimSuffix(strings.TrimPrefix(tok, "${"), "}"), "$")
		if !allowed[name] {
			return ""
		}
		return os.Getenv(name)
	})
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == 0 {
			return -1
		}
		return r
	}, out)
}

// runHTTPHook POSTs the hook payload to url and parses the JSON decision.
func runHTTPHook(h Command, stdin []byte) *Output {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(firstInt(h.Timeout, 30))*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", h.URL, bytes.NewReader(stdin))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	allowed := map[string]bool{}
	for _, n := range h.AllowedEnvVars {
		allowed[n] = true
	}
	for k, v := range h.Headers {
		req.Header.Set(k, expandHeaderEnv(v, allowed))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var o Output
	if json.Unmarshal(b, &o) != nil {
		return nil
	}
	return &o
}

func firstInt(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}
