// Package tools implements the Claude Code builtin toolset.
package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
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
	Ctx        context.Context
	CWD        string // current working directory (Bash persists this)
	mu         sync.Mutex
	readFiles  map[string]readState
	BgTasks    *Background
	Agent      AgentRunner // for Agent/SendMessage-style tools
	AskUser    func(question string, options []string) (string, error)
	Permission func(toolName string, input json.RawMessage, summary string) bool
	Notes      []string // queued system notes from background tasks
	Answerer   Answerer // WebFetch page-answer model
	Searcher   Searcher // WebSearch backend
}

type readState struct {
	mtimeNs int64
	size    int64
}

var registry = map[string]*Tool{}

func Register(t *Tool) { registry[t.Def.Name] = t }

// All returns every registered tool definition in a stable order.
func All() []*Tool {
	order := []string{
		"Agent", "Bash", "CronCreate", "CronDelete", "CronList", "DesignSync",
		"Edit", "EnterWorktree", "ExitWorktree", "ListAgents",
		"ListMcpResourcesTool", "LSP", "Monitor", "NotebookEdit",
		"PushNotification", "Read", "ReadMcpResourceDirTool",
		"ReadMcpResourceTool", "RemoteTrigger", "ReportFindings",
		"ScheduleWakeup", "SendMessage", "Skill", "TaskStop", "WebFetch",
		"WebSearch", "Workflow", "Write",
	}
	out := make([]*Tool, 0, len(order))
	for _, n := range order {
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
	c.readFiles[path] = readState{mtimeNs, size}
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
