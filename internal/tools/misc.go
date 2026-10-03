package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func init() {
	Register(&Tool{Def: loadSchema("Agent"), Exec: execAgent})
	Register(&Tool{Def: loadSchema("Skill"), Exec: execSkill})
	Register(&Tool{Def: loadSchema("EnterWorktree"), Exec: execEnterWorktree})
	Register(&Tool{Def: loadSchema("ExitWorktree"), Exec: execExitWorktree})
	Register(&Tool{Def: loadSchema("Monitor"), Exec: execMonitor})

	// Stubs: return a faithful "unavailable in this environment" result
	// rather than failing, matching how CC degrades without the service.
	for _, n := range []string{
		"DesignSync",
	} {
		n := n
		Register(&Tool{Def: loadSchema(n), Exec: func(c *Context, raw json.RawMessage) Result {
			return Result{Content: fmt.Sprintf("%s is not available in this build of claude-go.", n), IsError: true}
		}})
	}
}

// ---------- Agent ----------

type agentIn struct {
	Description     string `json:"description"`
	Prompt          string `json:"prompt"`
	SubagentType    string `json:"subagent_type"`
	Model           string `json:"model"`
	RunInBackground *bool  `json:"run_in_background"`
	Isolation       string `json:"isolation"`
}

func execAgent(c *Context, raw json.RawMessage) Result {
	var in agentIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	if c.Agent == nil {
		return Result{Content: "sub-agents are not available", IsError: true}
	}
	st := in.SubagentType
	if st == "" {
		st = "general-purpose"
	}
	bg := true // real default: background unless explicitly false
	if in.RunInBackground != nil {
		bg = *in.RunInBackground
	}
	return c.Agent.RunAgent(c, in.Description, in.Prompt, st, in.Model, bg)
}

// ---------- Skill ----------

type skillIn struct {
	Skill string `json:"skill"`
	Args  string `json:"args"`
}

// skillDirs mirrors the real skill-search roots: user, agents-compat,
// project, plugin skill dirs.
func skillDirs(cwd string) []string {
	home, _ := os.UserHomeDir()
	dirs := []string{
		filepath.Join(home, ".claude", "skills"),
		filepath.Join(home, ".agents", "skills"),
		filepath.Join(cwd, ".claude", "skills"),
		filepath.Join(cwd, ".agents", "skills"),
	}
	for _, pat := range []string{
		filepath.Join(home, ".claude", "plugins", "synced", "*", "*", "skills"),
		filepath.Join(home, ".claude", "plugins", "local", "*", "skills"),
	} {
		dirs = append(dirs, globAll(pat)...)
	}
	return dirs
}

func globAll(pat string) []string {
	m, _ := filepath.Glob(pat)
	return m
}

// resolveSkill supports `name`, `plugin:skill`, and scoped `dir:name` forms.
func resolveSkill(c *Context, name string) (path, plugin string) {
	if i := strings.LastIndex(name, ":"); i > 0 {
		plugin = name[:i]
		name = name[i+1:]
	}
	for _, base := range skillDirs(c.CWD) {
		if plugin != "" && !strings.Contains(base, plugin) {
			continue
		}
		p := filepath.Join(base, name, "SKILL.md")
		if _, err := os.Stat(p); err == nil {
			return p, plugin
		}
	}
	return "", plugin
}

func execSkill(c *Context, raw json.RawMessage) Result {
	var in skillIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	p, plugin := resolveSkill(c, in.Skill)
	if p == "" {
		return Result{Content: "Unknown skill: " + in.Skill, IsError: true}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return Result{Content: "Error reading skill: " + err.Error(), IsError: true}
	}
	fm, body := parseMDFrontmatter(string(b))
	// ${CLAUDE_*} substitution like the real skill engine
	sid := ""
	if c.Session != "" {
		sid = c.Session
	}
	vars := map[string]string{
		"${CLAUDE_SKILL_DIR}":   filepath.Dir(p),
		"${CLAUDE_PROJECT_DIR}": c.CWD,
		"${CLAUDE_SESSION_ID}":  sid,
		"${CLAUDE_EFFORT}":      "medium",
		"$ARGUMENTS":            in.Args,
	}
	for k, v := range vars {
		body = strings.ReplaceAll(body, k, v)
	}
	args := in.Args
	if args == "" {
		args = "(none)"
	}
	display := in.Skill
	if plugin != "" {
		display = plugin + ":" + filepath.Base(filepath.Dir(p))
	}
	hdr := fmt.Sprintf("Skill %q loaded from %s\nUser args: %s\n", display, p, args)
	if d := fm["description"]; d != "" {
		hdr = fmt.Sprintf("Skill %q (%s) loaded\nUser args: %s\n", display, d, args)
	}
	return Result{Content: hdr + "\n" + body}
}

func parseMDFrontmatter(s string) (map[string]string, string) {
	fm := map[string]string{}
	if !strings.HasPrefix(s, "---") {
		return fm, s
	}
	rest := s[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return fm, s
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok {
			fm[strings.TrimSpace(k)] = strings.TrimSpace(strings.Trim(v, `"'`))
		}
	}
	return fm, strings.TrimSpace(rest[end+4:])
}

// ---------- worktrees ----------

func gitRoot(cwd string) (string, error) {
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// worktreeSession tracks the pre-entry state for ExitWorktree.
var worktreeSession = struct {
	mu       sync.Mutex
	active   bool
	origCWD  string
	wtPath   string
	wtBranch string
}{}

func execEnterWorktree(c *Context, raw json.RawMessage) Result {
	var in struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	json.Unmarshal(raw, &in)
	root, err := gitRoot(c.CWD)
	if err != nil {
		return Result{Content: "not inside a git worktree", IsError: true}
	}
	name := in.Name
	if name == "" && in.Path == "" {
		name = "claude-" + fmt.Sprint(time.Now().Unix()%100000)
	}
	// name charset/length validation like real
	if name != "" {
		if len(name) > 64 {
			return Result{Content: "worktree name too long (max 64)", IsError: true}
		}
		for _, ch := range name {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
				return Result{Content: "worktree name may contain letters, digits, - and _ only", IsError: true}
			}
		}
	}
	worktreeSession.mu.Lock()
	worktreeSession.origCWD = c.CWD
	worktreeSession.mu.Unlock()

	if in.Path != "" {
		// adopt an existing worktree (must appear in git worktree list)
		out, _ := exec.Command("git", "-C", root, "worktree", "list", "--porcelain").Output()
		if !strings.Contains(string(out), "worktree "+in.Path) {
			return Result{Content: "path is not a git worktree of this repository: " + in.Path, IsError: true}
		}
		c.CWD = in.Path
		worktreeSession.wtPath = in.Path
		worktreeSession.active = true
		return Result{Content: "Entered existing worktree at " + in.Path}
	}

	// create inside <repo>/.claude/worktrees/ like real CC
	wtPath := filepath.Join(root, ".claude", "worktrees", name)
	// base ref: worktree.baseRef setting -> fresh uses origin/<default-branch>
	base := ""
	if ref := settingStr(c.CWD, "worktree.baseRef"); ref == "fresh" {
		def := strings.TrimSpace(string(func() []byte {
			b, _ := exec.Command("git", "-C", root, "symbolic-ref", "--short", "refs/remotes/origin/HEAD").Output()
			return b
		}()))
		if def != "" {
			base = strings.TrimPrefix(def, "origin/")
			exec.Command("git", "-C", root, "fetch", "origin", base).Run()
			base = "origin/" + base
		}
	}
	branch := "claude/" + name
	var cmd *exec.Cmd
	if base != "" {
		cmd = exec.Command("git", "-C", root, "worktree", "add", "-b", branch, wtPath, base)
	} else {
		cmd = exec.Command("git", "-C", root, "worktree", "add", "-b", branch, wtPath)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return Result{Content: "git worktree add failed: " + string(out), IsError: true}
	}
	worktreeSession.wtPath = wtPath
	worktreeSession.wtBranch = branch
	worktreeSession.active = true
	c.CWD = wtPath
	return Result{Content: fmt.Sprintf("Entered worktree at %s (branch %s)", wtPath, branch)}
}

func settingStr(cwd, key string) string {
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(cwd, ".claude", "settings.json"),
		filepath.Join(home, ".claude", "settings.json"),
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		parts := strings.Split(key, ".")
		v := any(m)
		for _, k := range parts {
			if mm, ok := v.(map[string]any); ok {
				v = mm[k]
			} else {
				v = nil
				break
			}
		}
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func execExitWorktree(c *Context, raw json.RawMessage) Result {
	var in struct {
		Action         string `json:"action"` // keep|remove
		DiscardChanges bool   `json:"discard_changes"`
	}
	json.Unmarshal(raw, &in)
	worktreeSession.mu.Lock()
	if !worktreeSession.active {
		worktreeSession.mu.Unlock()
		return Result{Content: "No active worktree session — EnterWorktree first", IsError: true}
	}
	origCWD := worktreeSession.origCWD
	wtPath := worktreeSession.wtPath
	wtBranch := worktreeSession.wtBranch
	worktreeSession.active = false
	worktreeSession.mu.Unlock()

	if in.Action == "remove" {
		if wtPath == "" {
			return Result{Content: "no created worktree to remove (entered via existing path)", IsError: true}
		}
		// refuse dirty removal unless discard_changes
		if !in.DiscardChanges {
			out, _ := exec.Command("git", "-C", wtPath, "status", "--porcelain").Output()
			if len(strings.TrimSpace(string(out))) > 0 {
				return Result{Content: "worktree has uncommitted changes — commit them, or retry with discard_changes:true", IsError: true}
			}
		}
		args := []string{"-C", origCWD, "worktree", "remove"}
		if in.DiscardChanges {
			args = append(args, "--force")
		}
		args = append(args, wtPath)
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			return Result{Content: "git worktree remove failed: " + string(out), IsError: true}
		}
		// delete the temp branch
		exec.Command("git", "-C", origCWD, "branch", "-D", wtBranch).Run()
		c.CWD = origCWD
		return Result{Content: "Worktree removed, restored to " + origCWD}
	}
	// keep: leave worktree, restore cwd
	c.CWD = origCWD
	return Result{Content: fmt.Sprintf("Exited worktree (kept at %s)", wtPath)}
}

// ---------- Monitor ----------

type monitorIn struct {
	Description string `json:"description"`
	TimeoutMs   int    `json:"timeout_ms"`
	Command     string `json:"command"`
}

func execMonitor(c *Context, raw json.RawMessage) Result {
	var in monitorIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	if in.Command == "" {
		return Result{Content: "command is required", IsError: true}
	}
	// Monitor = background command whose exit re-invokes the agent.
	in2, _ := json.Marshal(bashInput{
		Command:         in.Command,
		RunInBackground: true,
		Timeout:         float64(orZero(in.TimeoutMs, bgDefaultTimeout)),
	})
	return execBash(c, in2)
}

func orZero(v, d int) int {
	if v <= 0 {
		return d
	}
	return v
}

func monitorTimeout(ms int) time.Duration {
	if ms <= 0 {
		return 5 * time.Minute
	}
	if ms > 600000 {
		return 600000
	}
	return time.Duration(ms) * time.Millisecond
}
