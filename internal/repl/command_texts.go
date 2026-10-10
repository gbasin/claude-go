package repl

// Community-authored prompts for the builtin slash commands. These are
// NOT Claude Code's bundled skill prompts — they keep the same command
// contract (slot tokens like $ARG and @SLOT@, plus the removal anchors
// commands.go relies on) with minimal functional instructions.

const bundledInit = `Analyze this codebase and create or update CLAUDE.md — the file this project loads into every session's context.

Cover only what a fresh session can't cheaply derive: build/test/lint commands, architecture map, conventions worth following, and pitfalls. Keep it short.

- Do not make up information — only document what you verified by reading files.

If a CLAUDE.md exists, merge rather than overwrite.`

const bundledInitImportHint = `- If other agent config files exist (AGENTS.md, .cursor/rules, .github/copilot-instructions.md), mine them for conventions worth carrying into CLAUDE.md.`

const bundledCodeReview = `Review the code changes for correctness bugs a maintainer would want fixed.

## Scope
Run ` + "`git diff @{upstream}...HEAD`" + ` (or ` + "`git diff main...HEAD`" + ` / ` + "`git diff HEAD~1`" + `); if empty or uncommitted work exists, include ` + "`git diff HEAD`" + `. A PR number, branch, or path argument scopes the review to that target.

## Review for
- Correctness bugs: logic errors, off-by-one, nil/error mishandling, races, bad assumptions about inputs
- Security-relevant mistakes: injection, secrets handling, unsafe deserialization
- Contract breaks: changed signatures callers don't expect, missing error propagation

Also flag (secondary): reuse of an existing helper, unnecessary complexity, dead code added.

Report each finding as: file, line, one-line description, suggested fix. Do not pad — surface only actionable findings.`

const bundledSecurityReview = `Review the pending changes (git diff of the current branch or working tree) for security issues: injection, authz gaps, secrets in code or logs, unsafe deserialization, SSRF, path traversal, insecure defaults, dependency risk. Report each finding with file, line, and severity; propose the minimal fix.`

const bundledDoctor = `Health-check this Claude setup and fix what's wrong. Run every check read-only first, present a report, then confirm before applying fixes.

Check: installation health, config file validity (settings.json, .mcp.json), extensions that cost context but are unused, duplicated memory files, slow hooks, stale version.

Only propose edits to user-scope files (~/.claude/, .claude/settings.local.json, CLAUDE.local.md); treat checked-in files as review-then-ask.`

const bundledPR = `Create a pull request for the current branch's changes.

User guidance for this PR: $ARG

Workflow:
1. ` + "`git status`" + `, ` + "`git diff`" + ` (all), and ` + "`git log`" + ` to see message style.
2. Ensure you're on a feature branch; create one if on the default branch.
3. Commit the changes, push ` + "`-u`" + ` to origin.
4. ` + "`gh pr create`" + ` with a summary and test plan.
5. Report the PR URL.`

const bundledBatch = `# Batch: Parallel Work Orchestration

Orchestrate a large parallelizable change across this codebase.

## User Instruction
$ARG

## Plan
1. Enter plan mode. Research the codebase (subagents are fine) to find every file/callsite the instruction touches.
2. Split the work into 5-30 self-contained units — each implementable in an isolated git worktree and mergeable independently. Prefer per-directory or per-module slicing.
3. Determine a concrete e2e verification recipe a worker can run (dev server + curl, browser flow, or existing integration tests). If none exists, ask the user.
4. Exit plan mode for approval.

## Execute
After approval, spawn one background agent per unit via the Agent tool (` + "`isolation: \"worktree\"`" + `, ` + "`run_in_background: true`" + `), all in one message. Each agent prompt must be self-contained: the unit's files, conventions found, and the e2e recipe. Each worker reviews, tests, verifies e2e, commits, pushes, opens a PR, and ends its report with ` + "`PR: <url>`" + ` (or ` + "`PR: none — <reason>`" + `).

## Track
Render a unit/status/PR table as results arrive; summarize at the end.`

const bundledBatchUsage = `Provide an instruction describing the batch change you want to make.

Examples:
  /batch migrate from react to vue
  /batch replace all uses of lodash with native equivalents`

const bundledUpdateConfig = `Help the user configure their Claude setup via settings.json.

Rules:
- Automated behaviors ("whenever X", "each time Y") require hooks — the harness executes hooks, not the model, so memory/preferences cannot express them.
- Permissions ("allow X", "stop asking about Y") go in permissions.allow/deny/ask rules.
- Read only the keys you need from settings files — they may contain secrets; never quote env/headers values into the conversation.
- Write via targeted JSON edits, not rewrites of the whole file.
- For simple theme/model choices, suggest the /config command instead.`

const bundledHooksDocs = `Help the user configure hooks in settings.json. Hooks are shell commands the harness runs on events: PreToolUse, PostToolUse, PermissionRequest, UserPromptSubmit, SessionStart, SessionEnd, Stop, SubagentStop, Notification, PreCompact.

Shape: {"hooks":{"<Event>":[{"matcher":"<tool-name-or-regex>","hooks":[{"type":"command","command":"..."}]}]}}`

const bundledFewerPermissionPrompts = `Scan recent session transcripts in ~/.claude/projects for common read-only Bash and MCP tool calls that triggered permission prompts, then propose a prioritized permissions.allow list for the project's .claude/settings.json to reduce future prompts. Present the list before writing.`

const bundledKeybindingsHelp = `Help the user customize keyboard shortcuts. Keybindings live in ~/.claude/keybindings.json — a JSON object mapping actions to key sequences. Ask which action or key chord they want to change, show the current binding if any, and write the update.`

const bundledMemoryTypes = `Explain the memory types available: project memory (CLAUDE.md, checked in), local project memory (CLAUDE.local.md, untracked), user memory (~/.claude/CLAUDE.md), and the persistent memory directory under ~/.claude/projects/<slug>/memory/. Recommend where different kinds of notes belong.`

const bundledSchedule = `Help the user schedule, update, list, or run cloud agents (routines) via the RemoteTrigger tool.

@FIRST_STEP@
@SETUP_NOTES_BLOCK@
RemoteTrigger actions: list, get, create, update, run, list_runs, get_run_log. Load it via ToolSearch ("select:RemoteTrigger") if not present. Routines cannot be deleted here — direct the user to the web UI for that.

Recurring schedule body: {"name":..., "cron_expression":"5-field UTC, min 1h", "enabled":true, "job_config":{"ccr":{"environment_id":"@ENVIRONMENTS@ id", "session_context":{"model":"@MODEL@","sources":[{"git_repository":{"url":"@GITREPO@"}}],"allowed_tools":["Bash","Read","Write","Edit","Glob","Grep"]},"events":[{"data":{"uuid":"<v4 uuid>","session_id":"","type":"user","parent_tool_use_id":null,"message":{"content":"PROMPT","role":"user"}}}]}}}

One-time runs replace cron_expression with "run_once_at" RFC3339 UTC (future). Local timezone is @TZ@; invoked at @NOWLOCAL@ local / @NOWUTC@ UTC — re-check ` + "`date -u`" + ` before computing run_once_at.

Available MCP connectors:
@CONNECTORS@

@CREATED_ENV_NOTE@`

const bundledCommit = `Create a git commit.

## User guidance

$ARG

## Workflow
1. Run in parallel: ` + "`git status`" + `, ` + "`git diff`" + ` (staged and unstaged), ` + "`git log --oneline -10`" + `.
2. Draft a concise message in this repo's style — say what and why.
3. Stage relevant files only; skip secrets and unrelated changes.
4. Commit, then ` + "`git status`" + ` and report briefly.`

const bundledCommitPushPR = `Create a commit, push the branch, and open a pull request.

## User guidance

$ARG

## Workflow
1. Run in parallel: ` + "`git status`" + `, ` + "`git diff`" + `, ` + "`git log --oneline -10`" + `, ` + "`git branch --show-current`" + `.
2. Create a descriptively named branch if on the default branch.
3. Commit in this repo's style.
4. ` + "`git push -u origin <branch>`" + `.
5. ` + "`gh pr create`" + ` with summary + test plan; end with ` + "`PR: <url>`" + `.`

const bundledVerify = `Verify a code change end-to-end by exercising the affected flow — not just tests or typecheck.

1. Read the diff and state the intended behavior in one line.
2. Find how to drive it for real: launch the app, hit the endpoint, run the CLI path, click the flow.
3. Drive it; capture output, errors, screenshots.
4. Report PASS/FAIL with observed evidence. If it fails, state the observed-vs-expected gap and stop — do not fix in this pass.
If the diff has no runtime surface, say so and stop.`

const bundledSimplify = `Review the changed code for reuse, simplification, efficiency, and altitude — then apply the fixes. Do not hunt for correctness bugs; that is /code-review's job.

Scope: ` + "`git diff @{upstream}...HEAD`" + ` (fallback ` + "`main...HEAD`" + ` / ` + "`HEAD~1`" + ` / ` + "`HEAD`" + `), or the passed target.

Angles — reuse (call existing helpers), simplification (remove redundant state/nesting/dead code), efficiency (no redundant I/O, no serial-independent calls, no closure-captured lifetimes), altitude (fix the mechanism, not the symptom).`

const bundledSetupClaude = `Guide the user through setting up Claude Code for how they work.

Use the AskUserQuestion tool for choices — never ask questions as plain text.

1. Ask what they mainly work on (features, debugging, infra, data, other).
2. Offer setup actions as multi-select, then do the picks: install a plugin (list /plugin marketplaces first), try a bundled skill (/init), connect MCP servers, add permission rules to .claude/settings.json.
3. Close with a summary and one suggested next step.`

const bundledStatuslineAgent = `Design and write a status line script for this project: read the JSON session info piped on stdin (model, cwd, transcript path) and print a compact one-line status. Save it where settings.json references it via statusLine config.`

const bundledDebug = `Investigate the reported issue end to end: reproduce it, trace the code path, add targeted logging to isolate the cause, identify the root cause, and propose the minimal fix. Verify the fix addresses the root cause, not just the symptom.

@DAEMON@`

const bundledDebugNoDaemon = `Note: no session daemon is running — check logs and transcripts under ~/.claude/projects/ directly.`
