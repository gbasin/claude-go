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
