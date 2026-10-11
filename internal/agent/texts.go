package agent

import (
	"strings"

	"github.com/gbasin/claude-go/internal/extract"
)

// This file holds community-authored fallback text used when no
// extraction exists (~/.claude-go/extracted). These are NOT Claude Code's
// bundled prompts — run `claude-go extract` to harvest the real text from
// a local claude install for byte-exact parity.

// extractedSystemMain returns the harvested main+session block for the
// model class, tokens re-rendered for the live environment, when an
// extraction exists.
func extractedSystemMain(model, cwd, entrypoint string) (string, bool) {
	if entrypoint == "cli" {
		// the interactive capture carries the cli-only session lines
		if t, ok := extract.Text("system_main_cli_" + modelClass(model) + ".txt"); ok {
			return extract.Render(t, cwd), true
		}
	}
	t, ok := extract.Text("system_main_" + modelClass(model) + ".txt")
	if !ok {
		return "", false
	}
	return extract.Render(t, cwd), true
}

// modelClass buckets model ids the way real's session block does.
func modelClass(model string) string {
	switch {
	case strings.Contains(model, "fable"):
		return "fable"
	case strings.Contains(model, "opus"), strings.Contains(model, "sonnet"):
		return "opus"
	default:
		return "haiku"
	}
}

// fallbackSystemMain is the harness-head prompt (claude-go community
// text). Real's prompt is several hundred lines of guidance; this keeps
// the same role/behavior contract at ~20 lines.
const fallbackSystemMain = `You are an interactive coding agent that runs in the user's terminal.

You operate inside a project directory with tool access to read and edit files, run shell commands, search the codebase, and fetch documentation. Prefer acting over asking: make the change, verify it, then report concisely.

Working conventions:
- Investigate before changing: read the relevant code, follow imports, understand the conventions already present.
- Use the provided tools rather than asking the user to run commands. When a command is dangerous or irreversible, state what you will do first.
- Keep terminal output terse: lead with the answer, no filler.
- After making changes, verify them (build, test, or the closest available check) before claiming completion.
- Never fabricate results, files, or tool output. If you are unsure, say so.
- Tool calls run under the session's permission mode; a denial means the user declined — adapt instead of retrying the same call.

Hooks may intercept tool calls; treat hook output as user feedback.`

// fallbackSessionLong / fallbackSessionShort are the session-context
// block variants (long for newer frontier models, short for opus-class,
// omitted for haiku) with the same template slots real's block uses.
const fallbackSessionLong = `Persistent memory is stored in {{MEMDIR}} — read it for prior context and write durable notes there.
{{BANG}}{{DEFERRED_NOTE}}<total_tokens>{{TOTAL_TOKENS}} tokens left</total_tokens>{{MCP_BLOCKS}}`

const fallbackSessionShort = `Memory directory: {{MEMDIR}}.
{{BANG}}{{DEFERRED_NOTE}}<total_tokens>{{TOTAL_TOKENS}} tokens left</total_tokens>{{MCP_BLOCKS}}`

// fallbackCompactPrompt replaces the bundled /compact instruction text.
const fallbackCompactPrompt = `Write a detailed summary of the conversation so far for continuation by a fresh context. Include: the user's goal and constraints, decisions made, file paths touched with key details, code references, current state of any in-progress work, and all open tasks. Write it so a reader with no prior context can continue seamlessly.

<analysis>
First skim the messages to note important facts.
</analysis>
<summary>
The summary itself.
</summary>`

// fallbackBuiltinSkills is the builtin skill index format real emits in
// the env message — skill names only, community one-line descriptions.
const fallbackBuiltinSkills = `- init: Initialize a new CLAUDE.md file with codebase documentation
- code-review: Review the current diff or a PR target for correctness bugs and cleanups
- simplify: Review changed code for reuse and simplification cleanups, then apply them
- security-review: Complete a security review of pending changes on the current branch
- loop: Run a prompt or slash command on a recurring interval (e.g. /loop 5m /foo)
- run: Launch this project's app and verify a change works end to end
- update-config: Configure settings.json — hooks, permissions, env vars
- keybindings-help: Customize keyboard shortcuts and keybindings
- fewer-permission-prompts: Propose a permission allowlist to reduce prompts
- plugin-authoring: Make or debug a plugin, slash command, or hook module
- workflow-authoring: Reference for writing Workflow tool scripts
- claude-api: Claude API / Anthropic SDK reference — model ids, pricing, params
- dataviz: Charts, graphs, plots, dashboards, and other data visualization
`

// fallbackGeneralPurpose is the general-purpose subagent's prompt.
const fallbackGeneralPurpose = `You are a general-purpose subagent working inside a larger coding session. Perform the task the orchestrator delegated using your tools, then report the result concisely. Do not ask the user questions; you have no direct channel to them.`
