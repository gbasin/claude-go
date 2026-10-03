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
	OnCompact     func()
	OnAutoCompact func(tokens int)
}

// Loop runs the query/tool-use cycle.
type Loop struct {
	Client          *api.Client
	Model           string
	MaxTokens       int
	Effort          string
	Entrypoint      string // "cli" or "sdk-cli"
	PermMode        string
	CWD             string
	AppendSysP      string
	ExtraSystem     []string
	PermRules       map[string][]string
	AddDirs         []string
	User            UserConfig
	Messages        []api.Message
	ToolCtx         *tools.Context
	Events          Events
	MaxTurns        int // 0 = unlimited
	started         bool
	UsageTotal      api.Usage
	Transcript      string // path to JSONL transcript (for compact notes)
	Hooks           hooks.Config
	inCompact       bool
	AllowedTools    []string // nil = all
	Goal            string   // /goal stop-condition
	Bare            bool     // --bare: skip auto-discovery
	FallbackModel   string   // --fallback-model retry chain
	ThinkingMode    string   // --thinking enabled|disabled
	ThinkingDisplay string   // --thinking-display updates|omitted
	ThinkingBudget  int      // --max-thinking-tokens
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
		PermMode:   "default",
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
		Thinking:          l.thinkingConfig(),
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
		envBlocks := api.BlocksMessage("system", []api.ContentBlock{{
			Type:         "text",
			Text:         EnvMessage(l.ToolCtx.CWD, l.Model, l.AddDirs, l.PermMode, l.Client.SessionID),
			CacheControl: &api.CacheControl{Type: "ephemeral", TTL: "1h"},
		}})
		envBlocks.OutputConfig = DefaultOutputConfig()
		l.Messages = append(l.Messages, msg, envBlocks)
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
	var resp *api.Response
	var err error
	models := append([]string{l.Model}, l.fallbackModels()...)
	for mi, m := range models {
		if mi > 0 {
			l.Model = m // try the next fallback
		}
		resp, err = l.Client.StreamMessage(ctx, l.buildRequest(), func(ev *api.StreamEvent) error {
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
		if err == nil {
			break
		}
		if !retryableErr(err) || mi == len(models)-1 {
			break
		}
	}
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
		if b.Name == "advisor" {
			// the advisor tool forwards the whole conversation to a second
			// model call and returns its advice — real client behavior
			r = l.runAdvisor(ctx)
		} else if pre := l.runHook(hooks.PreToolUse, map[string]any{
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

// thinkingDisplay: interactive sessions show thinking updates.
func (l *Loop) thinkingDisplay() string {
	if l.ThinkingDisplay == "updates" || l.ThinkingDisplay == "omitted" {
		return l.ThinkingDisplay
	}
	if l.Entrypoint == "cli" {
		return "updates"
	}
	return "omitted"
}

// appendNotificationBlocks prepends <system-reminder> blocks for queued notes.
func appendNotificationBlocks(raw json.RawMessage, notes []string) json.RawMessage {
	var blocks []api.ContentBlock
	for _, n := range notes {
		blocks = append(blocks, api.ContentBlock{
			Type: "text", Text: "<system-reminder>\n" + n + "\n</system-reminder>",
		})
	}
	var orig []api.ContentBlock
	json.Unmarshal(raw, &orig)
	blocks = append(blocks, orig...)
	out, _ := json.Marshal(blocks)
	return out
}

func (l *Loop) fallbackModels() []string {
	if l.FallbackModel == "" {
		return nil
	}
	var out []string
	for _, m := range strings.Split(l.FallbackModel, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}

func retryableErr(err error) bool {
	s := err.Error()
	return strings.Contains(s, "429") || strings.Contains(s, "529") ||
		strings.Contains(s, "overloaded") || strings.Contains(s, "rate_limit") ||
		strings.Contains(s, "500") || strings.Contains(s, "502") || strings.Contains(s, "503")
}

// runAdvisor answers an advisor tool_use: a second model call over the
// current conversation, stripped of tool defs.
func (l *Loop) runAdvisor(ctx context.Context) tools.Result {
	if l.Events.OnTextDelta != nil {
		l.Events.OnTextDelta("\n  (advisor thinking…)\n")
	}
	req := l.buildRequest()
	req.Tools = nil // advisor sees context only
	resp, err := l.Client.StreamMessage(ctx, req, func(*api.StreamEvent) error { return nil })
	if err != nil {
		return tools.Result{Content: "advisor unavailable: " + err.Error(), IsError: true}
	}
	var text string
	for _, b := range resp.Content {
		if b.Type == "text" {
			text += b.Text
		}
	}
	if text == "" {
		return tools.Result{Content: "advisor returned no advice", IsError: true}
	}
	return tools.Result{Content: text}
}

func (l *Loop) thinkingConfig() json.RawMessage {
	if l.ThinkingMode == "disabled" {
		return nil
	}
	display := "omitted"
	if l.Entrypoint == "cli" {
		display = "updates"
	}
	if l.ThinkingDisplay == "updates" || l.ThinkingDisplay == "omitted" {
		display = l.ThinkingDisplay
	}
	return json.RawMessage(`{"type":"adaptive","display":"` + display + `"}`)
}
