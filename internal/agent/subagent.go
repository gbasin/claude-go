package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"claude-go/internal/api"
	"claude-go/internal/tools"
)

// Runner implements tools.AgentRunner with a nested Loop.
type Runner struct {
	Client     *api.Client
	PermMode   string
	Model      string
	User       UserConfig
	PermRules  map[string][]string
	Entrypoint string
}

func (r *Runner) RunAgent(c *tools.Context, description, prompt, subagentType, model string, background bool) tools.Result {
	m := model
	if m == "" || m == "inherit" {
		m = r.Model
	}
	if m == "" {
		m = defaultModel
	}
	sub := &Loop{
		Client:     r.Client,
		Model:      m,
		MaxTokens:  defaultMaxTokens,
		Effort:     "medium",
		Entrypoint: r.Entrypoint,
		PermMode:   r.PermMode,
		CWD:        c.CWD,
		User:       r.User,
		PermRules:  r.PermRules,
		ToolCtx: &tools.Context{
			Ctx:        c.Ctx,
			CWD:        c.CWD,
			BgTasks:    c.BgTasks,
			Permission: c.Permission,
			Answerer:   c.Answerer,
			Searcher:   c.Searcher,
		},
	}
	if background {
		done := make(chan tools.Result, 1)
		go func() {
			done <- runAgentTurn(sub, prompt)
		}()
		select {
		case res := <-done:
			return res
		default:
			return tools.Result{Content: fmt.Sprintf("Agent %q launched in background", description)}
		}
	}
	return runAgentTurn(sub, prompt)
}

func runAgentTurn(sub *Loop, prompt string) tools.Result {
	resp, err := sub.Ask(context.Background(), prompt)
	if err != nil {
		return tools.Result{Content: "agent error: " + err.Error(), IsError: true}
	}
	var out string
	for _, b := range resp.Content {
		if b.Type == "text" {
			out += b.Text
		}
	}
	if out == "" {
		out = "(agent produced no text output)"
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
		Tools:     []api.Tool{{Name: "web_search", InputSchema: tb}},
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
