# Behavioral delta audit: real Claude Code v2.1.288 → v2.1.294

Method: content-anchor matching between `refsrc-288/` (2183 mods) and `refsrc-294/` (2290
mods) — flag literals, command-name tables, zod wire schemas, model catalog, endpoint
lists, env-var literals, and the bundled changelog (`return'## 2.1…'` blob).
Replica column = grep over `*.go` (the replica partially targets 2.1.292 already).

Severity: **wire** = goes on the API/SDK wire; **user** = user-visible flag/string;
**internal** = daemon/telemetry/background plumbing.

## (a) Flag / subcommand deltas

| Change | 294 evidence | In replica? | Severity |
|---|---|---|---|
| `claude remote-control --allow-unattended-tool-calls` — new flag: lets cloud sessions use the folder for this run, auto-mode commands run unattended inside the sandbox; TTY-gated (`cannot start from a script, a pipe, or a Claude Code session`), writes a grant via `writeUnattendedRunGrant()` to `~/.claude/state`; conflicts with `--spawn=session`, `--session-id`, `--continue` | mod_1403.js flag parser + help text; `allow_unattended_tool_calls` telemetry field | no | user |
| `claude plugin install --marketplace <source>` — new CLI option (adds marketplace then installs). The *slash* form `/plugin install <p> --marketplace <src>` already existed in 288 | mod_1522.js `.option("--marketplace <source>",…)` calling `pluginInstallFromSourceHandler` (cf. 288 mod_1441.js: no such option) | no | user |
| `claude attach <name>` / `claude logs <name>` — partial session *name* now resolves in place of id; arg hint changed `claude logs <id>` → `claude logs <id|name>` | mod_0500.js vs 288 mod_0467.js; changelog 2.1.290 | no | user |
| `claude project purge` → `claude purge` rename | already in 288 (mod_0686.js deprecation string) — no delta | yes | — |
| Top-level subcommand dispatch (`attach,bg,bridge,daemon,doctor,plugin,rc,remote-control,respawn,…`) and `claude mcp`/`claude plugin` subcommand tables | identical sets in mod_0006 / mod_1441↔1522 / mod_1442↔1523 | n/a | — |
| Slash commands: identical name set (99 extracted both trees; `/setup-claude` still present in 294 via `xls="setup-claude"` constant, mod_1756/0303). `/autocompact` now saves the window *per model* (settings key write path, changelog 2.1.288) | mod_0416.js vs mod_0386.js | mostly | user |

## (b) Tool deltas

| Change | 294 evidence | In replica? | Severity |
|---|---|---|---|
| Agent/Task tool input schema gained `effort` param: `effort:W(Dc).optional().describe("Reasoning effort for this agent. Set this ONLY when the user, or instructions such as CLAUDE.md or a skill, explicitly ask…")` | mod_0576.js `Mo=` schema vs 288 mod_0542.js `Ro=` (no effort) | no — `agentIn` in internal/tools/misc.go lacks `effort` | wire |
| WebFetch input schema gained `offset`: "Character position in the page text to start reading from. Use it to read on through a page too long for one call…" | mod_0416.js `zqt=` vs 288 `Qfo=` | no | wire |
| Remote `claude-code-remote` MCP allowlist gained: `list_triggers, create_trigger, update_trigger, delete_trigger, fire_trigger, send_later` (+`add_repo, check_repo_access, list_repos`) | mod_0113.js `Sfo=`/`rys=` vs 288 mod_0109.js | no | wire |
| Server-side tool allowlist gained `approximate_location` (joins `current_time` etc.) | mod_0416.js `M0o` set vs 288 mod_0386.js | no | wire |
| MCP server config schema: `disableAutoBackground` on `type:"sdk"` entries | mod_0072.js `mgo=` schema | no | wire/user |
| New bundled "artifact-emulator" skill machinery: `/mnt/skills/examples/artifact-emulator`, `scripts/bootstrap.py`, `scripts/auto_look.py` (`/usr/bin/python3` `look` subcommand with `--source`/`--type-files`/`--project`/`--contract`, `pictures/marks/outline` results); gated by `CLAUDE_CODE_ARTIFACT_PREVIEW_EMULATOR` + `artifactPreviewEmulator` flag | mod_1669.js, mod_0661.js, mod_0117.js (flag list) | no | user |
| TOOL_NAMES list unchanged (`Bash,Read,…,ToolSearch,SendUserMessage`) | mod_1485.js vs 288 mod_1351.js | yes | — |
| New remote-tool-serve op `own_made_delete` + simctl verb allowlist (`launch,terminate,uninstall,get_app_container,appinfo,privacy`) | mod_1460.js | no | internal |

## (c) Wire / prompt deltas

All in the SDK stream-json schema module (288 mod_0082.js → 294 mod_0083.js) unless noted.

| Change | Evidence | In replica? | Severity |
|---|---|---|---|
| `agent_id` field added to `user` and `assistant` message schema ("ID of the subagent that produced this message: the task_id of its task events…") | mod_0083 | no | wire |
| `run_id` field added to `task_started`, `task_updated`, `task_progress`, `background_tasks_changed` | mod_0083 | no | wire |
| `awaited` field on `task_started`/`background_tasks_changed` ("True for a run of a resumed subagent that the tool call which resumed it waits for") | mod_0083 | no | wire |
| `handback_report` field on `task_notification` (handback report shown instead of `summary`) | mod_0083 | no | wire |
| `recipient_kind` field on cross-session message frames | mod_0112.js `dSr=["type","recipient","recipient_kind","content","request_id","approve"]` (288 mod_0109 lacked it) | no | wire |
| New system subtypes: `permission_check_status` (checking/done + agent_id), `file_attachments_missing`, `ui_client_fault` (phase load/render/run) | mod_0083 | no | wire |
| New control-request subtypes: `ui_prompt_autocomplete` (text+cursor), `ui_read_selection`; read_file base `kind` gains `diff_root`, `repo` (joining `workspace_root`) | mod_0083 | no | wire |
| New turn stop reason `usage_limit_reached` | mod_0083 stop-reason enum | no | wire |
| `x-claude-code-request-class` values gain `side_reply` (main/subagent/workflow/compaction/auxiliary/side_reply) | mod_0137.js | no | wire |
| Attachment/reminder kinds: +`withheld_memory`, +`elapsed_time_reminder` | mod_0624.js list vs 288 mod_0595.js; mod_0636 vs 0598 | no | wire |
| Cross-session digest tag kinds: +`non-user-message` | mod_0573.js vs 288 mod_0527.js | no | wire |
| Usage-quota bucket keys: +`wattle_ember` | mod_0304.js `qae=` vs 288 mod_0278.js | no | wire |
| Credential kinds: +`external-idp` in INFERENCE_CREDENTIAL_KINDS / KIND_PRECEDENCE / SIGN_IN_CREDENTIAL_KINDS | mod_1485.js vs 288 mod_1351.js | no | wire |
| New headers: `x-frame-content-route` (frame asset fetch), `x-claude-remote-session-origin` | mod_0537.js, mod_0137.js | no | wire |
| New endpoints: `/v1/claudeai-agents/mcp`, `/v1/code/sessions/{id}/synced_file` (signed put/begin/commit) | mod_2243.js, mod_1176.js | no | wire |
| Transcript writer accepts `control_request` entry type (joins assistant/user/stream_event/attachment/system) | mod_1285.js | ? | internal |
| Model catalog: +`claude-haiku-5-5` ("Haiku 5.5", cutoff June 2026, bedrock `us.anthropic.claude-haiku-5-5`, foundry/vertex `claude-haiku-5-5`, pricing `haiku_45`-keyed? verify), alias `haiku-5-5`, capability key `haiku55`, Vertex env `VERTEX_REGION_CLAUDE_HAIKU_5_5`. Several capability arrays dropped `mid_conv_system` | mod_0046.js vs 288 mod_0068.js; mod_1485 `haiku55`; mod_0029.js; mod_0304 alias map | no — replica knows haiku-4-5 only | wire |
| Plan-gate reasons list: `plan_mode,brief_mode,tools_missing,model_not_listed,thinking_budgeted,not_person_turn,not_plain_text,has_attachments` | mod_0624.js | ? | internal |
| Main system-prompt prologue strings unchanged ("You are Claude Code, Anthropic's official CLI…" set identical, mod_0304↔0278); tool-description templates mostly identical — a full prompt-text diff was too noisy to certify (bundler repacks); known text change: Bash-tool `run_in_background` timeout line now rounds minutes/hours via `Math.round()` | mod_0304.js | — | wire (verify separately) |
| `VERSION:"2.1.294"`, `BUILD_TIME:"2026-10-08T02:40:59Z"`, `GIT_SHA:"8f033c6ebe3d82a87f502e199307f38f5d55ccca"` | mod_0003.js | replica emits 2.1.292 | wire |

## (d) Settings / env / hook deltas

| Change | Evidence | In replica? | Severity |
|---|---|---|---|
| `CLAUDE_CODE_OVERLOADED_RETRY_BASE_DELAY_MS` — base delay for 529 backoff (changelog 2.1.292) | mod_0039.js, mod_0416.js | no | user |
| `disableAutoBackground` — new field on sdk-type MCP server config | mod_0072.js | no | user |
| Plugin/mod hook surface: new `prompt.autocomplete` event (autocomplete rows); new `ui.fault` (Client render failure); `tool.check` gains `agentId` + `ceiling` fields (`["tool","input","tool_use_id","agentId","ceiling"]`); `turn.step` result gains `serverToolUses`; `agent.spawn` now covers teammates + workflow agents (run+index args); `$.agent.list()` idle/waiting states; `$.ui.selection()`; `claude plugin validate --json` `gatingHooks` | mod_0083, mod_0153, mod_0148, changelog 2.1.289–.292 | partial (hook names exist; new fields likely absent) | user |
| New internal env vars (env registry mod_0039): `CLAUDE_AFK_COUNTDOWN_MS`, `CLAUDE_AFK_TIMEOUT_MS`, `CLAUDE_BG_BACKEND`, `CLAUDE_BG_CARRIED_PROMPTS_SHA256`, `CLAUDE_BG_RENDEZVOUS_SOCK`, `CLAUDE_BG_TCC_DISCLAIMED`, `CLAUDE_CHROME_CLASSIFIER_FLOOR`, `CLAUDE_CHROME_PERMISSION_MODE`, `CLAUDE_CODE_ARTIFACT_PREVIEW_EMULATOR`, `CLAUDE_CODE_ARTIFACT_VERSIONS`, `CLAUDE_CODE_DISABLE_PROACTIVITY`, `CLAUDE_CODE_ENABLE_CFC`, `CLAUDE_CODE_FLAG_FETCH_WAIT_MS`, `CLAUDE_CODE_GB_DISK_CACHE_WHEN_TELEMETRY_OFF`, `CLAUDE_CODE_HOSTED_DESKTOP`, `CLAUDE_CODE_HOST_SKILL_CATALOG`, `CLAUDE_CODE_MANAGED_CONFIG_PREFETCH`, `CLAUDE_CODE_PERFORCE_MODE`, `CLAUDE_CODE_PROJECT_DIR_NAME`, `CLAUDE_CODE_RELAUNCH_PROACTIVITY_{BASELINE,DECIDED,EVER_ON,LEVEL}`, `CLAUDE_CODE_REMOTE_SDK_URL`, `CLAUDE_CODE_REMOTE_TOOLS_HOST_ALLOWS_UNATTENDED`, `CLAUDE_CODE_REMOVE_PROMPT_STRINGS`, `CLAUDE_CODE_SILENT_TURN_REMINDER_SECONDS`, `CLAUDE_CODE_STREAMED_BUMBLEBEE_TEXT`, `CLAUDE_CODE_SYNC_REUSE_WITHIN_MS`, `CLAUDE_CODE_TUI_TRIAL`, `CLAUDE_CODE_USER_DIALOG_TIMEOUT_MS`, `CLAUDE_PTY_HOST_EXEC`, `MCP_TRANSPORT_POLICY_ONLY` | mostly mod_0039 env getter table | no | internal |
| `CLAUDE_CODE_DISABLE_STRUCTURED_OUTPUTS` — changelog'd under 2.1.288 but already in the 288 binary | both | ? | user |
| Settings-file hook events / permission modes / top-level settings keys: unchanged (PreToolUse… same; modes default/acceptEdits/bypassPermissions/plan/dontAsk same; schema-key diff of mod_0056↔0072 = only `disableAutoBackground`) | — | yes | — |

## (e) UI string deltas

- Bundled changelog (mod_0685.js vs 288 mod_0671.js) now tops at `## 2.1.292` and adds the full 2.1.288–2.1.292 sections (322 new lines). Notable *behavioral* entries not covered above: `$/loop` wakeup persistence, `/code-review --max-findings` (already in 288 binary), plan-mode restore on `--resume`, WebFetch 100k-char truncation notice, `NO_PROXY` honored for first-party API calls.
- Remote-control help text block gains the `--allow-unattended-tool-calls` stanza (mod_1403).
- `claude logs` usage: `<id>` → `<id|name>` (mod_0500).
- `auto mode` classifier denial text and several error strings reworded (countless small edits; representative diff work remains for full string parity).

## (f) Version-stamp sites to bump in the replica (currently emit `2.1.292`)

- `internal/api/client.go:23` — `const Version = "2.1.292"` (UA / API)
- `internal/agent/context.go:398` — `version()`
- `internal/tools/web.go:454` — `version()`
- `internal/tools/services.go:703` — `Version:` field
- `internal/mcp/client.go:356` — `clientInfo.version` (note: name is `claude-go`, real sends its own name)
- `internal/session/store.go:161,232` — transcript `"version"` fields
- `internal/repl/repl.go:1616` — stream-json init `"version"`
- Tests with `2.1.292` literals: internal/bridge/bridge_test.go, internal/repl/banner_test.go, internal/browsermcp/browsermcp_test.go
- Real's build metadata to mirror: VERSION 2.1.294, BUILD_TIME 2026-10-08T02:40:59Z, GIT_SHA 8f033c6ebe3d82a87f502e199307f38f5d55ccca (mod_0003)

## Surprises / notes

- No slash commands or top-level subcommands were added or removed — churn is all in flags of existing commands, wire schema fields, hook events, and the model catalog.
- The 288 binary's bundled changelog stops at 2.1.287, so several "Added" entries under `## 2.1.288` (e.g. `--max-findings`, `claude purge`, `CLAUDE_CODE_DISABLE_STRUCTURED_OUTPUTS`) are already in 288's code — don't treat the changelog as the delta boundary; verified by grep.
- `claude-haiku-5-5` is the only new model ID; this is the highest-value replica gap (model catalogs, alias map, Vertex region env).
- The artifact-emulator/auto_look pipeline is the largest genuinely-new module cluster in 294 (mod_1669 ~whole file, plus skill bootstrap in mod_0661) — all gated behind flags, likely inert for a CLI replica but its remote-tool action verbs are wire-adjacent.
