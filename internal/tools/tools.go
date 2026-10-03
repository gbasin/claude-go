// Package tools implements the Claude Code builtin toolset.
package tools

import (
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
}

// Tool is a callable builtin.
type Tool struct {
	Def  api.Tool
	Exec func(ctx *Context, input json.RawMessage) Result
}

// Context carries per-session tool state.
type Context struct {
	Ctx           context.Context
	CWD           string // current working directory (Bash persists this)
	mu            sync.Mutex
	readFiles     map[string]readState
	BgTasks       *Background
	Agent         AgentRunner // for Agent/SendMessage-style tools
	AskUser       func(question string, options []string) (string, error)
	Permission    func(toolName string, input json.RawMessage, summary string) bool
	Notes         []string // queued system notes from background tasks
	Answerer      Answerer // WebFetch page-answer model
	Searcher      Searcher // WebSearch backend
	AskStructured func(header, question string, options []string, multi bool) ([]string, error)
	SetPermMode   func(mode string)
	PlanApprove   func() bool
	EndSession    func()
	Session       string        // session id for skill substitutions
	OAuthToken    func() string // claude.ai oauth access token for remote APIs
	OnCwdChanged  func(newCWD string)
	OnFileChanged func(path, kind string)
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
		"EnterWorktree", "ExitPlanMode", "ExitWorktree", "ListAgents",
		"ListMcpResourcesTool", "LSP", "Monitor", "NotebookEdit",
		"PushNotification", "Read", "ReadMcpResourceDirTool",
		"ReadMcpResourceTool", "RemoteTrigger", "ReportFindings",
		"ScheduleWakeup", "SendFeedback", "SendMessage", "Skill",
		"TaskStop", "WebFetch", "WebSearch", "Workflow", "Write",
	}
	interactiveOnly := map[string]bool{
		"Artifact": true, "ArtifactComments": true, "ArtifactData": true,
		"AskUserQuestion": true, "EndConversation": true,
		"EnterPlanMode": true, "ExitPlanMode": true, "SendFeedback": true,
	}
	out := make([]*Tool, 0, len(order))
	for _, n := range order {
		if interactiveOnly[n] && !Interactive {
			continue
		}
		if t, ok := registry[n]; ok {
			out = append(out, t)
		}
	}
	out = append(out, mcpTools...)
	return out
}

// Dispatch runs a tool by name.
func Dispatch(ctx *Context, name string, input json.RawMessage) Result {
	t, ok := registry[name]
	if !ok {
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
	RunAgent(ctx *Context, description, prompt, subagentType, model string, background bool) Result
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
