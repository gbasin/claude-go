package tools

import (
	"encoding/json"

	"claude-go/internal/api"
)

// schema_cli.go — the interactive (cli entrypoint) variants of tool
// schemas. Real 294 ships different Agent/Bash shapes on the interactive
// wire: Agent drops run_in_background and explains fork; Bash trims the
// background-timeout clause. The sdk-cli (-p) wire keeps the full forms
// (builtin_schemas.json).

const cliBashDesc = "Executes a bash command and returns its output.\n" +
	"\n" +
	"- Working directory persists between calls, but prefer absolute paths — `cd` in a compound command can trigger a permission prompt. Shell state (env vars, functions) does not persist; the shell is initialized from the user's profile.\n" +
	"- Command output is displayed to you, not reliably to the user.\n" +
	"- `timeout` is in milliseconds: default 120000, max 600000 for a foreground command.\n" +
	"- `run_in_background` runs the command detached: it keeps running across turns and re-invokes you when it exits. No `&` needed.\n" +
	"\n" +
	"# Git\n" +
	"- Interactive flags (`-i`, e.g. `git rebase -i`, `git add -i`) are not supported in this environment.\n" +
	"- Use the `gh` CLI for GitHub operations (PRs, issues, API).\n" +
	"- Commit or push only when the user asks. If on the default branch, branch first.\n" +
	"- End git commit messages and PR bodies with the attribution lines given in the conversation's system-reminder, when one is present."

const cliBashRunBgDesc = "Set to true to run this command in the background."

const cliAgentDesc = "Launch a new agent to handle complex, multi-step tasks. Each agent type has specific capabilities and tools available to it.\n" +
	"\n" +
	"Available agent types are listed in <system-reminder> messages in the conversation.\n" +
	"\n" +
	"When using the Agent tool, specify a subagent_type to select an agent: `\"fork\"` forks yourself (the fork inherits your full conversation context and always runs on your model — a `model` override is ignored); any other type — or omitting it — starts a fresh agent (general-purpose by default).\n" +
	"\n" +
	"## When to use\n" +
	"\n" +
	"Reach for this when the task matches an available agent type, when you have independent work to run in parallel, or when answering would mean reading across several files — delegate it and you keep the conclusion, not the file dumps. For a single-fact lookup where you already know the file, symbol, or value, search directly. Once you've delegated a search, don't also run it yourself — wait for the result.\n" +
	"\n" +
	"A fork runs in the background and keeps its tool output out of your context. If you are the fork, execute directly — don't re-delegate. Subagents run in the background; you'll be notified when one completes. Never fabricate or predict a pending agent's results — the notification is never something you write yourself; if the user asks before it arrives, say it's still running.\n" +
	"\n" +
	"- The agent's final report is not shown to the user — relay what matters.\n" +
	"- Use SendMessage with the agent's ID or name to continue a previously spawned agent with its context intact; a new Agent call starts fresh (except subagent_type: \"fork\", which inherits your context).\n" +
	"- Each agent type's model, reasoning effort, and tools come from its definition (`.claude/agents/*.md` frontmatter or SDK `agents`).\n" +
	"- `isolation: \"worktree\"` gives the agent its own git worktree (auto-cleaned if unchanged)."

const cliAgentEffortDesc = "Reasoning effort for this agent. Set this ONLY when the user, or instructions such as CLAUDE.md or a skill, explicitly ask that this agent or delegated work run at a specific effort level, never on your own judgment; otherwise omit it and the agent runs at its usual effort. Ignored for subagent_type: \"fork\": a fork runs at your own effort."

// applyCLISchemaVariants swaps the Agent/Bash defs for their interactive
// forms when the entrypoint is "cli" — real 294's wire-verified variants.
func applyCLISchemaVariants(c *Context, defs []api.Tool) []api.Tool {
	if c == nil || c.Entrypoint != "cli" {
		return defs
	}
	for i, t := range defs {
		switch t.Name {
		case "Bash":
			var m map[string]any
			if err := json.Unmarshal(t.InputSchema, &m); err != nil || m == nil {
				continue
			}
			t.Description = cliBashDesc
			if props, ok := m["properties"].(map[string]any); ok {
				if rib, ok := props["run_in_background"].(map[string]any); ok {
					rib["description"] = cliBashRunBgDesc
				}
			}
			t.InputSchema, _ = json.Marshal(m)
			defs[i] = t
		case "Agent":
			var m map[string]any
			if err := json.Unmarshal(t.InputSchema, &m); err != nil || m == nil {
				continue
			}
			t.Description = cliAgentDesc
			if props, ok := m["properties"].(map[string]any); ok {
				delete(props, "run_in_background")
				if eff, ok := props["effort"].(map[string]any); ok {
					eff["description"] = cliAgentEffortDesc
				}
			}
			if req, ok := m["required"].([]any); ok {
				var keep []any
				for _, r := range req {
					if r != "run_in_background" {
						keep = append(keep, r)
					}
				}
				m["required"] = keep
			}
			t.InputSchema, _ = json.Marshal(m)
			defs[i] = t
		}
	}
	return defs
}
