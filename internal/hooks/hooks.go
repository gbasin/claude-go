// Package hooks executes Claude Code settings.json hooks.
// Shape: {"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"..."}]}],...}}
package hooks

import (
	"bytes"
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
	PreToolUse       Event = "PreToolUse"
	PostToolUse      Event = "PostToolUse"
	UserPromptSubmit Event = "UserPromptSubmit"
	SessionStart     Event = "SessionStart"
	SessionEnd       Event = "SessionEnd"
	Notification     Event = "Notification"
	Stop             Event = "Stop"
	SubagentStop     Event = "SubagentStop"
	PreCompact       Event = "PreCompact"
)

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
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(home, ".claude", "settings.json"),
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
	Continue           bool   `json:"continue"`
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
	toolName, _ := payload["tool_name"].(string)
	stdin, _ := json.Marshal(payload)
	for _, m := range matchers {
		if m.Matcher != "" && m.Matcher != "*" && toolName != "" {
			ok, err := regexp.MatchString("^("+m.Matcher+")$", toolName)
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
				if o.Decision == "block" || (!o.Continue) {
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
			} else if code != 0 {
				// non-zero exit = block with stderr as reason
				res.Block = true
				res.Reason = string(bytes.TrimSpace(out))
			}
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
