// Package tools implements the Claude Code builtin toolset.
package tools

import (
	"claude-go/internal/config"
	"claude-go/internal/hooks"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"claude-go/internal/api"
)

//go:embed builtin_schemas.json
var schemasJSON []byte

// Result is a tool execution result.
type Result struct {
	Content string // text result sent back as tool_result
	IsError bool
	// Refs, when non-empty, renders the tool_result as a list of
	// {type:"tool_reference", tool_name} blocks instead of text — real's
	// mapToolResultToToolResultBlockParam for ToolSearch matches.
	Refs []string
	// Handback marks SubagentHandback's report — the runner ends the
	// subagent's run and delivers Content as the Agent result (real's
	// contract: only a SubagentHandback call reaches the caller).
	Handback bool
}

// Tool is a callable builtin.
type Tool struct {
	Def      api.Tool
	Exec     func(ctx *Context, input json.RawMessage) Result
	IsMCP    bool // mcp__* tools — real defers them all (isMcp===true)
	IsHidden bool // registered for dispatch but never on the wire (SubagentHandback is added per-subagent)
}

// Context carries per-session tool state.
type Context struct {
	Ctx        context.Context
	CWD        string // current working directory (Bash persists this)
	mu         sync.Mutex
	readFiles  map[string]readState
	BgTasks    *Background
	Agent      AgentRunner // for Agent/SendMessage-style tools
	AskUser    func(question string, options []string) (string, error)
	Permission func(toolName string, input json.RawMessage, summary string) bool
	// PermissionAsk reports when the last Permission call resolved to
	// "ask" rather than a hard allow/deny — the loop feeds it to the
	// auto-mode safety eval instead of flat-denying (real's behavior
	// for unresolvable calls in non-interactive mode).
	PermissionAsk func() bool
	// Subagent marks Agent-spawned contexts — RequestDefs adds
	// SubagentHandback to the wire set.
	Subagent      bool
	// AdvisorModel is the configured advisor model (--advisor /
	// advisorModel / /advisor). Empty → the advisor server block stays
	// off the wire (real only ships it when configured).
	AdvisorModel string
	// Entrypoint selects the cli ("interactive") schema variants — real
	// ships different Agent/Bash shapes on the interactive wire (294).
	Entrypoint    string
	Notes         []string // queued system notes from background tasks
	Answerer      Answerer // WebFetch page-answer model
	Searcher      Searcher // WebSearch backend
	AskStructured func(header, question string, options []AskOption, multi bool) ([]string, error)
	// AskStructuredMulti renders real's multi-question wizard (question
	// tabs + Submit). Falls back to sequential AskStructured when unset.
	AskStructuredMulti func(questions []AskQuestion) (map[string][]string, error)
	SetPermMode        func(mode string)
	PlanApprove        func() bool
	EndSession         func()
	Session            string        // session id for skill substitutions
	OAuthToken         func() string // claude.ai oauth access token for remote APIs
	OnCwdChanged       func(newCWD string)
	OnFileChanged      func(path, kind string)
	// Discovered caches deferred tools whose schemas ToolSearch has served
	// this session (real: declared-tools latch). Guarded by mu.
	Discovered map[string]bool
	// AllowedTools restricts the wire surface (--tools) — nil offers all.
	AllowedTools []string
	// FireHook runs a hooks event from inside a tool exec (TaskCreated,
	// WorktreeCreate/Remove fire from tool bodies in real). Nil-safe:
	// callers no-op when unset.
	FireHook func(ev hooks.Event, extra map[string]any) hooks.Result
}

type readState struct {
	mtimeNs int64
	size    int64
	sha256  string
}

var registry = map[string]*Tool{}

// Interactive controls whether interactive-only tools (AskUserQuestion,
// plan mode, artifacts, feedback) are registered.
var Interactive bool

func Register(t *Tool) { registry[t.Def.Name] = t }

// All returns every registered tool definition in a stable order.
func All() []*Tool {
	order := []string{
		"Agent", "Artifact", "ArtifactComments", "ArtifactData",
		"AskUserQuestion", "Bash", "CronCreate", "CronDelete", "CronList",
		"DesignSync", "Edit", "EndConversation", "EnterPlanMode",
		"EnterWorktree", "ExitPlanMode", "ExitWorktree", "Glob", "Grep",
		"ListAgents",
		"ListMcpResourcesTool", "LS", "LSP", "Monitor", "MultiEdit",
		"NotebookEdit", "NotebookRead",
		"PushNotification", "Read", "ReadMcpResourceDirTool",
		"ReadMcpResourceTool", "ReadNotifications", "RemoteTrigger",
		"ReportFindings", "ScheduleWakeup", "SendFeedback", "SendMessage",
		"Skill", "TaskCreate", "TaskGet", "TaskList", "TaskStop",
		"TaskUpdate", "ToolSearch", "TodoWrite", "WebFetch", "WebSearch",
		"Workflow", "Write", "advisor",
	}
	interactiveOnly := map[string]bool{
		"Artifact": true, "ArtifactComments": true, "ArtifactData": true,
		"AskUserQuestion": true, "EndConversation": true,
		"EnterPlanMode": true, "ExitPlanMode": true, "SendFeedback": true,
		"ReadNotifications": true,
	}
	out := make([]*Tool, 0, len(order))
	for _, n := range order {
		if interactiveOnly[n] && !Interactive {
			continue
		}
		// real isEnabled gates: tengu_kairos_cron hides Cron*; the remote
		// gate (Jn&&ft&&!remote&&allow_remote_sessions&&allow_routines)
		// hides RemoteTrigger; tasks.enabled / CLAUDE_CODE_TASK_TOOLS
		// gates the Task* surface.
		if (n == "CronCreate" || n == "CronDelete" || n == "CronList") && !cronEnabled() {
			continue
		}
		if isTaskTool(n) && !taskToolsEnabledFor(ProjectDir()) {
			continue
		}
		if n == "RemoteTrigger" && !remoteTriggerEnabled() {
			continue
		}
		if n == "ToolSearch" && !toolSearchEnabled() {
			continue
		}
		if n == "Workflow" && !workflowEnabled() {
			continue
		}
		if (n == "Artifact" || n == "ArtifactComments" || n == "ArtifactData") && !artifactEnabled() {
			continue
		}
		if t, ok := registry[n]; ok {
			if n == "CronCreate" || n == "CronDelete" || n == "CronList" {
				t = cronToolDef(t)
			}
			out = append(out, t)
		}
	}
	out = append(out, mcpTools...)
	return out
}

// cronToolDef swaps Cron* descriptions to the durable-capable variant when
// the tengu_kairos_cron_durable gate is on — real's X2r/J2r/Q2r/eWr/nWr(e)
// text switch.
func cronToolDef(t *Tool) *Tool {
	if !cronDurableEnabled() {
		return t
	}
	d := t.Def // value copy
	switch d.Name {
	case "CronCreate":
		d.Description = strings.Replace(d.Description,
			"## Session-only\n\nJobs live only in this Claude session — nothing is written to disk, and the job is gone when Claude exits.",
			"## Durability\n\nBy default (durable: false) the job lives only in this Claude session — nothing is written to disk, and the job is gone when Claude exits. Pass durable: true to write to .claude/scheduled_tasks.json so the job survives restarts. Only use durable: true when the user explicitly asks for the task to persist (\"keep doing this every day\", \"set this up permanently\"). Most \"remind me in 5 minutes\" / \"check back in an hour\" requests should stay session-only.", 1)
		d.Description = strings.Replace(d.Description,
			"Jobs only fire while the REPL is idle (not mid-query). The scheduler adds",
			"Jobs only fire while the REPL is idle (not mid-query). Durable jobs persist to .claude/scheduled_tasks.json and survive session restarts — on next launch they resume automatically. One-shot durable tasks that were missed while the REPL was closed are surfaced for catch-up. Session-only jobs die with the process. The scheduler adds", 1)
		var schema map[string]any
		if json.Unmarshal(d.InputSchema, &schema) == nil {
			if props, ok := schema["properties"].(map[string]any); ok {
				if dur, ok := props["durable"].(map[string]any); ok {
					dur["description"] = "true = persist to .claude/scheduled_tasks.json and survive restarts. false (default) = in-memory only, dies when this Claude session ends. Use true only when the user asks the task to survive across sessions."
					if b, err := json.Marshal(schema); err == nil {
						d.InputSchema = b
					}
				}
			}
		}
	case "CronDelete":
		d.Description = "Cancel a cron job previously scheduled with CronCreate. Removes it from .claude/scheduled_tasks.json (durable jobs) or the in-memory session store (session-only jobs)."
	case "CronList":
		d.Description = "List all cron jobs scheduled via CronCreate, both durable (.claude/scheduled_tasks.json) and session-only."
	}
	return &Tool{Def: d, Exec: t.Exec}
}

// Dispatch runs a tool by name.
// legacyName maps real's old tool names to current ones (mod_0018.js):
// Task→Agent, KillShell/KillBash→TaskStop, ListPeers→ListAgents,
// Brief→SendUserMessage, ListMcpResources/ReadMcpResource→*Tool.
var legacyName = map[string]string{
	"Task":               "Agent",
	"KillShell":          "TaskStop",
	"KillBash":           "TaskStop",
	"ListPeers":          "ListAgents",
	"Brief":              "SendUserMessage",
	"ListMcpResources":   "ListMcpResourcesTool",
	"ReadMcpResource":    "ReadMcpResourceTool",
	"ReadMcpResourceDir": "ReadMcpResourceDirTool",
}

func Dispatch(ctx *Context, name string, input json.RawMessage) Result {
	if n2, ok := legacyName[name]; ok {
		name = n2
	}
	t, ok := registry[name]
	if !ok {
		return Result{Content: fmt.Sprintf("Unknown tool: %s", name), IsError: true}
	}
	if isTaskTool(name) && !taskToolsEnabledFor(ProjectDir()) {
		return Result{Content: fmt.Sprintf("Unknown tool: %s", name), IsError: true}
	}
	defer func() {}()
	return t.Exec(ctx, input)
}

// Defs returns the wire tool definitions.
func Defs() []api.Tool {
	var out []api.Tool
	for _, t := range All() {
		out = append(out, t.Def)
	}
	return out
}

// loadSchema pulls one tool def from the embedded schemas file.
func loadSchema(name string) api.Tool {
	var all []api.Tool
	if err := json.Unmarshal(schemasJSON, &all); err != nil {
		panic(err)
	}
	for _, t := range all {
		if t.Name == name {
			return t
		}
	}
	return api.Tool{Name: name}
}

// --- file-read state (Edit/Write require prior Read) ---

func (c *Context) MarkRead(path string, mtimeNs, size int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readFiles == nil {
		c.readFiles = map[string]readState{}
	}
	c.readFiles[path] = readState{mtimeNs, size, fileSHA(path)}
}

// Stale returns true when the file changed since it was last read (sha or stat).
func (c *Context) Stale(path string) bool {
	c.mu.Lock()
	st, ok := c.readFiles[path]
	c.mu.Unlock()
	if !ok {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil {
		return true // gone = stale
	}
	if fi.Size() != st.size || fi.ModTime().UnixNano() != st.mtimeNs {
		return true
	}
	return false
}

func (c *Context) WasRead(path string) (readState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.readFiles[path]
	return s, ok
}

// AgentRunner abstracts sub-agent execution.
type AgentRunner interface {
	RunAgent(ctx *Context, description, prompt, subagentType, model, effort string, background bool) Result
}

// Answerer answers a prompt over fetched page content (WebFetch's small model).
type Answerer func(ctx context.Context, prompt, pageText string) (string, error)

// Searcher performs a web search (WebSearch backend).
type Searcher func(ctx context.Context, query string, allow, block []string) (string, error)

// Names returns registered tool names (for stream-json init event).
func Names() []string {
	var out []string
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ExtraAllowedDirs widens the writable workspace (settings
// permissions.additionalDirectories). File tools treat these like the cwd.
var ExtraAllowedDirs []string

// DirAllowed reports whether p is inside the working dir or an additional dir.
func DirAllowed(c *Context, p string) bool {
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	abs = filepath.Clean(abs)
	if strings.HasPrefix(abs, filepath.Clean(c.CWD)+string(filepath.Separator)) || abs == filepath.Clean(c.CWD) {
		return true
	}
	for _, d := range ExtraAllowedDirs {
		if abs == d || strings.HasPrefix(abs, d+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// isTaskTool reports whether n is one of the tasks.enabled-gated tools.
func isTaskTool(n string) bool {
	return n == "TaskCreate" || n == "TaskGet" || n == "TaskList" || n == "TaskUpdate"
}

// workflowEnabled gates the Workflow tool on settings
// enableWorkflows/disableWorkflows (real keys — disable wins).
func workflowEnabled() bool {
	if boolSetting("disableWorkflows") {
		return false
	}
	if boolSetting("enableWorkflows") {
		return true
	}
	return true // real default: workflows on
}

// boolSetting reads a boolean key from ~/.claude/settings.json.
func boolSetting(key string) bool {
	v, _ := config.UserSetting(key).(bool)
	return v
}

// artifactEnabled gates the Artifact tool family on settings
// enableArtifact/disableArtifact (real keys — disable wins).
func artifactEnabled() bool {
	if boolSetting("disableArtifact") {
		return false
	}
	return true
}
