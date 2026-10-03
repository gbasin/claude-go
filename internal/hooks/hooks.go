// Package hooks executes Claude Code settings.json hooks.
// Shape: {"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"..."}]}],...}}
package hooks

import (
	"bytes"
	"claude-go/internal/config"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// matcherField maps an event to the payload field its matcher regex applies to.
var matcherField = map[Event]string{
	PreToolUse: "tool_name", PostToolUse: "tool_name",
	PostToolUseFailure: "tool_name", PostToolBatch: "tool_name",
	PermissionDenied: "tool_name", PermissionRequest: "tool_name",
	UserPromptSubmit: "command_name", UserPromptExpansion: "command_name",
	SessionStart: "source", SessionEnd: "reason",
	PreCompact: "trigger", PostCompact: "trigger",
	PreModelSwitch: "to_model", PostModelSwitch: "to_model",
	Notification: "notification_type", StopFailure: "error",
	SubagentStart: "agent_type", SubagentStop: "agent_type",
	Setup: "trigger", TaskCreated: "task_type", TaskCompleted: "task_type",
	ConfigChange: "source", InstructionsLoaded: "load_reason",
	FileChanged: "file_path", CwdChanged: "cwd",
	DirectoryAdded: "source", Elicitation: "tool_name",
}

type Command struct {
	Type    string `json:"type"`    // "command" | "prompt"
	Command string `json:"command"` // for type=command
	Prompt  string `json:"prompt"`  // for type=prompt (LLM-judged)
	Timeout int    `json:"timeout"` // seconds
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
	out := Config{}
	for _, p := range []string{
		filepath.Join(config.ClaudeDir(), "settings.json"),
		filepath.Join(cwd, ".claude", "settings.json"),
		filepath.Join(cwd, ".claude", "settings.local.json"),
	} {
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
	return out
}

// Output is a hook's parsed JSON stdout.
type Output struct {
	Continue           string `json:"continue"` // "true"/"false" or absent
	Decision           string `json:"decision"` // "block" | "approve" | "allow"
	Reason             string `json:"reason"`
	SystemMessage      string `json:"systemMessage"`
	HookSpecificOutput *struct {
		HookEventName      string `json:"hookEventName"`
		PermissionDecision string `json:"permissionDecision"` // allow|deny|ask
		AdditionalContext  string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

type Result struct {
	Block             bool
	Reason            string
	AdditionalContext string
	SystemMessage     string
	Permission        string // allow|deny|ask|""
}

// Run executes all matching hooks for an event. Input is the JSON payload
// CC sends on stdin (session_id, transcript_path, cwd, tool_name, tool_input...).
func Run(c Config, ev Event, payload map[string]any, cwd string) Result {
	var res Result
	matchers := c[string(ev)]
	if len(matchers) == 0 {
		return res
	}
	field := matcherField[ev]
	if field == "" {
		field = "tool_name"
	}
	matchVal, _ := payload[field].(string)
	stdin, _ := json.Marshal(payload)
	for _, m := range matchers {
		if m.Matcher != "" && m.Matcher != "*" {
			// a matcher that doesn't bind to this event's field must not run
			// (matchVal "" means the field wasn't in the payload — never match)
			if matchVal == "" {
				continue
			}
			ok, err := regexp.MatchString("^("+m.Matcher+")$", matchVal)
			if err != nil || !ok {
				continue
			}
		}
		for _, h := range m.Hooks {
			if h.Type == "prompt" && Judge != nil {
				payloadStr := string(stdin)
				allow, reason := Judge(h.Prompt+"\n\nContext:\n"+payloadStr, "")
				if !allow {
					res.Block = true
					res.Reason = reason
				}
				continue
			}
			if h.Type != "command" {
				continue
			}
			out, code := runCmd(h, stdin, cwd)
			var o Output
			if json.Unmarshal(bytes.TrimSpace(out), &o) == nil {
				if o.Decision == "block" || o.Continue == "false" {
					res.Block = true
					res.Reason = firstNonEmpty(o.Reason, o.SystemMessage)
				}
				if o.HookSpecificOutput != nil {
					res.Permission = o.HookSpecificOutput.PermissionDecision
					res.AdditionalContext += o.HookSpecificOutput.AdditionalContext
				}
				if o.SystemMessage != "" {
					res.SystemMessage += o.SystemMessage
				}
			} else if code == 2 {
				// exit 2 = block; stderr (here merged) becomes the reason
				res.Block = true
				res.Reason = string(bytes.TrimSpace(out))
			}
			// other non-zero exits: stderr to user only, execution continues
		}
	}
	return res
}

func runCmd(h Command, stdin []byte, cwd string) ([]byte, int) {
	to := h.Timeout
	if to <= 0 {
		to = 60
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(to)*time.Second)
	defer cancel()
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}
	cmd := exec.CommandContext(ctx, shell, "-c", h.Command)
	cmd.Dir = cwd
	cmd.Stdin = bytes.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	return out.Bytes(), code
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
