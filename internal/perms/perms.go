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
	ModeDontAsk    Mode = "dontAsk" // noninteractive: would-ask -> deny
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
	// Bash commands are split into subcommands — each is checked.
	specs := []string{spec}
	if toolName == "Bash" {
		specs = splitShell(spec)
	}
	d := func(check func(string) Decision) Decision {
		worst := Allow
		for _, s := range specs {
			if d := check(s); d != Allow {
				return d
			}
			_ = worst
		}
		return Allow
	}
	// order: deny > ask > allow
	if dd := d(func(s string) Decision {
		for _, r := range deny(rules) {
			if matchRule(r, toolName, s) {
				return Deny
			}
		}
		return Allow
	}); dd == Deny {
		return Deny
	}
	if dd := d(func(s string) Decision {
		for _, r := range askList(rules) {
			if matchRule(r, toolName, s) {
				return Ask
			}
		}
		return Allow
	}); dd == Ask {
		return Ask
	}
	for _, s := range specs {
		allAllowed := true
		matched := false
		for _, r := range allow(rules) {
			if matchRule(r, toolName, s) {
				matched = true
				break
			}
		}
		if !matched {
			allAllowed = false
		}
		if allAllowed && len(specs) == len(specs) {
			continue
		}
	}
	if allSpecsAllowed(rules, toolName, specs) {
		return Allow
	}
	if mode == ModeAcceptEdit && isEditTool(toolName) {
		return Allow
	}
	// file tools writing outside the workspace (cwd + additionalDirectories)
	// always require an ask — real CC treats this as a separate dimension.
	if isEditTool(toolName) && !fileInsideWorkspace(input, cwd) {
		return Ask
	}
	if mode == ModeAuto {
		// "auto" defers to the server-side dangerous_tool_use classifier
		// (sent in the request safeguards block). Locally, only explicit
		// rules restrict; everything else proceeds.
		return Allow
	}
	if mode == ModeDontAsk {
		return Deny
	}
	return Ask // default mode asks for anything not allowed
}

// allSpecsAllowed: every subcommand must be covered by an allow rule.
func allSpecsAllowed(rules *Rules, toolName string, specs []string) bool {
	for _, s := range specs {
		ok := false
		for _, r := range allow(rules) {
			if matchRule(r, toolName, s) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return len(specs) > 0
}

// splitShell splits a command line on &&, ||, ;, | (naive, quote-aware).
func splitShell(cmd string) []string {
	var parts []string
	var cur strings.Builder
	var quote rune
	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			cur.WriteRune(c)
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			quote = c
			cur.WriteRune(c)
			continue
		}
		if c == ';' || c == '|' || (c == '&' && i+1 < len(rs) && rs[i+1] == '&') {
			if t := strings.TrimSpace(cur.String()); t != "" {
				parts = append(parts, t)
			}
			cur.Reset()
			if c == '&' {
				i++
			} else if c == '|' && i+1 < len(rs) && rs[i+1] == '|' {
				i++
			}
			continue
		}
		cur.WriteRune(c)
	}
	if t := strings.TrimSpace(cur.String()); t != "" {
		parts = append(parts, t)
	}
	if len(parts) == 0 {
		return []string{cmd}
	}
	return parts
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
		// mcp__server rules match every tool on that server
		if strings.HasPrefix(toolName, name+"__") {
			return true
		}
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

// AdditionalDirs is set once from settings permissions.additionalDirectories.
var AdditionalDirs []string

func fileInsideWorkspace(input json.RawMessage, cwd string) bool {
	var in struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
	}
	if json.Unmarshal(input, &in) != nil {
		return true // unparseable input: let the tool report the error
	}
	p := in.FilePath
	if p == "" {
		p = in.Path
	}
	if p == "" {
		return true
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	abs = filepath.Clean(abs)
	for _, root := range append([]string{filepath.Clean(cwd)}, AdditionalDirs...) {
		if abs == root || strings.HasPrefix(abs, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
