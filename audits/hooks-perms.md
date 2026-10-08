# Audit: hooks + permissions + sandbox — refsrc-294 (Claude Code 2.1.294) vs Go replica

Evidence keys: `refsrc-294/mod_*.js` = bundled real JS; `refsrc-288/` = prior bundle ours was mirrored from; `ours` paths are relative to the worktree root. Status: **match** / **partial** / **gap** / **n/a**.

## Counts

- **Hook events**: 33 defined in real (`lh`, refsrc-294/mod_1337.js); 33 constants in `internal/hooks/hooks.go:24-58`. All 33 names match. Ours has call sites for 32/33 (MessageDisplay never fired; several others fire with reduced payloads — per-event rows).
- **Rows**: ~115 audited — **match ~55, partial ~40, gap ~20, n/a 1** (a few cells contain literal `|` in backticks, so counts are approximate).
- **e2e coverage**: hooks covered for ~18 events; permissions covered by package tests + ~5 e2e; sandbox covered by package tests only (no e2e executes a real sandboxed command or the proxy deny→prompt path).

## Top gaps (ordered by severity)

| # | Gap | Where |
|---|-----|-------|
| 1 | `permissions.defaultMode:"auto"` honored from project/local settings — real refuses repo-controllable `auto` ("repo-level settings cannot grant", mod_1112.js). Cloned repo can silently widen permission posture. | `internal/config/config.go:76-97` (no source check on DefaultMode value) |
| 2 | Hook `type:"prompt"`/`"agent"` run `Judge` for **every** event; real gates them to PreToolUse/PostToolUse/PermissionRequest and rejects others ("not yet supported outside REPL" for Stop context, mod_0416.js). Agent hooks don't run a real subagent-with-tools. | `internal/hooks/hooks.go:297-307` |
| 3 | No `async`/`asyncRewake`/`asyncTimeout`/`rewakeMessage`/`once`/`if` hook fields — real's async-hook pipeline (`asyncTimeout` default 15s, `rewake*` for SessionStart re-entry) unsupported; hooks with these fields are silently treated as sync command hooks. | `internal/hooks/hooks.go:82-89` (Command struct) vs `Bee` schema (mod_0416.js) |
| 4 | Hook types `mcp_tool`, `script`, `callback`, `function` unimplemented (silently skipped) — real executes all four. | `internal/hooks/hooks.go:314` |
| 5 | `CLAUDE_ENV_FILE` never set for SessionStart/Setup/CwdChanged/FileChanged hooks — real hooks can emit session env vars (`mod_0416.js:2939xxx` sets it for those 4 events). Ours only exports `CLAUDE_PROJECT_DIR`. | `internal/hooks/hooks.go:431-443` |
| 6 | http hook `allowedEnvVars` ignored — real restricts env interpolation in headers to a whitelist; ours expands **every** env var into every header value (`os.ExpandEnv`). | `internal/hooks/hooks.go:527` |
| 7 | Sandbox schema fields missing: `httpProxyPort`, `socksProxyPort`, `tlsTerminate`(+`excludeDomains`), `mitmProxy`, `allowLocalBinding` (ours invented `allowAllLocal` — real's `allowLocalBinding` is silently unparsed), `allowMachLookup`, `allowAppleEvents`, `allowPty`, `seccomp`, `socatPath`, `javaAgentJarPath`, `enabledPlatforms`, `extraCaCertPaths`, `git.safeDirectories`, `windows.*`. Real rejects unknown-keys silently → ours ignores real configs. | `internal/tools/sandbox.go:187-200` vs `mod_1261.js` schema |
| 8 | `network.interactiveHostApproval` is a claude-go invention (not in real schema); real's equivalent is the implicit ask-callback gated only by `strictAllowlist`. | `internal/tools/sandbox.go:196`, `sandbox.go:652-662` |
| 9 | `ignoreViolations` parsed but never enforced (comment admits it); real suppresses matching sandbox violations. | `internal/tools/sandbox.go:38` |
| 10 | MCP permission rule `mcp__server(pattern)` accepted; real **rejects** parens on MCP rules ("MCP rules do not support patterns in parentheses", mod_0072.js). Ours maps it to `mcp__s__arg` and matches. Wildcard allow-rules `mcp__srv__*`/tool-position globs also unimplemented. | `internal/perms/perms.go:296-305` |
| 11 | Hook-output fields unhandled: `suppressOriginalPrompt`, `initialUserMessage`, `sessionTitle`, `watchPaths`, `reloadSkills`, `updatedToolOutput`, `updatedMCPToolOutput`, `classifierContext`, `retry` (PermissionDenied), `action`/`content` (Elicitation), `worktreePath`, `displayContent`, `metrics`, `suppressOutput` (parsed, never applied). | `internal/hooks/hooks.go:223-238` |
| 12 | Base stdin payload missing `agent_id`, `agent_type`, `prompt_id`, `effort`, `scratchpad_dir` (real's `xd()` base context, mod_0416.js). | `internal/agent/loop.go:794-800` |
| 13 | `PostToolUse`/`PostToolUseFailure` payload missing `duration_ms`; failure payload missing `is_interrupt`. | `internal/agent/loop.go:631-637` |
| 14 | dangerous_tool_use classifier divergent: real = server-side `safeguard_tool_use`/`safeguard_results` + beta `dangerous-tool-use-2026-09-03`, verdicts `evaluated{flagged,not_flagged}`/`skipped`/`unavailable`, hook-edited input invalidates verdict; ours = local 2-stage sonnet-5→opus-5 severity eval with CLAUDE.md preamble, threshold 50. Functionally similar, wire/shape divergent. | `internal/api/auxreq.go:718+`, `internal/agent/loop.go:601-622` |

## Hook events (authoritative list)

Real's event registry `lh` (refsrc-294/mod_1337.js). "Fired" = ours emits the event; "payload" = ours sends real's documented stdin fields.

| item | real + evidence | ours | status | e2e | severity |
|------|-----------------|------|--------|-----|----------|
| Event list (33) | `lh` array, mod_1337.js: PreToolUse, PostToolUse, PostToolUseFailure, PostToolBatch, Notification, UserPromptSubmit, UserPromptExpansion, SessionStart, SessionEnd, Stop, StopFailure, SubagentStart, SubagentStop, PreCompact, PostCompact, PreModelSwitch, PostModelSwitch, PermissionRequest, PermissionDenied, Setup, TeammateIdle, TaskCreated, TaskCompleted, Elicitation, ElicitationResult, ConfigChange, WorktreeCreate, WorktreeRemove, InstructionsLoaded, CwdChanged, FileChanged, DirectoryAdded, MessageDisplay | All 33 consts, hooks.go:24-58 | match | TestIncludeHookEvents (enum only) | — |
| PreToolUse | stdin: base+`tool_name`,`tool_input`,`tool_use_id`; output: `permissionDecision` allow/deny/ask, `updatedInput`, `additionalContext` (mod_0416.js, `Bee` schema) | loop.go:578 fires w/ tool_name/input/id; deny/allow/updatedInput applied (587-599) | match | TestHookPermissionAllow, TestHookUpdatedInput, TestHookPermissionRequest | — |
| PostToolUse | stdin +`tool_response`,`duration_ms`; output adds `classifierContext`,`updatedToolOutput`,`updatedMCPToolOutput` | fires w/ name/input/id/response; `duration_ms` missing; response is plain string not real's structured object | partial | TestHookPostToolUseFailure (failure side), hook matrix | medium |
| PostToolUseFailure | stdin +`error`,`is_interrupt`,`duration_ms` | fires w/ name/input/id/error; `is_interrupt`,`duration_ms` missing; error is result content string | partial | TestHookPostToolUseFailure | medium |
| PostToolBatch | stdin `tool_calls[]` (name/input/id/response each); once per batch | loop.go:666-671 fires w/ tool_calls | match | TestPostToolBatchHook | — |
| Notification | stdin `message`,`title`,`notification_type`; matcher on notification_type | dialogs.go:126 fires w/ notification_type=permission_prompt etc. | partial | none | low — only the permission_prompt type is wired; idle_prompt/auth/elicitation/agent_* types not all produced |
| UserPromptSubmit | stdin `prompt`,`session_title`; output `additionalContext`,`sessionTitle`,`suppressOriginalPrompt` | fires (UserPromptSubmit at prompt submit); sessionTitle/suppressOriginalPrompt unhandled | partial | TestHookExit2FeedsModel (stop-ish path), TestHookPlainTextContext | medium |
| UserPromptExpansion | stdin `expansion_type`,`command_name`,`command_args`,`command_source`,`prompt`; matcher command_name | repl.go:1663 fires w/ command_name, prompt | partial | none | low — expansion_type/command_args/command_source absent |
| SessionStart | stdin `source`,`agent_type`,`model`,`session_title`; output `additionalContext`,`initialUserMessage`,`sessionTitle`,`watchPaths`,`reloadSkills`; CLAUDE_ENV_FILE set | commands.go:66/103 fires w/ source; model/agent_type/sessionTitle/watchPaths/reloadSkills/ENV_FILE missing | partial | TestSessionStartHookOnWire, TestSessionStartSourcePayload, TestClearSessionHookSequence | medium |
| SessionEnd | stdin `reason`,`mode`,`url`,`elicitationId`; matcher reason | repl.go:1566, commands.go:59 fire w/ reason | partial | TestSessionEndReasonPayload, TestSessionEndReasonSlashExit | low — mode/url/elicitationId absent |
| Stop | stdin +`last_assistant_message`, stop state; output `additionalContext`; "stop-hook-active" loop control | repl.go:1265 fires; Stop→StopFailure cascade implemented (hooks.go:338-348) | partial | none direct (TestHookContinueFalse uses it) | low — last_assistant_message absent |
| StopFailure | stdin `error`,`error_details`,`last_assistant_message`; matcher `error` | fired from Stop-hook failure path w/ `error` only | partial | TestHookStopFailure | low |
| SubagentStart | stdin `agent_id`,`agent_type`; matcher agent_type | subagent.go:436 fires | partial | none | low |
| SubagentStop | stdin `stop_hook_active`,`agent_id`,`agent_type`,`agent_transcript_path`,`last_assistant_message`; matcher agent_type; can block → keeps subagent running | subagent.go:560 fires | partial | TestHookSubagentStop | medium — transcript_path/stop_hook_active unverified |
| PreCompact | stdin `trigger`,`custom_instructions`; matcher trigger ("manual"/"auto") | fires (e2e proves) | match | TestHookPreCompact | — |
| PostCompact | stdin `trigger`,`compact_summary`; matcher trigger | fired (PostCompact const; e2e matrix covers) | partial | TestHookMatrixRound2/3 | low |
| PreModelSwitch / PostModelSwitch | stdin `from_model`,`to_model`,`requested_model`,`source`; matcher normalized `to_model` (`LVe` strips `[1m]`/`[2m]` markers, mod_0416.js) | events exist; matcher normalization `LVe` not implemented — `[1m]`/`[2m]` in matcher won't normalize | partial | none | low |
| PermissionRequest | stdin `tool_name`,`tool_input`,`tool_use_id`,`permission_suggestions`; output `hookSpecificOutput.decision` = `{behavior:allow,updatedInput,updatedPermissions}` or `{behavior:deny,message,interrupt}` | dialogs.go:88 fires; our Output parses flat permissionDecision instead of nested `decision` object | partial | TestPermissionRequestAutoAllow, TestPermissionRequestDeny | high — nested `decision.behavior` object form (real's documented shape) is not parsed |
| PermissionDenied | stdin `tool_name`,`tool_input`,`tool_use_id`,`reason`; output `retry` | dialogs.go:253 fires; `retry` output unhandled | partial | none | medium — real's retry field re-prompts |
| Setup | stdin `trigger`; matcher trigger | commands.go:199 fires w/ trigger:"init" | match | TestSetupHook | — |
| TeammateIdle | stdin `teammate_name`,`team_name`; no matcher | repl.go:111, services.go:2705 fire | match | none | low |
| TaskCreated / TaskCompleted | stdin `task_id`,`task_subject`,`task_description`,`teammate_name`,`team_name`; no matcher; TaskCreated exit-2 prevents creation | tasks.go:176 fires TaskCreated w/ block semantics; TaskCompleted fired (e2e) | match | TestTaskCreatedHook, TestHookTaskCompleted | — |
| Elicitation / ElicitationResult | stdin `mcp_server_name`,`elicitation_id`,`mode`,`url`,`requested_schema`; result adds `action`,`content`; matcher mcp_server_name; output can answer (`action`,`content`) | repl.go:2312/2369 fire w/ server name; `action`/`content` answer path unhandled | partial | none | medium — hook cannot auto-answer elicitation |
| ConfigChange | stdin `source`,`file_path`; matcher source | commands.go:4716 fires | match | TestConfigChangeHook | — |
| WorktreeCreate | stdin `name`; **stdout = created absolute path** (hook provisions worktree); blockable | misc.go:330-336 blocks on h.Block and adopts PlainText stdout as the worktree path | match | none | — |
| WorktreeRemove | stdin `worktree_path`; non-blocking | misc.go:486 fires, NonBlockErr surfaced | match | none | — |
| InstructionsLoaded | stdin `file_path`,`memory_type`,`load_reason`,`globs`,`trigger_file_path`,`parent_file_path`; matcher load_reason | fired per memory file (main.go:791 comment) | partial | TestInstructionsLoadedHook | low — globs/trigger_file_path unverified |
| CwdChanged | stdin `old_cwd`,`new_cwd`; no matcher; output `watchPaths`; CLAUDE_ENV_FILE set | repl.go:173 fires w/ old/new cwd | partial | TestHookCwdChanged, TestHookCwdChangedPrint | low — watchPaths/ENV_FILE unhandled |
| FileChanged | stdin `file_path`,`event`; matcher = **watch pattern** on filename (`OVe`, mod_0416.js DVe); output `watchPaths`; CLAUDE_ENV_FILE set | repl.go:180 fires w/ file_path; matcher treated as "no matcher field" — fires for every change regardless of watch pattern | partial | TestHookFileChanged | medium — watch-pattern gating absent |
| DirectoryAdded | stdin `directory`,`source`; matcher source | commands.go:133 fires | match | none | — |
| MessageDisplay | stdin `turn_id`,`message_id`,`index`,`final`,`delta`; output `displayContent`; display-only (stored message untouched) | const exists; no call site found — never fired | gap | none | medium |

## Matcher semantics

| item | real + evidence | ours | status | e2e | severity |
|------|-----------------|------|--------|-----|----------|
| Matcher field per event | `DVe` (mod_0416.js): tool-name events→`tool_name`; UserPromptExpansion→`command_name`; SessionStart→`source`; SessionEnd/StopFailure→`reason`/`error`; Compact/Setup→`trigger`; ModelSwitch→`to_model`; Notification→`notification_type`; Subagent→`agent_type`; Elicitation→`mcp_server_name`; ConfigChange/DirectoryAdded→`source`; InstructionsLoaded→`load_reason`; FileChanged→basename watch pattern; none for UserPromptSubmit/Stop/PostToolBatch/TeammateIdle/Task*/Worktree*/CwdChanged/MessageDisplay | matcherField map (hooks.go:63-80) mirrors DVe exactly | match | TestHookMatcherCommaList | — |
| Simple matcher = exact/list | `B0e=/^[a-zA-Z0-9_|, -]+$/` (tool events); non-tool events use `[a-zA-Z0-9_|]+` (no space/comma → treated as regex) — `Frn` (mod_0416.js) | ours uses one charset `[A-Za-z0-9_ ,|-]` for all events | partial | — | low — non-tool matchers containing space/comma diverge (ours exact-list, real regex) |
| Regex matchers | `new RegExp(matcher)` unanchored `.test(value)` + aliases `xU`/case variants `Jce`; invalid regex → warn + no match | `regexp.Compile` + unanchored `MatchString`; invalid → silent no-match | partial | TestHookMatcherCommaList | low — no warning; tool-alias matching (`hookMatcherFamilyNames`, familyParentToolName) absent |
| `mcp__server` bare matcher | Warns "matches no tool (it is compared as an exact string); use `mcp__srv__.*`" — `Wmr`/`jmr`, mod_0416.js | ours treats `mcp__github` in a hook matcher as simple exact — consistent outcome (no match) but no warning; the warn text helps users | partial | none | low |
| Matcher on no-matcher event | Configured matcher ignored — hooks bucketed under `""` run anyway (`oMt`, mod_1337.js) | hooks.go:288-291 runs anyway | match | — | — |
| matcher `*`/`.*`/empty | `uGe` — `*`,`.*`,empty all = match-all | `m.Matcher != "" && != "*"` skip-check → `".*"` becomes regex `.*` which also matches everything | match | — | — |
| `if` precondition | Hook entries carry `if` (permission-rule-syntax expression evaluated pre-spawn, mod_1573.js `evaluateCondition`) | not parsed | gap | none | medium |
| Source ordering | `M7o` sorts matcher buckets by source tier (policySettings/flagSettings < user < project < local < session < plugin/builtin=999) | ours appends user→project→local→plugin (Load, hooks.go:107-135); per-bucket ordering not sorted by source | partial | none | low |

## Hook config shape & types

| item | real + evidence | ours | status | e2e | severity |
|------|-----------------|------|--------|-----|----------|
| Hook entry fields | Schema `Bee` (mod_0416.js): `type`, `command`, `prompt`, `url`, `headers` (+`allowedEnvVars`), `timeout`, `async`, `asyncRewake`, `asyncTimeout`, `rewakeMessage`, `rewakeSummary`, `once`, `if`, `statusMessage`, `model`, `shell`, `args`, `continueOnBlock`, `server`/`tool` (mcp_tool), `file`/`script` (script), `callbackId` | Command struct: `type`,`command`,`prompt`,`url`,`headers`,`timeout` only | gap | none | high |
| type=command | shell `-c` spawn, stdin JSON payload | runCmd, hooks.go:412 | match | many | — |
| type=prompt | LLM-judged; **tool events only** (PreToolUse/PostToolUse/PermissionRequest); "not yet supported outside REPL" (mod_0416.js:3009951) | Judge callback for prompt AND agent on **every** event; wired main.go:859 | partial | none | medium |
| type=agent | real subagent with tools; same event gating as prompt | same Judge path as prompt — no subagent execution | gap | none | high |
| type=http | POST JSON to url; headers; `allowedEnvVars` gate on env expansion in header values | runHTTPHook (hooks.go:518): POST + JSON parse; `os.ExpandEnv` on all header values (no allowlist); timeout=Timeout or 30s | partial | none | medium — env exfiltration surface wider than real |
| type=mcp_tool / script / callback / function | executed (mcp_tool → MCP call; script → bundled script runner; callback/function → in-proc) | `h.Type != "command"` → skipped silently | gap | none | medium |
| `args` exec form | `{type:command, command, args[]}` runs without shell | unsupported — always `shell -c` | gap | none | low |
| `shell:"powershell"` | Windows PowerShell support | `$SHELL`/`/bin/bash` only | gap | none | low (windows handled by proc_windows.go partially) |
| `statusMessage`/`model`/`once`/`continueOnBlock`/`async*` | parsed+honored | not parsed | gap | none | medium |
| Plugin hooks | `hooks/hooks.json` per enabled plugin merged; `${CLAUDE_PLUGIN_ROOT}` expanded at spawn | pluginHooks() merges + expands; `CLAUDE_GO_PLUGIN_DIR` extension | match | TestPluginHook, TestPluginHooksMerge, TestPluginHookDisabled, TestPluginDisabled | — |
| Managed/policy hooks | policySettings hooks loaded; `allowManagedHooksOnly`, `disableAllHooks` gates (mod_1337.js `H7o`, mod_1338.js) | managed-settings.json hooks never loaded; policy gates absent | gap | none | high — policy hook enforcement missing |
| Session hooks (in-memory) | `sessionHook` source via SDK registrations | not present | gap | none | low |

## Exit code & output interpretation

| item | real + evidence | ours | status | e2e | severity |
|------|-----------------|------|--------|-----|----------|
| exit 2 | Blocking; stderr → reason (mod_2260.js; per-event docs in mod_1337.js registry) | hooks.go:322-327 sets Block+Reason from stderr | match | TestHookExit2FeedsModel | — |
| exit 0 + JSON stdout | Parsed against `Bee` schema; unknown keys warned ("Did you mean hookSpecificOutput.additionalContext", `Aoe` mod_0416.js); malformed fields degraded with logged reason | json.Unmarshal; requires ≥1 recognized field else falls through to plaintext | partial | — | low — no unknown-key warning; all-unknown JSON silently becomes plaintext |
| exit 0 + non-JSON stdout | Plain text → context/user display per event (UserPromptSubmit/SessionStart inject as context) | `res.PlainText` accumulates; callers use it for context | match | TestHookPlainTextContext | — |
| nonzero ≠2 | Non-blocking; stderr shown to user/debug (event-dependent) | NonBlockErr accumulated; printed `hook error (...)` in runHook | match | covered via matrix | — |
| `continue:false` | Stops continuation; `stopReason` is the reason | continueFalse + Reason precedence reason>systemMessage>stopReason | match | TestHookContinueFalse | — |
| `decision:"block"`/`"approve"` | top-level decision honored (block where supported; approve = allow) | block→Block; approve→Permission allow | match | TestHookPermissionAllow | — |
| `suppressOutput` | Hides stdout from transcript/UI | parsed into Output but never read in applyOutput | gap | none | low |
| `systemMessage` | Shown to user | accumulated into res.SystemMessage | match | — | — |
| `hookSpecificOutput.hookEventName` | Must match the event else output discarded (`yH` validation) | parsed but hookEventName never compared to ev — a mismatched payload applies anyway | partial | none | medium — cross-event output can leak |
| PreToolUse `permissionDecision` | allow/deny/ask; `updatedInput` rewrites input; hook-modified input re-validates and invalidates safeguard verdicts | allow/deny/ask + updatedInput applied (loop.go:581-599) | match | TestHookUpdatedInput, TestHookPermissionAllow | — |
| PermissionRequest decision object | `hookSpecificOutput.decision={behavior:allow,updatedInput?,updatedPermissions?}|{behavior:deny,message,interrupt}` | not parsed — only flat permissionDecision string | gap | TestPermissionRequestAutoAllow/Deny (flat form only) | high |
| Elicitation `action`/`content`, CwdChanged/FileChanged `watchPaths`, WorktreeCreate `worktreePath`, MessageDisplay `displayContent`, Stop `sessionTitle`-style fields | consumed per event | none parsed | gap | none | medium |
| `terminalSequence` | whitelisted safe sequences emitted for notification hooks | not parsed | gap | none | low |
| `metrics` (async hook output) | numeric/bool record passthrough | not parsed | gap | none | low |

## Timeout / process / env

| item | real + evidence | ours | status | e2e | severity |
|------|-----------------|------|--------|-----|----------|
| Default timeouts | `t2n` (mod_1571.js): tool-ish/PermissionDenied/SubagentStart=15s, UserPromptSubmit/UserPromptExpansion=30s, Stop/StopFailure/SubagentStop=120s; `ae()` = timeoutS*1000 **+5000ms grace**; outer `Pe()` adds +30s watchdog | defaultTimeout identical 15/30/120 + 5s grace (hooks.go:413-419); no +30s outer bound | match | none | low — missing outer bound cosmetic |
| Custom timeout | seconds field, user-set | `timeout` seconds, `firstInt` fallback | match | — | — |
| Kill semantics | detached process group killed on timeout | detached + killProcessGroup (proc_unix.go) | match | none | — |
| Output caps | stdout/stderr 64 KiB each | cappedBuf 64KiB both | match | none | — |
| Env scrub | strips `LD_*`, `DYLD_*`, `GCONV_PATH`, `NLSPATH`, `DEVELOPER_DIR`, `PYTHON*`; plus shell/config vars (BASH_ENV, ENV, GIT_CONFIG*, SUDO_ASKPASS, BASH_FUNC_) | hookEnvDeny: LD_/DYLD_/PYTHON/BASH_FUNC_/GIT_CONFIG prefixes + GCONV_PATH/NLSPATH/DEVELOPER_DIR/BASH_ENV/ENV/SUDO_ASKPASS | match | none | — |
| CLAUDE_PROJECT_DIR | set to project dir | set to cwd | match | TestSettingsEnv-ish | — |
| CLAUDE_ENV_FILE | set for SessionStart/Setup/CwdChanged/FileChanged — hook stdout/bash lines land in session env | never set | gap | none | medium |
| CLAUDE_PLUGIN_ROOT / CLAUDE_PLUGIN_DATA | set for plugin hooks | PLUGIN_ROOT expanded into command text only; neither exported in env | partial | — | low |
| stdin payload base | `xd()`: session_id, transcript_path, cwd, permission_mode, agent_id?, agent_type?, prompt_id?, scratchpad_dir?, effort? | session_id, transcript_path, cwd, hook_event_name, permission_mode (loop.go:794-800); agent fields only at some subagent call sites | partial | TestSessionStartSourcePayload etc. | medium |
| spawn cwd | hook runs with cwd = hookCwd/projectDir semantics | cmd.Dir = cwd | match | — | — |
| `hook_started`/`hook_response` system records | emitted under --include-hook-events w/ hook_id, exit_code, outcome | OnHookEvent emits both incl. blocked/reason (loop.go:802-822) | partial | TestIncludeHookEvents | low — ours omits stdout/stderr/exit_code fields |

## Permissions

| item | real + evidence | ours | status | e2e | severity |
|------|-----------------|------|--------|-----|----------|
| Rule grammar | `Tool` or `Tool(content)`, must end at `)`; parens inside content literal (`mTe`, mod_0072.js) | matchRuleAt parses `name(arg)` | match | TestBashRules, TestRuleCorners | — |
| Bash rule semantics | `bJ` (mod_0072.js): `\*`/`\\` escaped; `*`→`.*`; `**`→`/(?:.*/)?` globstar; trailing ` *`→`( .*)?` (bare prefix also matches); `:*` prefix form word-boundary; case-insensitive+whitespace-fold flags for some tools | matchCmdPrefix + wildcardRegex replicate `:*`, `*`→`.*`, `\*`, trailing `" *"`→`( .*)?` | partial | TestBashRules, TestSubcommandSplit | low — `**` globstar maps to `/(?:.*/)?` in real; ours treats `**` as `.*` (equivalent for Bash paths, differs on path rules where real uses gitignore too) |
| Subcommand split | compound commands decomposed; every segment must pass (real's permission check per segment) | splitShell on `;` `\|` `&&` `\|\|` quote-aware; deny/ask checked per-segment; allow requires ALL | match | TestSubcommandSplit | — |
| File path rules | `Edit(path)`/`Read(path)` matched per file tool via gitignore-style globs; `Write(path)`/`NotebookEdit(path)`/`Glob(path)` rules NOT matched by file checks (per our bundled docs mirroring real's note); cwd-relative handling via per-root grouping | matchGlob = gitignoreMatch; ruleSuggestion maps Read/Edit/Write/NotebookEdit to `Name(path)`; matchRuleAt tries rel-to-cwd too | match | TestFileGlob | — |
| MCP rules | `mcp__server` (all tools), `mcp__server__tool`, `mcp__server__*` (wildcard in tool position allowed in allow rules; deny/ask wildcards anywhere); **`mcp__s(...)` rejected**: "MCP rules do not support patterns in parentheses" (mod_0072.js:193480); `mcp__s__*` wildcard validation `Kxt` | bare `mcp__s` prefix-matches `mcp__s__*`; `mcp__s(arg)` maps to `mcp__s__arg` (real rejects); `*` wildcards in tool position not supported | partial | TestMcpServerRule | medium |
| WebFetch | `WebFetch(domain:x)` host rule; content spec `domain:<hostname>` | `domain:` prefix, host==x or suffix `.x`; URL host extracted | match | none | — |
| Bare tool rules | `Read`, `Edit`, etc. = all uses | `name != toolName` → `!hasArg` → true | match | — | — |
| Eval order | deny > ask > allow (kx/M_e) | Check: deny→ask→allow passes | match | TestOrderAskBeatsAllow | — |
| Source precedence | managed/policy deny+ask+allow can't be overridden; local > project > user for scalars; arrays merge across all tiers | Load: ManagedDeny/Ask/Allow tracked separately and merged ahead of tier rules (main.go:712-714); allow/deny/ask arrays merge; DefaultMode last-wins unless managed set | match | TestPerProjectAllowedTools | — |
| `auto` source restriction | `defaultMode:"auto"` ignored from project/local settings — "repo-level settings cannot grant" (mod_1112.js); only policy/user/CLI may set auto | any tier's DefaultMode honored (config.go:97) | gap | none | high |
| `disableAutoMode` / `disableBypassPermissionsMode` | managed gates honored ("disable" value blocks the mode) | parsed (config.go:49-50) + enforced main.go:719-723 | match | none | — |
| `additionalDirectories` | workspace extension paths; file-outside-workspace → ask | merged across tiers; fileInsideWorkspace consults it | match | TestAdditionalDirectories | — |
| defaultMode values | `default`,`acceptEdits`,`plan`,`auto`,`bypassPermissions`,`dontAsk` (enum `es`; `manual` alias for default); /config options exclude bypassPermissions | Mode consts identical | match | TestModes | — |
| Mode cycle order | shift-tab: default→acceptEdits→plan→auto (bypass/dontAsk excluded) | repl.go:83 `["default","acceptEdits","plan","auto"]` | match | none | — |
| bypass → allow-all | `bypassPermissions` short-circuits | `ModeBypass → Allow` first | match | — | — |
| plan mode | read-only allow; others → auto-eval lane (`useAutoModeDuringPlan`) or ask | readOnly allow else Ask (autoModeDuringPlan) / Deny | match | TestModes | — |
| acceptEdits | in-workspace edits auto-allowed; outside-workspace still asks | isEditTool + fileInsideWorkspace guard | match | TestModes | — |
| dontAsk | would-ask → deny with don't-ask text | ModeDontAsk → Deny; text at loop.go:622 | match | TestDontAskDenyText, TestDontAsk | — |
| Read-only tools auto-allow | read-only never prompts in any mode (explicit deny/ask still apply) | readOnly() same intent; list: Read/Glob/Grep/LS/WebFetch/WebSearch/ListAgents/…/AskUserQuestion | partial | TestModes | low — verify tool list vs real's isReadOnly set (e.g. Monitor? WebSearch in list confirmed) |
| "Don't ask again" | persists allow rule → `.claude/settings.local.json` `permissions.allow[]` + `~/.claude.json` `projects[cwd].allowedTools` (addRules→localSettings, YFn mod_0072.js); Bash rule = `Bash(<first-word>*)`; WebFetch = `domain:<host>` | addLocalPermissionRule (repl.go:1761) writes settings.local.json allow[]; mirrorAllowRule mirrors to ~/.claude.json; ruleSuggestion = `Bash(cmd:*)`, `WebFetch(domain:host)`, `Edit(path)` | match | TestPerProjectAllowedTools (read side) | low — write shape parity unverified by test; real may write `Bash(cmd *)` vs ours `:*` |
| PermissionRequest hook → updatedPermissions | hook can return updatedPermissions that get persisted | not parsed | gap | none | medium |
| `blockReadsOutsideWorkingDirectories` | real gate (mod_0241.js person_read_rules check) disabling personal read rules | not found | gap | none | medium |

## Sandbox

| item | real + evidence | ours | status | e2e | severity |
|------|-----------------|------|--------|-----|----------|
| Top-level schema | `ne` (mod_1261.js): network, filesystem, credentials, ignoreViolations, enableWeakerNestedSandbox, enableWeakerNetworkIsolation, allowAppleEvents, allowPty, seccomp, bwrapPath, socatPath, javaAgentJarPath, windows, git{safeDirectories}; plus enabled/failIfUnavailable/autoAllowBashIfSandboxed/allowUnsandboxedCommands/excludedCommands | SandboxSettings: enabled, failIfUnavailable, autoAllowBashIfSandboxed, allowUnsandboxedCommands, excludedCommands, ignoreViolations, filesystem, network, credentials, bwrapPath | partial | TestSandboxApplyMerging | medium — ~11 top-level fields unparsed |
| filesystem | disabled, allowRead, denyRead, allowWrite, denyWrite, allowManagedReadPathsOnly | identical fields | match | TestSeatbeltProfile* | — |
| network | allowedDomains (domain \| `*.`wild \| `[ipv6]` \| `:port`), deniedDomains (+ bare `*`), deniedDomainReasons, allowedCidrs, deniedResolvedAddresses, allowUnixSockets, allowAllUnixSockets, allowLocalBinding, allowMachLookup, strictAllowlist, allowManagedDomainsOnly, httpProxyPort, socksProxyPort, tlsTerminate{excludeDomains…}, mitmProxy{socketPath,domains}, parentProxy{http,https,noProxy}, extraCaCertPaths | allowedDomains, deniedDomains, deniedDomainReasons, allowedCidrs, deniedResolvedAddresses, allowUnixSockets, allowAllUnixSockets, strictAllowlist, allowManagedDomainsOnly, parentProxy + extensions `allowAllLocal`, `interactiveHostApproval` | partial | TestSandboxNetworkDenied, TestSandboxManagedDomainsOnly, TestSandboxStrictAllowlistTiers, TestSandboxDeniedDomainReasonsMerge | high — `allowLocalBinding` name divergent (ours `allowAllLocal`); `httpProxyPort/socksProxyPort/tlsTerminate/mitmProxy/extraCaCertPaths/allowMachLookup` unparsed |
| credentials | files/envVars {path|name, mode deny|mask, extract (regex w/ ≥1 capture group — validated), onExtractNoMatch warn|deny|error, decode jwt, maskClaims, maskDuplicates, injectHosts}, allowPlaintextInject, awsPairs, sigv4{streaming,presigned,sigv4a}; schema cross-validates injectHosts vs allowedDomains & tlsTerminate.excludeDomains | identical fields incl. maskDuplicates, injectHosts, awsPairs, sigv4; no cross-validation of injectHosts vs allowedDomains/excludeDomains | partial | TestSandboxCredentialsSchemaParse, TestPrepareCredentials* (5 tests) | low |
| ignoreViolations | per-command violation suppression | parsed map, "not yet enforced" | gap | none | medium |
| Merge precedence | policy tier locks scalars it sets; lists merge; strictAllowlist honored only from policy/user (+--settings); allowManagedDomainsOnly policy-only | identical tier model (apply(), sandbox.go:242-349) incl. `nonProjectAllow`/`policyAllowed` tracking | match | TestSandboxApplyMerging, TestSandboxStrictAllowlistTiers, TestSandboxManagedDomainsOnly | — |
| excludedCommands | pattern per command segment | matchSandboxPattern + shellSegments | match | TestSandboxExcluded, TestSandboxWrapExcluded | — |
| Darwin seatbelt profile | SRT-generated SBPL: `(allow default)` + deny-read `_jn` list + read exceptions `kjn` + devices `Sct`, deny-write `/` + writable carve-outs, last-match-wins ordering; deny-read set identical in 294 | hand-generated profile; denyRead/allowRead/devices lists verified byte-identical to `_jn`/`kjn`/`Sct`; ordering deny→meta-allow→allow→re-deny→write-deny→write-allow→re-deny-write→devices | partial | TestSeatbeltProfile, TestSeatbeltProfileFilesystemDisabled, TestSeatbeltProfileProxy, TestSeatbeltProfileManagedReadOnly | low — profile emitted to a fixed tmp path; Mach/apple-events/pty rules absent (fields unsupported anyway) |
| Filesystem realpath | seatbelt evaluates resolved paths; real expands realpath per entry | withReal() adds EvalSymlinks variants | match | via seatbelt tests | — |
| Network w/ proxy | seatbelt denies all egress except loopback→proxy port(s); HTTP+SOCKS | same: deny network* + allow localhost:proxyPort (+socksPort); allowLocalBinding equivalent via allowAllLocal | match (behavior) | TestSeatbeltProfileProxy, TestSandboxWrapProxyEnv | — |
| Network deny-only | no proxy started; deny-all | networkDenied→blunt deny | match | TestSandboxNetworkDenied | — |
| Proxy binary | real = bundled SRT proxy (socat/HTTP/SOCKS MITM w/ CA, domain+CIDR+deniedResolved enforcement, credential injectHosts, SigV4 re-signing, parentProxy) | in-process netproxy.Proxy w/ MITM CA (LoadOrCreateCA), CONNECT-passthrough fallback, rules incl. deniedResolved/parentProxy/sigv4 | partial | none at e2e; package tests exercise parts | medium — capability subset: TLS terminate exists but tlsTerminate.excludeDomains/mitmProxy unix-socket variant/extraCaCertPaths unmodeled |
| WebCrypto/cert | real: generated CA + per-host certs; Java agent jar; extraCaCertPaths | LoadOrCreateCA; CA readable via seatbelt allowRead for SSL_CERT_FILE | partial | — | medium |
| Interactive host approval | non-strictAllowlist → ask callback on unmatched host | prompt via c.AskStructured when `interactiveHostApproval` set (extra gate real doesn't have — real prompts whenever !strictAllowlist and interactive) | partial | none | medium — real doesn't require the extra opt-in flag |
| Violations feed | `<sandbox_violations>` block populated from proxy denials per command | sandboxViolations→ViolationsFor | partial | none | low |
| ignoreViolations | suppresses listed violation patterns | unenforced | gap | none | medium |
| dangerouslyDisableSandbox | Bash input flag; honored only if `allowUnsandboxedCommands` (default true); when false → runs sandboxed w/ note; managed lock → hard error; still subject to normal permissions | sandboxWrap: identical three-way (allow→unsandboxed; locked→error; else sandboxed+note) | match | TestSandboxWrapDangerouslyDisable | — |
| autoAllowBashIfSandboxed | sandboxed Bash skips prompt when enabled | SandboxAutoAllow consulted at repl.go:210-212 | match | TestSandboxAutoAllow | — |
| failIfUnavailable | sandbox-unavailable → error vs unsandboxed+note | honored both layers | match | — | — |
| enabledPlatforms | platform gate list | absent | gap | none | low |
| Linux sandbox | bwrap + seccomp + socat binary resolution | sandbox_linux.go exists | unverified | — | — |
| Windows WFP | `windows` subconfig | absent (sandbox_other.go stub) | n/a | — | — |

## dangerous_tool_use / auto-mode classifier

| item | real + evidence | ours | status | e2e | severity |
|------|-----------------|------|--------|-----|----------|
| When consulted | permission result `ask` in auto mode (or plan+autoModeDuringPlan); read-only tools excluded; `Every X call is reviewed by the auto-mode classifier; saved allow rules and hook allows do not apply` (mod_0416.js g1e/dpe — verdict must carry `decisionReason.type==="classifier"`, an allow from any other source is rejected post-hoc) | loop.go:601 — SafetyEval called when Permission() returns false in auto mode | partial | none | medium — ours runs classifier only after interactive deny; real's classifier verdict overrides/invalidates hook-allows and saved-rule allows in auto mode, ours honors hook allow before classifier |
| What is sent | server-side `safeguard_tool_use` (single call) + `safeguard_results`; beta `dangerous-tool-use-2026-09-03`; transcript context; hook `classifierContext` | SafetyEvalCall/Stage2 (auxreq.go:718+): transcript serialized into `<transcript>`, `safetyPreamble` = user's CLAUDE.md with strict intent rules | partial | none | high — different transport (server safeguard API vs two local model calls), different context payload |
| Verdict shape | `evaluated{flagged|not_flagged}` / `skipped` / `unavailable`; unavailable → deny after repeated failures; flagged → deny | stage-1 sonnet-5 harm grade (no user intent), stage-2 opus-5 with CLAUDE.md intent; `<severity>N</severity>` ≥50 blocks; error → blocks with "could not evaluate" text | partial | none | medium |
| Input-mutation invalidation | hook `updatedInput` invalidates prior safeguard verdict → re-evaluation | ours evaluates the post-hook input (loop.go order: hooks → updatedInput → permission → SafetyEval) | match | — | — |
| Failure semantics | unavailable → block w/ distinct "transient, not a judgment" text | err → "Auto mode could not evaluate… usually transient" (mirrors real text) | match | none | — |
| Compound Bash | whole call classified (server receives the call) | SafetyEval(name, input) whole input | match | — | — |
| `disableAutoMode` | disables the feature entirely | honored via main.go:722 | match | — | — |

## E2E coverage map (audited item → test)

| area | covering tests | uncovered |
|------|----------------|-----------|
| Hook events | SessionStart/SessionEnd payload (3 tests), PreCompact, SubagentStop, CwdChanged(2), FileChanged, ConfigChange, InstructionsLoaded, Setup, TaskCreated, TaskCompleted, PostToolBatch, TeammateIdle(indirect), PermissionRequest(3: allow/deny/hook), StopFailure, PostToolUseFailure, include-hook-events wire records, plugin hooks (4 tests), matcher comma-list, exit-2, continue-false, plaintext context, permission allow, updatedInput, clear-session sequence, dontAsk deny text | UserPromptSubmit suppress/sessionTitle, UserPromptExpansion, Stop loop-control, SubagentStart, PreModelSwitch/PostModelSwitch, Notification types, PermissionDenied retry, Elicitation/ElicitationResult, WorktreeCreate stdout-path, WorktreeRemove, DirectoryAdded, MessageDisplay, prompt/agent/http hook types, timeout kill, env scrub, 64KB cap, suppressOutput, CLAUDE_ENV_FILE, hookEventName validation, async hooks |
| Permissions | package: BashRules, Modes, FileGlob, OrderAskBeatsAllow, SubcommandSplit, DontAsk, McpServerRule, RuleCorners; e2e: PerProjectAllowedTools, DontAskDenyText, PermissionRequest(Auto)Allow/Deny | auto-from-project rejection, managed-lock e2e, mode-cycle, don't-ask-again write shape, WebFetch domain rule, MCP `mcp__s__*` wildcard, parens-rejection |
| Sandbox | package only: ApplyMerging, EnvAlias, NetworkDenied, MatchSandboxPattern, Excluded, WrapDisabled/DangerouslyDisable/Excluded, AutoAllow, ManagedDomainsOnly, StrictAllowlistTiers, DeniedDomainReasonsMerge, CredentialsSchemaParse, PrepareCredentials(Env/Extract/AWSPairs/File), Seatbelt×4, WrapProxyEnv | zero e2e — no test runs a real sandboxed process, exercises the MITM proxy over wire, or covers ignoreViolations / interactiveHostApproval / sigv4 / credential injection end-to-end |

## Notes & judgment calls

- **Hook allow vs classifier ordering (real)**: in auto mode, real does not short-circuit on a hook `allow`/`saved allow` — the classifier verdict must be `decisionReason.type==="classifier"` (`dpe`/`g1e`, mod_0416.js:2215450). Ours lets `pre.Permission == "allow"` dispatch directly (loop.go:599). This is the most consequential behavioral divergence found: a project-supplied PreToolUse hook returning `allow` bypasses auto-mode safety review.
- **Real delegates sandboxing to SRT** (`@anthropic-ai/sandbox-runtime`) — ours re-implements; the surface that's verified identical (seatbelt lists, tier locks) is small but correct.
- **`hook_event_name` field**: real payload includes it (extracted construction sites confirm); ours sends it — consistent.
- **Windows-only paths** excluded from severity where irrelevant to this darwin-primary replica.

## File references

- Ours: `internal/hooks/hooks.go`, `internal/perms/perms.go`, `internal/tools/sandbox{,_darwin,_linux,_other,_proxy,_cred}.go`, `internal/config/config.go`, `internal/agent/loop.go`, `internal/api/auxreq.go`, `internal/repl/{repl,dialogs,commands}.go`, `internal/tools/{policy,misc,tasks,services}.go`, `main.go`.
- Real: `refsrc-294/mod_1337.js` (event registry), `mod_0416.js` (matcher G3/DVe/LVe/Frn, payload builders, hook output `Bee`, classifier dpe/g1e, prompt/agent gating), `mod_1571.js` (timeout table `t2n`), `mod_0304.js` (B0e charset, source ordering M7o, hook-type labels ED), `mod_0072.js` (rule validation mTe, matcher bJ), `mod_1261.js` (sandbox schema `ne`), `mod_0241.js` (credential plan `_b`/`Ad`), `mod_1573.js` (device-hook serve, `if` conditions), `mod_1112.js` (auto-mode source restriction message), `mod_2260.js` (exit-code dispatch), `refsrc-288/mod_0386.js` (seatbelt lists `_jn`/`kjn`/`Sct` — identical in 294).
