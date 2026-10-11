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
