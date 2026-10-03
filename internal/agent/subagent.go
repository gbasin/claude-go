package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	// resolve agent type -> custom system prompt + tool restriction
	agentPrompt, allowedTools := resolveAgentType(c.CWD, subagentType)
	if agentPrompt != "" {
		prompt = agentPrompt + "\n\n" + prompt
	}
	sub := &Loop{
		AllowedTools: allowedTools,
		Client:       r.Client,
		Model:        m,
		MaxTokens:    defaultMaxTokens,
		Effort:       "medium",
		Entrypoint:   r.Entrypoint,
		PermMode:     r.PermMode,
		CWD:          c.CWD,
		User:         r.User,
		PermRules:    r.PermRules,
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

// resolveAgentType reads ~/.claude/agents/<name>.md and <cwd>/.claude/agents/<name>.md.
// Returns (system prompt, allowed tool names or nil for all).
func resolveAgentType(cwd, name string) (string, []string) {
	home, _ := os.UserHomeDir()
	for _, dir := range []string{
		filepath.Join(home, ".claude", "agents"),
		filepath.Join(cwd, ".claude", "agents"),
	} {
		b, err := os.ReadFile(filepath.Join(dir, name+".md"))
		if err != nil {
			continue
		}
		fm, body := splitFrontmatter(string(b))
		var allowed []string
		if tl := fm["tools"]; tl != "" && tl != "*" {
			for _, t := range strings.Split(tl, ",") {
				allowed = append(allowed, strings.TrimSpace(t))
			}
		}
		return body, allowed
	}
	return "", nil
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
	p, _ := resolveAgentType(cwd, name)
	return p
}
