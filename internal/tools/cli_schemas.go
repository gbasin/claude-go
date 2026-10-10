package tools

import (
	"encoding/json"

	"claude-go/internal/api"
	"claude-go/internal/extract"
)

// cli_schemas.go — the interactive (cli entrypoint) variants of tool
// schemas. Real ships different Agent/Bash shapes on the interactive
// wire: Agent drops run_in_background and explains fork; Bash trims the
// background-timeout clause. The sdk-cli (-p) wire keeps the full forms.
//
// When an extraction exists, cli defs come verbatim from the harvested
// interactive capture (tools_cli.json). The consts below are
// community-authored fallbacks — same contract, different wording.

const cliBashDesc = "Runs a bash command and returns the output.\n" +
	"\n" +
	"- The working directory carries over between calls, but prefer absolute paths — `cd` inside a compound command can trigger a permission prompt. Environment and shell functions do not persist; the shell loads the user's profile each call.\n" +
	"- Command output goes to you, not directly to the user.\n" +
	"- `timeout` is milliseconds: default 120000, max 600000 in the foreground.\n" +
	"- `run_in_background` detaches the command — it keeps running across turns and re-invokes you when it exits. No `&` suffix needed.\n" +
	"\n" +
	"# Git\n" +
	"- Interactive git flags (`-i`, e.g. `git rebase -i`, `git add -i`) are unavailable here.\n" +
	"- Use the `gh` CLI for GitHub work (PRs, issues, API).\n" +
	"- Commit or push only when asked; branch off the default branch first.\n" +
	"- When the conversation's system-reminder specifies commit/PR attribution lines, end messages and bodies with them."

const cliBashRunBgDesc = "Set true to run the command detached in the background."

const cliAgentDesc = "Launch a new agent to handle complex, multi-step tasks. Each agent type has specific capabilities and tools available to it.\n" +
	"\n" +
	"Available agent types are listed in <system-reminder> messages in the conversation.\n" +
	"\n" +
	"Pick the agent via subagent_type: `\"fork\"` clones yourself (the fork inherits your full context and your model — a `model` override is ignored); anything else — or omitting it — spawns a fresh agent (general-purpose by default).\n" +
	"\n" +
	"## When to use\n" +
	"\n" +
	"Use this when the task fits an available agent type, when independent work can run in parallel, or when the answer needs reading across many files — delegate so you keep the conclusion, not the file dumps. For a single fact you can locate directly, search yourself. Once a search is delegated, don't redo it — wait for the result.\n" +
	"\n" +
	"A fork runs in the background and keeps its tool output out of your context. If you are the fork, just do the work — don't delegate further. Agents run in the background; you're notified on completion. Never fabricate a pending agent's result — if asked before the notification lands, report that it's still running.\n" +
	"\n" +
	"- The agent's final report is not shown to the user — relay what matters.\n" +
	"- Use SendMessage with the agent's id or name to continue an existing agent with its context; a new Agent call starts fresh (except `subagent_type: \"fork\"`, which inherits yours).\n" +
	"- Each agent type's model, effort, and tools come from its definition (`.claude/agents/*.md` frontmatter or SDK `agents`).\n" +
	"- `isolation: \"worktree\"` gives the agent its own git worktree (auto-cleaned if it ends unchanged)."

const cliAgentEffortDesc = "Reasoning effort for this agent. Set it ONLY when the user — or instructions such as CLAUDE.md or a skill — explicitly ask for a specific effort level on this agent or its delegated work, never on your own judgment; otherwise omit it and the agent uses its usual effort. Ignored for subagent_type: \"fork\" — a fork inherits your effort."

// applyCLISchemaVariants swaps the defs for their interactive forms when
// the entrypoint is "cli" — harvested cli defs when extracted, else the
// community fallbacks (Bash/Agent shape patches).
func applyCLISchemaVariants(c *Context, defs []api.Tool) []api.Tool {
	if c == nil || c.Entrypoint != "cli" {
		return defs
	}
	if raw, ok := extract.JSON("tools_cli.json"); ok {
		var cli []api.Tool
		if json.Unmarshal(raw, &cli) == nil {
			byName := make(map[string]api.Tool, len(cli))
			for _, t := range cli {
				byName[t.Name] = t
			}
			for i, t := range defs {
				if nt, ok := byName[t.Name]; ok {
					defs[i] = nt
				}
			}
			return defs
		}
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
