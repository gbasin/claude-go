// Package hooks executes Claude Code settings.json hooks.
// Shape: {"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"..."}]}],...}}
package hooks

import (
	"bytes"
	"claude-go/internal/config"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
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
	// no matcher: UserPromptSubmit, Stop, PostToolBatch, TeammateIdle,
	// TaskCreated, TaskCompleted, WorktreeCreate/Remove, CwdChanged,
	// MessageDisplay. FileChanged matcher = filename watch pattern.
}

type Command struct {
	Type    string            `json:"type"`    // "command" | "prompt" | "agent" | "http"
	Command string            `json:"command"` // for type=command
	Prompt  string            `json:"prompt"`  // for type=prompt|agent (LLM-judged / subagent)
	URL     string            `json:"url"`     // for type=http
	Headers map[string]string `json:"headers,omitempty"`
	Timeout int               `json:"timeout"` // seconds
}

// Judge evaluates prompt-type hooks. Set by the agent loop (Answerer).
var Judge func(prompt, contextJSON string) (allow bool, reason string)

type Matcher struct {
	Matcher string    `json:"matcher"`
	Hooks   []Command `json:"hooks"`
}

type Config map[string][]Matcher

// Load reads hooks from user + project settings files.
func Load(cwd string) Config {
	if os.Getenv("CLAUDE_CODE_SIMPLE") != "" {
		return Config{} // --bare: no hooks
	}
	out := Config{}
	for _, p := range []string{
		filepath.Join(config.ClaudeDir(), "settings.json"),
		filepath.Join(cwd, ".claude", "settings.json"),
		filepath.Join(cwd, ".claude", "settings.local.json"),
	} {
		if !config.SettingSourceOK(cwd, p) {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var top struct {
			Hooks Config `json:"hooks"`
		}
		if json.Unmarshal(b, &top) != nil {
			continue
		}
		for ev, ms := range top.Hooks {
			out[ev] = append(out[ev], ms...)
		}
	}
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
	for _, dir := range strings.Fields(os.Getenv("CLAUDE_GO_PLUGIN_DIR")) {
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
		DisplayContent           string          `json:"displayContent"` // MessageDisplay: replaces the rendered delta
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
	if field != "" {
		matchVal, _ = payload[field].(string)
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
			if field == "" {
				// event has no matcher field — real ignores matchers, runs anyway
			} else if matchVal == "" {
				continue // field absent from payload: never match
			} else if !matcherMatch(m.Matcher, matchVal) {
				continue
			}
		}
		for _, h := range m.Hooks {
			if (h.Type == "prompt" || h.Type == "agent") && Judge != nil {
				payloadStr := string(stdin)
				allow, reason := Judge(h.Prompt+"\n\nContext:\n"+payloadStr, "")
				if !allow {
					if !res.Block {
						res.Block = true
						res.Reason = reason
					}
				}
				continue
			}
			if h.Type == "http" && h.URL != "" {
				if o := runHTTPHook(h, stdin); o != nil {
					applyOutput(&res, o)
				}
				continue
			}
			if h.Type != "command" {
				continue
			}
			stdout, stderr, code := runCmd(h, stdin, cwd, ev)
			Progress.Add(1)
			var o Output
			if json.Unmarshal(bytes.TrimSpace(stdout), &o) == nil && (len(o.Continue) > 0 || o.Decision != "" || o.Reason != "" || o.SystemMessage != "" || o.StopReason != "" || o.HookSpecificOutput != nil || len(o.UpdatedInput) > 0) {
				applyOutput(&res, &o)
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
	}
	return res
}

// matcherMatch: real semantics — a string of only [A-Za-z0-9_ ,|-] is an
// exact match or a "|"/","-separated exact list; anything else is an
// UNANCHORED regex test.
func matcherMatch(matcher, val string) bool {
	if regexp.MustCompile(`^[A-Za-z0-9_ ,|\\-]+$`).MatchString(matcher) {
		for _, part := range strings.FieldsFunc(matcher, func(r rune) bool { return r == '|' || r == ',' }) {
			if strings.TrimSpace(part) == val {
				return true
			}
		}
		return false
	}
	re, err := regexp.Compile(matcher)
	if err != nil {
		return false
	}
	return re.MatchString(val) // unanchored, like JS test()
}

// applyOutput folds one parsed hook Output into the aggregate Result.
// First blocked wins for the reported reason; fields accumulate.
func applyOutput(res *Result, o *Output) {
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
	if o.HookSpecificOutput != nil {
		if o.HookSpecificOutput.PermissionDecision != "" {
			res.Permission = o.HookSpecificOutput.PermissionDecision
		}
		if o.HookSpecificOutput.PermissionDecisionReason != "" && res.Reason == "" {
			res.Reason = o.HookSpecificOutput.PermissionDecisionReason
		}
		res.AdditionalContext += o.HookSpecificOutput.AdditionalContext
		if len(o.HookSpecificOutput.UpdatedInput) > 0 {
			res.UpdatedInput = o.HookSpecificOutput.UpdatedInput
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

// runCmd executes a command hook with real semantics: separate stdout/stderr
// (64KB caps each), per-event default timeout +5s grace, CLAUDE_PROJECT_DIR
// env, dangerous env vars stripped, whole process group killed on timeout.
func runCmd(h Command, stdin []byte, cwd string, ev Event) (stdout, stderr []byte, code int) {
	to := h.Timeout
	if to <= 0 {
		to = defaultTimeout(ev)
	}
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(to)*time.Second+5*time.Second)
	defer cancel()
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}
	cmd := exec.CommandContext(ctx, shell, "-c", h.Command)
	cmd.Dir = cwd
	cmd.Stdin = bytes.NewReader(stdin)
	var ob, eb cappedBuf
	ob.cap, eb.cap = 64*1024, 64*1024
	cmd.Stdout = &ob
	cmd.Stderr = &eb
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
	cmd.Env = append(kept, "CLAUDE_PROJECT_DIR="+cwd)
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

var _ = fmt.Sprint

// runHTTPHook POSTs the hook payload to url and parses the JSON decision.
func runHTTPHook(h Command, stdin []byte) *Output {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(firstInt(h.Timeout, 30))*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", h.URL, bytes.NewReader(stdin))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range h.Headers {
		req.Header.Set(k, os.ExpandEnv(v))
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
