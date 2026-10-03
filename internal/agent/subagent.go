package agent

import (
	"context"
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
