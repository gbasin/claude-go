package tools

import (
	"bufio"
	"claude-go/internal/config"
	"claude-go/internal/hooks"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"claude-go/internal/api"
	"claude-go/internal/plugins"
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
	Effort          string `json:"effort"`
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
	sub := c
	if in.Isolation == "worktree" {
		// run the agent inside a fresh git worktree (real isolation:"worktree")
		wr := execEnterWorktree(c, json.RawMessage(`{}`))
		if wr.IsError {
			return wr
		}
		sub = c.cloneForWorktree(worktreeSession.wtPath)
	}
	if xr, ok := c.Agent.(AgentRunnerTriState); ok {
		return xr.RunAgentTriState(sub, in.Description, in.Prompt, st, in.Model, in.Effort, in.RunInBackground, bg)
	}
	return c.Agent.RunAgent(sub, in.Description, in.Prompt, st, in.Model, in.Effort, bg)
}

// ---------- Skill ----------

type skillIn struct {
	Skill string `json:"skill"`
	Args  string `json:"args"`
}

// skillDirs mirrors the real skill-search roots: user, agents-compat,
// project, plugin skill dirs.
func agentsSkillsDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".agents", "skills")
	}
	return ""
}

func skillDirs(cwd string) []string {
	dirs := []string{
		filepath.Join(config.ClaudeDir(), "skills"),
		agentsSkillsDir(),
		filepath.Join(cwd, ".claude", "skills"),
		filepath.Join(cwd, ".agents", "skills"),
	}
	for _, pat := range []string{
		filepath.Join(config.ClaudeDir(), "plugins", "cache", "*", "*", "*", "skills"),
		filepath.Join(config.ClaudeDir(), "plugins", "local", "*", "skills"),
	} {
		dirs = append(dirs, globAll(pat)...)
	}
	for _, pd := range plugins.EnabledPluginDirs() {
		dirs = append(dirs, filepath.Join(pd, "skills"))
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
	truthy := func(v string) bool {
		return v == "true" || v == "yes" || v == "1" || v == "on"
	}
	// disableModelInvocation: the body is user-facing text, not a prompt —
	// return it without ever invoking the model.
	if truthy(fm["disableModelInvocation"]) || truthy(fm["disable-model-invocation"]) {
		return Result{Content: hdr + "\n" + body}
	}
	// context: fork or agent: <name> → run the skill inside a subagent;
	// only the agent's result (or, for background, the spawned agent's
	// name) comes back.
	agentType := fm["agent"]
	fork := fm["context"] == "fork"
	if agentType != "" || fork {
		if c.Agent == nil {
			return Result{Content: "skill requires a subagent but sub-agents are not available", IsError: true}
		}
		if agentType == "" {
			agentType = "fork" // context: fork inherits the parent conversation
		}
		prompt := body
		if in.Args != "" {
			prompt += "\n\nArguments: " + in.Args
		}
		desc := "skill: " + display
		allowed := parseCSVList(firstNonEmpty(fm["allowed-tools"], fm["allowedTools"], fm["tools"]))
		background := truthy(fm["background"])
		var res Result
		if xr, ok := c.Agent.(interface {
			RunAgentAllowed(*Context, string, string, string, string, string, bool, []string) Result
		}); ok {
			res = xr.RunAgentAllowed(c, desc, prompt, agentType, fm["model"], fm["effort"], background, allowed)
		} else {
			res = c.Agent.RunAgent(c, desc, prompt, agentType, fm["model"], fm["effort"], background)
		}
		if res.IsError {
			return res
		}
		if background {
			// the spawned agent's name is the whole result — its output
			// arrives later as a task notification
			return Result{Content: res.Content}
		}
		return Result{Content: fmt.Sprintf(
			"Skill %q completed (forked execution).\n\nResult:\n%s", display, res.Content)}
	}
	return Result{Content: hdr + "\n" + body}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// parseCSVList parses "a, b, c" or "[a, b]" into a string slice.
func parseCSVList(s string) []string {
	s = strings.TrimSpace(strings.Trim(s, "[]"))
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
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

	// real's WorktreeCreate hook can provision the worktree itself —
	// hook stdout is the absolute path, which we then adopt
	if c.FireHook != nil {
		if h := c.FireHook(hooks.WorktreeCreate, map[string]any{"name": name}); h.Block {
			return Result{Content: "WorktreeCreate hook blocked: " + h.Reason, IsError: true}
		} else if p := strings.TrimSpace(h.PlainText); p != "" && filepath.IsAbs(p) {
			c.CWD = p
			worktreeSession.wtPath = p
			worktreeSession.active = true
			return Result{Content: "Entered worktree provisioned by WorktreeCreate hook at " + p}
		}
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

// dottedSetting walks a dotted key ("tasks.enabled") in a settings map.
func dottedSetting(m map[string]any, key string) any {
	v := any(m)
	for _, k := range strings.Split(key, ".") {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[k]
	}
	return v
}

// settingBool resolves a dotted settings key as a bool (real's
// tasks.enabled et al are booleans, not strings). Project tier wins.
func settingBool(cwd, key string) bool {
	for _, p := range []string{
		filepath.Join(cwd, ".claude", "settings.json"),
		filepath.Join(config.ClaudeDir(), "settings.json"),
	} {
		if !config.SettingSourceOK(cwd, p) {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		v := any(m)
		for _, k := range strings.Split(key, ".") {
			mm, ok := v.(map[string]any)
			if !ok {
				v = nil
				break
			}
			v = mm[k]
		}
		if bv, ok := v.(bool); ok {
			return bv
		}
	}
	return false
}

func settingStr(cwd, key string) string {
	for _, p := range []string{
		filepath.Join(cwd, ".claude", "settings.json"),
		filepath.Join(config.ClaudeDir(), "settings.json"),
	} {
		if !config.SettingSourceOK(cwd, p) {
			continue
		}
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
		Action         string `json:"action"` // keep|remove — required
		DiscardChanges bool   `json:"discard_changes"`
	}
	json.Unmarshal(raw, &in)
	if in.Action != "keep" && in.Action != "remove" {
		return Result{Content: "action is required (keep|remove)", IsError: true}
	}
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
		// real's WorktreeRemove hook — non-blocking, stderr shown to user
		if c.FireHook != nil {
			if h := c.FireHook(hooks.WorktreeRemove, map[string]any{"worktree_path": wtPath}); h.NonBlockErr != "" {
				return Result{Content: "worktree remove hook error: " + h.NonBlockErr, IsError: true}
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
	Persistent  bool   `json:"persistent"`
	Ws          string `json:"ws"` // ws:// endpoint — stream frames as events
}

// Monitor: stream stdout lines as <task-notification> events (200ms batching,
// real semantics); exit/expiry also notify. Persistent monitors don't auto-stop.
func execMonitor(c *Context, raw json.RawMessage) Result {
	var in monitorIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	if in.Command == "" && in.Ws == "" {
		return Result{Content: "command or ws is required", IsError: true}
	}
	timeout := in.TimeoutMs
	if timeout <= 0 {
		timeout = 300_000 // real default 5min
	}
	if timeout > 1_800_000 {
		timeout = 1_800_000 // real cap 30min local
	}
	if c.BgTasks == nil {
		c.BgTasks = NewBackground()
	}
	taskID := fmt.Sprintf("m%d", c.BgTasks.seq.Add(1))
	t := &BgTask{ID: taskID, Command: func() string {
		if in.Command != "" {
			return in.Command
		}
		return "ws " + in.Ws
	}(), Started: time.Now(), Done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Millisecond)
	t.cancel = cancel
	c.BgTasks.tasks[taskID] = t
	delivered := 0

	if in.Ws != "" {
		go monitorWS(ctx, t, in.Ws, &delivered)
		return Result{Content: fmt.Sprintf("Monitor %s armed on ws://%s (timeout %ds).", taskID, in.Ws, timeout/1000)}
	}

	cmd, warn, err := c.shellCmd(ctx, in.Command, false)
	if err != nil {
		cancel()
		delete(c.BgTasks.tasks, taskID)
		return Result{Content: err.Error(), IsError: true}
	}
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = &t.Out
	if err := cmd.Start(); err != nil {
		cancel()
		delete(c.BgTasks.tasks, taskID)
		return Result{Content: "Error: " + err.Error(), IsError: true}
	}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var pending []string
		var lastFlush = time.Now()
		for sc.Scan() {
			pending = append(pending, sc.Text())
			delivered++
			if time.Since(lastFlush) >= 200*time.Millisecond && len(pending) > 0 {
				queuePrompt("<task-notification>Monitor " + taskID + " — " +
					strings.Join(pending, "\n") + "</task-notification>")
				pending = pending[:0]
				lastFlush = time.Now()
			}
			// flood guard — real auto-stops high-rate monitors
			if delivered > 500 {
				queuePrompt("<task-notification>Monitor " + taskID +
					" auto-stopped after 500 events (flood guard).</task-notification>")
				cancel()
				break
			}
		}
		if len(pending) > 0 {
			queuePrompt("<task-notification>Monitor " + taskID + " — " +
				strings.Join(pending, "\n") + "</task-notification>")
		}
		err := cmd.Wait()
		if !in.Persistent {
			var tail string
			if ctx.Err() == context.DeadlineExceeded {
				if delivered > 0 {
					tail = fmt.Sprintf("[Monitor expired after %ds with %d events delivered. Re-arm it if you still need the watch.]", timeout/1000, delivered)
				} else {
					tail = fmt.Sprintf("[Monitor expired after %ds with no events delivered. Re-arm it or widen the filter.]", timeout/1000)
				}
			} else {
				code := 0
				if ee, ok := err.(*exec.ExitError); ok {
					code = ee.ExitCode()
				}
				tail = fmt.Sprintf("[Monitor %s exited code %d, %d events delivered]", taskID, code, delivered)
			}
			queuePrompt("<task-notification>" + tail + "</task-notification>")
		}
		t.Err = err
		close(t.Done)
	}()
	_ = warn
	return Result{Content: fmt.Sprintf("Monitor %s armed: %s (timeout %ds, %s).", taskID, in.Command, timeout/1000, map[bool]string{true: "persistent", false: "one-shot"}[in.Persistent])}
}

// monitorWS: minimal RFC6455 text-frame reader (no deps). Each text frame
// becomes a task-notification; binary frames report size only.
func monitorWS(ctx context.Context, t *BgTask, url string, delivered *int) {
	conn, err := wsDial(url)
	if err != nil {
		queuePrompt("<task-notification>Monitor " + t.ID + " ws connect failed: " + err.Error() + "</task-notification>")
		close(t.Done)
		return
	}
	defer conn.Close()
	go func() { <-ctx.Done(); conn.Close() }()
	for {
		op, payload, err := wsRead(conn)
		if err != nil {
			if ctx.Err() == nil {
				queuePrompt("<task-notification>Monitor " + t.ID + " ws closed: " + err.Error() + "</task-notification>")
			}
			close(t.Done)
			return
		}
		if op == 1 {
			queuePrompt("<task-notification>Monitor " + t.ID + " — " + string(payload) + "</task-notification>")
			*delivered++
		} else if op == 2 {
			queuePrompt(fmt.Sprintf("<task-notification>Monitor %s — [binary frame, %d bytes]</task-notification>", t.ID, len(payload)))
			*delivered++
		}
	}
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

// cloneForWorktree returns a Context for a worktree sub-agent: pointer
// fields shared, mutex-protected state fresh (a copied sync.Mutex is a
// vet error and a copied readFiles map would share the parent's locks
// wrongly — the worktree tracks its own read-before-edit set).
func (c *Context) cloneForWorktree(wtPath string) *Context {
	return &Context{
		Ctx:           c.Ctx,
		CWD:           wtPath,
		BgTasks:       c.BgTasks,
		Agent:         c.Agent,
		AskUser:       c.AskUser,
		Permission:    c.Permission,
		Answerer:      c.Answerer,
		Searcher:      c.Searcher,
		AskStructured: c.AskStructured,
		SetPermMode:   c.SetPermMode,
		PlanApprove:   c.PlanApprove,
		EndSession:    c.EndSession,
		Session:       c.Session,
		OAuthToken:    c.OAuthToken,
		OnCwdChanged:  c.OnCwdChanged,
		OnFileChanged: c.OnFileChanged,
	}
}

// SubagentHandback delivers the spawned agent's final report — real's
// subagent-only tool; calling it ends the subagent's run (the message
// becomes the Agent tool's result). Hidden from the main registry's
// request surface — subagent ToolCtx carries it.
func init() {
	Register(&Tool{
		IsHidden: true,
		Def: api.Tool{
			Name:        "SubagentHandback",
			Description: "***REMOVED***",
			InputSchema: json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"message":{"description":"Your full report for your caller","type":"string"}},"required":["message"],"additionalProperties":false}`),
		},
		Exec: func(c *Context, input json.RawMessage) Result {
			var in struct {
				Message string `json:"message"`
			}
			json.Unmarshal(input, &in)
			// the runner sees the sentinel and ends the run
			return Result{Content: in.Message, Handback: true}
		},
	})
}
