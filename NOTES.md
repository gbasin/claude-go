
# Transcript record-type notes (subagent workstreams)

## recs-meta
# Session-metadata transcript records (2.1.296 parity)

Real CC (mod_0416.js `appendEntry` + `planReAppendSessionMetadata`) splits
transcript rows into two emit paths:

- **Chained entries** (`user`/`assistant`/`system`/`attachment`/…): carry
  `uuid`, `timestamp`, and `parentUuid` = the *previous row's* uuid (the
  chain threads through attachments and system rows, not just messages;
  `null` at the root). Plus the session envelope: `isSidechain`,
  `userType`, `entrypoint`, `cwd`, `sessionId`, `version`, `gitBranch`.
  Streaming scanners anchor on `{"parentUuid":` at byte 0, so these rows
  must emit `parentUuid` FIRST.
- **State records** (the `fG` merge-policy table in mod_0300): bare
  `{type, <fields>, sessionId}` — no `uuid`/`parentUuid`/`timestamp`
  (except the timestamped subset below). Each latched value re-stamps on
  every metadata flush, which runs after each appended prompt batch.

`internal/session/store.go` now mirrors both paths: `logMessage` emits
ordered pairs, `LogRecord` routes state types through `logState` (order
tables `stateKeyOrder`/`chainedKeyOrder`), `restampMeta` replays the flush
block after each user prompt, and `Open` tail-scans the existing file to
restore latched state + chain anchors on resume.

## Implemented

| type | schema (emit order) | emit sites |
|---|---|---|
| `mode` | `{type,mode:"normal"\|"coordinator",sessionId}` | `StampMode` — session start (main.go). Coordinator → `"coordinator"`. Shift+tab cycles are permission-mode, NOT this row (bug fixed). |
| `permission-mode` | `{type,permissionMode,sessionId}` | `StampPermMode` — session start, shift+tab cycle, `ToolCtx.SetPermMode` |
| `tag` | `{type,tag,sessionId}` | `LogAgentMeta` writer + latch + restamp. No slash command sets it — callable for parity. |
| `agent-name` | `{type,agentName,sessionId}` | `LogAgentMeta` — teammate sessions (`--agent-name`/`TeammateCtx`) |
| `agent-color` | `{type,agentColor,sessionId}` | `/color` command, `TeammateCtx.Color` |
| `agent-setting` | `{type,agentSetting,sessionId}` | `--agent <type>` flag at session start |
| `ai-title` | `{type,aiTitle,sessionId}` | `LogAITitle` — `OnTurnEnd` title-generation path (existing wiring) |
| `custom-title` | `{type,customTitle,sessionId}` | `/rename` (`LogTitle`), picker rename (`WriteTitle` — now bare, no uuid/ts) |
| `last-prompt` | `{type,lastPrompt?,leafUuid,sessionId}` | `restampMeta` per user prompt; `lastPrompt` normalized (\n→space, 200-char ellipsis) like `normalizeLastPrompt` |
| `ended-by-model` | `{type,timestamp,sessionId}` | `ToolCtx.EndSession` (EndConversation tool → `endSession`) |
| `continued-in` | `{type,timestamp,sessionId,continuedInSessionId}` | `StampContinuedIn(path, srcID, newID)` — stamped on the SOURCE file after `--fork-session` `ForkInto` succeeds |
| `pr-link` | `{type,sessionId,prNumber,prUrl,prRepository,timestamp}` | `LogPRLink` — `loop.OnSessionRecord` (commitAndPR paths; existing wiring) |
| `relocated` | `{type,sessionId,relocatedCwd}` | `ToolCtx.OnCwdChanged` (Bash cd sentinel, `/cd`) — `LogRelocated` dedups |
| `frame-link` | `{type,sessionId,path,frameUrl,title,artifactCount,timestamp}` | `emitFrameLink` after successful Artifact page publish (`artPublish` → `ToolCtx.OnSessionRecord`) |
| `isolation-latch` | `{type,side,sessionId}` | `LogIsolationLatch` writer + latch + restamp — no backing feature yet |
| `atis-latch` | `{type,atis,sessionId}` | `LogAtisLatch("")` at session start; latched, restamped per prompt |
| `dev-mods` | `{type,folder,sessionId}` | `LogDevModsFolder` writer + latch + restamp — no backing feature yet |
| `observer-ref` | `{type,agentId?,observerTaskId,observerAgentType,timestamp,sessionId}` | `LogObserverRef` writer only — no observer feature |
| `queue-operation` | `{type,operation,timestamp,sessionId,content?}` | existing `LogRecord` caller; now emitted bare (no uuid/parentUuid) |
| `summary` | `{type,summary,leafUuid}` | `LogSummary`/`LogTitle`/`main.go` — now bare off-chain |

Also converted to the state path (bare rows, per real): `file-history-snapshot`
`{type,messageId,snapshot,isSnapshotUpdate}`, `file-history-delta`
`{type,messageId,snapshotMessageId,trackingPath,backup,timestamp}`.

## Message-row ordering (the byte-0 `{"parentUuid"` anchor)

`logMessage` now emits user/assistant rows with real's key order:

- user: `parentUuid, isSidechain, promptId, agentId?, type, message,
  isCompactSummary?, modelOnlyText?, uuid, timestamp, permissionMode?,
  userType, entrypoint, cwd, sessionId, version, gitBranch`
- assistant: `parentUuid, isSidechain, agentId?, message, type, uuid,
  timestamp, …envelope…`

New fields emitted to match real: `promptId` (rotates per typed prompt,
shared by that turn's tool_result rows — real's `requestJournal.promptId`),
`permissionMode` on typed-prompt user rows, `agentId` on sidechain
(subagent transcript) rows, `parentUuid:null` at the chain root,
`isSidechain:false` always present, ISO8601 **millisecond** timestamps
(`isoNow` — real's `toISOString()`), and `gitBranch` omitted when empty.

Resume restore: `Open` tail-scans (512KB) existing content — restores
`chainParent`/`lastUUID`, `lastPrompt`, `mode`, `permissionMode`, `atis`,
titles, tag, agent identity, `relocatedCwd`, latches, `pr-link`,
`artifactCount` — so resumed sessions restamp the right values and keep
the uuid chain (`SeedParent` still overrides `lastUUID` for leaf resume).

## Skipped / partial

- `isolation-latch` — real fires it when the tool-use isolation latch
  engages (`isolationLatch.current` → `saveIsolationLatch`). Ours has no
  isolation latch; `LogIsolationLatch` + latch/restamp exist for callers.
- `dev-mods` — real latches the dev-mods folder (`saveDevModsFolder`).
  No equivalent feature; `LogDevModsFolder` writer only.
- `observer-ref` — real writes it when an agent-observer pairing
  delivers (`recordObserverRef`). No observer feature; writer only.
- `continued-in` for `/background` — real's bg spawn forks to a NEW
  session id and stamps `continued-in` on the source file. Ours'
  `bg.Spawn` resumes the SAME session id in one file, so there is no
  superseded transcript to mark. Wired for `--fork-session` instead.
- `worktree-state`, `memory-mode`, `bridge-session`,
  `history-suppression`, `attribution-snapshot`, `cost-state`,
  `artifact-comment-monitor`, `artifact-autoreact-ledger`,
  `content-replacement`, `fork-context-ref`, `remote-*` — real flush
  members outside the requested 15; `worktree-state` is in the order
  table, the rest have no backing features.
- Message rows skip `requestId`, `thinkingDurationMs`, `apiBlockIndex`,
  `perTurnEffort`, `origin`/`promptSource`/`turnOrigin`/`turnPosition`,
  `wireIngestContext`, `slug` — fields ours doesn't track.
- Real also RELOCATES the transcript file on cwd change (moves it to the
  new project dir); ours stamps the `relocated` row but keeps the file.

## Schema surprises (vs prior assumptions)

- `mode` is the COORDINATOR/NORMAL session mode — the shift+tab cycle is
  `permission-mode` (`{type:"permission-mode",permissionMode}`), a
  separate record. Ours previously wrote the perm cycle as `{type:"mode"}`
  and programmatic changes as `{type:"system",subtype:"permission-mode"}`.
- `isolation-latch` carries `side` (which side of the isolation fence),
  not an `isolation` field; `dev-mods` carries `folder`; `atis-latch`
  carries `atis`.
- The per-prompt flush re-appends the WHOLE latched set — `mode`,
  `permission-mode`, `atis-latch`, etc. repeat per prompt in real files
  (observed: the block lands right after each `last-prompt` stamp).
- `frame-link` has two shapes: the full row on publish
  (path/frameUrl/title/artifactCount) and a count-only variant
  (`{type:"frame-link",artifactCount,sessionId,timestamp}`) in flushes.
- `queue-operation` is a bare state row, not a chained transcript entry.
- `summary`/`custom-title`/`ai-title` carry no `uuid`/`timestamp` — the
  earlier `WriteTitle`/`LogSummary` envelope fields were inventions.
- `compact_boundary` stamps `parentUuid:null` + `logicalParentUuid` =
  last message uuid — it deliberately breaks the uuid chain.

## recs-links
# Transcript record types — linkage/lifecycle rows

Branch `work/recs-links`. Sources: carved real source
`~/Code/claude-extracted-296/` (module refs below) plus captured real
`~/.claude/projects/*/[id].jsonl` transcripts.

Key finding — **record classes differ in envelope**: chain records
(`user`, `assistant`, `attachment`, `progress`?) carry uuid/parentUuid/
message envelope; bookkeeping rows (last-wins/boundary/accumulate) carry
exactly the emit-site literal — no uuid, no parentUuid, no auto timestamp.
`Store.LogBare` writes those verbatim; `Store.LogAttachmentMeta` builds
the full attachment envelope and joins the parentUuid chain.

## worktree-state — implemented

- Real: `saveWorktreeState` (`AM`, mod_0416) writes
  `{type:"worktree-state",worktreeSession:{originalCwd,
  preEnterOriginalCwd,worktreePath,worktreeName,worktreeBranch,
  originalBranch,originalHeadCommit,sessionId,tmuxSessionName?,
  hookBased,enteredExisting},sessionId}` — bare, no uuid/timestamp.
  `tmuxSessionName` only when the worktree carries a tmux session;
  `hookBased` marks SessionStart-hook worktrees (old `hookId`).
  `AM(null)` on ExitWorktree writes `worktreeSession:null`.
- Ours: `flags.go:setupWorktree` builds the descriptor on `--worktree`
  (captures originalBranch/originalHeadCommit pre-worktree,
  `hookBased:false`, `enteredExisting` on the reuse branch, tmuxSessionName
  when `--tmux` creates one); `main.go` emits `st2.LogWorktreeState` after
  `LogInitMeta`. No ExitWorktree path exists locally (no emit of null).
- Surprise: real re-stamps the tail row on resume (restore path dedupes
  an identical serialized row — a same-state re-stamp is a no-op), so
  emit-on-enter alone is equivalent here.
- Tests: `TestBareRecordSchemas` (session).

## fork-context-ref — implemented

- Real: `fJr` (mod_0314-ish emit site) writes `{type:"fork-context-ref",
  agentId,parentSessionId,parentLastUuid,contextLength}` — captured rows
  have NO sessionId/uuid/timestamp. Emitted when a fork subagent opens
  with a reference to the parent's transcript tail instead of copied
  messages; routed to the agent transcript.
- Ours: `agent/subagent.go` — `runAgentTurn` stamps it on the subagent
  transcript right after `OpenSubagent` when `sub.forkedContext` (set in
  the `def.isFork() && r.Parent != nil` block), using
  `session.LeafUUID(r.Transcript)` for parentLastUuid and
  `len(r.Parent.Messages)` for contextLength. Skipped when no leaf exists
  (real's `R!==void 0` guard) or no transcript.
- Note: this is NOT the same as `forkedFrom` on `--fork-session` copies —
  that stays as-is (real stamps forkedFrom on copied rows there too).
- Tests: `TestBareRecordSchemas`, `TestForkFlagSet` (agent).

## continued-in — implemented

- Real: `{type:"continued-in",timestamp,sessionId,
  continuedInSessionId}` written by `TRe`/`Jns` (mod_0292) onto the SOURCE
  transcript when work hands off to a different session id;
  `onlyIfExists`. Picker hides the superseded file.
- Ours: `session.LogContinuedIn` (bare writer) +
  `session.LogContinuedInTo`/`AppendContinuedIn` for foreign files.
  Wired in `main.go` `--bg -r <id>`: after `bg.Spawn` succeeds and the
  source resolves to a different session id (project-dir lookup via
  `ResolveArg`, or a `.jsonl` path directly). `/background` keeps the
  same session id — no marker (matches real: same-id continue has
  nothing to mark).
- Fix: picker's `continuedIn` scan now matches the real field name
  `continuedInSessionId`.
- Tests: `TestAppendContinuedIn`, `TestPickerFilters` (existing).

## ended-by-model — implemented

- Real: `{type:"ended-by-model",timestamp,sessionId}` — bare, from
  `markSessionEndedByModel` (TJr) when `EndConversation` ends the session
  (mod_1681 call site). Picker hides ended sessions.
- Ours: `repl.go` `endSession` (the `EndSession` tool callback) emits it
  via `currentStore.LogBare`. `LogEndedByModel` writer also corrected to
  the bare literal.
- Emit site matters: only the model's EndConversation triggers it — quit,
  ctrl+c, /exit do NOT stamp (real equivalent: they don't call TJr).
- Tests: `TestBareRecordSchemas`, `TestPickerFilters`.

## ai-title — implemented (already wired; schema corrected)

- Real: `{type:"ai-title",aiTitle,sessionId}` — bare (captured rows show
  exactly those three keys).
- Ours: `main.go` already calls `st2.LogAITitle(t)` on first-turn title
  generation; `LogAITitle` now writes the bare literal (was stamped with
  uuid/parentUuid/timestamp).
- Tests: `TestBareRecordSchemas` (exact key set).

## attachment: queued_command — implemented

- Real (capture-verified): full attachment envelope — top-level
  `{type:"attachment",uuid,parentUuid,timestamp,isSidechain,sessionId,
  session_id,cwd,version,gitBranch,userType,entrypoint,rendered,
  renderedInHumanTurn?}`, attachment `{type:"queued_command",prompt,
  source_uuid,delivery_id,commandMode:"prompt",origin:{kind:"human"},
  timestamp}` — plus `isMeta` when the queued item was meta.
- Ours: `repl.go` — when a mid-turn queue item is delivered to the model
  (after local #//!// dispatch decides it's a prompt), the loop emits
  `queue-operation:dequeue` (bare) + the queued_command attachment via
  `queuedCommandAttachment()`. Typed items: `commandMode:"prompt"`,
  `origin:{kind:"human"}`, `rendered` = real's `<system-reminder>The user
  sent a new message while you were working:…</system-reminder>` body
  (mod_0416 `$Dn`) + `renderedInHumanTurn`. Session-injected items
  (tools.SessionQueue: cron/monitor) emit `commandMode:"task-notification"`,
  `origin:{kind:"task-notification"}`, no rendered body (real's task
  rendered text wasn't capture-verified).
- Also fixed: queue-operation enqueue/dequeue now write bare rows with
  `timestamp` (was LogRecord, which stamped uuid/parentUuid extras real
  doesn't have).
- Tests: `TestQueuedCommandAttachment` (repl), `TestAttachmentEnvelope`
  (session — verifies chain participation).

## attachment: hook_system_message — implemented

- Real: `{type:"attachment",…envelope…,attachment:{type:
  "hook_system_message",content,hookName,toolUseID,hookEvent}}` —
  captured as `hookName:"PostToolUse:Bash",toolUseID:"toolu_…",
  hookEvent:"PostToolUse"` and `hookName:"Stop",toolUseID:"hook-…",
  hookEvent:"Stop"`. Real builds these in the hook runner for any hook
  systemMessage.
- Ours: `agent/loop.go` `runHook` emits it centrally via the new
  `Loop.OnAttachment` callback (wired to `st2.LogAttachmentMeta` in
  main.go; subagent turns route to their subagent transcript). hookName
  is `"<Event>:<matcher>"` when the event has exactly one matcher with a
  pattern, else the bare event name. toolUseID = the payload's
  `tool_use_id` when present, else `hook-<uuid>` (real's `hook-${id}`
  fallback). `fireStopHook` now routes through `l.RunHookEvent` so Stop
  hooks emit the same row (and hook_started/response events under
  --include-hook-events, matching real).
- Tests: `TestHookSystemMessageAttachment`, `TestHookSystemMessageNoTool`
  (agent).

## attachment: cowork_memory_context — skipped

- Real: `{type:"cowork_memory_context",version,content,leg:"laptop"|
  "ccr",…}` emitted by the cowork/account-memory prefetch machinery
  (mod_0063/memory context pipeline) — content can carry a rendered
  block and leg/anchor/toolsOnly variants.
- Ours has no cowork memory service: no memory-context attachment
  producer exists. Emitting an empty/mocked row would fabricate data;
  wiring point doesn't exist. Skipped.

## content-replacement — partially implemented (fork replay only)

- Real: `{type:"content-replacement",sessionId,agentId?,replacements:
  [{kind:"tool-result",toolUseId,replacement}]}` — accumulate class.
  Emitted when the persisted-tool-result budget rewrites tool-result
  content; the session-copy path (mod_1044 `ne`) collects every
  `content-replacement` row from the source and re-emits ONE
  consolidated `{type:"content-replacement",sessionId:<new>,replacements:
  [...]}` after the copied messages.
- Ours: `Store.replayContentReplacements` implements the fork replay
  (called from `Fork` and `ForkInto` — so `--fork-session` and `/fork`).
  The LIVE emit is skipped: ours has no persisted-tool-result budget
  enforcement that rewrites wire content — there is nothing to record.
- Tests: `TestForkReplaysContentReplacements` (consolidated row, source
  sessionId filter, bare shape).

## file-history-delta — implemented (+ snapshot restructure)

- Real: snapshots are per USER MESSAGE — `{type:"file-history-snapshot",
  messageId:<user-msg-uuid>,snapshot:{messageId,trackedFileBackups:
  {path:{backupFileName,version,backupTime,realParentDir?}},timestamp},
  isSnapshotUpdate:false}` emitted when the user message is appended
  (Tke; empty `trackedFileBackups:{}` rows exist in captures). Delta:
  `{type:"file-history-delta",messageId,snapshotMessageId,trackingPath,
  backup,timestamp}` emitted when a file is FIRST tracked mid-turn —
  real merges it into the latest snapshot.
- Ours: restructured to match. `tools.Checkpoints` now tracks
  `tracked`/`pending` sets: `Snapshot()` still writes the pre-write blob
  (+ per-path `version` numbering now), records a pending FileBackup on
  first track. `Store.OnUserPrompt` (set in main.go) fires after each
  user prompt row lands → emits the snapshot row (messageId = user
  uuid) then flushes pending deltas with `messageId=snapshotMessageId=
  <that uuid>`. Respects `CLAUDE_CODE_DISABLE_FILE_CHECKPOINTING`.
  `--rewind-files` (`RewindFiles`) now restores via messageId linkage
  (last snapshot for the anchor + deltas by snapshotMessageId), with a
  positional fallback for transcripts written by the old per-write
  scheme.
- Divergences (documented): ours emits the snapshot when the user
  message is LOGGED (post-turn batching), so snapshot/delta rows land
  after the user row and before that turn's assistant rows — real
  interleaves deltas among the turn's assistant records. delta
  `messageId` is the turn's user-message uuid (real: the assistant
  message issuing the edit); `snapshotMessageId` linkage is exact.
  trackedFileBackups carry the latest pre-write backup per path — real
  re-backs-up every tracked file at each user message (also catches
  external edits); ours only re-backs-up on tool writes.
- Tests: `TestFileHistoryRows`, `TestRewindFilesLinked`,
  `TestCheckpointPendingTracking`, `TestCheckpointBackupNullForNewFile`.

## Adjacent fixes while here

- `last-prompt` now writes real's literal: `{type,sessionId,leafUuid,
  explicit:true,lastPrompt:<truncated-160>}` — bare (was LogRecord).
- `LogPRLink`/`LogAgentMeta`/`LogContinuedIn`/`LogEndedByModel`/
  `LogAITitle` switched from LogRecord to LogBare — real's captures show
  no uuid/parentUuid on these rows.
- Attachments now join the parentUuid chain (real stamps parentUuid on
  them and the next message's parentUuid is the attachment uuid).

## recs-cost
# Transcript record notes — cost-state / api-request* / history-suppression

Audit: `audits/wire-stream.md` (2.1.296). Sources: carved binary at
`claude-extracted-296` (mod_0416 emitters, mod_0300 schema, mod_0092
history-suppression builder, mod_0623 blob sanitization). Real example
rows verified against live transcripts under `~/.claude/projects/`.

All five types write verbatim via `Store.LogRawRecord` /
`AppendReplayRecords` — none of them carry `uuid`/`parentUuid`/
`timestamp` stamps in real, so they bypass `LogRecord`'s auto-stamping.

## cost-state — implemented

Real schema (mod_0300 validator; verified against a real transcript row):

```
{type:"cost-state", sessionId, totalCostUSD, totalAPIDuration,
 totalAPIDurationWithoutRetries, totalToolDuration, totalLinesAdded,
 totalLinesRemoved, totalDuration, startTime, modelUsage,
 hasUnknownModelCost}
modelUsage[model] = {inputTokens, outputTokens, thinkingTokens,
 cacheReadInputTokens, cacheCreationInputTokens, webSearchRequests,
 costUSD}
```

- Merge policy: last-wins. One row per save event; resume reads the
  latest row to restore the ledger.
- Real emit path: `s3t()` builds the snapshot from the live cost ledger;
  `Wvt()` → `Gxo()` writes it when the ledger is saved — conversation
  reset (`/clear`, mod_1144), session switch (`/resume`), fork-boundary
  saves, and graceful shutdown (React cleanup in mod_1596, gated on
  `!jn()` = not remote-workspace and `Ds()` = graceful flag).
- `/cost` (real and ours) reads the live ledger, not this row. The row
  exists for restore-on-resume and cost attribution.
- Ours emits via `agent.Loop.CostState()` at the same boundaries:
  - `/clear` `/reset` `/new` — before `ResetForNewSession`, which now
    also clears the ledger (real's `jxo` claims a fresh ledger).
  - `/resume` session switch — written to the outgoing session's file
    before the store rebinds.
  - Interactive exit — `defer` in `RunWithStore`, covers quit/exit paths.
  - `-p` — after the run's messages flush in `runPrint`.
- `totalAPIDuration` sums every attempt's round-trip (fallback legs
  included); `totalAPIDurationWithoutRetries` accumulates the settled
  attempt only — retries/fallback legs and their backoff are excluded.
- `totalToolDuration` accumulates per-call tool execution time.
- `modelUsage` keys on the response's `model` (fallback `l.Model`),
  including `thinkingTokens` (`usage.output_tokens_details.thinking_tokens`)
  and `webSearchRequests` (new `usage.server_tool_use.web_search_requests`
  field on `api.Usage`).
- `hasUnknownModelCost` flips when a model misses our price table (the
  default sonnet/fable rate). Real's flag comes from its managed-pricing
  lookup; ours is a substring heuristic — noted gap.
- `startTime` = first-turn start (epoch ms); fallback is loop start /
  emit time. Real's is the ledger-claim time (`R1e`, ~session start) —
  close, not identical, for sessions whose first turn starts late.

## api-request — implemented (flag-gated)

Real emit: `ust()` in mod_0416, called per request attempt inside the
request builder; gated on `as(querySource)` ∈ {main, subagent}, a live
replay recorder (`!Fa()` — persistence enabled — and not remotely
mirrored), and the `tengu_elegant_meadow` GB flag /
`CLAUDE_CODE_ELEGANT_MEADOW` env (env wins when set, any non-empty
value including "0").

Real schema (mod_0416 `ust`):

```
{type:"api-request", sessionId, agentId?, id, timestamp, version,
 querySource, shapeHash, params, withheld?, base?, keep, tail,
 cacheControl, requestDigest}
```

- `params`: the wire request minus `system`/`tools`/`messages`, minus
  `metadata`/`stream`/`thread`/`diagnostics` (real's `BCe`), minus
  `withheld` keys; each value zD-scrubbed (cache_control + undefined
  dropped recursively).
- `withheld`: `CLAUDE_CODE_EXTRA_BODY` keys (other than the big three)
  that merely restated an existing param value — compared via canonical
  JSON; withheld keys leave both `params` and the digest input.
- `keep`/`tail`/`base`: `keep` = length of the message-hash prefix shared
  with this chain's previous request; `tail` = hashes after `keep`;
  `base` = previous request id when `keep>0`. Chain key is
  `agentId ?? sessionId`; ours lives per-loop (equivalent — each loop is
  one agent/main session).
- `cacheControl`: `[{path, value}]` collected over system/tools/messages
  — `path` is the key/array-index path to the object containing
  `cache_control` (real's `Uq`).
- `requestDigest`: sha256 over sorted-key compact params with the
  messages array replaced by per-message canonical JSON
  (cache_control included) — real's `qCe`.
- The laptop-memory variant (`cut`/`cutRequestDigest`) is unreachable —
  no local account memory (real's `Neo()` returns empty for us), so the
  branch is unimplemented.

Ours: `Loop.recordAPIRequest(req)` called in `tryModels` per attempt,
after `buildRequest`. Emit sites:
- Main loop → session store via `OnRequestReplay` (bound in main.go;
  rebound on /resume).
- Subagent loop → its own `subagents/<agentID>.jsonl` store
  (real's route-by-agent: records carry `sessionId`=parent sid +
  `agentId`).
- `querySource` = `repl_main_thread` (interactive), `sdk` (-p),
  `agent:builtin:<type>` / `agent:custom:<type>` (subagent defs).
- Aux calls (advisor, answerer, title/summary/compaction calls) never
  set `ReplaySource` → skipped, matching real's `as()` gate.

## api-request-shape — implemented (flag-gated)

```
{type:"api-request-shape", sessionId, agentId?, shapeHash, timestamp,
 version, shape}
```

- `shape` = `{system?, tools?}` with billing-header system blocks
  filtered (`x-anthropic-billing-header:` prefix, real's `dhs`) and
  cache_control stripped (`zD`); `shapeHash` = sha256 of the canonical
  sorted-key form with cache_control omitted.
- Written once per shapeHash per transcript (real's `requestReplay`
  index; ours: `Store.replayIdx`, lazily seeded by scanning the file on
  first write so resumed sessions don't re-emit).
- Shape rows over 2,000,000 bytes (real's `XKr`) are refused →
  `"shape_too_large"`; the loop marks the shape unrecordable and skips
  later requests using it.

## api-request-blob — implemented (flag-gated)

```
{type:"api-request-blob", sessionId, agentId?, hash, message}
```

- One row per unseen blob candidate: the messages after `keep` in the
  request's chain position. `message` is the raw wire message minus
  cache_control keys (string content is NOT normalized — real's
  `zD(raw)` on the original object).
- `hash` = sha256 of the canonical normalized message; normalized means
  string content → `[{type:"text",text}]` (real's `t0n`), and the hash
  input omits cache_control when the message carries one (real's `GCe`)
  — so re-sent messages with different cache tags dedupe to one blob.
- Dedup is per transcript file (same `replayIdx` as shapes).

Order on disk: shape row (when new) → blob rows (new only) → the
api-request row, matching real's `Promise.all([...b, r])` append order.

## history-suppression — implemented (fork_inherit only)

Real schema (mod_0092 `c8e`; verified against a real transcript):

```
{type:"history-suppression", sessionId, cause,
 vetoedAgainstAccountUuid?, ts}
```

- `ts` is `new Date().toISOString()` (millisecond ISO — not the
  `timestamp` field name message rows use). No uuid/parentUuid.
- Merge policy: last-wins.
- Real causes: `fork_inherit` (fork from a suppressed transcript —
  written at the top of the new file, before copied records),
  `meta_seed` (session metadata says suppressed but the file lacks the
  row), `migration` (metadata migration re-stamps the row — repeated
  rows observed in real transcripts), `restored_owner_mismatch` and
  `chokepoint_veto` (cross-account/bridge veto of history reads).
- Ours emits `fork_inherit` in `session.Fork`/`ForkInto` when the source
  transcript contains the row — the only cause whose equivalent exists
  here. `meta_seed`/`migration`/`restored_owner_mismatch`/`chokepoint_veto`
  are skipped: we have no session-metadata suppression flag, no
  account-uuid trust veto, and no bridge chokepoint machinery. Emitting
  them speculatively would fabricate state real derives from elsewhere.

## Schema / emit surprises worth knowing

- These rows are deliberately *unstamped*: no uuid, no parentUuid, and
  `history-suppression` uses `ts` where message rows use `timestamp`.
  `cost-state` and `api-request-blob` carry no time field at all (the
  blob row's real literal has none).
- `api-request`'s `tail`/`cacheControl` are always present (possibly
  `[]`); `withheld`, `base`, `agentId`, `vetoedAgainstAccountUuid` are
  conditional.
- Real writes the api-request row even when zero blobs are new (the row
  itself is always appended; only shape/blob rows dedupe).
- `totalAPIDurationWithoutRetries` semantics: our best reconstruction —
  real's exact accounting (sum of settled-attempt durations vs
  total-minus-backoff) isn't directly visible in the carve; the row
  field exists and ours stays ≤ totalAPIDuration, matching real's
  observed relation.
- Digest/canonical hashing is process-internal (no cross-impl byte
  comparison), but the canonical form follows real's sorted-key JSON
  shape; U+2028/U+2029 handling matches `JSON.stringify` (raw, not
  escaped).
- The `requestReplay` index check `!fileExisted && index empty` →
  "no_transcript" is approximated: our store file always exists once
  open; a `fork-context-ref` row would still suppress replay rows (we
  never write that record).
