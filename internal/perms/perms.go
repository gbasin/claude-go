// Package perms implements Claude Code's permission model:
// rules like "Bash(npm run *)", "Read(./src/**)", "Edit", plus modes.
package perms

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

type Mode string

const (
	ModeDefault    Mode = "default"
	ModeAcceptEdit Mode = "acceptEdits"
	ModePlan       Mode = "plan"
	ModeBypass     Mode = "bypassPermissions"
	ModeAuto       Mode = "auto"
)

type Decision int

const (
	Ask Decision = iota
	Allow
	Deny
)

// Rules are the merged allow/deny/ask lists from settings.
type Rules struct {
	Allow []string
	Deny  []string
	Ask   []string
}

// Check evaluates a tool call against mode + rules.
func Check(mode Mode, rules *Rules, toolName string, input json.RawMessage, cwd string) Decision {
	if mode == ModeBypass {
		return Allow
	}
	// plan mode: only read-only tools
	if mode == ModePlan {
		if readOnly(toolName) {
			return Allow
		}
		return Deny
	}
	spec := toolSpec(toolName, input, cwd)
	for _, r := range deny(rules) {
		if matchRule(r, toolName, spec) {
			return Deny
		}
	}
	for _, r := range allow(rules) {
		if matchRule(r, toolName, spec) {
			return Allow
		}
	}
	for _, r := range askList(rules) {
		if matchRule(r, toolName, spec) {
			return Ask
		}
	}
	if mode == ModeAcceptEdit && isEditTool(toolName) {
		return Allow
	}
	if mode == ModeAuto {
		// "auto" relies on the server-side dangerous_tool_use classifier;
		// locally treat read-only as safe and ask for the rest.
		if readOnly(toolName) {
			return Allow
		}
		return Ask
	}
	return Ask // default mode asks for anything not allowed
}

func deny(r *Rules) []string {
	if r == nil {
		return nil
	}
	return r.Deny
}
func allow(r *Rules) []string {
	if r == nil {
		return nil
	}
	return r.Allow
}
func askList(r *Rules) []string {
	if r == nil {
		return nil
	}
	return r.Ask
}

func readOnly(name string) bool {
	switch name {
	case "Read", "Glob", "Grep", "LS", "WebFetch", "WebSearch",
		"ListAgents", "ListMcpResourcesTool", "ReadMcpResourceTool",
		"ReadMcpResourceDirTool", "LSP", "Skill":
		return true
	}
	return false
}

func isEditTool(name string) bool {
	switch name {
	case "Edit", "Write", "NotebookEdit":
		return true
	}
	return false
}

// toolSpec extracts the rule-relevant specifier: bash command, file path, url.
func toolSpec(name string, input json.RawMessage, cwd string) string {
	var m map[string]any
	json.Unmarshal(input, &m)
	s := func(k string) string {
		if v, ok := m[k]; ok {
			if str, ok := v.(string); ok {
				return str
			}
		}
		return ""
	}
	switch name {
	case "Bash", "Monitor":
		return s("command")
	case "Read", "Edit", "Write", "NotebookEdit":
		p := s("file_path")
		if p == "" {
			p = s("notebook_path")
		}
		if p != "" && !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		return p
	case "WebFetch":
		return s("url")
	case "Agent":
		return s("subagent_type")
	}
	return ""
}

// matchRule matches "Tool" or "Tool(spec)" against the call.
// Bash rules use "prefix:*" (or "prefix *") wildcard prefix matching;
// file tools use glob matching on the path.
func matchRule(rule, toolName, spec string) bool {
	name, arg := rule, ""
	if i := strings.Index(rule, "("); i >= 0 && strings.HasSuffix(rule, ")") {
		name, arg = rule[:i], rule[i+1:len(rule)-1]
	}
	if name != toolName {
		return false
	}
	if arg == "" {
		return true // "Bash" allows all bash
	}
	switch toolName {
	case "Bash", "Monitor":
		return matchCmdPrefix(arg, spec)
	default:
		return matchGlob(arg, spec)
	}
}

// matchCmdPrefix implements Bash("git status:*") prefix rules.
func matchCmdPrefix(pat, cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if strings.HasSuffix(pat, ":*") {
		pref := strings.TrimSuffix(pat, ":*")
		return cmd == pref || strings.HasPrefix(cmd, pref+" ")
	}
	if strings.HasSuffix(pat, " *") {
		pref := strings.TrimSuffix(pat, " *")
		return cmd == pref || strings.HasPrefix(cmd, pref+" ")
	}
	if strings.HasSuffix(pat, "*") {
		return strings.HasPrefix(cmd, strings.TrimSuffix(pat, "*"))
	}
	return cmd == pat
}

// matchGlob does filepath-style globbing, treating "**" as recursive.
func matchGlob(pat, path string) bool {
	if pat == path {
		return true
	}
	if strings.Contains(pat, "**") {
		parts := strings.Split(pat, "**")
		if len(parts) == 2 {
			pref, suff := parts[0], parts[1]
			return strings.HasPrefix(path, pref) && strings.HasSuffix(path, suff)
		}
	}
	ok, err := filepath.Match(pat, path)
	return err == nil && ok
}
