// Package perms implements Claude Code's permission model:
// rules like "Bash(npm run *)", "Read(./src/**)", "Edit", plus modes.
package perms

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"

	"claude-go/internal/config"
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
	// plan mode: only read-only tools — unless useAutoModeDuringPlan
	// (default true) hands non-read-only calls to the auto-eval lane,
	// which decides allow/ask/deny like auto mode
	if mode == ModePlan {
		if readOnly(toolName) {
			return Allow
		}
		if autoModeDuringPlan() {
			return Ask
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
			if matchRuleAt(r, toolName, s, cwd) {
				return Deny
			}
		}
		return Allow
	}); dd == Deny {
		return Deny
	}
	if dd := d(func(s string) Decision {
		for _, r := range askList(rules) {
			if matchRuleAt(r, toolName, s, cwd) {
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
			if matchRuleAt(r, toolName, s, cwd) {
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
	if allSpecsAllowed(rules, toolName, specs, cwd) {
		return Allow
	}
	// read-only tools never prompt in any mode (deny/ask rules already ran
	// above — a user can still explicitly deny Glob)
	if readOnly(toolName) {
		return Allow
	}
	// file tools writing outside the workspace (cwd + additionalDirectories)
	// always require an ask — real's acceptEdits only auto-allows
	// IN-workspace edits ("acceptEdits already auto-allows in-workspace
	// edits" — mod_0400 rule warning).
	if isEditTool(toolName) && !fileInsideWorkspace(input, cwd) {
		return Ask
	}
	if mode == ModeAcceptEdit && isEditTool(toolName) {
		return Allow
	}
	if mode == ModeAuto {
		// "auto" auto-allows read-only tools and routes everything
		// else to the security-monitor eval — Ask so the loop's
		// SafetyEval decides (real's dangerous_tool_use lane).
		if readOnly(toolName) {
			return Allow
		}
		return Ask
	}
	if mode == ModeDontAsk {
		return Deny
	}
	return Ask // default mode asks for anything not allowed
}

// allSpecsAllowed: every subcommand must be covered by an allow rule.
func allSpecsAllowed(rules *Rules, toolName string, specs []string, cwd string) bool {
	for _, s := range specs {
		ok := false
		for _, r := range allow(rules) {
			if matchRuleAt(r, toolName, s, cwd) {
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
		"ReadMcpResourceDirTool", "LSP", "Skill", "ToolSearch",
		"AskUserQuestion":
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
	case "WebSearch":
		return s("query")
	case "Agent":
		return s("subagent_type")
	}
	return ""
}

// matchRule matches "Tool" or "Tool(spec)" against the call — real's
// parsePermissionRule union: exact / prefix (":*") / wildcard ("*").
func matchRule(rule, toolName, spec string) bool {
	return matchRuleAt(rule, toolName, spec, "")
}

// matchRuleAt additionally tries the spec relativized to cwd — real
// groups file patterns by root (the dir the rule was written in).
func matchRuleAt(rule, toolName, spec, cwd string) bool {
	name, arg := rule, ""
	hasArg := false
	if i := strings.Index(rule, "("); i >= 0 && strings.HasSuffix(rule, ")") {
		name, arg = rule[:i], rule[i+1:len(rule)-1]
		hasArg = true
	}
	if name != toolName {
		// bare mcp__server matches every tool on that server; an arg
		// narrows to the specific tool (mcp__github(get_issue))
		if strings.HasPrefix(toolName, name+"__") {
			if !hasArg {
				return true
			}
			return toolName == name+"__"+arg
		}
		return false
	}
	if !hasArg {
		return true // bare "Bash" allows all bash
	}
	switch toolName {
	case "Bash", "Monitor":
		return matchCmdPrefix(arg, spec)
	case "WebFetch", "WebSearch":
		// WebFetch(domain:x) is a host rule (real: mod_1687.js)
		if strings.HasPrefix(arg, "domain:") {
			host := arg[len("domain:"):]
			u := spec
			if i := strings.Index(u, "://"); i >= 0 {
				u = u[i+3:]
			}
			if i := strings.IndexAny(u, "/?#"); i >= 0 {
				u = u[:i]
			}
			return u == host || strings.HasSuffix(u, "."+host)
		}
		return matchGlob(arg, spec)
	default:
		if matchGlob(arg, spec) {
			return true
		}
		if cwd != "" {
			if rel, err := filepath.Rel(cwd, spec); err == nil && !strings.HasPrefix(rel, "..") {
				return matchGlob(arg, rel)
			}
		}
		return false
	}
}

// matchCmdPrefix — real's Bash rule semantics (mod_0056.js):
//
//	"cmd:*"    prefix rule — matches `cmd` or `cmd args` (word boundary)
//	"cmd *"    wildcard — the trailing " *" special-case also matches bare `cmd`
//	interior * wildcard regex (* → .*, \* escaped)
//	"Bash()"   exact "" — matches NOTHING
func matchCmdPrefix(pat, cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	pat = strings.TrimRight(pat, " ")
	if pat == "" {
		return false // Bash() — exact-empty, matches nothing
	}
	// ":*" prefix — word boundary only
	if strings.HasSuffix(pat, ":*") {
		pref := strings.TrimSuffix(pat, ":*")
		return cmd == pref || strings.HasPrefix(cmd, pref+" ")
	}
	// wildcard: any unescaped * → regex; trailing " *" becomes optional
	re := wildcardRegex(pat)
	return re.MatchString(cmd)
}

// wildcardRegex converts real's pattern syntax to an anchored regex:
// `*` → `.*`, `**` → `.*` (same for commands), `\*` → literal *,
// `\\` → literal backslash; a trailing " *" → "( .*)?" so
// `Bash(git push *)` also allows bare `git push`.
func wildcardRegex(pat string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	trail := false
	for i := 0; i < len(pat); i++ {
		ch := pat[i]
		if ch == '\\' && i+1 < len(pat) && (pat[i+1] == '*' || pat[i+1] == '\\') {
			b.WriteString(regexp.QuoteMeta(string(pat[i+1])))
			i++
			continue
		}
		if ch == ' ' && i+1 < len(pat) && pat[i+1] == '*' && i+2 == len(pat) {
			// trailing " *" → optional arg segment
			b.WriteString("( .*)?")
			trail = true
			break
		}
		if ch == '*' {
			b.WriteString(".*")
			continue
		}
		b.WriteString(regexp.QuoteMeta(string(ch)))
	}
	_ = trail
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return regexp.MustCompile("^$") // bad pattern matches nothing
	}
	return re
}

// matchGlob — gitignore semantics for file-path rules (real uses the
// ignore package): `dir/**` matches dir itself + all descendants,
// `**/*.env` matches at any depth, `*` doesn't cross /.
func matchGlob(pat, path string) bool {
	if pat == path {
		return true
	}
	return gitignoreMatch(pat, path)
}

func gitignoreMatch(pat, path string) bool {
	// trailing "/**" also matches the base directory itself
	if strings.HasSuffix(pat, "/**") {
		base := strings.TrimSuffix(pat, "/**")
		if path == base || strings.HasPrefix(path, base+"/") {
			return true
		}
	}
	// pattern → regex: **/ → (?:.*/)? , ** → .* , * → [^/]* , ? → [^/]
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pat); i++ {
		if pat[i] == '*' && i+1 < len(pat) && pat[i+1] == '*' {
			if i+2 < len(pat) && pat[i+2] == '/' {
				b.WriteString("(?:.*/)?")
				i += 2
			} else {
				b.WriteString(".*")
				i++
			}
			continue
		}
		if pat[i] == '*' {
			b.WriteString("[^/]*")
			continue
		}
		if pat[i] == '?' {
			b.WriteString("[^/]")
			continue
		}
		b.WriteString(regexp.QuoteMeta(string(pat[i])))
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	return err == nil && re.MatchString(path)
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

// autoModeDuringPlan honors settings.useAutoModeDuringPlan (real default
// true — plan mode uses auto semantics when auto mode is available).
func autoModeDuringPlan() bool {
	if v, ok := config.UserSetting("useAutoModeDuringPlan").(bool); ok {
		return v
	}
	return true
}
