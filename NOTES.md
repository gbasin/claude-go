# Transcript record types — research + disposition (vs real CC 2.1.296)

Schemas verified against the extracted source (`/Users/garybasin/Code/claude-extracted-296`,
mostly `mod_0416.js`, `mod_0567.js`, `mod_1681.js`) AND against real transcripts on
disk in `~/.claude/projects/*/*.jsonl`.

## Schema surprises (apply broadly)

- **Metadata rows are BARE.** `{type:"mode"}`, `{type:"permission-mode"}`,
  `{type:"ai-title"}`, `{type:"tag"}`, `{type:"relocated"}`,
  `{type:"custom-title"}`, `{type:"agent-name"}`, … carry only the fields the
  emit site builds — no `uuid`, no `parentUuid`, no implicit `timestamp`.
  Ours used to decorate every record via `LogRecord`; fixed for the rows
  below via `Store.logBare`.
- `ended-by-model` carries an explicit `timestamp` (`new Date().toISOString()`,
  millisecond Z format) but no `uuid`.
- `history-suppression` uses `ts`, not `timestamp`.
- `api-request*` rows live behind the `tengu_elegant_meadow` /
  `CLAUDE_CODE_ELEGANT_MEADOW` flag — request-replay capture for debugging.
- These rows are written through a central append (`appendEntry`/`DC`) and a
  "session metadata re-append" (`reAppendSessionMetadata`) that re-stamps
  latched metadata at file materialize, resume adopt, post-compact, and exit
  (sJr), deduped by a tail scan.

## Implemented (wired to a real event)

| type | real schema | trigger in real | ours |
|------|-------------|-----------------|------|
| `mode` | `{type,mode:"normal"|"coordinator",sessionId}` | `saveMode` caches; row lands via metadata re-append (materialize/resume/compact/exit). NOT stamped on a perm-mode cycle. | `Store.LogMode` + `Store.StampMeta`; stamped at session materialize & exit. REPL is always "normal" (no coordinator mode). |
| `permission-mode` | `{type,permissionMode,sessionId}` | `savePermissionMode` caches; row lands via the same re-append. | `LogPermissionMode` via `StampMeta` at materialize & exit. **Removed** the bogus `{type:"system",subtype:"permission-mode"}` row real never emits, and the `{type:"mode"}` row ours wrongly stamped on cycleMode. |
| `relocated` | `{type,sessionId,relocatedCwd}` | `relocateSessionTranscript` (cnt): /cd moves `<oldslug>/<sid>.jsonl` + `<sid>/` sidecar dir into the new cwd's project dir, stamps the row; same-dir case stamps when relocatedCwd changed. | `Store.Relocate` wired from `/cd` in `internal/repl/commands.go`. Moves file + sidecar dir, sets an existing dest aside as `<file>.superseded-<ms>`, stamps the bare row. |
| `ended-by-model` | `{type,timestamp,sessionId}` | EndConversation tool handler stamps it before aborting the turn (`mod_1681` → TJr/markSessionEndedByModel). | `LogEndedByModel` (now bare + timestamp) wired into `endSession` in `internal/repl/repl.go`. Picker already filters sessions carrying it. |
| `ai-title` | `{type,aiTitle,sessionId}` | `saveAiGeneratedTitle` (Ple) after the first-turn title call. | Already wired (`main.go` → `LogAITitle` after `TitleCall`); switched to bare shape. |
| `tag` | `{type,tag,sessionId}` | `saveTag` via re-append; value set by resuming a tagged (e.g. remote) session. No live CLI producer found in extracted source. | `LogAgentMeta` keeps emitting `{type:"tag",tag,sessionId}` (bare now) when a caller passes a tag; no CLI event sets one. |

`StampMeta` dedupes repeated stamps of an unchanged value, mirroring real's
tail-scan dedup. Bare rows verified byte-for-byte vs real transcripts:
`{"type":"mode","mode":"normal","sessionId":…}`,
`{"type":"permission-mode","permissionMode":"acceptEdits","sessionId":…}`,
`{"type":"relocated","sessionId":…,"relocatedCwd":…}` (field order varies
between real's emit paths; map marshaling is fine).

## Skipped — no backing event / feature absent

| type | real schema | why skipped |
|------|-------------|-------------|
| `bridge-session` | `{type,sessionId,bridgeSessionId,lastSequenceNum,declaredDialogKinds?,sessionGroupingId?,noHistoryBackfill?,ownerAccountUuid?,ownerOrganizationUuid?}` | Written when remote control attaches to a REPL session (`dnt` at `mod_1596` ~1464994, cleanup paths `mod_0416`, `mod_1009`). Ours' `internal/bridge` is the `claude remote-control` **server** skeleton; session spawning is delegated and it never attaches remote control to a local REPL transcript — no bridgeSessionId on a session store to stamp. |
| `attribution-snapshot` | opaque record, `messageId`-keyed; last-wins | The emit helper `recordAttributionSnapshot` (`Q5t`, `mod_1272.js`) exists but has **no callers** in extracted 296 — dead code (probably gated to a now-extracted or flag-dead feature). Nothing to trigger it in ours. |
| `artifact-autoreact-ledger` | `{type,v:1,sessionId,accountUuid?,artifacts:{<id>:{savedAt,stampHighWater,everBaselined,everHadThreads,turnTimestamps,threads}}}` | Artifact auto-react ledger (mod_0300/mod_0572) — ours has artifact tools but no autoreact worker, no armed monitor state. |
| `artifact-comment-monitor` | `{type,v:1,sessionId,artifacts:{<id>:{state:"armed"|"stopped",writtenAtMs,title?,holder?,holderPid?,holderProcStart?/holderProcStartFt?,holderJob?,holderPidSpace?}}}` | Written by the comment-monitor intent tracker behind `--watch-artifact` (mod_0567) — ours parses the flag but doesn't implement artifact watching. |
| `memory-mode` | `{type,mode,afterUuid,timestamp,reason?,account?,accountChoice?,sessionId}` | Account/auto-memory "mode stamps" written per stamp and carried across `/clear` (mod_0416 `saveMemoryMode`/$Jr, `mod_1144`). Ours has no account-memory subsystem; `/pause-memory` just toggles `MemoryPaused` — not the same mechanism. |
| `history-suppression` | `{type,sessionId,cause,vetoedAgainstAccountUuid?,ts}` | Written on remote-control history-upload vetoes ("restored_owner_mismatch","chokepoint_veto"), migration, and carried forward on fork/resume of a suppressed session (`mod_0092 $le` + `mod_0416` re-append). Ours never suppresses history (no remote-control history upload / cross-account veto), so no cause exists. |
| `content-replacement` | `{type,sessionId,agentId?,replacements}` | Flag-gated (`tengu_hawthorn_steeple` / `CLAUDE_CODE_HAWTHORN_STEPLE`) tool-result elision; written when replaying pruned content (mod_0296 `pOe`, `mod_1044`). Ours has no tool-result-replacement state. |
| `api-request-shape` | `{type,sessionId,agentId?,shapeHash,timestamp,version,shape}` | Request-replay capture, gated by `yK()` = `CLAUDE_CODE_ELEGANT_MEADOW`/`tengu_elegant_meadow`, deduped by shapeHash (`mod_0416` `ust`). Flag off by default; ours has no request-replay recorder. Checked for Agent-B overlap: `grep -r 'api-request' --include='*.go'` in this worktree → nothing; not implemented elsewhere. |
| `api-request-blob` | `{type,sessionId,agentId?,hash,message}` | Same capture path (`api-request` row + deduped blob rows). Same skip reason. |

## Files

- `internal/session/store.go` — `logBare` verbatim writer; `LogMode`,
  `LogPermissionMode`, `StampMeta`, `LogRelocated`, `Relocate`; `LogAITitle`,
  `LogEndedByModel`, `LogAgentMeta` switched to bare shapes.
- `internal/repl/repl.go` — `endSession` stamps ended-by-model; cycleMode no
  longer stamps a `{type:"mode"}` row (real never does on cycle);
  `SetPermMode` no longer emits `{type:"system",subtype:"permission-mode"}`;
  `fireSessionEndHook` re-stamps meta at exit.
- `internal/repl/commands.go` — `/cd` calls `Store.Relocate`.
- `main.go` — `StampMeta("normal", permMode)` after materialize and deferred
  before `st2.Close()` (exit re-stamp with final mode).
- `internal/session/store_test.go` — tests for bare shapes, stamp dedup, and
  relocate.
