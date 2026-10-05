// Package agent implements the Claude Code agentic loop.
package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	Client                *api.Client
	Model                 string
	Wants1m               bool // [1m] suffix → context-1m beta
	MaxTokens             int
	Effort                string
	Entrypoint            string // "cli" or "sdk-cli"
	PermMode              string
	CWD                   string
	AppendSysP            string
	ExtraSystem           []string
	PermRules             map[string][]string
	AddDirs               []string
	User                  UserConfig
	Messages              []api.Message
	ToolCtx               *tools.Context
	Events                Events
	MaxTurns              int // 0 = unlimited
	started               bool
	UsageTotal            api.Usage
	Transcript            string // path to JSONL transcript (for compact notes)
	Hooks                 hooks.Config
	inCompact             bool
	AllowedTools          []string // nil = all
	Goal                  string   // /goal stop-condition
	Bare                  bool     // --bare: skip auto-discovery
	FallbackModel         string   // --fallback-model retry chain
	ThinkingMode          string   // --thinking enabled|disabled
	ThinkingDisplay       string   // --thinking-display updates|omitted
	ThinkingBudget        int      // --max-thinking-tokens
	SessionStartSource    string   // startup|resume|clear|compact|fork
	SessionEndReason      string   // clear|logout|prompt_input_exit|other
	FastMode              bool     // fast mode: output_config.speed="fast"
	AutoCompactWindow     string   // settings autoCompactWindow ("auto"|"500k"|N)
	AdvisorModel          string   // settings advisorModel — the /advisor pick
	MemoryPaused          bool     // /pause-memory: skip MEMORY.md injection
	AlwaysThinking        bool     // settings alwaysThinkingEnabled
	pendingTrigger        string   // last compact trigger for the boundary record
	compactSummaryPending bool     // next user log is the compact continuation
	announcedDeferred     string   // last announced deferred-tool name set (sorted join)
	Precompute            bool     // precomputeCompactionEnabled gate
	RateLimitNote         string   // set once a slow-lane rate-limit note was shown
	noThinking            bool     // thinking config dropped after a thinking 400
	dropSafeguards        bool
	dropOutputConfig      bool
	dropBetas             bool
	maxTokensClamp        bool
	lastTotal             int // last request's total input tokens (incl. cache)
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
		Effort:     "",
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

func isXHighModel(m string) bool {
	// per-turn effort (xhigh/output_config) is a 5.x-class capability —
	// live-verified: opus-4-8 rejects output_config outright
	return strings.Contains(m, "fable") || strings.Contains(m, "-5")
}

func (l *Loop) buildRequest() *api.Request {
	effort := l.Effort
	if effort == "" {
		effort = "medium"
	}
	// xhigh is fable-class only — opus rejects it ('This model does not
	// support effort level xhigh'); map to the top supported tier.
	if effort == "xhigh" && !isXHighModel(l.Model) {
		effort = "max"
	}
	// output_config only exists on per-turn-effort models (verified live:
	// opus-4-8 rejects it — 'requires a model that supports per-turn
	// effort'). Real omits the key for those models.
	var ocRaw json.RawMessage
	if isXHighModel(l.Model) {
		oc := map[string]string{"effort": effort}
		if l.FastMode {
			oc["speed"] = "fast"
		}
		ocRaw, _ = json.Marshal(oc)
	}
	msgs := l.Messages
	if l.nearContextLimit() {
		msgs = withEphemeralBreakpoint(msgs)
	}
	msgs = capBreakpoints(msgs, 2) // 2 system breakpoints reserve; API max is 4 total
	msgs = l.withDeferredReminder(msgs)
	var safeguards json.RawMessage
	if !l.dropSafeguards {
		safeguards = Safeguards(l.PermMode, l.ToolCtx.CWD, l.PermRules)
	}
	var outCfg json.RawMessage
	if !l.dropOutputConfig && ocRaw != nil {
		outCfg = ocRaw
	}
	mt := l.MaxTokens
	if l.maxTokensClamp && mt > 64000 {
		mt = 64000
	}
	l.Client.Model = l.Model // beta list is model-conditional
	l.Client.Wants1m = l.Wants1m
	return &api.Request{
		Model:             l.Model,
		MaxTokens:         mt,
		Messages:          msgs,
		System:            SystemBlocks(l.Entrypoint, l.CWD, l.Model, l.AppendSysP, l.ExtraSystem),
		Tools:             tools.RequestDefs(l.ToolCtx),
		Metadata:          Metadata(l.Client.SessionID, l.User),
		Thinking:          l.thinkingConfig(),
		ContextManagement: DefaultContextManagement(),
		Safeguards:        safeguards,
		OutputConfig:      outCfg,
	}
}

// withDeferredReminder prepends the deferred-tools announcement
// (<system-reminder>, real's deferred_tools_delta attachment / MTr text)
// to the newest user message when the still-deferred set changed since the
// last request — first turn lists every deferred name; post-discovery the
// shrunken pool re-announces. Returns msgs unchanged when nothing to say.
func (l *Loop) withDeferredReminder(msgs []api.Message) []api.Message {
	names := l.ToolCtx.DeferredNames()
	sort.Strings(names)
	key := strings.Join(names, "\x00")
	if key == l.announcedDeferred {
		return msgs
	}
	l.announcedDeferred = key
	if key == "" {
		return msgs
	}
	out := make([]api.Message, len(msgs))
	copy(out, msgs)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Role != "user" {
			continue
		}
		var blocks []api.ContentBlock
		if err := json.Unmarshal(out[i].Content, &blocks); err != nil {
			return msgs
		}
		reminder := api.ContentBlock{
			Type: "text",
			Text: "<system-reminder>\n" + tools.DeferredReminder(names) + "\n</system-reminder>",
		}
		blocks = append([]api.ContentBlock{reminder}, blocks...)
		if raw, err := json.Marshal(blocks); err == nil {
			out[i].Content = raw
		}
		return out
	}
	return msgs
}

// nearContextLimit: last measured input total crossed 80% of the auto-compact
// threshold — real adds a cache breakpoint on the newest content then.
func (l *Loop) nearContextLimit() bool {
	return l.lastTotal > (l.contextWindow()-20000-8000-4000)*4/5
}

// withEphemeralBreakpoint returns a copy of msgs with cache_control=ephemeral
// on the last content block of the newest user message.
func withEphemeralBreakpoint(msgs []api.Message) []api.Message {
	out := make([]api.Message, len(msgs))
	copy(out, msgs)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Role != "user" {
			continue
		}
		var blocks []api.ContentBlock
		if err := json.Unmarshal(out[i].Content, &blocks); err != nil || len(blocks) == 0 {
			return out
		}
		blocks[len(blocks)-1].CacheControl = &api.CacheControl{Type: "ephemeral"}
		if raw, err := json.Marshal(blocks); err == nil {
			out[i].Content = raw
		}
		return out
	}
	return out
}

// Ask submits one user turn and runs the tool loop to completion.
func (l *Loop) Ask(ctx context.Context, prompt string) (*api.Response, error) {
	if h := l.runHook(hooks.UserPromptSubmit, map[string]any{"prompt": prompt}); h.Block {
		return nil, fmt.Errorf("prompt blocked by hook: %s", h.Reason)
	} else {
		// plain-text hook stdout becomes additional context (real behavior)
		if h.PlainText != "" {
			prompt += "\n\n" + strings.TrimSpace(h.PlainText)
		}
		if h.AdditionalContext != "" {
			prompt += "\n\n" + h.AdditionalContext
		}
	}
	var msg api.Message
	if !l.started {
		msg = FirstUserMessage(prompt, l.CWD, l.User.Email, l.MemoryPaused)
		l.started = true
		envBlocks := api.BlocksMessage("system", []api.ContentBlock{{
			Type:         "text",
			Text:         EnvMessage(l.ToolCtx.CWD, l.Model, l.AddDirs, l.PermMode, l.Client.SessionID),
			CacheControl: &api.CacheControl{Type: "ephemeral", TTL: "1h"},
		}})
		if isXHighModel(l.Model) {
			envBlocks.OutputConfig = DefaultOutputConfig()
		}
		l.Messages = append(l.Messages, msg, envBlocks)
	} else {
		msg = api.TextMessage("user", prompt)
		// if the previous turn was interrupted before the assistant
		// replied, the env system block sits mid-array with no assistant
		// after it — the API rejects that ("system must precede an
		// assistant message or end the array"). Keep it last.
		if n := len(l.Messages); n > 0 && l.Messages[n-1].Role == "system" {
			l.Messages = append(l.Messages[:n-1], msg, l.Messages[n-1])
		} else {
			l.Messages = append(l.Messages, msg)
		}
	}
	resp, err := l.run(ctx, 0)
	if err != nil {
		return resp, err
	}
	return l.maybeGoalCheck(ctx, resp)
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
	resp, err := l.streamWithSanitizeRetry(ctx)
	if err != nil {
		return nil, err
	}
	l.lastTotal = resp.Usage.InputTokens + resp.Usage.CacheReadInputTokens +
		resp.Usage.CacheCreationInputTokens
	l.Messages = append(l.Messages, assistantMessage(resp))

	var results []api.ContentBlock
	for _, b := range resp.Content {
		if b.Type != "tool_use" {
			continue
		}
		if l.Events.OnToolUse != nil {
			l.Events.OnToolUse(b.Name, b.Input)
		}
		if os.Getenv("CG_DBGINPUT") != "" {
			fmt.Fprintf(os.Stderr, "[dbginput %s] %q\n", b.Name, string(b.Input))
		}
		var r tools.Result
		if len(l.AllowedTools) > 0 && !containsStr(l.AllowedTools, b.Name) {
			r = tools.Result{Content: fmt.Sprintf("tool %s not allowed for this agent", b.Name), IsError: true}
			results = append(results, toolResultBlock(b.ID, r))
			continue
		}
		// PreToolUse: block / updatedInput rewrite / permissionDecision
		pre := l.runHook(hooks.PreToolUse, map[string]any{
			"tool_name": b.Name, "tool_input": json.RawMessage(b.Input),
			"tool_use_id": b.ID,
		})
		if len(pre.UpdatedInput) > 0 {
			b.Input = pre.UpdatedInput // hooks can rewrite tool_input
		}
		if b.Name == "advisor" {
			// the advisor tool forwards the whole conversation to a second
			// model call and returns its advice — real client behavior
			r = l.runAdvisor(ctx)
		} else if pre.Block || pre.Permission == "deny" {
			reason := pre.Reason
			if reason == "" {
				reason = "blocked by hook"
			}
			r = tools.Result{Content: "Tool call blocked by hook: " + reason, IsError: true}
		} else if pre.Permission == "allow" {
			r = l.dispatch(b.Name, b.Input) // hook approved — skip the prompt
		} else if l.ToolCtx.Permission != nil &&
			!l.ToolCtx.Permission(b.Name, b.Input, toolSummary(b.Name, b.Input)) {
			if l.PermMode == "dontAsk" {
				r = tools.Result{
					Content: "Permission to use " + b.Name + " has been denied because Claude Code is running in don't ask mode. Only use tools that are allowed in this mode via the permission settings.",
					IsError: true,
				}
			} else {
				r = tools.Result{
					Content: "Permission for this tool use was denied. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). Try a different approach or report the limitation to complete your task.",
					IsError: true,
				}
			}
		} else {
			r = l.dispatch(b.Name, b.Input)
		}
		post := l.runHook(hooks.PostToolUse, map[string]any{
			"tool_name": b.Name, "tool_input": json.RawMessage(b.Input),
			"tool_use_id": b.ID, "tool_response": r.Content,
		})
		if r.IsError {
			l.runHook(hooks.PostToolUseFailure, map[string]any{
				"tool_name": b.Name, "tool_input": json.RawMessage(b.Input),
				"tool_use_id": b.ID, "error": r.Content,
			})
		}
		if post.AdditionalContext != "" {
			r.Content += "\n\nHook feedback: " + post.AdditionalContext
		}
		// exit-2 / decision:block from PostToolUse feeds stderr to the model
		if post.Block && post.Reason != "" {
			r.Content += "\n\n" + post.Reason
		}
		if l.Events.OnToolResult != nil {
			l.Events.OnToolResult(b.Name, r)
		}
		results = append(results, toolResultBlock(b.ID, r))
	}
	if len(results) == 0 {
		if l.ShouldAutoCompact(resp.Usage) && !l.inCompact {
			l.inCompact = true
			_ = l.compactAuto(ctx)
			l.inCompact = false
		} else {
			l.maybePrecompute(resp.Usage)
		}
		return resp, nil
	}
	l.PushUser(results)
	return l.run(ctx, depth+1)
}

// tryModels streams one request, walking the model fallback chain on
// retryable errors (429/5xx/overloaded).
func (l *Loop) tryModels(ctx context.Context) (*api.Response, error) {
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
	return resp, err
}

// SetFastMode toggles fast mode, firing Pre/PostModelSwitch hooks; a blocking
// PreModelSwitch declines the toggle (real model-switch semantics).
func (l *Loop) SetFastMode(on bool) {
	if l.FastMode == on {
		return
	}
	if h := l.runHook(hooks.PreModelSwitch, map[string]any{
		"to_model": l.Model, "fast_mode": on,
	}); h.Block {
		return
	}
	l.FastMode = on
	l.runHook(hooks.PostModelSwitch, map[string]any{
		"to_model": l.Model, "fast_mode": on,
	})
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
	res := hooks.Run(l.Hooks, ev, payload, l.ToolCtx.CWD)
	if res.NonBlockErr != "" {
		fmt.Fprintf(os.Stderr, "  hook error (%s): %s\n", ev, strings.TrimSpace(res.NonBlockErr))
	}
	return res
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

// dispatch routes a tool call, refusing calls into still-deferred tools —
// real's InputValidationError when the model invokes a name whose schema
// was never loaded via ToolSearch.
func (l *Loop) dispatch(name string, input json.RawMessage) tools.Result {
	// refusal only when the tool is actually deferred-on-the-wire — a tool
	// that's not offered at all (interactive-only in -p, gated-out) falls
	// through to its own error like real.
	for _, t := range l.ToolCtx.DeferredPool() {
		if t.Def.Name == name {
			return tools.Result{
				Content: "InputValidationError: " + name + " is a deferred tool — its schema is not loaded. Fetch it with ToolSearch (query \"select:" + name + "\") before calling it.",
				IsError: true,
			}
		}
	}
	return tools.Dispatch(l.ToolCtx, name, input)
}

func toolResultBlock(id string, r tools.Result) api.ContentBlock {
	if len(r.Refs) > 0 {
		// ToolSearch matches → tool_reference blocks (real's
		// mapToolResultToToolResultBlockParam).
		blocks := make([]api.ContentBlock, 0, len(r.Refs))
		for _, n := range r.Refs {
			blocks = append(blocks, api.ContentBlock{Type: "tool_reference", ToolName: n})
		}
		content, _ := json.Marshal(blocks)
		return api.ContentBlock{Type: "tool_result", ToolUseID: id, Content: content}
	}
	content, _ := json.Marshal(resultBlocks(r))
	if r.IsError && !strings.Contains(r.Content, "<tool_use_error>") {
		// real wraps every error result in <tool_use_error> tags
		wrapped, _ := json.Marshal([]api.ContentBlock{{
			Type: "text",
			Text: "<tool_use_error>" + strings.TrimSpace(r.Content) + "</tool_use_error>",
		}})
		content = wrapped
	}
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
	if l.AdvisorModel != "" {
		req.Model = l.AdvisorModel // the /advisor pick fronts the second call
	}
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
	if l.noThinking {
		return nil // stripped after a thinking-config 400
	}
	if l.ThinkingMode == "disabled" {
		return json.RawMessage(`{"type":"disabled"}`)
	}
	if l.ThinkingMode == "enabled" || l.ThinkingBudget > 0 || l.AlwaysThinking {
		budget := l.ThinkingBudget
		if budget <= 0 {
			budget = 10000
		}
		if v := os.Getenv("MAX_THINKING_TOKENS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n < budget {
				budget = n
			}
		}
		return json.RawMessage(`{"type":"enabled","budget_tokens":` + strconv.Itoa(budget) + `}`)
	}
	display := l.thinkingDisplay()
	return json.RawMessage(`{"type":"adaptive","display":"` + display + `"}`)
}

// ContextWarnThreshold is ~90% of the auto-compact window for the model.
func (l *Loop) ContextWarnThreshold() int {
	return (l.contextWindow() - 20000 - 8000 - 4000) * 9 / 10
}

// assistantMessage wraps the response as a full wire message for transcripts.
func assistantMessage(resp *api.Response) api.Message {
	b, _ := json.Marshal(resp)
	return api.Message{Role: "assistant", Content: mustMarshalBlocks(sanitizedBlocks(resp.Content)), Raw: b}
}

// sanitizedBlocks drops thinking blocks with empty thinking text — the API
// rejects {"type":"thinking"} with "thinking.thinking: Field required"
// (live-verified on compact replay). Signature-only thinking is never
// replayable. Fresh allocation required: filtering in place left a
// duplicate of the last element inside resp.Content's original length,
// which double-dispatched tool calls (live-verified: duplicate
// tool_result blocks → API 400).
func sanitizedBlocks(bs []api.ContentBlock) []api.ContentBlock {
	out := make([]api.ContentBlock, 0, len(bs))
	for _, b := range bs {
		if b.Type == "thinking" && b.Thinking == "" {
			continue
		}
		out = append(out, b)
	}
	return out
}

func mustMarshalBlocks(bs []api.ContentBlock) json.RawMessage {
	b, _ := json.Marshal(bs)
	return b
}

// CompactMeta returns (isCompactSummary, trigger) for the transcript writer.
func (l *Loop) CompactMeta() (bool, string) { return l.compactSummaryPending, l.pendingTrigger }

// LastInputTokens returns the last request's total input tokens (preTokens).
func (l *Loop) LastInputTokens() int { return l.lastTotal }

// capBreakpoints enforces the API's max-4 cache_control blocks across the
// WHOLE request (system breakpoints reserve slots): strips the oldest
// message breakpoints first. Verified live: resumed transcripts carry
// stale breakpoints that 400 without this.
func capBreakpoints(msgs []api.Message, sysReserved int) []api.Message {
	count := 0
	idx := make([][2]int, 0, 8) // (msg, block) of each cache_control, oldest first
	var blocksOf = func(c json.RawMessage) []api.ContentBlock {
		var b []api.ContentBlock
		json.Unmarshal(c, &b)
		return b
	}
	for i, m := range msgs {
		for j, b := range blocksOf(m.Content) {
			if b.CacheControl != nil {
				count++
				idx = append(idx, [2]int{i, j})
			}
		}
	}
	allowed := 4 - sysReserved
	if count <= allowed {
		return msgs
	}
	out := make([]api.Message, len(msgs))
	copy(out, msgs)
	dirty := map[int]bool{}
	for _, p := range idx[:count-allowed] {
		m := p[0]
		blocks := blocksOf(out[m].Content)
		if p[1] < len(blocks) {
			blocks[p[1]].CacheControl = nil
			if raw, err := json.Marshal(blocks); err == nil {
				out[m].Content = raw
				dirty[m] = true
			}
		}
	}
	return out
}
