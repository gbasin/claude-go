# Wire + output-contract audit — real claude 2.1.294 vs claude-go replica

Scope: `/v1/messages` request shape, headers, `--output-format stream-json` events,
`~/.claude/projects/*.jsonl` transcript records, aux request lanes, tengu telemetry.

Evidence sources:
- `refsrc-294/` (bundled real source; mod_0304 = stainless SDK wrapper, mod_0416 =
  request assembly `oc={...}` @~2775763, mod_0083 = SDK schemas, mod_0137 = beta
  registry `Nne`/`aDn`, mod_1346 = print-pipeline output filter `uS`)
- `refsrc-288/` (prior version, for drift checks)
- `reference/request_full.json` (2.1.288 live capture, main checkout)
- `e2e/testdata/realwire_{print,resume,toolloop}_fable.json` (2.1.289 captures)

Status legend: **ok** = parity, **diff** = field present but shape/order differs,
**missing** = absent in ours, **extra** = we send something real doesn't.

Severity order: wire-visible > transcript-visible > internal.

---

## 1. POST /v1/messages?beta=true — request body

Real 294 key order (mod_0416.js @2775605):
`model, messages, system, tools?, tool_choice, betas?(bedrock), metadata,
max_tokens, thinking, temperature?, context_management?, safeguards?,
<extra spreads kp/xy/Ia/Ui>, output_config?, speed?, thread?, diagnostics?`
then `stream` appended by the SDK call. Captured 289 order confirms:
`model, messages, system, tools, metadata, max_tokens, thinking,
context_management, safeguards, output_config, diagnostics, stream`.

Ours (`internal/api/types.go` Request struct order):
`model, max_tokens, messages, system, tools, metadata, thinking,
context_management, safeguards, diagnostics, thread, output_config, stream,
temperature`.

| surface | real + evidence | ours | status | e2e | severity |
|---|---|---|---|---|---|
| body key order | model, messages, system, tools, metadata, max_tokens, thinking, ctx_mgmt, safeguards, output_config, diagnostics, stream (golden bodies; mod_0416 oc literal) | model, max_tokens, messages, system, tools, metadata, thinking, ctx_mgmt, safeguards, diagnostics, thread, output_config, stream | diff — max_tokens #2 vs #7; diagnostics/output_config swapped | golden test is structural (key-set + per-field), not byte order | wire-low |
| `model` | `"claude-fable-5-1"` (golden) | same | ok | golden_wire | — |
| `messages` | user/system mix incl. `role:"system"` mid-conversation records (resume golden: user, system, assistant, ...); content may carry `clear_at` markers (mod_0416 `_ve`) | supports string|blocks content; role system records; no `clear_at` on message objects | diff — no clear_at support seen | resume golden | wire-med |
| `system[]` block 0 | billing line `x-anthropic-billing-header: cc_version=2.1.289.4cb; cc_entrypoint=…; cch=…; cc_prompt_id=…; cc_turn_origin=…; cc_prompt_index=…; cc_turn_index=…;` — NO cache_control | same fields incl. cch (fnv approx — real's derivation unknown), cc_workload opt | ok (cch value not derivable — approximated, noted in code) | golden checks count+cc only | wire-med (cch is a real fingerprint mismatch) |
| `system[]` block 1 | identity text, NO cache_control (289 capture) / 1h ephemeral (288 capture — version drift) | no cache_control | ok for 289/294 | golden | — |
| `system[]` block 2 | main prompt `{ephemeral, ttl:1h, scope:global}` | same (ephemeral1hGlobal) | ok | golden | — |
| `system[]` block 3+ | session block `{ephemeral, ttl:1h}` (289 print capture) + custom `--append-system-prompt` blocks each `{ephemeral,1h}` | session block emitted via system_session*.txt; extra blocks appended | ok (session text 294-drift possible — not byte-checked) | golden shape only | wire-low |
| `tools[]` order | print capture: Agent, Bash, Edit, ListAgents, Read, ReportFindings, ScheduleWakeup, Skill, ToolSearch, Workflow, DeferredToolPlaceholder, Write, advisor; interactive 288: alphabetical + mcp__ tail | RequestDefs: same order, DeferredToolPlaceholder spliced before Write, advisor tail | ok | golden exact-order assert | — |
| `tools[]` schema | each `{name, description, input_schema}`; placeholder `{…, defer_loading:true}`; server/advisor tools `{type:"advisor_20260301",...}` | same incl. DeferLoading + Raw passthrough | ok | golden + toolsearch_test | — |
| `tool_choice` | emitted when caller supplies toolChoice; `{type:"tool"}` demoted to `auto` under thinking (mod_0416 @2771663) | no field at all | missing (only reachable via SDK-style hosts we don't expose) | none | wire-low |
| `metadata.user_id` | JSON-encoded string `{"device_id","account_uuid","session_id"}` + optional `ti`, `parent_session_id`, `tk`, and CLAUDE_CODE_EXTRA_METADATA keys **merged before** device fields with a size cap (KWe, mod_0304) | same triple + EXTRA_METADATA appended **after** session_id; no parent_session_id/ti/tk | diff — extra-meta position; parent_session_id missing | none asserted | wire-low |
| `max_tokens` | 64000 (fable captures) | MaxTokens, clamped to 64000 via maxTokensClamp | ok | golden | — |
| `thinking` | `{"type":"adaptive","display":"omitted"}` (sdk-cli) / `"updates"` (cli); enabled/disabled variants; post-assembly rewrite `yfr()` may substitute | adaptive w/ display updates|omitted; enabled/disabled; dropped after thinking-400 (noThinking) | ok | golden + aux tests | — |
| `temperature` | conditional (`c0`), e.g. title call sends 1 | sent only on aux title call (temp=1) | ok | aux_e2e | — |
| `context_management` | `{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}` gated on context-management beta in list (mod_0416 `Vo.includes(IAe)`) | DefaultContextManagement always on main/subagent when beta sent | ok | golden | — |
| `safeguards` | `[{type:"dangerous_tool_use", classifier_context:{v, permission_mode, platform, live_cwd, home_dir, rule_roots{11 roots}, trusted_directories{primary,additional,network,block_reads...}, rules{allow,deny,ask}, auto_mode:{allow,soft_deny,hard_deny,environment}, artifact_consent_holdback, case_insensitive_paths, restricted, is_remote_mode, classify_all_shell, user_identity, dropped?, truncated?, chrome_navigation?}}]` (zod @179489 + 288 capture) | same key set BUT `auto_mode` sent as **bool** (`permMode=="auto"`) vs real's **object of 4 arrays**; classify_all_shell sent `permMode=="auto"` vs real `false` in the same capture; Go map marshal sorts keys (real is insertion-ordered); no chrome_navigation/truncated | diff | none assert field-level shape | wire-high |
| `output_config` | `{effort:"medium"}` on per-turn-effort models; `format` for --json-schema | same gating (isXHighModel) + format + correlation_id (ours-only?) | mostly ok — **correlation_id nested here is ours' choice; real puts correlation_id on session-create APIs, not messages** | flags e2e | wire-med |
| `speed` | top-level `speed:"fast"` field when fast mode (mod_0416 `...cR!==void 0&&{speed:cR}`) | puts `speed:"fast"` **inside output_config** | diff — wrong location | none | wire-med |
| `thread` | `{type:"create"}` when thread state non-null (message-threads beta, haiku class) | threadField() on haiku models | ok | none | wire-low |
| `diagnostics` | `{previous_message_id}` emitted only when cache-diagnosis beta present AND (st||fork) AND main lane (`!Ly` — not subagent/aux) | always on main **and subagent** requests; null first turn, last assistant msg id after | diff — we emit on subagent requests where real omits | golden asserts presence | wire-low |
| `stream` | `true` | true | ok | golden | — |
| `betas` body field | only on bedrock lane (`u0`), via `anthropic_beta` after middleware | rewriteBedrock sets `anthropic_beta` | ok | provider_test | — |
| missing-in-ours spreads | `kp`/`xy` (non-aux extra params), `Ia`, `Ui` (incl. dynamic beta pushes like thinking-resumption `Lqe`, fast-mode `dXt` at request time) | none — request-time beta additions are static per entrypoint | diff — real conditionally adds `thinking-resumption-2026-07-17`, `fast-mode-2026-02-01` etc. per-request | none | wire-med |

## 2. Headers

Real 289 wire order (goldens): `accept, authorization, content-type,
user-agent, x-claude-code-session-id, x-stainless-{arch,lang,os,
package-version,retry-count,runtime,runtime-version,timeout},
anthropic-beta, anthropic-dangerous-direct-browser-access,
anthropic-dispatch-id, anthropic-version, x-app,
x-claude-code-prev-tool-durations?, x-claude-code-prompt-id,
x-claude-code-request-class, x-client-request-id, connection, host,
accept-encoding, content-length`.

| surface | real + evidence | ours | status | e2e | severity |
|---|---|---|---|---|---|
| request line | `POST /v1/messages?beta=true HTTP/1.1` | same | ok | golden | — |
| user-agent | `claude-cli/2.1.289 (external, sdk-cli)` — version is real's own | `claude-cli/2.1.292 (external, …)` — **pinned 292, real target is 294** | diff | none | wire-med (server-visible version skew) |
| x-stainless-package-version | `0.128.0` (still in 294 bundle, mod_0282) | 0.128.0 | ok | — | — |
| x-stainless-runtime-version | Bun version string of the build (`v26.3.0` in 288/289 captures) | pinned `v26.3.0` | ok-ish — may drift if 294 ships newer Bun | none | wire-low |
| anthropic-beta list | 18 flags, 289 order incl. `context-1m` at idx 2 when entitled; cli adds `thinking-display-updates` before afk-mode; haiku adds `message-threads`; **294 registry adds structured-outputs, task-budgets, prompt-caching-evict, cache-keepalive, fast-mode, redact-thinking, thinking-resumption, timing, inline-tools, server-side-fallback, fallback-credit, auto-mode-classifier etc.** (mod_0083 Wy table + jN rules — selection is rule-driven, not static) | static 289 list + context-1m/thinking-display-updates/message-threads conditionals | diff — matches 289 captures; 294 rule table can emit different sets (e.g. `fast-mode-2026-02-01` when fast mode, `auto-mode-classifier-2026-07-16`, `structured-outputs-2025-12-15` when schema output) | TestBetasHeader | wire-med |
| anthropic-dispatch-id | `v2d`, sits inside the anthropic-* block (before anthropic-version on wire) | sent, but serialized in the extras bucket after x-app | diff — order only | none | wire-low |
| x-app | `cli` | `cli` | ok | golden | — |
| x-claude-code-session-id | session uuid | same | ok | golden | — |
| x-claude-code-prompt-id | per-prompt uuid, stable across a turn's retries | same | ok | golden | — |
| x-client-request-id | fresh uuid per request attempt | same | ok | golden | — |
| x-claude-code-request-class | `main|subagent|workflow|compaction|auxiliary|side_reply` (mod_0137 gps) | `main|subagent|auxiliary|compaction` — no workflow/side_reply lanes | partial | subagent wire test | wire-low |
| **x-claude-code-prev-tool-durations** | `Tool=ms` pairs, url-encoded names, cap 32 tools/4096 chars (mod_0416 pxt); seen in toolloop golden (`Bash=645`) | **absent** | missing | toolloop golden carries it; test doesn't assert it | wire-med |
| x-claude-code-agent-id / -parent-agent-id | set for subagent/agent-context requests (mod_0304 header build) | absent (only x-claude-code-agent-type via env) | missing | none | wire-med |
| x-cc-context-compacted / x-cc-compaction-request / x-claude-code-compaction / x-claude-code-context-compacted | compaction-request markers (mod_0416 @871403, gon()) | **absent** — we send class=compaction but not the headers | missing | none | wire-med |
| x-claude-remote-session-origin / x-client-app / x-claude-remote-session-id / x-claude-remote-container-id | conditional remote/CCR headers | absent (x-correlation-id env var instead — real's header for corr id on /v1/messages is unverified; corr id lands on session-create APIs) | missing/diff | TestCorrelationID | wire-low |
| Accept-Encoding | `gzip, deflate, br, zstd` (fetch) / `gzip, compress, deflate, br` (axios aux lane) | same split via req.Close | ok | — | — |
| TLS ClientHello | BoringSSL-shaped (Bun) | utls hello (utls_hello.go) | ok (smoke test) | utls_smoke_test | — |

## 3. stream-json output (-p --output-format stream-json)

Real's print drain (mod_1346 `uS` filter + Z7/result builders). Emitted:
`system/init` (first), assistant, user, progress (wrapped), tool_use_summary,
attachment-derived user events (hook_system_message→user, queued_command,
tool_host_result_lines), system subtypes {status (non-"requesting" or
partial-mode), init, notification, api_retry, model_refusal_fallback (sans
`provisional`), model_refusal_no_fallback, memory_recall, thinking_tokens,
compact_boundary, model_fallback, model_consent_fallback, informational},
stream_event (only --include-partial-messages), result.
Dropped from stdout: tombstone, stream_request_start, tool_progress,
command_lifecycle, auth_status, prompt_suggestion, rate_limit_event,
sdk_status, compact_progress, stream_mode, response_length, set_expanded_view,
post_turn_summary, active_goal, set_in_progress_tool_use_ids,
conversation_reset, hint_clears, api_metrics, os_notification,
open_message_selector, apply_flag_settings, refusal_continuation,
query_model_change.

| surface | real + evidence | ours | status | e2e | severity |
|---|---|---|---|---|---|
| system/init | `{type:"system",subtype:"init",cwd,session_id,tools,mcp_servers:[{name,status,source?}],model,permissionMode,slash_commands,terminal_slash_commands?,apiKeySource,betas:<real list>,claude_code_version:"2.1.294",output_style,agents:[agentTypes],skills:[invocable skill names],plugins:[{name,path,source?,version?}],plugin_errors?,plugin_warnings?,capabilities?,mcp_server_errors?,analytics_disabled,product_feedback_disabled,uuid,memory_paths?,scratchpad_path?,startup_timing?(CCR)}` (z0 schema mod_0083 + builder mod_0527) | emits subset: no mcp_servers[].source, no terminal_slash_commands, `betas:[]` (empty — real ships actual list), agents hardcoded `[general-purpose,Explore,Plan]`, skills=slash command names (wrong set), plugins=[] (real emits objects when any), no analytics_disabled/product_feedback_disabled/mcp_server_errors/memory_paths, version "2.1.292" | diff | TestPrintStreamJSON asserts key presence only | wire-visible(stdout)-med |
| assistant events | one per content block sharing message.id; `message` = full API message (id, model, usage, stop_reason…); fields: uuid, session_id, parent_tool_use_id, request_id?, user_message_uuid?, error? | **text turns emit NO assistant event** — only tool_use blocks emitted, with fabricated `toolu_<uuid12>` id instead of the API-assigned id; message lacks id/model/usage | missing/diff | none assert assistant text events | wire-visible HIGH |
| user (tool_result) events | `{type:"user",message:{role:"user",content:[{tool_result, tool_use_id:<real id>}]},parent_tool_use_id,tool_use_result:<tool Output object>,uuid,session_id,isSynthetic?}` | emits tool_result with the fabricated local id; no tool_use_result | diff | print tool round-trip asserts echo text only | wire-visible med |
| user echo | real echoes the submitted user message (typed prompt + compact summary isReplay flags) | only --replay-user-messages echoes stdin frames; no echo of the -p prompt | missing | none | wire-low |
| stream_event (partial) | raw SSE-shaped frames incl. message_start/delta, content_block_start/stop; stamped session_id | only content_block_delta text/thinking fabricated (index 0, no message_start/stop) | diff — partial | none | wire-med |
| system subtypes | init + status, notification, api_retry, compact_boundary, model_*fallback, thinking_tokens, memory_recall, informational | init only | missing | none | wire-med |
| tool_use_summary | emitted after consecutive tool calls | absent | missing | none | wire-low |
| result success | `{type:"result",subtype:"success",is_error,duration_ms,duration_api_ms,ttft_ms?,ttft_stream_ms?,time_to_request_ms?,num_turns,stop_reason,session_id,total_cost_usd,usage:{input_tokens,output_tokens,cache_*,server_tool_use,service_tier,cache_creation{1h,5m},...},modelUsage:<per-model map incl contextWindow/costUSD>,permission_denials,fast_mode_state,fast_mode_disabled_reason?,origin?,subagent_stats?,safety_stops?,result,uuid}` | emits core subset; missing ttft_ms, time_to_request_ms, fast_mode_disabled_reason, origin, subagent_stats, safety_stops; modelUsage lacks contextWindow/maxOutputTokens/etc.; usage lacks inference_geo, speed, iterations fields real adds | diff | TestPrintStreamJSON asserts type/subtype/is_error/result | wire-visible med |
| result error | subtype error_during_execution, errors[], terminal_reason | same shape (no terminal_reason) | ok-ish | print err path | wire-low |
| hook events | hook output becomes attachment hook_system_message → emitted as user event; hook progress = progress events | env-gated CLAUDE_GO_INCLUDE_HOOK_EVENTS raw records | diff | hook tests use text mode | wire-low |
| rate_limit_event | **dropped on -p stdout** (uS filter) — only on remote/bridge lanes | n/a (we don't emit either — coincidentally consistent) | ok for -p | — | — |

## 4. Transcript records (~/.claude/projects/<slug>/<sid>.jsonl)

Real 294 record-type universe (cWr routing table, mod_0416 @4462506):
`user, assistant, system, attachment, progress` (transcript class);
`file-history-snapshot, file-history-delta, last-prompt, continued-in,
api-request-shape, api-request-blob, api-request` (boundary-cleared);
`content-replacement, fork-context-ref, frame-link` (accumulate);
`summary, custom-title, ended-by-model, ai-title, tag, relocated,
agent-name, agent-color, agent-setting, pr-link, artifact-autoreact-ledger,
bridge-session, history-suppression, mode, permission-mode, isolation-latch,
dev-mods, atis-latch, worktree-state, cost-state` (last-wins);
`artifact-comment-monitor, memory-mode` (accumulate); `observer-ref`.

| surface | real + evidence | ours | status | e2e | severity |
|---|---|---|---|---|---|
| record line key order | real writes `{"parentUuid":…` FIRST (the streaming scanner oOn anchors on `{"parentUuid":` at byte 0) | ours' event struct emits `type` first | diff — field order | none assert order | transcript-low |
| user/assistant records | `{parentUuid,isSidechain,userType,cwd,sessionId,version,type,message,uuid,timestamp,gitBranch?,isMeta?,toolUseResult?,requestId?,agentId?,thinkingMetadata?…}` | same field family; `toolUseResult` field exists but **never populated** (no caller) | diff | store_test, session e2e | transcript-med |
| summary | `{type:"summary",summary,leafUuid}` | same | ok | — | — |
| system/init | `{type:"system",subtype:"init",model,cwd,version,userType,gitBranch,entrypoint}` (+ real: agentType, permissionMode, sessionKind…) | same + parentSessionId/agentType/systemPromptSnapshot/permissionMode | ok | store_test init | — |
| system/compact_boundary | `{type:"system",subtype:"compact_boundary",compactMetadata:{trigger,preTokens,…},logicalParentUuid?}` | same (LogCompactBoundaryTrig) | ok | compact e2e | — |
| attachment | `{type:"attachment",attachment:{…}}` many subtypes (queued_command, hook_system_message, cowork_memory_context…) | generic LogAttachment | ok | — | — |
| progress | `{type:"progress",data,toolUseID,parentToolUseID}` | same | ok | — | — |
| **ai-title** | `{type:"ai-title",aiTitle,sessionId}` written by saveAiGeneratedTitle | ours writes summary + custom-title on title set; no ai-title record | missing | none | transcript-med |
| custom-title | `{type:"custom-title",customTitle}` + sidecar `<sid>/custom-title.json` {customTitle} | same + extra `title` key in sidecar (forward-compat — real writes only customTitle) | ok/extra | store_test | transcript-low |
| file-history-snapshot/-delta | written for rewind/checkpoint | ours writes snapshot w/ isSnapshotUpdate (main.go:893); delta absent | partial | checkpoint/rewind tests | transcript-low |
| last-prompt | `{type:"last-prompt",…}` (session picker metadata) | absent | missing | none | transcript-low |
| queue-operation | `{type:"queue-operation",operation,timestamp,sessionId,content?,reason?,commandUuid?,deliveryId?}` | absent | missing | none | transcript-low |
| pr-link | `{type:"pr-link",sessionId,prNumber,prUrl,prRepository,timestamp}` | absent | missing | none | transcript-low |
| mode / permission-mode / tag / agent-name/-color/-setting / ended-by-model / continued-in / cost-state / worktree-state / history-suppression / bridge-session / attribution-snapshot / api-request-* / fork-context-ref / observer-ref / dev-mods / memory-mode / atis-latch / isolation-latch / artifact-* / frame-link / relocated / content-replacement | full record-type universe above | absent — ~25 record types never written | missing (acceptable if feature absent; several map to unimplemented features) | none | transcript-low each |

## 5. Aux requests

| surface | real + evidence | ours | status | e2e | severity |
|---|---|---|---|---|---|
| startup GET burst | sequence incl. /v1/mcp_servers×2, /api/oauth/account/settings, /mcp-registry/v0/servers, /api/claude_cli/bootstrap, /api/claude_code_penguin_mode, /api/claude_code_grove, org-scoped skills/plugins/marketplaces + /v1/code/sessions + /v1/ultrareview/quota (cli) | same list in captured order (auxreq.go startupCalls) | ok | TestAuxStartupSequence | — |
| aux header families | axios lane, per-endpoint UA split (claude-code vs claude-cli), mcp beta/proto headers, org uuid header | famMCP/famCLI/famCode/famRegistry/famOrg | ok | aux tests | — |
| quota probe | `POST /v1/messages?beta=true` `{model:<probe model>,max_tokens:1,messages:[{role:"user",content:"quota"}],metadata:{user_id:…}}`, source:"quota_check", maxRetries:0 (gnt @65597) | same shape; hardcodes `claude-haiku-4-5-20251001` — **real passes the resolved model `e`, not necessarily haiku**; request class "auxiliary" | diff (model choice) | aux gated tests | wire-med |
| title generation | goes through the in-process query pipeline (sH) with `outputFormat:{type:"json_schema",{title:string}}`, querySource "generate_session_title", toolFreeHelper, content truncated by tengu_lucky_brooks cap | raw auxMessageCall: haiku, temp 1, thinking disabled, sys=[billing,identity,title prompt]; **no output_config.format json_schema**; no input truncation | diff — wire-visible (missing format param) | none | wire-med |
| walkaway classifier | prompt verbatim in mod_2197 (4-state done/working/blocked/failed; prefix verified identical to ours) | aux_walkaway_prompt.txt matches; bare-string user msg, 1024 max, thinking disabled | ok | — | — |
| safety classifier | two-stage sonnet-5 → opus-5; stage-1 grading text `Wlo` verbatim matches ours; transcript blocks carry `{ephemeral,1h}` cache_control; classifier beta `auto-mode-classifier-2026-07-16` (mXt/Cuo in registry) can be added | same prompts/models/shape via safetyEval; beta not added to that request's header (ours uses the standard client beta list — real's classifier request pushes the classifier beta) | diff | TestSafetyEval* | wire-med |
| compaction call | class "compaction", FULL tool surface, plus headers x-cc-compaction-request/x-claude-code-compaction | class "compaction" + full tools; headers absent | diff | compact e2e | wire-med |
| event_logging | POST /api/event_logging/v2/batch {events:[{event_type:"ClaudeCodeInternalEvent",event_data:{event_name,client_timestamp,model,session_id,parent_session_id,user_type,betas(first 7),env{…25 fields},entrypoint,is_interactive,client_type,process(b64),additional_metadata(b64),event_id,device_id,auth{org,acct}}}]} | same envelope | ok | none (fire-and-forget) | — |

## 6. Telemetry (tengu events)

| surface | real + evidence | ours | status | e2e | severity |
|---|---|---|---|---|---|
| event catalog | **2877 distinct `tengu_*` names** in the 294 bundle (counted) | ~20 emitted/read (tengu_feature_ok/bad/sad, tengu_skill_loaded, tengu_plugin_enabled_for_session, tengu_started, tengu_cli_flags + flag-name reads) | massive coverage gap — we emit only a fabricated startup burst | aux e2e asserts batch shape only | internal |
| per-request API events | real emits tengu_api_query/api_success/api_error/api_retry/api_cache_breakpoints etc. per /v1/messages call | none per request | missing | none | internal (network-visible volume) |
| core_metadata | real's `Dyr` builds model/sessionId/userType/betas/envContext/isClaudeAiAuth/entrypoint/sessionKind/hasAttacher/agentSdkVersion/desktopAppVersion/isInteractive… | subset via tenguEnv/tenguEvent | diff | none | internal |

## 7. e2e coverage summary

| test | covers |
|---|---|
| golden_wire_e2e_test.go | request line, model/max_tokens/stream, system block count+cache_control, exact tool order, thinking/context_management/diagnostics, header presence (not values/order) |
| golden toolloop/resume | multi-request message shapes, system-string records, tool_result flow |
| aux_e2e_test.go | aux burst, safety eval, subagent wire, betas header, 401 retry, deferred tools, correlation id, workload tag |
| print_e2e_test.go | stream-json init keys + result subtype; **no assistant-text assertion** |
| gaps | no assertion on: safeguards shape, body key order, prev-tool-durations, compaction headers, subagent agent-id headers, stream-json assistant/user event fidelity, transcript record field order, tengu per-request events |

## Top gaps (by severity)

1. **stream-json assistant events**: text turns emit nothing; tool_use events carry fabricated ids — the dominant output-contract divergence (wire-visible HIGH).
2. **safeguards.auto_mode**: bool vs real's `{allow,soft_deny,hard_deny,environment}` object; classify_all_shell also wrong for auto mode; map-marshal sorts keys (wire-visible HIGH if classifier reads it — server may tolerate).
3. **Version skew**: UA/sys[0]/init/transcripts all claim 2.1.292 vs target 2.1.294 (wire-visible MED).
4. **Missing headers**: x-claude-code-prev-tool-durations (captured live), x-cc-compaction-*/x-claude-code-*compaction, x-claude-code-agent-id/-parent-agent-id (wire-visible MED).
5. **speed field placement**: output_config.speed vs real's top-level `speed` (wire-visible MED).
6. **294 beta drift**: static 289 list vs 294's rule-driven set (fast-mode, auto-mode-classifier, structured-outputs, thinking-resumption conditionals) (wire-visible MED).
7. **Title call** missing output_config.format json_schema (wire-visible MED).
8. **Transcript**: missing ai-title + ~25 record types; toolUseResult never populated; record key order (parentUuid first) differs (transcript-visible MED/LOW).
9. **Telemetry**: only fabricated startup burst vs real's per-request tengu_api_* + 2877-name catalog (internal).
