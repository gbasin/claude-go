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
