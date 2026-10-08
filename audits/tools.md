# Tool surface audit — real claude 2.1.294 (refsrc-294/) vs Go replica

Method: extracted every tool-builder object (`qt({...})` in 294, `Vt({...})` in 288)
from the minified bundles, resolved `name:`/`inputSchema`/`description`/`isEnabled`
symbol chains across chunk modules, and cross-checked against the live wire
captures in `e2e/testdata/realwire_*.json` (claude-cli/2.1.289) and
`reference/tools.json` + `reference/interactive_tools.json` (main checkout).

## Wire model (how tools reach the API)

Real does NOT send all tools eagerly. With tool-search on (default on
first-party), `tools[]` = a small declared set + `DeferredToolPlaceholder`
(`defer_loading:true`) + the `advisor` server tool
(`{type:"advisor_20260301", name:"advisor", model, defer_loading:true}`).
Everything else carries `defer_loading` implicitly via the deferred pool and is
expanded later by `tool_reference` blocks / ToolSearch `select:` results.
Deferred decision in 294: `pte()` (refsrc-294/mod_0353.js) — deferred iff not in
the declared-inline name set, unless never-defer (`i()`: ToolSearch,
StructuredOutput, Agent when fork-subagents on, PushNotification when peer mode
on, MCP tools are `isMcp→defer` under `h()`). `shouldDefer:!0` on a tool object
is the fallback defer signal used when no declared set exists.

Eager set on the captured wire (289): Agent, Bash, Edit, Read, Write, Skill,
ToolSearch, Workflow, ListAgents, ReportFindings, ScheduleWakeup (+ Artifact,
AskUserQuestion, ReadNotifications, SendFeedback in interactive sessions).
Ours matches this via `shouldDeferTools` in internal/tools/toolsearch.go plus
the placeholder splice before Write and advisor at tail — verified against all
three golden captures (golden_wire_e2e_test.go).

## Replica inventory

builtin_schemas.json (37): Agent Artifact ArtifactComments ArtifactData
AskUserQuestion Bash CronCreate CronDelete CronList DesignSync Edit
EndConversation EnterPlanMode EnterWorktree ExitPlanMode ExitWorktree LSP
ListAgents ListMcpResourcesTool Monitor NotebookEdit PushNotification Read
ReadMcpResourceDirTool ReadMcpResourceTool ReadNotifications RemoteTrigger
ReportFindings ScheduleWakeup SendFeedback SendMessage Skill TaskStop WebFetch
WebSearch Workflow Write.
Go-generated (search.go, tasks.go, toolsearch.go, misc.go): Glob Grep LS
MultiEdit NotebookRead TodoWrite TaskCreate TaskGet TaskList TaskUpdate
ToolSearch SubagentHandback advisor DeferredToolPlaceholder.

## New in 294 (absent from 288)

- `OfferChromeSetup` (mod_0599.js, `{reason}`, deferred) — Chrome-setup offer tool.
- `PublishPlugin` (mod_1564.js, deferred, `isEnabled` gated).
- WebFetch gains an `offset` input prop ("Character position in the page text
  to start reading from..."). **Ours lacks it.**
- WebSearch gains flag-gated `mode` enum (standard|extended, prefault extended)
  via `Znt()?cs():fo()`.
- AskUserQuestion gains launch-option-gated extended schema (`title`,
  `kind:"choice"|"text"|"number"`, `description`, `placeholder`, `min`, `max`,
  `step`, `defaultValue`, `unit`) via `sSe()`/`TEo()` launch options.

## Real tools we do not implement (all gate- or product-scoped)

ObserverReport, Poll (`CLAUDE_CODE_POLL_EVENTS`), Projects
(`allow_projects_tool`), ProposeGoal, FetchInboxMessage, StructuredOutput
(internal output-format shim, never model-callable), WaitForMcpServers,
RefreshMcpTools, ListConnectors, SearchMcpRegistry, SuggestConnectors,
SuggestPluginInstall, SuggestSkills, OfferChromeSetup, PublishPlugin,
ShareOnboardingGuide (`tengu_flint_harbor_share`), ShowOnboardingRolePicker,
AppifactRepl, ClaudeDesign, PowerShell (windows), SendFile, SendUserFile,
SendUserMessage (`pewter_owl`), TestingPermission (`isEnabled()===false`,
disabled sentinel), GetTask (legacy `tengu_violin_rosin` alias for task get),
memory_{list,read,write}, self_hosted_runner_* (9), propose_skills,
ArtifactCheck (third artifact addon; ours has only ArtifactComments+ArtifactData),
`mcp` (internal).

None of these appear in the captured wire set; all are off by default or
context-scoped. Risk is only that real *can* surface them under flags we don't
honor (e.g. `tengu_violin_rosin` GetTask, extended-questions AskUserQuestion).

## We offer what real 294 does not

LS, MultiEdit, NotebookRead exist in 294 only as *permission-rule* literals
(mod_1169.js `filePatternTools`/allow lists), not registered tools — real folded
them (LS→Glob/Grep era, MultiEdit→Edit `edits` coercion, NotebookRead→Read).
Our ToolSearch deferred pool still advertises all three. Remove from
`shouldDeferTools`/registry for strict parity, or keep deliberately if callers
depend on the names (they're wire-visible via ToolSearch select).

## Per-tool table

| tool | schema-diff? | behavior-diff? | evidence | ours (file) | status | e2e | severity |
|---|---|---|---|---|---|---|---|
| Agent | zod has extra fields (name/team_name/mode/effort; cwd omitted; run_in_background/model/effort conditionally omitted) — wire capture (289) shows reduced set = ours | desc is `prompt()`-dynamic (coordinator/resume variant differs: realwire_resume Agent desc 1811 vs ours 1668; run_in_background prop dropped in that context) | mod_0576.js `_Or`; realwire_resume_fable.json | builtin_schemas.json | ok-capture, variant gaps | aux/audit/tools_more | med |
| AskUserQuestion | 294 adds flag-gated extended variant (title/kind/description/placeholder/min/max/step/defaultValue/unit) | multi-question wizard path | mod_0589.js `inputSchema(){if(sSe())be();TEo()?ge():ye()}` | builtin_schemas.json | base variant ok | none | med |
| Bash | props match; `_simulatedSedEdit` + `run_in_background` omitted per context; `...t6()` attached-machine field when `zR()` | desc context-dynamic (sCt lean/trimmed); output cap env clamp real [4000,150000] vs ours [4000,128000]; fg max timeout real max(600000, BASH_DEFAULT_TIMEOUT_MS) | mod_0416.js lxn/fxn; mod_0351.js hQr=150000; bash.go:149 | builtin_schemas.json + bash.go | ok-capture | everywhere | med |
| CronCreate/Delete/List | ok (incl. durable-flag desc swap — we implement cronToolDef) | — | tools.go cronToolDef vs X2r/J2r/Q2r | builtin_schemas.json + cron.go | ok | tools_e2e (Create/List only; CronDelete none) | low |
| DesignSync | props match | — | mod_1682.js | builtin_schemas.json | ok | tools_more | low |
| Edit | props match; alias coercion (path/old_str/new_str/replace_name) — we implement | desc: ours ships lean variant always; real has long non-lean text (+ `tengu_edit_minimalanchor_jrn` line); stale-check ours mtime+size only, real also sha256 | mod_0416.js Wkn/GFr; file.go Stale() | builtin_schemas.json + file.go | partial | audit/tools/tools_more | med |
| EndConversation | ok | — | mod_1671.js | builtin_schemas.json | ok | tools_more | low |
| EnterPlanMode | ok | — | mod_0586.js | builtin_schemas.json | ok | print/tools_more | low |
| EnterWorktree | ok | — | mod_0599.js | builtin_schemas.json | ok | tools_more | low |
| ExitPlanMode | ok | — | mod_0559.js | builtin_schemas.json | ok | lifecycle/print | low |
| ExitWorktree | ok | — | mod_0599.js | builtin_schemas.json | ok | tools_more | low |
| Glob | props match; ours lacks `$schema`/`additionalProperties:false` (zod emits both) | desc "…use the Agent tool instead (if available)" matches real's conditional Agent line | mod_0416.js Cto | search.go | ok-props | aux/hook_matrix/tools_more | med |
| Grep | ours adds `-e` (not in real); real has `-o`, `context`, `head_limit`(default 250, all modes), `offset`; desc text drift (output_mode desc now documents context/-n/head_limit) | — | mod_0416.js Oto | search.go | **schema drift** | tools_more | med |
| LS | n/a — not a real 294 tool | ours executes | mod_1169.js (legacy name only) | search.go | **ours-only** | tools_more | low-med |
| LSP | props match | — | mod_0599.js | builtin_schemas.json + lsp.go | ok | tools/tools_more | low |
| ListAgents | props match | — | mod_1659.js | builtin_schemas.json | ok | tools_more | low |
| ListMcpResourcesTool | ok | — | mod_0590.js | builtin_schemas.json | ok | mcpsse | low |
| Monitor | ours = non-persistent variant; real has `ute()?Ce():Te()` persistent variant + `inputJSONSchema` override path | expires/timeoutMs semantics; result text "Monitor started (task …)" | mod_0544.js | builtin_schemas.json | ok-capture | tools/tools_more | med |
| MultiEdit | n/a — real folds into Edit | ours standalone | mod_1169.js legacy only | search.go | **ours-only** | tools_more | low-med |
| NotebookEdit | props match | — | mod_0416.js | builtin_schemas.json | ok | lifecycle/tools_more | low |
| NotebookRead | n/a — not a real 294 tool | ours executes | mod_1169.js legacy only | search.go | **ours-only** | tools_more | low-med |
| PushNotification | props match ({message,status:const "proactive"}) | real `isEnabled`=tengu_kairos_push_notifications flag default OFF — ours offers unconditionally | mod_1681.js; tools.go order | builtin_schemas.json | **gate diff** | tools_more | med |
| Read | props match | **line format: real emits `{N}\t{line}` unpadded (`sEr`, mod_0089.js); ours emits `%6d\t` padded**; real separator can be `:` when tengu_tab_read_sep+tabs; truncation marker real `... [N lines truncated] ...` vs ours `[Truncated: PARTIAL view …]`/`[N line(s) truncated to 2000 chars]` | mod_0089.js clt/sEr; file.go:144,164 | builtin_schemas.json + file.go | **format drift** | many | high |
| ReadMcpResourceDirTool | ok | — | mod_0590.js | builtin_schemas.json | ok | none | low |
| ReadMcpResourceTool | ok | — | mod_0590.js | builtin_schemas.json | ok | mcpsse | low |
| ReadNotifications | ok | — | mod_0599.js | builtin_schemas.json | ok | none | low |
| RemoteTrigger | props match | gated both sides | mod_1690.js | builtin_schemas.json + remote.go | ok | tools_more | low |
| ReportFindings | ok | — | mod_0599.js | builtin_schemas.json | ok | none | low |
| ScheduleWakeup | props match (delaySeconds/noop/prompt/reason/stop) | eager on wire despite `shouldDefer` on object — defer is by declared-set, ours matches capture | mod_0599.js | builtin_schemas.json | ok | none | low |
| SendFeedback | props match | gate `DW()` = feedbackDrafts≠off && f1r() | mod_0599.js | builtin_schemas.json | ok | tools_more | low |
| SendMessage | real schema is flag-dependent union (`ve()` → Rs/Ms/Zs/et: subscribe-only vs message-required variants); ours ships one static variant | — | mod_1673.js | builtin_schemas.json | partial | tools_more | med |
| Skill | props match | — | mod_0586.js | builtin_schemas.json | ok | none | low |
| SubagentHandback | hidden both sides | — | mod_0568.js | misc.go | ok | aux | low |
| TaskCreate | props match | **gate: real `VZ()=HR()&&YW()` — tasks on by default (`CLAUDE_CODE_ENABLE_TASKS!==false` + todo-capable); ours requires `tasks.enabled`/`CLAUDE_CODE_TASK_TOOLS`** | mod_0599.js + mod_0345/0416 | tasks.go | gate diff | aux/hook/tools_more | med |
| TaskGet | props match | same gate | mod_0599.js | tasks.go | gate diff | tools_more | med |
| TaskList | props match (empty obj) | same gate | mod_0599.js | tasks.go | gate diff | tools_more | med |
| TaskStop | props match (task_id?, shell_id deprecated?) | — | mod_0599.js so()/Zn() | builtin_schemas.json | ok | tools_more | low |
| TaskUpdate | props match (status enum = task-status ∪ "deleted") | same gate | mod_0599.js | tasks.go | gate diff | tools_more | med |
| TodoWrite | props match ({todos:[{content,status,activeForm}]}) | **gate inverted: real enables TodoWrite only when `!HR()&&YW()` (legacy fallback when Task* off); ours offers it whenever deferred pool is on** | mod_0599.js | tasks.go | **gate diff** | tools_more/tui | med |
| ToolSearch | **wire diff: real `required:[query,max_results]`, `default:5`, `additionalProperties:false`, `$schema` key; ours has none of those** | — | realwire_print_fable.json ToolSearch block vs toolsearch.go:117 | toolsearch.go | **schema drift** | many | high |
| WebFetch | **real 294 adds `offset` prop; ours lacks it** | pagination-of-fetch behavior missing | mod_0416.js WebFetch inputSchema | builtin_schemas.json + web.go | **schema drift** | lifecycle | med-high |
| WebSearch | flag-gated `mode` prop (standard|extended, prefault extended) | — | mod_0599.js fo/cs | builtin_schemas.json | conditional diff | lifecycle | low-med |
| Workflow | props match | — | mod_1695.js | builtin_schemas.json + workflow.go | ok | none | low |
| Write | props match | — | mod_0416.js SEt | builtin_schemas.json + file.go | ok | many | low |
| advisor (server) | matches capture exactly | dispatch special-cased | toolsearch.go:109 | toolsearch.go | ok | tools_more | low |
| DeferredToolPlaceholder | matches | splice before Write verified | toolsearch.go | toolsearch.go | ok | aux | low |
| Artifact | matches capture (interactive) | — | mod_1652.js Bue | builtin_schemas.json | ok | none | low |
| ArtifactComments | ok | — | mod_1655.js | builtin_schemas.json | ok | tools_more | low |
| ArtifactData | ok | — | mod_1655.js | builtin_schemas.json | ok | tools_more | low |

## Cross-cutting diffs

1. **Generated schemas lack zod JSON-schema shape.** Our Go-built defs
   (Glob, Grep, LS, MultiEdit, NotebookRead, TodoWrite, Task*, ToolSearch,
   SubagentHandback) omit `$schema` and `additionalProperties:false` that
   zod→JSONSchema emits on every real tool (see any captured schema).
   Also several required/default attrs (ToolSearch max_results).
2. **Descriptions are context-dynamic in real** (`prompt({model, leanPrompt,
   tools, …})`); ours are static strings. Captures only cover the lean/fable
   variant — subagent (`Subagent` ctx) and non-lean sessions send different
   text (proven: Agent/Bash differ between print_fable and resume_fable).
3. **Read output format** (line-number padding, truncation markers) diverges —
   biggest routine-tool wire diff; fix `{N}\t` unpadded + real truncation
   markers.
4. **Gate drift**: Task* default-on in real vs settings-gated in ours;
   TodoWrite inverted vs ours; PushNotification flag-default-off vs ours
   always-on.
5. **Stale-file check**: real compares sha256 of read-state; ours compares
   mtime+size only (sha stored, unused).
6. **Bash output cap env clamp** ceiling 150000 real vs 128000 ours;
   `BASH_MAX_TIMEOUT_MS` floor semantics match.

## e2e gaps (per e2e/*_test.go scan)

No e2e at all: AskUserQuestion, CronDelete, ReadMcpResourceDirTool,
ReadNotifications, ReportFindings, ScheduleWakeup, Skill, Workflow, Artifact.
Golden-wire test only compares print_fable (non-interactive) — no coverage of
the interactive eager set (AskUserQuestion/SendFeedback/Artifact/
ReadNotifications on wire) or of ToolSearch-expanded deferred schemas.

## Top gaps (ranked)

1. Read line-number format + truncation markers (high, routine-path).
2. ToolSearch wire schema (required/default/additionalProperties/$schema).
3. WebFetch missing `offset` (294 addition).
4. All generated schemas missing `$schema`/`additionalProperties:false`.
5. Grep extra `-e`, missing `-o`/`context` desc drift.
6. Gate diffs: PushNotification (flag-off in real), TodoWrite vs Task*
   (inverted default), Task* default-on.
7. Ours-only tools on wire via ToolSearch: LS, MultiEdit, NotebookRead.
8. Conditional schema variants not replicated: Agent (coordinator/fork),
   AskUserQuestion extended, SendMessage 4-variant, WebSearch `mode`, Monitor
   persistent variant, Bash context omits.
9. Missing real-only gated tools (long tail; only matters under flags):
   GetTask, ArtifactCheck, OfferChromeSetup, PublishPlugin (294-new), etc.
