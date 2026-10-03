// Package agent implements the Claude Code agentic loop.
package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"claude-go/internal/api"
	"claude-go/internal/hooks"
	"claude-go/internal/tools"
)

const defaultModel = "claude-fable-5-1"
const defaultMaxTokens = 64000

// Events surfaces streaming/tool activity to the UI.
type Events struct {
	OnTextDelta   func(text string)
	OnThinking    func(text string)
	OnToolUse     func(name string, input json.RawMessage)
	OnToolResult  func(name string, r tools.Result)
	OnUsage       func(u api.Usage)
	OnAutoCompact func(tokens int)
}

// Loop runs the query/tool-use cycle.
type Loop struct {
	Client       *api.Client
	Model        string
	MaxTokens    int
	Effort       string
	Entrypoint   string // "cli" or "sdk-cli"
	PermMode     string
	CWD          string
	AppendSysP   string
	ExtraSystem  []string
	PermRules    map[string][]string
	AddDirs      []string
	User         UserConfig
	Messages     []api.Message
	ToolCtx      *tools.Context
	Events       Events
	MaxTurns     int // 0 = unlimited
	started      bool
	UsageTotal   api.Usage
	Transcript   string // path to JSONL transcript (for compact notes)
	Hooks        hooks.Config
	inCompact    bool
	AllowedTools []string // nil = all
}

func NewLoop(client *api.Client, cwd string) *Loop {
	abs, err := filepath.Abs(cwd)
	if err != nil || cwd == "" {
		abs = cwd
	}
	return &Loop{
		Client:     client,
		Model:      defaultModel,
		MaxTokens:  defaultMaxTokens,
		Effort:     "medium",
		Entrypoint: "sdk-cli",
		PermMode:   "auto",
		CWD:        abs,
		ToolCtx: &tools.Context{
			Ctx: context.Background(),
			CWD: abs,
		},
		PermRules: map[string][]string{},
	}
}

func (l *Loop) buildRequest() *api.Request {
	effort := l.Effort
	if effort == "" {
		effort = "medium"
	}
	return &api.Request{
		Model:             l.Model,
		MaxTokens:         l.MaxTokens,
		Messages:          l.Messages,
		System:            SystemBlocks(l.Entrypoint, l.CWD, l.Model, l.AppendSysP, l.ExtraSystem),
		Tools:             tools.Defs(),
		Metadata:          Metadata(l.Client.SessionID, l.User),
		Thinking:          DefaultThinking(),
		ContextManagement: DefaultContextManagement(),
		Safeguards:        Safeguards(l.PermMode, l.ToolCtx.CWD, l.PermRules),
		OutputConfig:      json.RawMessage(fmt.Sprintf(`{"effort":%q}`, effort)),
	}
}

// Ask submits one user turn and runs the tool loop to completion.
func (l *Loop) Ask(ctx context.Context, prompt string) (*api.Response, error) {
	if h := l.runHook(hooks.UserPromptSubmit, map[string]any{"prompt": prompt}); h.Block {
		return nil, fmt.Errorf("prompt blocked by hook: %s", h.Reason)
	}
	var msg api.Message
	if !l.started {
		msg = FirstUserMessage(prompt, l.CWD, l.User.Email)
		l.started = true
		l.Messages = append(l.Messages, msg,
			api.TextMessage("system", EnvMessage(l.ToolCtx.CWD, l.Model, l.AddDirs, l.PermMode)))
	} else {
		msg = api.TextMessage("user", prompt)
		l.Messages = append(l.Messages, msg)
	}
	return l.run(ctx, 0)
}

// PushUser injects a user message built from blocks (tool results, notes).
func (l *Loop) PushUser(blocks []api.ContentBlock) {
	l.Messages = append(l.Messages, api.BlocksMessage("user", blocks))
}

func (l *Loop) run(ctx context.Context, depth int) (*api.Response, error) {
	if l.MaxTurns > 0 && depth >= l.MaxTurns {
		return nil, fmt.Errorf("max turns (%d) reached", l.MaxTurns)
	}
	l.ToolCtx.Ctx = ctx
	resp, err := l.Client.StreamMessage(ctx, l.buildRequest(), func(ev *api.StreamEvent) error {
		if ev.Type == "content_block_delta" && ev.Delta != nil {
			switch ev.Delta.Type {
			case "text_delta":
				if l.Events.OnTextDelta != nil {
					l.Events.OnTextDelta(ev.Delta.Text)
				}
			case "thinking_delta":
				if l.Events.OnThinking != nil {
					l.Events.OnThinking(ev.Delta.Thinking)
				}
			}
		}
		if ev.Type == "message_start" && ev.Message != nil {
			l.UsageTotal.InputTokens += ev.Message.Usage.InputTokens
			l.UsageTotal.CacheReadInputTokens += ev.Message.Usage.CacheReadInputTokens
			l.UsageTotal.CacheCreationInputTokens += ev.Message.Usage.CacheCreationInputTokens
		}
		if ev.Type == "message_delta" && ev.Usage != nil {
			l.UsageTotal.OutputTokens += ev.Usage.OutputTokens
			if l.Events.OnUsage != nil {
				l.Events.OnUsage(*ev.Usage)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	l.Messages = append(l.Messages, api.BlocksMessage("assistant", resp.Content))

	var results []api.ContentBlock
	for _, b := range resp.Content {
		if b.Type != "tool_use" {
			continue
		}
		if l.Events.OnToolUse != nil {
			l.Events.OnToolUse(b.Name, b.Input)
		}
		var r tools.Result
		if len(l.AllowedTools) > 0 && !containsStr(l.AllowedTools, b.Name) {
			r = tools.Result{Content: fmt.Sprintf("tool %s not allowed for this agent", b.Name), IsError: true}
			results = append(results, toolResultBlock(b.ID, r))
			continue
		}
		if pre := l.runHook(hooks.PreToolUse, map[string]any{
			"tool_name": b.Name, "tool_input": json.RawMessage(b.Input),
		}); pre.Block {
			r = tools.Result{Content: "Tool call blocked by hook: " + pre.Reason, IsError: true}
		} else if l.ToolCtx.Permission != nil &&
			!l.ToolCtx.Permission(b.Name, b.Input, toolSummary(b.Name, b.Input)) {
			r = tools.Result{
				Content: "The user declined to run this tool. Do not retry the same call; adjust or ask what they'd prefer.",
				IsError: true,
			}
		} else {
			r = tools.Dispatch(l.ToolCtx, b.Name, b.Input)
		}
		if post := l.runHook(hooks.PostToolUse, map[string]any{
			"tool_name": b.Name, "tool_input": json.RawMessage(b.Input),
			"tool_response": r.Content,
		}); post.AdditionalContext != "" {
			r.Content += "\n\nHook feedback: " + post.AdditionalContext
		}
		if l.Events.OnToolResult != nil {
			l.Events.OnToolResult(b.Name, r)
		}
		results = append(results, toolResultBlock(b.ID, r))
	}
	if len(results) == 0 {
		if l.ShouldAutoCompact(resp.Usage) && !l.inCompact {
			l.inCompact = true
			_ = l.Compact(ctx, l.Transcript)
			l.inCompact = false
		}
		return resp, nil
	}
	l.PushUser(results)
	return l.run(ctx, depth+1)
}

func (l *Loop) runHook(ev hooks.Event, extra map[string]any) hooks.Result {
	if len(l.Hooks) == 0 {
		return hooks.Result{}
	}
	payload := map[string]any{
		"session_id":      l.Client.SessionID,
		"transcript_path": l.Transcript,
		"cwd":             l.ToolCtx.CWD,
		"hook_event_name": string(ev),
		"permission_mode": l.PermMode,
	}
	for k, v := range extra {
		payload[k] = v
	}
	return hooks.Run(l.Hooks, ev, payload, l.ToolCtx.CWD)
}

func toolSummary(name string, input json.RawMessage) string {
	var m map[string]any
	json.Unmarshal(input, &m)
	for _, k := range []string{"command", "file_path", "notebook_path", "url", "query", "description", "skill"} {
		if v, ok := m[k]; ok && v != "" {
			return fmt.Sprint(v)
		}
	}
	return name
}

func toolResultBlock(id string, r tools.Result) api.ContentBlock {
	content, _ := json.Marshal(resultBlocks(r))
	return api.ContentBlock{Type: "tool_result", ToolUseID: id, Content: content, IsError: r.IsError}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s || v == "*" {
			return true
		}
	}
	return false
}

// resultBlocks builds the tool_result content; converts the internal image
// marker produced by Read into a base64 image block.
func resultBlocks(r tools.Result) []api.ContentBlock {
	const marker = "\x00IMAGE\x00"
	if strings.HasPrefix(r.Content, marker) {
		rest := strings.TrimPrefix(r.Content, marker)
		path, _, _ := strings.Cut(rest, "\x00")
		if b, err := os.ReadFile(path); err == nil {
			mt := "image/png"
			switch strings.ToLower(filepath.Ext(path)) {
			case ".jpg", ".jpeg":
				mt = "image/jpeg"
			case ".gif":
				mt = "image/gif"
			case ".webp":
				mt = "image/webp"
			}
			return []api.ContentBlock{{
				Type:   "image",
				Source: &api.ImageSource{Type: "base64", MediaType: mt, Data: base64.StdEncoding.EncodeToString(b)},
			}}
		}
	}
	return []api.ContentBlock{{Type: "text", Text: r.Content}}
}
