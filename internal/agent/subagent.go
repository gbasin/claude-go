package agent

import (
	"claude-go/internal/config"
	"claude-go/internal/hooks"
	"claude-go/internal/session"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"claude-go/internal/api"
	"claude-go/internal/plugins"
	"claude-go/internal/tools"
)

// ---------- Agent type registry ----------

// AgentDef is a resolved agent-type definition. Sources: built-in types,
// agent markdown files (~/.claude/agents, <cwd>/.claude/agents, plugin
// agents dirs), and --agents JSON (CLAUDE_GO_AGENTS_JSON, source "cli").
type AgentDef struct {
	Name            string
	Description     string
	WhenToUse       string
	Tools           []string // nil or ["*"] = all tools
	DisallowedTools []string
	Model           string // "", or "inherit" for parent model
	Effort          string // "", or an effort level from frontmatter
	Color           string
	InitialPrompt   string // prefix for the subagent's first user message
	SystemPrompt    string // markdown body of the agent file
	Source          string // builtin|user|project|plugin|cli
	BaseDir         string
	// Unlisted keeps the type dispatchable but out of the env record's
	// "Available agent types" listing — real's fork agent is registered
	// for spawning but never appears in the listing (mod_0416 uOe).
	Unlisted bool
}

// readOnlyDeny is the tool set built-in read-only agents (Explore, Plan)
// may not use — matches real's "All tools except …" list.
var readOnlyDeny = []string{
	"Agent", "Artifact", "ArtifactComments", "ArtifactData", "ArtifactCheck",
	"ExitPlanMode", "Edit", "Write", "NotebookEdit",
}

var generalPurposePrompt = fallbackGeneralPurpose

// builtinAgentDefs are the default agent types (mod_0416 uOe, 294).
// Real registers: general-purpose, statusline-setup, the "claude"
// catch-all, Explore+Plan, web-fetch (flag-gated), and claude-code-guide
// (every entrypoint except the SDK family). The fork def stays
// dispatchable but unlisted.
var builtinAgentDefs = []AgentDef{
	{
		Name:   "general-purpose",
		Source: "builtin",
		Tools:  []string{"*"},
		Description: "General-purpose agent for researching complex questions, searching for code, " +
			"***REMOVED***" +
			"***REMOVED***" +
			"perform the search for you.",
		SystemPrompt: generalPurposePrompt,
	},
	{
		Name:            "Explore",
		Source:          "builtin",
		Model:           "inherit",
		DisallowedTools: readOnlyDeny,
		Description: "***REMOVED***" +
			"***REMOVED***" +
			"***REMOVED***" +
			"***REMOVED***" +
			"\"very thorough\" for multiple locations and naming conventions.",
		SystemPrompt: "You are a read-only search agent. Locate code and report conclusions — " +
			"never modify files, run mutating commands, or write anything.",
	},
	{
		Name:            "Plan",
		Source:          "builtin",
		Model:           "inherit",
		DisallowedTools: readOnlyDeny,
		Description: "Software architect agent for designing implementation plans. Use this when you " +
			"***REMOVED***" +
			"identifies critical files, and considers architectural trade-offs.",
		SystemPrompt: "You are a software architect agent. Explore the codebase read-only and " +
			"return a concrete, step-by-step implementation plan. Do not modify files.",
	},
	{
		Name:   "fork",
		Source: "builtin",
		Tools:  []string{"*"},
		Description: "Fork — inherits full conversation context. Selected explicitly via " +
			`subagent_type: "fork" when the fork gate is on; never the default.`,
		Unlisted: true,
	},
	{
		Name:   "claude",
		Source: "builtin",
		Tools:  []string{"*"},
		Description: "Catch-all for any task that doesn't fit a more specific agent. FleetView's " +
			"default when no agent name is typed.",
	},
	{
		Name:   "claude-code-guide",
		Source: "builtin",
		Model:  "haiku",
		Tools:  []string{"Bash", "Read", "WebFetch", "WebSearch"},
		Description: `***REMOVED***` +
			"`client.beta.messages.tool_runner`" + `) for running an agentic loop over your own tools, manual tool-use loops, Managed Agents for server-hosted agents with a managed sandbox, prompt caching, and general Anthropic SDK usage; (4) Claude Tag (Claude in Slack) - what it is, setting it up for a Slack workspace, ` +
			"`/install-slack-app`" + `; (5) ` + "`claude plugin eval`" + ` (writing and running plugin eval suites, its JSON/report, sandbox, CI) and the ` + "`/skill-doctor`" + ` ***REMOVED***`,
	},
	{
		Name:        "statusline-setup",
		Source:      "builtin",
		Model:       "sonnet",
		Tools:       []string{"Read", "Edit"},
		Description: "Use this agent to configure the user's Claude Code status line setting.",
	},
}

func (d AgentDef) isFork() bool { return strings.EqualFold(d.Name, "fork") }

// effectiveSystemPrompt prefers the bridged bundled prompt for the
// statusline-setup builtin (real text lives in the repl package's extracted
// prompts file).
func (d AgentDef) effectiveSystemPrompt() string {
	p := d.SystemPrompt
	if d.Name == "statusline-setup" && StatuslineSetupPrompt != "" {
		p = StatuslineSetupPrompt
	}
	// --append-subagent-system-prompt[-file]: real appends the text to
	// every subagent's system prompt
	if extra := os.Getenv("CLAUDE_GO_APPEND_SUBAGENT_SYS"); extra != "" {
		p += "\n\n" + extra
	}
	return p
}

func strIn(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

func strSet(list []string) map[string]bool {
	m := make(map[string]bool, len(list))
	for _, v := range list {
		m[strings.ToLower(strings.TrimSpace(v))] = true
	}
	return m
}

// resolveAllowed computes the tool allowlist for this def (nil = all tools).
// extra is an additional restriction layered on top (e.g. a skill's
// allowed-tools frontmatter passed through to the spawned agent).
func (d AgentDef) resolveAllowed(extra []string) []string {
	var base []string
	star := len(d.Tools) == 0 || strIn(d.Tools, "*") || strIn(d.Tools, "all")
	if star {
		if len(d.DisallowedTools) > 0 {
			dis := strSet(d.DisallowedTools)
			for _, n := range tools.Names() {
				if !dis[strings.ToLower(n)] {
					base = append(base, n)
				}
			}
		}
	} else {
		base = append(base, d.Tools...)
	}
	if len(extra) > 0 {
		if base == nil {
			base = append(base, extra...)
		} else {
			ex := strSet(extra)
			kept := base[:0]
			for _, n := range base {
				if ex[strings.ToLower(n)] {
					kept = append(kept, n)
				}
			}
			base = kept
		}
	}
	if base != nil && len(base) == 0 {
		return []string{"__none__"} // deny everything — empty means "all" in Loop
	}
	return base
}

// parseToolList parses a frontmatter/JSON tool list: "*", "a, b", "[a, b]".
func parseToolList(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "[]")
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// agentFileDirs returns (dir, source) pairs scanned for <name>.md agent defs.
// Order matters: later dirs win on name collision (project > user > plugin).
func agentFileDirs(cwd string) []struct{ dir, source string } {
	var out []struct{ dir, source string }
	for _, pat := range []string{
		filepath.Join(config.ClaudeDir(), "plugins", "cache", "*", "*", "*", "agents"),
		filepath.Join(config.ClaudeDir(), "plugins", "local", "*", "agents"),
	} {
		if dirs, _ := filepath.Glob(pat); len(dirs) > 0 {
			for _, d := range dirs {
				out = append(out, struct{ dir, source string }{d, "plugin"})
			}
		}
	}
	for _, pd := range plugins.EnabledPluginDirs() {
		out = append(out, struct{ dir, source string }{filepath.Join(pd, "agents"), "plugin"})
	}
	out = append(out,
		struct{ dir, source string }{filepath.Join(config.ClaudeDir(), "agents"), "user"},
		struct{ dir, source string }{filepath.Join(cwd, ".claude", "agents"), "project"},
	)
	return out
}

// parseAgentFile reads one <name>.md agent definition.
func parseAgentFile(path, source string) (AgentDef, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return AgentDef{}, false
	}
	fm, body := splitFrontmatter(string(b))
	name := fm["name"]
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	return AgentDef{
		Name:            name,
		Description:     fm["description"],
		WhenToUse:       fm["whenToUse"],
		Tools:           parseToolList(fm["tools"]),
		DisallowedTools: parseToolList(fm["disallowedTools"]),
		Model:           fm["model"],
		Effort:          fm["effort"],
		Color:           fm["color"],
		InitialPrompt:   fm["initialPrompt"],
		SystemPrompt:    body,
		Source:          source,
		BaseDir:         filepath.Dir(path),
	}, true
}

// loadCLIAgents merges --agents JSON (env CLAUDE_GO_AGENTS_JSON, written by
// main.go) into the registry as source "cli": [{"name","description","prompt","tools"}].
func loadCLIAgents() []AgentDef {
	raw := os.Getenv("CLAUDE_GO_AGENTS_JSON")
	if raw == "" {
		return nil
	}
	var arr []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Prompt      string          `json:"prompt"`
		Tools       json.RawMessage `json:"tools"`
	}
	if json.Unmarshal([]byte(raw), &arr) != nil {
		return nil
	}
	var out []AgentDef
	for _, a := range arr {
		if a.Name == "" {
			continue
		}
		d := AgentDef{
			Name:         a.Name,
			Description:  a.Description,
			SystemPrompt: a.Prompt,
			Source:       "cli",
		}
		var tl []string
		if json.Unmarshal(a.Tools, &tl) == nil {
			d.Tools = tl
		} else {
			var s string
			if json.Unmarshal(a.Tools, &s) == nil {
				d.Tools = parseToolList(s)
			}
		}
		out = append(out, d)
	}
	return out
}

// AgentTypes returns the full agent-type registry for a cwd: built-ins,
// plugin agents, user agents, project agents, then --agents JSON — later
// sources override earlier ones on name collision (case-insensitive).
func AgentTypes(cwd string) []AgentDef {
	var order []string
	m := map[string]AgentDef{}
	add := func(d AgentDef) {
		key := strings.ToLower(d.Name)
		if _, ok := m[key]; !ok {
			order = append(order, key)
		}
		m[key] = d
	}
	for _, d := range builtinAgentDefs {
		add(d)
	}
	for _, ds := range agentFileDirs(cwd) {
		ents, err := os.ReadDir(ds.dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			if d, ok := parseAgentFile(filepath.Join(ds.dir, e.Name()), ds.source); ok {
				add(d)
			}
		}
	}
	for _, d := range loadCLIAgents() {
		add(d)
	}
	out := make([]AgentDef, 0, len(order))
	for _, k := range order {
		out = append(out, m[k])
	}
	return out
}

// LookupAgentDef resolves an agent type by name (exact, then case-insensitive).
func LookupAgentDef(cwd, name string) (AgentDef, bool) {
	defs := AgentTypes(cwd)
	for _, d := range defs {
		if d.Name == name {
			return d, true
		}
	}
	for _, d := range defs {
		if strings.EqualFold(d.Name, name) {
			return d, true
		}
	}
	return AgentDef{}, false
}

func agentTypeNames(cwd string) []string {
	var out []string
	for _, d := range AgentTypes(cwd) {
		out = append(out, d.Name)
	}
	sort.Strings(out)
	return out
}

// ---------- Runner ----------

// Runner implements tools.AgentRunner with a nested Loop.
type Runner struct {
	Client     *api.Client
	PermMode   string
	Model      string
	User       UserConfig
	PermRules  map[string][]string
	Entrypoint string
	Hooks      hooks.Config
	Transcript string // parent's transcript path — subagent logs nest under it
	// Parent, when set, is the spawning loop — fork subagents inherit its
	// messages. Wired by the caller that owns the parent Loop.
	Parent *Loop
	// stopHookActive guards SubagentStop recursion — a blocked stop
	// re-runs the turn but the hook must not fire again on the retry.
	stopHookActive bool
}

func (r *Runner) RunAgent(c *tools.Context, description, prompt, subagentType, model, effort string, background bool) tools.Result {
	return r.runAgent(c, description, prompt, subagentType, model, effort, background, nil)
}

// RunAgentTriState is RunAgent with the raw run_in_background tri-state —
// the stats recorder distinguishes an omitted field ("unset") from
// explicit true/false like real's recordSpawn.
func (r *Runner) RunAgentTriState(c *tools.Context, description, prompt, subagentType, model, effort string, runInBackground *bool, background bool) tools.Result {
	return r.runAgentRaw(c, description, prompt, subagentType, model, effort, background, nil, runInBackground)
}

// RunAgentAllowed extends RunAgent with an extra tool allowlist (skills pass
// their allowed-tools frontmatter through to the spawned agent).
func (r *Runner) RunAgentAllowed(c *tools.Context, description, prompt, subagentType, model, effort string, background bool, allowedTools []string) tools.Result {
	return r.runAgent(c, description, prompt, subagentType, model, effort, background, allowedTools)
}

func effortOr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func (r *Runner) runAgent(c *tools.Context, description, prompt, subagentType, model, effort string, background bool, extraAllowed []string) tools.Result {
	return r.runAgentRaw(c, description, prompt, subagentType, model, effort, background, extraAllowed, nil)
}

func (r *Runner) runAgentRaw(c *tools.Context, description, prompt, subagentType, model, effort string, background bool, extraAllowed []string, runInBackground *bool) tools.Result {
	if subagentType == "" {
		subagentType = "general-purpose"
	}
	def, ok := LookupAgentDef(c.CWD, subagentType)
	if !ok {
		return tools.Result{
			Content: fmt.Sprintf("unknown agent type %q — available agent types: %s",
				subagentType, strings.Join(agentTypeNames(c.CWD), ", ")),
			IsError: true,
		}
	}
	// model precedence: call arg > agent def > inherit (parent model)
	m := model
	if m == "" {
		m = def.Model
	}
	if def.isFork() || m == "" || m == "inherit" {
		m = r.Model
	}
	if m == "" {
		m = defaultModel
	}
	name := description
	if name == "" {
		name = def.Name
	}
	depth := 1
	if r.Parent != nil {
		depth = r.Parent.Depth + 1
	}
	recordSpawn(r.Client.SessionID, def.Name, runInBackground, background, depth)
	rec := &tools.AgentRecord{
		Name:    name,
		AgentID: tools.NewAgentID(),
		Type:    def.Name,
		Session: c.Session,
		Inbox:   make(chan string, 16),
		Done:    make(chan struct{}),
	}
	tools.RegisterAgent(rec)
	if len(r.Hooks) > 0 {
		payload := map[string]any{
			"session_id": r.Client.SessionID, "cwd": c.CWD,
			"hook_event_name": "SubagentStart", "permission_mode": r.PermMode,
			"agent_type": def.Name, "agent_id": rec.AgentID,
		}
		hooks.Run(r.Hooks, hooks.SubagentStart, payload, c.CWD)
	}
	parentAID := ""
	if r.Parent != nil {
		parentAID = r.Parent.LoopAgentID
	}
	sub := &Loop{
		IsSubagent:    true,
		Depth:         depth,
		LoopAgentID:   rec.AgentID,
		ParentAgentID: parentAID,
		AllowedTools:  def.resolveAllowed(extraAllowed),
		AppendSysP:    def.effectiveSystemPrompt(),
		ExtraSystem: func() []string {
			if v := os.Getenv("CLAUDE_GO_APPEND_SUBAGENT_SYSTEM_PROMPT"); v != "" {
				return []string{v}
			}
			return nil
		}(),
		Client:     r.Client,
		Model:      m,
		MaxTokens:  MaxOutputFor(m),
		Effort:     effortOr(effort, def.Effort, "medium"),
		Entrypoint: r.Entrypoint,
		PermMode:   r.PermMode,
		CWD:        c.CWD,
		User:       r.User,
		PermRules:  r.PermRules,
		ToolCtx: &tools.Context{
			Subagent:   true,
			Ctx:        c.Ctx,
			CWD:        c.CWD,
			BgTasks:    c.BgTasks,
			Permission: c.Permission,
			Answerer:   c.Answerer,
			Searcher:   c.Searcher,
			Session:    c.Session,
		},
	}
	if def.isFork() && r.Parent != nil {
		// fork inherits the parent's full conversation
		sub.Messages = append(sub.Messages, r.Parent.Messages...)
		sub.started = true
	}
	if def.InitialPrompt != "" {
		prompt = def.InitialPrompt + "\n\n" + prompt
	}
	// real's subagent msg[0] ends with the SubagentHandback reminder
	// block — emitted as a separate text block, not folded into the prompt
	sub.handbackTail = true
	finish := func(res tools.Result) tools.Result {
		rec.Finish(res.Content)
		recordComplete(r.Client.SessionID, res.IsError)
		if c.Session != "" {
			_ = tools.NotifyOnIdle(c.Session,
				fmt.Sprintf("[agent %q completed] %s", rec.Name, firstLine(res.Content)))
		}
		// real's agent_completed Notification fires on bg-agent finish
		// (K8 notification categories, mod_0071)
		if background && len(r.Hooks) > 0 {
			hooks.Run(r.Hooks, hooks.Notification, map[string]any{
				"session_id": r.Client.SessionID, "cwd": c.CWD,
				"message":           fmt.Sprintf("Agent %q completed", rec.Name),
				"notification_type": "agent_completed",
				"hook_event_name":   "Notification",
				"permission_mode":   r.PermMode,
				"agent_type":        rec.Type, "agent_id": rec.AgentID,
			}, c.CWD)
		}
		return res
	}
	if background {
		done := make(chan tools.Result, 1)
		runID := api.NewUUID() // real stamps run_id on both bookends
		go func() {
			res := finish(r.runAgentTurn(sub, prompt, c, rec))
			// real's task_notification edge on bg completion
			// (mod_0069: {task_id, run_id, status, summary})
			if l2 := c.OnTaskEvent; l2 != nil {
				status := "completed"
				if res.IsError {
					status = "failed"
				}
				l2(map[string]any{
					"subtype": "task_notification", "task_id": rec.AgentID,
					"run_id": runID, "status": status,
					"summary": firstLine(res.Content),
				})
			}
			done <- res
		}()
		// real emits system/task_started when the bg agent joins the set
		if l2 := c.OnTaskEvent; l2 != nil {
			l2(map[string]any{
				"subtype": "task_started", "task_id": rec.AgentID,
				"run_id": runID, "description": description,
				"subagent_type":   def.Name,
				"is_backgrounded": true,
			})
		}
		select {
		case res := <-done:
			return res
		default:
			return tools.Result{Content: fmt.Sprintf(
				"Launched agent %q (agentId %s, type %s) in the background — "+
					"SendMessage to:%q reaches it; its completion notice arrives automatically",
				rec.Name, rec.AgentID, def.Name, rec.Name)}
		}
	}
	res := r.runAgentTurn(sub, prompt, c, rec)
	rec.Finish(res.Content)
	recordComplete(r.Client.SessionID, res.IsError)
	// real wraps results with usage stats
	out := res.Content
	if sub.UsageTotal.OutputTokens > 0 || sub.UsageTotal.InputTokens > 0 {
		out = fmt.Sprintf("%s\n\n<usage><subagent_tokens>%d</subagent_tokens></usage>",
			out, sub.UsageTotal.InputTokens+sub.UsageTotal.OutputTokens)
	}
	return tools.Result{Content: out, IsError: res.IsError}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 200
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func (r *Runner) runAgentTurn(sub *Loop, prompt string, c *tools.Context, rec *tools.AgentRecord) tools.Result {
	ctx := context.Background()
	if sub.ToolCtx != nil && sub.ToolCtx.Ctx != nil {
		ctx = sub.ToolCtx.Ctx // cancellation propagates from the parent turn
	}
	// real persists subagent turns under <sid>/subagents/<agentID>.jsonl
	var subStore *session.Store
	if r.Transcript != "" {
		if st, err := session.OpenSubagent(r.Transcript, rec.AgentID, c.CWD); err == nil {
			subStore = st
			// a mid-turn auto-compact in the subagent rewrites history —
			// log the replaced span + boundary so the sidechain
			// transcript keeps the full record like the parent does
			sub.Events.OnCompactSpan = func(span []api.Message) {
				for _, m := range span {
					st.Log(m)
				}
			}
			sub.Events.OnCompact = func() {
				_, trig := sub.CompactMeta()
				st.LogCompactBoundaryTrig(sub.LastInputTokens(), len(sub.Messages), trig)
			}
		}
	}
	resp, err := sub.Ask(ctx, prompt+"\n")
	var out string
	if err == nil {
		for _, b := range resp.Content {
			if b.Type == "text" {
				out += b.Text
			}
		}
	}
	if out == "" {
		out = "(agent produced no text output)"
	}
	var subStorePath string
	if subStore != nil {
		subStorePath = subStore.Path
		for _, m := range sub.Messages {
			subStore.Log(m)
		}
		subStore.Close()
	}
	if len(r.Hooks) > 0 && !r.stopHookActive {
		// SubagentStop: block keeps the agent working — real feeds the
		// hook's reason back and re-runs the turn (stop_hook_active
		// guards recursion into the hook itself)
		h := hooks.Run(r.Hooks, hooks.SubagentStop, map[string]any{
			"session_id": r.Client.SessionID, "cwd": c.CWD,
			"hook_event_name": "SubagentStop", "permission_mode": r.PermMode,
			"agent_type": rec.Type, "agent_id": rec.AgentID,
			"agent_transcript_path":  subStorePath,
			"last_assistant_message": out,
			"stop_hook_active":       false,
		}, c.CWD)
		if h.Block {
			reason := h.Reason
			if reason == "" {
				reason = "SubagentStop hook blocked the stop"
			}
			r.stopHookActive = true
			res := r.runAgentTurn(sub, reason+"\n\nContinue working.", c, rec)
			r.stopHookActive = false
			return res
		}
	}
	if err != nil {
		return tools.Result{Content: "agent error: " + err.Error(), IsError: true}
	}
	return tools.Result{Content: out}
}

// WebSearch implements tools.Searcher via the API's server-side web_search tool.
func (r *Runner) WebSearch(ctx context.Context, query string, allow, block []string) (string, error) {
	tool := map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": 8}
	if len(allow) > 0 {
		tool["allowed_domains"] = allow
	}
	if len(block) > 0 {
		tool["blocked_domains"] = block
	}
	tb, _ := json.Marshal(tool)
	req := &api.Request{
		Model:     "claude-haiku-4-5-20251001",
		MaxTokens: 4096,
		Messages:  []api.Message{api.TextMessage("user", query)},
		Tools:     []api.Tool{{Raw: tb}}, // server tool wire form: {"type":"web_search_20250305",...}
	}
	resp, err := r.Client.StreamMessage(ctx, req, func(*api.StreamEvent) error { return nil })
	if err != nil {
		return "", err
	}
	var out string
	for _, b := range resp.Content {
		if b.Type == "text" {
			out += b.Text
		}
	}
	return out, nil
}

func splitFrontmatter(s string) (map[string]string, string) {
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
			fm[strings.TrimSpace(k)] = strings.TrimSpace(strings.Trim(v, " \t\"'"))
		}
	}
	return fm, strings.TrimSpace(rest[end+4:])
}

// ResolveAgentPrompt returns the system prompt for a named agent type
// (used by --agent to pick the main-session agent).
func ResolveAgentPrompt(name, cwd string) string {
	if d, ok := LookupAgentDef(cwd, name); ok {
		return d.effectiveSystemPrompt()
	}
	return ""
}

// StatuslineSetupPrompt is assigned by the repl package (bundled real prompt);
// when empty a minimal default applies.
var StatuslineSetupPrompt string

// handbackReminder is real's msg[0] tail block for spawned agents —
// the final report only reaches the caller through SubagentHandback.
const handbackReminderBlock = `<system-reminder>
***REMOVED***
</system-reminder>`
