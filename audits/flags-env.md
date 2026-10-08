# Flags / Env / Settings audit — claude-go vs real `claude` v2.1.294

Sources: `refsrc-294/mod_0093.js` (argv scanner: value-flag set `i`, multi-value
set `IEr`, optional-value set `a`, boolean set `p`), `refsrc-294/mod_0724.js`
(commander program: option defs, `.choices()`, argParsers, descriptions),
`refsrc-294/mod_0706.js` (startup validation errors), `refsrc-294/mod_0072.js`
(settings zod schema + env-var groups), `refsrc-294/mod_0083.js` (enums),
`refsrc-294/mod_0198.js` (`--effort` parser `Drt`). Ours: `flags.go`,
`main.go`, `internal/config/{config,settings}.go`.

Severity classes: **wire** (changes API request/headers/URL or request identity),
**user** (observable behavior, wrong prompt/exit/error), **cosmetic**.

## CLI flags

| item | real behavior + evidence | ours (file:line) | status | e2e | severity |
|---|---|---|---|---|---|
| `--allow-dangerously-skip-permissions` | Enables bypass *as an option*, does NOT enable it ("without it being enabled by default", mod_0724) | sets `permMode=bypassPermissions` outright — flags.go:267 | divergent | NONE (only `--dangerously-skip-permissions` tested: TestFlagDangerouslySkipPermissions) | **wire** — sessions bypass permissions when real would not |
| `--permission-mode <mode>` | choices `[default acceptEdits bypassPermissions plan dontAsk auto]`, commander errors on invalid (mod_0724 `choices(Ue)`; enum mod_0083) | any string → `loop.PermMode` → lands in safeguards `classifier_context.permission_mode` on the wire — flags.go:119, internal/agent/context.go:346 | divergent | TestFlagPlanModeRequired (mode via different flag) | **wire** — invalid mode sent to API; "dontAsk" works but invalid strings leak |
| `--effort <level>` | normalized via `Drt` (mod_0198): low\|medium\|high\|xhigh\|max, aliases `med`→medium, `ultracode`→xhigh; unknown → stderr warning + ignored | any string → `output_config.effort` on the wire (loop.go:171-198); usage() even advertises "auto" which real rejects | divergent | TestFlagEffort/TestEffortAccepted | **wire** — `--effort bogus` → API 400 in ours, warning in real |
| `--thinking <mode>` | choices `[enabled adaptive disabled]`; "enabled (equivalent to adaptive)" (mod_0724) | `enabled` → `{"type":"enabled","budget_tokens":10000}` (loop.go:999-1007); any other string silently → adaptive; no validation | divergent | TestFlagThinkingDisabled | **wire** — enabled≠adaptive on the wire; invalid values pass |
| `--thinking-display <display>` | choices `["summarized","omitted","highlights"]` (`$qe`, mod_0083) | accepts anything; only "updates"/"omitted" recognized downstream (loop.go:921); usage() advertises "updates" which real doesn't accept | divergent | TestThinkingDisplay | **wire** — `updates` rejected by API; `summarized`/`highlights` unreachable |
| `--task-budget <tokens>` | "API-side task budget in tokens (output_config.task_budget)" (mod_0724) | parse-only → `CLAUDE_GO_TASK_BUDGET` env; never reaches `output_config` — flags.go:191 | divergent | NONE | **wire** — request param dropped |
| `--allowedTools`/`--allowed-tools` / `--disallowedTools`/`--disallowed-tools` / `--tools` | variadic `<tools...>` AND each value is comma-**or space**-separated (mod_0093 `IEr`, mod_0724 desc, mod_0390 `Wbt`) | single value, comma-split only (flags.go:145-149, main.go:757-765); extra bare args fall into the prompt | divergent | TestFlagAllowedDeniedTools, TestFlagDisallowedTools, TestFlagToolsRestricts | **user** — `--allowedTools A B` makes "B" the prompt; `"Bash Read"` becomes one bogus rule |
| `--add-dir <dirs...>` | variadic (mod_0724 `<directories...>`) | single value per flag; extra dirs become prompt — flags.go:152 | divergent | TestFlagAddDir | **user** — silent prompt corruption |
| `--file <specs...>` | "File resources to download at startup. Format: file_id:relative_path" (mod_0724) — API-hosted file download, variadic | reads local file contents into prompt — flags.go:155, main.go:669-672 | divergent | TestFlagFile, TestFileAttach | **user** — different feature entirely (local attach vs file_id download) |
| `--mcp-config <configs...>` | variadic, "JSON files or strings (space-separated)" (mod_0724) | single file path only; inline JSON ignored (os.ReadFile fails silently) — main.go:1151-1166 | divergent | TestFlagStrictMCPConfig | **user** |
| `--betas <betas...>` | variadic (mod_0724) | single value; extra args → prompt — flags.go:154 | divergent | TestBetasHeader | **user** |
| `--channels <servers...>` | variadic; registers session for MCP channel notifications (mod_0724) | single value → `CLAUDE_GO_CHANNELS` env (consumed in mcp client.go) | divergent | NONE | **user** |
| `--dangerously-load-development-channels <servers...>` | variadic (mod_0724) | single value → env, read in internal/mcp/client.go | divergent | NONE | **user** |
| `--max-turns <n>` | argParser throws "must be a number" on NaN (mod_0724 `Yt`) | `Sscanf` failure → silently 0 (unlimited) — flags.go:122 | divergent | TestFlagMaxTurns | **user** — `--max-turns abc` silently unlimited |
| `--max-thinking-tokens <n>` | same `Yt` parser | silent 0 — flags.go:123-127 | divergent | TestMaxThinkingTokens | user |
| `--max-budget-usd <amount>` | argParser errors "must be a positive number greater than 0"; budget enforced (mod_0724) | ParseFloat, no validation; usage admits "not enforced" — flags.go:128 | divergent | TestMaxBudget | **user** — budget flag is a no-op |
| `--session-id <uuid>` | "must be a valid UUID"; cannot combine with `-c/-r/--teleport`, `--init-only` (mod_0706 `ok:!1` errors) | no UUID check, no combination check — flags.go:181, main.go:343 | divergent | TestFlagSessionID/TestSessionIDFlag | **user** |
| `--system-prompt <s>` | replaces the default system prompt (print mode) | written into `loop.AppendSysP` → appended as an extra system block — main.go:657-658 | divergent | TestFlagSystemPrompt | **wire/user** — replace vs append |
| `--system-prompt` vs `--append-system-prompt` | distinct fields; both can coexist (mod_0706 `systemPrompt`/`appendSystemPrompt`) | same `AppendSysP` slot — last write wins; both given → append flag silently lost | divergent | TestFlagAppendSystemPrompt+TestFlagSystemPrompt (not combined) | user |
| `--system-prompt-file <f>` | ENOENT → `Error: System prompt file not found: <path>` fatal (mod_0706) | ReadFile error silently ignored — main.go:652-655 | divergent | TestFlagSystemPromptFile | **user** — missing file silently runs with no sys prompt |
| `--append-subagent-system-prompt` + `--append-subagent-system-prompt-file` | mutually exclusive → "Please use only one" error (mod_0706) | file flag overwrites env, no check — flags.go:167-176 | divergent | TestAppendSubagentSystemPrompt | user |
| `--debug` / `-d` | optional value `[filter]` e.g. `"api,hooks"` / `"!1p,!file"` (mod_0093 `a` set, mod_0724 `-d, --debug [filter]`) | boolean — `--debug api` puts "api" in the prompt — flags.go:246-247 | divergent | TestVerboseDebug | **user** — prompt corruption |
| `--debug-file <path>` | "implicitly enables debug mode" (mod_0724) | redirects stderr; does NOT set debug — main.go:1012 | divergent | TestFlagDebugFile | user |
| `--tmux` | bool; `--tmux=classic` accepted per help text (mod_0724: "use --tmux=classic"); requires --worktree (mod_0724) | requires --worktree ✓ but `--tmux=classic` → `fatal("does not take a value")` — the `"--tmux=classic"` boolFlags key is dead code because `name` is pre-split at `=` — flags.go:271-272, 337-340 | divergent | TestTmuxFlag | **user** |
| `--forward-home-settings <v>` | choices `true|false` (+`1`,`0` per `.choices(["true","false","1","0"])`), requires `--cloud`/`--environment` (mod_0724/0706 error) | true|false only, no --cloud requirement — flags.go:210-216 | divergent | NONE | user |
| `--output-format` | choices `[text json stream-json]` — commander error on invalid (mod_0724) | any string; unknown silently behaves like text-ish path — flags.go:120, main.go:1441+ | divergent | TestPrintJSON (valid values only) | **user** |
| `--input-format` | choices `[text stream-json]` (mod_0724) | only `stream-json` branch; other values silently ignored — main.go:1020 | divergent | TestInputFormatStreamJSON | user |
| `--restricted` | removes code-running tools (Bash/PowerShell/WebFetch unless `--tools` names them), ignores user/project/local settings, refuses bypassPermissions, permission-handler-gated writes (mod_0724 desc) | stub env → only gates `--teleport` — flags.go:281ish, internal/bridge/teleport.go:89 | divergent | NONE | **user** — flagship flag does nothing |
| `--safe-mode` | sets `CLAUDE_CODE_SAFE_MODE=1` + `CLAUDE_CODE_DISABLE_CLAUDE_MDS=1`, strips CLAUDE.md/skills/plugins/hooks/MCP/commands/agents/output-styles (mod_0706 `Pr()`, mod_0724 desc) | `CLAUDE_GO_SAFE_MODE` env → browsermcp wiring only — flags.go:280, main.go:2009 | divergent | NONE | **user** |
| `--init` / `--maintenance` | run Setup hooks with init/maintenance trigger, then continue (mod_0724) | stub note "accepted but not supported" — flags.go:274, main.go:393 | divergent | NONE | user |
| `--resume <v>` (`-r`) | session ID **or interactive picker search term** (mod_0724 `(s)=>s||!0`) | non-ID value → `session.Load` error; picker only for bare `-r` — main.go:416-441 | divergent | TestResume, TestResumePickerInteractive | user |
| `--ide` / `--desktop` | auto-connect IDE / desktop integration (mod_0724) | noted stubs (`note: --ide accepted but not supported`) — flags.go:269-270 | divergent | NONE | user |
| `--cloud`, `--remote`, `--remote-control`, `--rc` | real cloud sessions: creates/resumes/validates; many error paths (mod_0706) | stub notes; `--cloud` marks teleport only — flags.go:308-311 | divergent | NONE | user (feature absent) |
| `--environment`, `--pool`, `--ref`, `--on-branch` | cloud-session params; `--ref`/`--on-branch` without `--cloud` → error (mod_0706) | silent env stubs — flags.go:193-200 | divergent | NONE | user |
| `--attach-serve <id>` | validates `cse_...` prefix, errors (mod_0706) | env stub + note — flags.go:204-207 | divergent | NONE | user |
| `--await-initialize` / `--await-claim` | stdin control-request handshake for SDK hosts (mod_0724) | env stubs — flags.go:281 | divergent | NONE | wire-adjacent (SDK protocol) |
| `--session-mirror` | emits transcript_mirror frames on stdout (mod_0724, hidden) | env stub — flags.go:281 | divergent | NONE | user |
| `--watch-artifact`, `--watch-artifact-no-autoreact` | subscribe to claude.ai artifact updates (mod_0724) | env stubs — flags.go:193 + prefix rule | divergent | NONE | user |
| `--handle-uri` | deep-link URI handling | env stub — flags.go:190 | divergent | NONE | user |
| `--deep-link-repo`, `--deep-link-last-fetch`, `--deep-link-cwd-b64`, `--deep-link-origin` | deep-link trampoline params; `-cwd-b64` base64-decoded, `-last-fetch` parsed to number (mod_0724 argParsers) | generic `--deep-link-*` → env string, no decode — flags.go:362-369 | divergent | NONE | user |
| `--reply-on-resume` | resume then immediately query when tail is user msg (mod_0724) | env stub — flags.go:283 | divergent | NONE | user |
| `--disable-slash-commands` | "Disable all skills" (mod_0724) | env stub, no consumer found | divergent | NONE | user |
| `--forward-subagent-text` | forwards subagent text (SDK) | env stub | divergent | NONE | user |
| `--exclude-dynamic-system-prompt-sections` | strips cwd/env/git-status sections from sys prompt (mod_0724) | env stub — flags.go:281 | divergent | NONE | **wire** — sys prompt shape differs |
| `--permission-prompt-tool <t>` | MCP tool for permission prompts, print mode (mod_0724) | env → read in repl/dialogs.go:102 (perm-prompt delegation works in print mode per e2e) | partial | TestPermPromptTool (e2e/mcpsse_e2e_test.go:591) | user |
| `--permission-prompts <target>` | permission prompt routing | env stub | divergent | NONE | user |
| `--proactivity <level>` | `.choices(Mee)` — validated enum | any string → env — flags.go:196 | divergent | NONE | cosmetic |
| `--teammate-mode <mode>` | choices auto\|tmux\|iterm2\|in-process | same validation — flags.go:219-226 | parity | NONE | cosmetic |
| `--system-prompt-snapshot <on\|off>` | value flag; label digest plumbing | env → `loop.SysPromptSnapshot` label in init meta — main.go:587 | parity-ish | NONE | cosmetic |
| `--managed-settings <json>` | takes a JSON value from the spawning parent (mod_0724 `<json>`) | value treated as a file PATH (`CLAUDE_GO_MANAGED_SETTINGS` → `managedSettingsPath()`) — config.go:193 | divergent | TestManagedSettingsFlag | **user** — real callers pass JSON, ours ReadFile's it |
| `--settings <file-or-json>` | file path OR inline JSON; applied as a full settings tier (all keys) | file/JSON both accepted but ONLY `permissions.{allow,deny,ask}` + `hooks` applied; model/env/etc. ignored — main.go:768-798 | divergent | TestFlagSettingsFile, TestFlagSettingsJSON | **user** |
| `--setting-sources <list>` | user/project/local tiers filter | env `CLAUDE_GO_SETTING_SOURCES` + `managed` extra layer — config.go:389 | parity-ish | TestSettingSources, TestSettingSourcesSkipsMCP | cosmetic |
| `--project-config-root <dir>` | project .claude root override | `SetProjectConfigRoot` wired — config.go:402 | parity | NONE | cosmetic |
| `--fork-session` | new session id on resume | ✓ wired | parity | TestFlagForkSession/TestForkSession | — |
| `--continue`/`-c`, `--resume`/`-r` | most-recent / picker+id | ✓ (minus search-term) | parity | TestContinueFlag/TestResume | — |
| `--fork-session` combos | real rejects `--session-id` with -c/-r unless --fork-session | no check | divergent | NONE | user |
| `--include-hook-events` | hook events in stream-json | env `CLAUDE_GO_INCLUDE_HOOK_EVENTS` → OnHookEvent — main.go:1447 | parity | TestIncludeHookEvents | — |
| `--include-partial-messages` | stream_event partials | ✓ — main.go:1455+ | parity | TestFlagPartialMessages | — |
| `--replay-user-messages` | echoes user msgs on stream-json input | ✓ flagReplayUser | parity | TestFlagReplayUser | — |
| `--print`/`-p` | print mode | ✓ | parity | TestPrint* | — |
| `--output-format stream-json` | JSONL frames | ✓ | parity | TestPrintStreamJSON | — |
| `--model` | alias or full id; alias env overrides | ✓ resolveModel — main.go:1762 | parity | TestFlagModel | — |
| `--agent <name>` | agent override | ResolveAgentPrompt → AppendSysP — main.go:673 | parity-ish (replace-vs-append nuance) | TestFlagAgent/TestAgentFlag | cosmetic |
| `--agents <json-or-file>` | inline defs (also accepts FILE path per flag sig) | inline JSON only — a path arg fails JSON parse → fatal | divergent | TestFlagAgents | user |
| `--json-schema <s>` | structured output `output_config.format` | ✓ loop.OutputSchema — loop.go:200 | parity | TestFlagJSONSchema/TestJSONSchemaWire | — |
| `--fallback-model <m>` | fallback chain | ✓ loop.FallbackModel | parity | TestFlagFallbackModel | — |
| `--advisor <m>` | advisor model | ✓ env → loop.AdvisorModel | parity | TestAdvisorFlag | — |
| `--correlation-id <id>` | `output_config.correlation_id` | ✓ — loop.go:194 | parity | TestCorrelationID | — |
| `--workload <tag>` | billing-header `cc_workload` | ✓ CLAUDE_GO_WORKLOAD → context.go:509 | parity | TestFlagWorkload/TestWorkloadTag | — |
| `--autocompact <auto\|tokens>` | argParser 100k–1M or `auto` | ✓ same bounds — main.go:304-310 | parity | TestAutocompactFlag | — |
| `--session-id` | UUID | accepted unconditionally | divergent | TestFlagSessionID | user |
| `--verbose` | "Override verbose mode setting from config" | ✓ sets debug channels | parity | TestVerboseDebug | — |
| `--debug`/`--verbose` channel envs | `DEBUG`/`DEBUG_*` namespaces; `CLAUDE_CODE_DEBUG_LOG_LEVEL`, `CLAUDE_CODE_DEBUG_LOGS_DIR` | `CLAUDE_GO_DEBUG{,_KB,_HTTP,_REQ}` | divergent | NONE | cosmetic |
| `--append-system-prompt(-file)` | append to default sys prompt | ✓ file ENOENT fatals ✓ — flags.go:130-139 | parity | TestFlagAppendSystemPrompt(File), TestAppendSystemPromptWire | — |
| `--plan-mode-instructions` | plan-mode workflow body | ✓ env → tools/interactive.go:514 | parity | TestPlanModeInstructions | — |
| `--plan-mode-required` | start in plan mode | ✓ permMode=plan | parity | TestFlagPlanModeRequired/TestPlanModeRequired | — |
| `--enable-auto-mode` | deprecated; opt in to auto | ✓ permMode=auto | parity | TestAutoMode* | — |
| `--no-session-persistence` | no transcript write | ✓ noPersist — main.go:879 | parity | TestFlagNoPersistence | — |
| `--resume-session-at` / `--resume-drops-turn` | resume at message id + drop-guard | ✓ flags.go:583-698 | parity | TestFlagResumeSessionAt/TestResumeDropsTurn | — |
| `--rewind-files` | restore files at user msg | ✓ requires --resume, standalone | parity | TestFlagRewindFiles? (TestRewindFiles/TestRewindRestoresFile) | — |
| `--worktree`/`-w` | create worktree + cd | ✓ .claude/worktrees — flags.go:535+ | parity-ish (naming `worktree-<name>` vs real's scheme) | TestFlagWorktree/TestWorktreeRoundTrip | cosmetic |
| `--project <dir>` | different project dir | ✓ chdir — main.go:331 | parity | TestFlagProject | — |
| `--teleport [session]` | resume cloud session | ✓ teleportResume — main.go:1082+ | parity | TestFlagTeleportNoSessions | — |
| `--from-pr [ref]` | session linked to PR / picker | scans local transcripts for PR link — main.go:396-407 | partial | TestFlagFromPR | user |
| `--prefill` / `--prefill-b64` | seed input buffer; b64=base64url | ✓ but ours decodes `base64.StdEncoding` (main.go:600) while real uses `base64url` argParser (mod_0724) — payloads containing `-`/`_` fail silently | divergent | TestPrefillFlag/TestPrefillWire | user |
| `--prompt-suggestions [v]` | optional value | ✓ env | parity | NONE | — |
| `--strict-mcp-config` | only --mcp-config servers | ✓ — main.go:1153 | parity | TestFlagStrictMCPConfig | — |
| `--bare` | sets `CLAUDE_CODE_SIMPLE=1` | ✓ same env — main.go:567 | parity | TestBareMode | — |
| `--bg`/`--background` | background agent + id | ✓ bg.Spawn | parity | TestBgSessionLifecycle | — |
| `--init-only` | onboarding/migrations then exit | ✓ — main.go:389 | parity | TestFlagInitOnly/TestInitOnly | — |
| `--plugin-dir`, `--plugin-dir-no-mcp`, `--plugin-url` | plugin roots (dir repeatable, url space-split) | env → plugins.go:69 (dir and dir-no-mcp; url→CLAUDE_GO_PLUGIN_URL consumer? env set, plugin fetch path unverified) | partial | TestPluginDirHooks | user |
| `--no-chrome`/`--chrome` | enable/disable Claude-in-Chrome | env `CLAUDE_GO_CHROME` → main.go:1922 | partial | TestStartupChrome | cosmetic |
| `--name`/`-n` | session display name | ✓ sessionName | parity | TestFlagName | — |
| `--tools` | restrict tool surface; `"default"`/`"all"` keywords | comma-split → loop.AllowedTools; keywords unverified — main.go:760 | partial | TestFlagToolsRestricts | user |
| `--inherit-permission-mode` | inherit session perm mode | env → session.InitMode — main.go:968 | partial | TestInheritPermissionMode | cosmetic |
| `--exec`/`-m` | NOT a real commander option (argv-scanner value flag only; mod_0093 `i`/`m` sets) | implemented: `--exec <p>` → print-mode prompt — flags.go:157-158 | extra | TestFlagExec | cosmetic |
| `--mock`, `--mock-port`, `--write-session-id`, `--claude-in-chrome-mcp`, `--chrome-native-host` | not in real | Go-only extras | extra | TestUnknownFlagWriteSessionID asserts rejection, mock tests | cosmetic |
| `-v`/`-V`/`--version`, `-h`/`--help` | version/help | ✓ custom usage text (real renders commander help) | parity-ish | TestHelpFlag/TestVersionFlag | cosmetic |
| `--enable-auth-status` | SDK auth status messages | env stub | divergent | NONE | user |
| `--session-mirror`, `--client-data-url`, `--remote-control-session-name-prefix`, `--parent-session-id`, `--team-name`, `--agent-id`, `--agent-name`, `--agent-color`, `--agent-type`, `--routine`, `--task-budget`, `--workload` env stubs | SDK/teammate plumbing | `CLAUDE_GO_*` envs; several wired (workload, team fields via env) | partial | NONE | user |

## Env vars

Canonical real list: `process.env.*` reads (401 names) ∪ mod_0039 export table
(~1099 names, mostly feature flags). Below: every documented/user-facing or
behavioral var; the ~900 internal tuning/feature-kill envs
(`CLAUDE_CODE_*_MS`, `CLAUDE_CODE_<codename>`, `SELF_HOSTED_RUNNER_*`, OTEL
transport details, cloud/artifact/bg plumbing) are **missing** wholesale —
Go replica implements none of them.

| env var | real (evidence) | ours | status | e2e | severity |
|---|---|---|---|---|---|
| ANTHROPIC_API_KEY | auth | auth.go:60 | parity | TestAuthAPIKeyHeader | — |
| ANTHROPIC_AUTH_TOKEN | auth bearer | auth.go:57 | parity | — | — |
| CLAUDE_CODE_OAUTH_TOKEN | oauth | auth.go:63 | parity | — | — |
| CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR, CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR | fd creds | auth.go:70 | parity | — | — |
| CLAUDE_CODE_GATEWAY_TOKEN_FILE_DESCRIPTOR | fd cred | — | missing | NONE | user |
| ANTHROPIC_AWS_API_KEY | AWS key auth | — | missing | NONE | wire (auth) |
| ANTHROPIC_BASE_URL | API base | ✓ auth.go/client | parity | — | — |
| CLAUDE_CODE_API_BASE_URL | base override | — | missing | NONE | wire |
| _CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL | first-party check | — | missing | NONE | wire |
| ANTHROPIC_BEDROCK_BASE_URL / ANTHROPIC_VERTEX_BASE_URL / ANTHROPIC_FOUNDRY_BASE_URL / ANTHROPIC_AWS_BASE_URL / ANTHROPIC_GOOGLE_CLOUD_BASE_URL / ANTHROPIC_BEDROCK_MANTLE_BASE_URL | provider bases | first three ✓; rest missing | partial | NONE | wire |
| ANTHROPIC_UNIX_SOCKET | unix-socket transport | ✓ | parity | — | — |
| ANTHROPIC_BETAS | extra beta headers env equivalent | — (only `--betas` flag) | missing | NONE | **wire** |
| ANTHROPIC_CUSTOM_HEADERS | header merge | ✓ rawhttp/client | parity | TestBetasHeader | — |
| ANTHROPIC_MODEL | model pin | ✓ main.go:688 | parity | — | — |
| ANTHROPIC_DEFAULT_MODEL | default-model override | — | missing | NONE | **wire** |
| ANTHROPIC_DEFAULT_{FABLE,HAIKU,OPUS,SONNET}_MODEL | alias→id override | ✓ resolveModel main.go:1767-1787 | parity | — | — |
| ANTHROPIC_DEFAULT_*_MODEL_NAME / _DESCRIPTION / _SUPPORTED_CAPABILITIES | picker display/capability hints | — | missing | NONE | cosmetic |
| ANTHROPIC_CUSTOM_MODEL_OPTION{,_NAME,_DESCRIPTION,_SUPPORTED_CAPABILITIES} | custom picker entry | — | missing | NONE | cosmetic |
| ANTHROPIC_SMALL_FAST_MODEL | haiku alias | ✓ main.go:1780 | parity | — | — |
| ANTHROPIC_SMALL_FAST_MODEL_AWS_REGION | bedrock small-model region | — | missing | NONE | user |
| ANTHROPIC_PROFILE / ANTHROPIC_CONFIG_DIR / ANTHROPIC_ORGANIZATION_ID / ANTHROPIC_WORKSPACE_ID / ANTHROPIC_SERVICE_ACCOUNT_ID / ANTHROPIC_IDENTITY_TOKEN(_FILE) / ANTHROPIC_SCOPE / ANTHROPIC_FEDERATION_RULE_ID | identity/profile stack | — | missing | NONE | user |
| CLAUDE_CODE_USE_BEDROCK / USE_VERTEX / USE_FOUNDRY / USE_MANTLE / USE_ANTHROPIC_AWS / USE_ANTHROPIC_GOOGLE_CLOUD / USE_GATEWAY | provider select | BEDROCK/VERTEX/FOUNDRY/GATEWAY read (auxreq path), MANTLE + ANTHROPIC_AWS + GOOGLE_CLOUD missing | partial | NONE | wire |
| CLAUDE_CODE_SKIP_{BEDROCK,VERTEX,FOUNDRY,ANTHROPIC_AWS,ANTHROPIC_GOOGLE_CLOUD,MANTLE}_AUTH | skip provider auth | bedrock/vertex/foundry ✓; mantle/aws/gcloud missing | partial | NONE | user |
| ANTHROPIC_VERTEX_PROJECT_ID / ANTHROPIC_GOOGLE_CLOUD_{PROJECT,LOCATION,WORKSPACE_ID} / CLOUD_ML_REGION / GCLOUD_PROJECT | vertex config | CLOUD_ML_REGION + GOOGLE_APPLICATION_CREDENTIALS read; vertex project id missing | partial | NONE | wire |
| ANTHROPIC_AWS_WORKSPACE_ID / ANTHROPIC_FOUNDRY_RESOURCE / ANTHROPIC_FOUNDRY_API_KEY(_TOKEN) | provider ids/creds | foundry ✓, aws workspace missing | partial | NONE | wire |
| CLAUDE_CODE_SUBAGENT_MODEL{,_FORCE} | subagent model pin | — | missing | NONE | **wire** |
| CLAUDE_CODE_MAX_OUTPUT_TOKENS | max_tokens | — | missing | NONE | **wire** |
| CLAUDE_CODE_MAX_TURNS | turn cap | — | missing | NONE | user |
| CLAUDE_CODE_MAX_RETRIES | retry cap | ✓ | parity | — | — |
| API_TIMEOUT_MS / API_FORCE_IDLE_TIMEOUT | request timeouts | — | missing | NONE | wire |
| CLAUDE_STREAM_FIRST_BYTE_TIMEOUT_MS / CLAUDE_STREAM_IDLE_TIMEOUT_MS / CLAUDE_BYTE_STREAM_IDLE_TIMEOUT_MS / CLAUDE_SLOW_FIRST_BYTE_MS | stream timeouts | — | missing | NONE | user |
| MAX_THINKING_TOKENS | forces enabled thinking with this budget | only clamps an already-enabled budget; never enables thinking — loop.go:1003 | divergent | TestFlagThinkingBudget (flag, not env) | **wire/user** |
| MAX_MCP_OUTPUT_TOKENS | MCP result cap | — | missing | NONE | user |
| MCP_TIMEOUT / MCP_CONNECT_TIMEOUT_MS | request/handshake | ✓ mcp/client.go:896 | parity | — | — |
| MCP_TOOL_TIMEOUT | per-tool call timeout | — | missing | NONE | user |
| MCP_OAUTH_CALLBACK_PORT / MCP_OAUTH_CLIENT_METADATA_URL / MCP_CLIENT_SECRET / MCP_XAA_IDP_CLIENT_SECRET | MCP oauth | — | missing | NONE | user |
| MAX_STRUCTURED_OUTPUT_RETRIES | json-schema retries | — | missing | NONE | user |
| BASH_DEFAULT_TIMEOUT_MS / BASH_MAX_TIMEOUT_MS / BASH_MAX_OUTPUT_LENGTH | bash tool limits | ✓ bash.go:131-150 | parity | TestBashTimeout | — |
| CLAUDE_BASH_MAINTAIN_PROJECT_WORKING_DIR | keep cwd | ✓ bash.go:277 | parity | — | — |
| TASK_MAX_OUTPUT_LENGTH | task output cap | — | missing | NONE | user |
| CLAUDE_CODE_FILE_READ_MAX_OUTPUT_TOKENS / CLAUDE_CODE_GLOB_TIMEOUT_SECONDS / CLAUDE_CODE_GLOB_HIDDEN / CLAUDE_CODE_GLOB_NO_IGNORE / CLAUDE_CODE_MAX_SUBAGENTS_PER_SESSION / CLAUDE_CODE_MAX_TOOL_USE_CONCURRENCY / CLAUDE_CODE_MCP_AUTO_BACKGROUND_MS / CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT | tool limits | — | missing | NONE | user |
| SLASH_COMMAND_TOOL_CHAR_BUDGET | slash listing budget | — | missing | NONE | cosmetic |
| USE_BUILTIN_RIPGREP | bundled rg | — | missing | NONE | user |
| ENABLE_TOOL_SEARCH | deferred tools | ✓ toolsearch.go:148 | parity | TestToolSearchKeyword | — |
| DISABLE_AUTOUPDATER / DISABLE_UPDATES / DISABLE_INSTALLATION_CHECKS | update checks | only DISABLE_AUTOUPDATER read | partial | NONE | cosmetic |
| DISABLE_TELEMETRY / DO_NOT_TRACK / CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC / DISABLE_NON_ESSENTIAL_MODEL_CALLS | telemetry gates | ✓ (telemetry.go, auxreq.go:113) | parity | TestAuxDisabledByEnv | — |
| DISABLE_ERROR_REPORTING | sentry gate | — | missing | NONE | cosmetic |
| DISABLE_PROMPT_CACHING{,_FABLE,_HAIKU,_MYTHOS,_OPUS,_SONNET} | cache_control strip | — | missing | NONE | **wire** — no way to disable caching |
| DISABLE_INTERLEAVED_THINKING | thinking stream shape | — | missing | NONE | wire |
| DISABLE_COMPACT / DISABLE_AUTO_COMPACT | compact kill switches | DISABLE_COMPACT ✓; DISABLE_AUTO_COMPACT missing | partial | — | user |
| CLAUDE_CODE_AUTO_COMPACT_WINDOW / CLAUDE_AUTOCOMPACT_PCT_OVERRIDE / CLAUDE_CODE_MAX_CONTEXT_TOKENS / CLAUDE_CODE_IDLE_COMPACT_MIN_TOKENS / CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT | compact window tuning | first two ✓; rest missing | partial | TestAutoCompact* | user |
| DISABLE_BUG_COMMAND / DISABLE_DOCTOR_COMMAND / DISABLE_LOGIN_COMMAND / DISABLE_LOGOUT_COMMAND / DISABLE_UPGRADE_COMMAND / DISABLE_EXTRA_USAGE_COMMAND / DISABLE_INSTALL_GITHUB_APP_COMMAND / DISABLE_FEEDBACK_SURVEY / DISABLE_BRIEF_MODE_STOP_HOOK | slash-command kill switches | — | missing | NONE | user |
| CLAUDE_CODE_DISABLE_* feature gates (ADVISOR_TOOL, AGENT_VIEW, ARTIFACT, BACKGROUND_TASKS, BUNDLED_SKILLS, EXPLORE_PLAN_AGENTS, FILE_CHECKPOINTING, MCP_TASK_BACKGROUND, MEMORY_RO_UNSAVED_NOTICE, WORKFLOWS, ADAPTIVE_THINKING, FAST_MODE, LEGACY_MODEL_REMAP, NONSTREAMING_FALLBACK, THINKING, CLAUDE_MDS, AUTO_MEMORY, PRECOMPACT_SKIP, UNKNOWN_MODEL_WINDOW_ENFORCEMENT, DISABLE_EXPERIMENTAL_BETAS, DISABLE_TERMINAL_TITLE, DISABLE_ALTERNATE_SCREEN, DISABLE_MOUSE*, DISABLE_VIRTUAL_SCROLL, DISABLE_PROACTIVITY, PROMPT_STRINGS…) | feature kill switches | only DISABLE_CRON, DISABLE_NONESSENTIAL_TRAFFIC, DISABLE_CFC_PROMPT | missing | TestAuxDisabledByEnv | **user** — most kill switches dead |
| CLAUDE_CODE_ENABLE_{TASKS,AWAY_SUMMARY,FINE_GRAINED_TOOL_STREAMING,FUNCTION_HOOKS,PROMPT_SUGGESTION,AUTO_MODE,DESIGN_SYNC,FEEDBACK_SURVEY_FOR_OTEL,TELEMETRY,PROXY_AUTH_HELPER,APPEND_SUBAGENT_PROMPT} | feature enables | ENABLE_TELEMETRY ✓, others missing | missing | NONE | user |
| ENABLE_CLAUDEAI_MCP_SERVERS | connector mount | — | missing | NONE | wire |
| ENABLE_SESSION_BACKGROUNDING / ENABLE_SESSION_PERSISTENCE / TEST_ENABLE_SESSION_PERSISTENCE | session plumbing | — | missing | NONE | user |
| ENABLE_LSP_TOOL / ENABLE_MCP_LARGE_OUTPUT_FILES / ENABLE_LOCKLESS_UPDATES / ENABLE_PID_BASED_VERSION_LOCKING / ENABLE_BETA_TRACING_DETAILED / ENABLE_ENHANCED_TELEMETRY_BETA | misc enables | — | missing | NONE | cosmetic |
| CLAUDE_CODE_EFFORT_LEVEL | effort pin | ✓ loop.go:174 | parity | — | — |
| CLAUDE_CODE_ALWAYS_ENABLE_EFFORT / CLAUDE_CODE_MAX_EFFORT_REMINDER | effort gates | — | missing | NONE | wire |
| CLAUDE_CODE_DISABLE_FAST_MODE | fast-mode kill | — | missing | NONE | wire |
| CLAUDE_CODE_PROMPT_CACHE_TTL / CLAUDE_CODE_SUBAGENT_PROMPT_CACHE_TTL | cache ttl | — | missing | NONE | **wire** |
| CLAUDE_CODE_REMOTE / CLAUDE_CODE_REMOTE_SESSION_ID / CLAUDE_CODE_REMOTE_ENVIRONMENT_TYPE / CLAUDE_CODE_REMOTE_MEMORY_DIR / CLAUDE_CODE_REMOTE_SDK_URL / CLAUDE_CODE_REMOTE_SEND_KEEPALIVES / CLAUDE_CODE_REMOTE_SESSION_ORIGIN / CLAUDE_CODE_REMOTE_SETTINGS_PATH / CLAUDE_CODE_REMOTE_SETTINGS_POLL_MS / CLAUDE_CODE_REMOTE_TOOLS_* / CLAUDE_CODE_REMOTE_HERMETIC_MODE / CLAUDE_CODE_REMOTE_RAW_EVENTS_FILE | remote sessions | REMOTE + REMOTE_SESSION_ID read (tools/coordinator.go, remote.go); rest missing | partial | NONE | user |
| CLAUDE_CODE_RESTRICTED | restricted flag | ✓ bridge/teleport.go:91 | parity | NONE | — |
| CLAUDE_CODE_SAFE_MODE | safe mode env | — (flag sets CLAUDE_GO_SAFE_MODE instead) | missing | NONE | user |
| CLAUDE_CODE_DISABLE_CLAUDE_MDS | skip CLAUDE.md | — | missing | NONE | **wire** — sys prompt/memory |
| CLAUDE_CODE_DISABLE_AUTO_MEMORY | memory writes | — | missing | NONE | user |
| CLAUDE_CODE_ENTRYPOINT | entrypoint tag | ✓ services.go:3287 | parity | — | — |
| CLAUDE_CODE_SIMPLE | --bare propagation | ✓ (also set by our --bare) | parity | TestBareMode | — |
| CLAUDE_CODE_SIMPLE_SYSTEM_PROMPT | simple sys prompt | — | missing | NONE | wire |
| CLAUDE_CODE_EXTRA_BODY / CLAUDE_CODE_EXTRA_METADATA | request body merge | EXTRA_METADATA ✓; EXTRA_BODY missing | partial | NONE | **wire** |
| CLAUDE_CODE_SESSION_ID / CLAUDE_CODE_SESSION_KIND / CLAUDE_CODE_SESSION_NAME / CLAUDE_CODE_SESSION_ORIGIN / CLAUDE_CODE_SESSION_LOG / CLAUDE_CODE_SESSION_ACCESS_TOKEN / CLAUDE_CODE_SESSION_ATTENDED / CLAUDE_CODE_TAGS | session identity to children | KIND/LOG read; rest missing | partial | — | user |
| CLAUDE_CODE_HOST_SESSION_ID / CLAUDE_CODE_AGENT / CLAUDE_CODE_ACTION / CLAUDE_CODE_ACCOUNT_UUID | host/child plumbing | ✓ services.go:708+ | parity | — | — |
| CLAUDE_CODE_MESSAGING_SOCKET / CLAUDE_CODE_MESSAGING_TOKEN | peer messaging socket | SOCKET ✓ (tools/services.go:1550 uses CLAUDE_GO_MESSAGING_SOCKET_PATH — different name!); TOKEN missing | divergent | NONE | user |
| CLAUDE_CODE_PLAN_MODE_REQUIRED | env equivalent of flag | — | missing | NONE | user |
| CLAUDE_CODE_DONT_INHERIT_ENV / CLAUDE_CODE_SUBPROCESS_ENV_SCRUB | env propagation control | — | missing | NONE | user |
| CLAUDE_CODE_SHELL / CLAUDE_CODE_SHELL_PREFIX | shell overrides | — (SHELL used directly) | missing | NONE | user |
| CLAUDE_CODE_SCRIPT_CAPS / CLAUDE_CODE_SUPERVISED | script/supervised mode | — | missing | NONE | user |
| CLAUDE_CODE_SKIP_PROMPT_HISTORY / CLAUDE_CODE_REMOVE_PROMPT_STRINGS | history | — | missing | NONE | cosmetic |
| CLAUDE_CODE_SKIP_PROJECT_BACKFILL / CLAUDE_CODE_SKIP_REPO_UPLOAD / CLAUDE_CODE_SKIP_MODEL_ACCESS_MEMORY / CLAUDE_CODE_SKIP_AWS_CRED_CACHE / CLAUDE_CODE_SKIP_HFI_VERSION_CHECK / CLAUDE_CODE_SKIP_PLUGIN_MCP_SERVERS(_EXCEPT) / CLAUDE_CODE_SKIP_FAST_MODE_* | skip gates | — | missing | NONE | user |
| CLAUDE_CODE_RESUME_* family (RESUME_FROM_SESSION, INTERRUPTED_TURN, PROMPT, REASON, SOURCE_ALIVE, THRESHOLD_MINUTES, TOKEN_THRESHOLD, TOLERATES_*) | resume plumbing | — | missing | NONE | user |
| CLAUDE_CODE_PLUGIN_* (DIRS read? PLUGIN_DIRS missing; rest missing) | plugin envs | — (flag only via CLAUDE_GO_*) | missing | NONE | user |
| CLAUDE_CODE_OAUTH_CLIENT_ID / CLAUDE_CODE_OAUTH_REFRESH_TOKEN / CLAUDE_CODE_OAUTH_SCOPES / CLAUDE_CODE_CUSTOM_OAUTH_URL / USE_LOCAL_OAUTH / USE_STAGING_OAUTH / CLAUDE_LOCAL_OAUTH_* | oauth config | only DESIGN_OAUTH_CLIENT_ID; rest missing | missing | NONE | wire |
| CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST / CLAUDE_CODE_HOST_AUTH_ENV_VAR / CLAUDE_CODE_HOST_AUTH_REFRESH_TIMEOUT_MS / CLAUDE_CODE_HOST_CREDS_FILE / CLAUDE_CODE_SDK_HAS_* / CLAUDE_CODE_SDK_HAS_HOST_AUTH_REFRESH / CLAUDE_CODE_SDK_HAS_OAUTH_REFRESH / CLAUDE_CODE_SDK_READS_SESSION_STATE | SDK/host auth | — | missing | NONE | user |
| IS_SANDBOX | sandbox detection | — (CLAUDE_GO_SANDBOX internal instead) | missing | NONE | user |
| DEBUG / DEBUG_AUTH / DEBUG_SDK / DEBUG_CLAUDE_AGENT_SDK | debug namespaces | CLAUDE_GO_DEBUG* instead | divergent | NONE | cosmetic |
| NODE_EXTRA_CA_CERTS / SSL_CERT_FILE / NODE_TLS_REJECT_UNAUTHORIZED / CLAUDE_CODE_CERT_STORE / CLAUDE_CODE_CLIENT_CERT(_KEY, _KEY_PASSPHRASE) | TLS | NODE_EXTRA_CA_CERTS+SSL_CERT_FILE ✓; rest missing | partial | NONE | wire |
| HTTP(S)_PROXY / NO_PROXY / ALL_PROXY / CLAUDE_CODE_PROXY_RESOLVES_HOSTS | proxy | proxies ✓ rawhttp.go:149; RESOLVES_HOSTS missing | partial | — | wire |
| AGENT_PROXY_URL / AGENT_PROXY_AUTH_TOKEN / CCR_AGENT_PROXY_* | agent proxy | — | missing | NONE | wire |
| OTEL_* / CLAUDE_CODE_ENABLE_TELEMETRY / CLAUDE_CODE_OTEL_* / ANT_OTEL_* | telemetry export | ENABLE_TELEMETRY + OTEL_LOGS_EXPORTER + ANT_OTEL_LOGS_EXPORTER only | missing | NONE | user |
| GROWTHBOOK: DISABLE_GROWTHBOOK / CLAUDE_CODE_GB_BASE_URL | feature-flag service | — | missing | NONE | user |
| CLAUDE_CONFIG_DIR | config home | ✓ config.go:222 | parity | TestConfigDirOverride | — |
| CLAUDE_TMPDIR / CLAUDE_CODE_TMPDIR | tmp dir | ✓ netproxy/services | parity | — | — |
| CLAUDE_ENV_FILE | env file load | — | missing | NONE | user |
| CLAUDE_CODE_PACKAGE_MANAGER_AUTO_UPDATE | pm update | — | missing | NONE | cosmetic |
| CLAUDE_CODE_ACCESSIBILITY / CLAUDE_AX_SCREEN_READER / CLAUDE_CODE_SCROLL_SPEED / CLAUDE_CODE_SYNTAX_HIGHLIGHT / CLAUDE_CODE_HIDE_CWD / CLAUDE_CODE_NO_FLICKER / CLAUDE_CODE_FORCE_TERMINAL_IMAGES / CLAUDE_CODE_ALT_SCREEN_FULL_REPAINT / CLAUDE_CODE_NATIVE_CURSOR / CLAUDE_CODE_TMUX_* / CLAUDE_CODE_HOVER_REST | TUI envs | ACCESSIBILITY, AX_SCREEN_READER, SCROLL_SPEED, TMUX_SESSION read; rest missing | partial | — | cosmetic |
| FORCE_COLOR / NO_COLOR / COLORFGBG | color | ✓ theme.go | parity | — | — |
| CLAUDE_CODE_VOICE_FORWARD_INTERIMS_TYPED / VOICE_STREAM_BASE_URL | voice | ✓ | parity | — | — |
| BRAVE_API_KEY / EXA_API_KEY / TAVILY_API_KEY | not in real (web search is API-side) | ours reads for web backend — tools/web.go:262-266 | extra | TestWebSearchBackend | cosmetic |
| CLAUDE_CODE_API_KEY | not in real | ours reads (subcommands?) — flag env? | extra | — | cosmetic |
| CCR_OAUTH_TOKEN_FILE / CLAUDE_GO_* whole family (~60 vars) | Go-only plumbing mirroring flag envs | ours | extra | — | cosmetic |
| GOOGLE_OAUTH_ACCESS_TOKEN | not in real table | ours reads | extra | — | cosmetic |
| CLAUDE_CODE_AUTO_BACKGROUND_TIMEOUT_MS / CLAUDE_CODE_TASK_TOOLS / CLAUDE_CODE_WORKSPACE / CLAUDE_CODE_MESSAGING_TOKEN / CLAUDE_BG_CLI_VERSION | not in real (invented) | ours reads | extra | — | cosmetic |

## settings.json keys

Real schema: mod_0072.js (~140 top-level keys; `permissions`, `hooks`, `env`,
`statusLine`, `theme`, `enabledPlugins`, `attribution`, `modelPicker`, `remote`,
`remoteControl`, `remoteTools`, `quietHours`, `breakReminder`, `spellcheck`,
`sshConfigs`, `xaaIdp` are objects). Ours: `internal/config/config.go` Settings
struct + `UserSetting()` reads (user-tier only!) + hooks/plugins/theme loaders.

| key | real | ours | status | e2e | severity |
|---|---|---|---|---|---|
| fastMode | **boolean** (mod_0072 `H()`) | `FastMode string` — `fastMode:true` breaks `json.Unmarshal` → **whole settings file silently dropped** (config.go:21, Load's `== nil` check) | **divergent** | TestFastModeKey | **critical-user** |
| statusLine / apiKeyHelper / prefersReducedMotion / promptSuggestionEnabled / showTurnDuration / syntaxHighlightingDisabled / useAutoModeDuringPlan / viewMode / remote | honored from merged tiers (user+project+local+--settings) | `UserSetting()` = `~/.claude/settings.json` only — project/local ignored | divergent | TestStatuslineCmd (user file only) | **user** |
| effortLevel | enum `low|medium|high|xhigh` per model map or string; invalid → `.catch(void 0)` silently dropped | string passthrough → wire `output_config.effort` — config.go:20, main.go:692 | divergent | NONE | wire |
| permissions.defaultMode | enum + `"manual"` alias → default (mod_0072) | raw string; "manual" lands on wire as perm mode | divergent | TestPlanMode* | user |
| permissions.blockReadsOutsideWorkingDirectories | settings key (mod_0072 `blockReadsOu...`) | not in Permissions struct | missing | NONE | user |
| permissions.additionalDirectories | ✓ | ✓ config.go | parity | TestAdditionalDirectories | — |
| includeCoAuthoredBy | bool, default true | ✓ CoAuthoredBy() | parity | — | — |
| includeGitInstructions | bool | — not read | missing | NONE | wire (sys prompt) |
| autoCompactEnabled / autoCompactWindow | bool + window | window ✓; `autoCompactEnabled` not read (DISABLE_AUTO_COMPACT env also missing) | partial | TestAutoCompact* | user |
| autoMemoryEnabled / autoMemoryDirectory / autoDreamEnabled / autoUploadSessions / autoContinueAtUsageLimit / awaySummaryEnabled / idleCompaction / precomputeCompactionEnabled | lifecycle toggles | — | missing | NONE | user |
| advisorModel | ✓ | ✓ config.go:26 | parity | TestAdvisorFlag | — |
| agent | default agent name | — (flag only) | missing | NONE | user |
| model | ✓ | ✓ main.go:872 | parity | — | — |
| verbose / editorMode / theme / language / timeFormat / timeZone | TUI prefs | theme+statusLine (user-only); verbose/editorMode/language/timeFormat/timeZone not read | partial | TestThemePersist | cosmetic |
| cleanupPeriodDays / desktopSessionCleanupPeriodDays | transcript GC | dialog label only (commands.go:4324); GC enforcement unverified | partial | NONE | user |
| respectGitignore | bool | — | missing | NONE | user |
| apiKeyHelper | auth helper | ✓ auth.go:298 (user only) | partial | NONE | user |
| otelHeadersHelper / proxyAuthHelper / awsAuthRefresh / awsCredentialExport / gcpAuthRefresh / processWrapper | helper scripts | — | missing | NONE | wire |
| forceLoginMethod | login gating | — | missing | NONE | user |
| hooks | all tiers | ✓ internal/hooks Load (user+project+local) + --settings | parity | TestHook* | — |
| env | per-key union across tiers | ✓ ApplyEnv later-tier-wins — config.go:99 | parity | TestSettingsEnv/TestSettingsEnvInjection | — |
| enabledPlugins | map plugin@src→bool | ✓ plugins.go:370 | parity | TestPlugin* | — |
| extraKnownMarketplaces / additionalMarketplaces / allowedMarketplaces / pluginSuggestionMarketplaces / allowedChannelPlugins / appendPlugins / prependPlugins / syncClaudeAiPlugins / syncClaudeAiSkills / disableBundledSkills / disableCommandPluginSources / disableSideloadFlags | plugin/marketplace policy | — | missing | NONE | user |
| attribution {commit,pr,sessionUrl} | trailer text | — (only includeCoAuthoredBy) | missing | NONE | cosmetic |
| modelPicker / maxEffortLevel / enforceAvailableModels / minimumVersion / requiredMinimumVersion / requiredMaximumVersion / switchModelsOnFlag | model policy | — | missing | NONE | user |
| disableAllHooks / disableRemoteControl / disableClaudeAiConnectors / disableAgentView / disableArtifact / disableAutoMode / disableWorkflows / disableSkillShellExecution / allowManaged*Only / allowAllClaudeAiMcps / allowClaudeInChromeWithManagedMcp / allowedMcpServers / deniedMcpServers / allowedMcpjsonServers?(n/a) | managed/policy gates | only disableRemoteControl + disableClaudeAiConnectors | missing | NONE | **user (policy)** |
| enabledMcpjsonServers / disabledMcpjsonServers / enableAllProjectMcpServers | mcp.json gates | ✓ | parity | — | — |
| outputStyle / spinnerVerbs / spinnerTipsEnabled / spinnerTipsOverride / showMessageTimestamps / showThinkingSummaries / prefersReducedMotion / wheelScrollAccelerationEnabled / maxProseWidth / terminalProgressBarEnabled / terminalTitleFromRename / todoFeatureEnabled / emojiCompletionEnabled / bashEditDiffEnabled / bashOutputMaxChars / taskOutputMaxChars / fileCheckpointingEnabled / channelsEnabled / inputNeededNotifEnabled / agentPushNotifEnabled / preferredNotifChannel / companyAnnouncements / quietHours / breakReminder / respondToBashCommands / spellcheck / teammateMode / subagentPromptCacheTtl / ultracode / doneMeansMerged / isolatePeerMachines / crossSessionInbound / feedbackSurveyRate / forceRemoteSettingsRefresh / remoteControlAtStartup / remote.defaultEnvironmentId / remoteControl.* / remoteTools.* / plansDirectory / gatewayInternalNetworks / modelProposedGoals / httpHookAllowedEnvVars / claudeMd / $schema / wslInheritsWindowsSettings / syntaxHighlightingDisabled / skillListingBudgetFraction / skillListingMaxDescChars / skipDangerousModePermissionPrompt / skipWebFetchPreflight / skipWorkflowUsageWarning / sshConfigs / xaaIdp | remaining real keys | syntaxHighlightingDisabled + prefersReducedMotion + spinnerVerbs (user-only) read; all others unhandled | missing | NONE | cosmetic |

## Notes on precedence

- Real tier order: managed (policy) > local > project > user > flag(--settings);
  `--settings` applies the whole file as a tier. Ours: --settings only merges
  permissions+hooks (main.go:768-798) — all other keys ignored.
- Real evaluates managed deny/ask/disable* in ANY tier'd file but ours only
  reads managed gates from the managed file (config.go:60-76) — managed deny
  keys placed in user settings are ignored by real too, so partial parity.
- Ours adds `policy-limits.json` + `CLAUDE_GO_*` policy env gates — not in real.

