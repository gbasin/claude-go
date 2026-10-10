# Audit: session lifecycle, bg/daemon, MCP, plugins, messenger — claude-go vs real CC v2.1.294

Scope: `internal/session/`, `internal/bg/`, `internal/mcp/`, `internal/plugins/`,
`subcommands.go`, `internal/repl/picker.go`, messenger in `internal/tools/services.go`,
resume plumbing in `main.go`/`flags.go`. Real JS evidence from `refsrc-294/mod_*.js`.

Status legend: MATCH · PARTIAL (works, reduced fidelity) · GAP (missing) · DIVERGENT (present but incompatible).

## 1. Session lifecycle

| item | real + evidence | ours | status | e2e | severity |
|---|---|---|---|---|---|
| Transcript path | `~/.claude/projects/<slug>/<uuid>.jsonl`, slug = realpath'd cwd with `/`→`-` | identical incl. `EvalSymlinks` (store.go:62) | MATCH | TestTranscriptWritten | — |
| Branch/tree semantics | active branch = parentUuid walk from last non-sidechain user/assistant leaf; sidechain+meta excluded | `activeBranch`/`LeafUUID` identical, incl. cycle guard (store.go:479) | MATCH | TestResume | — |
| `--continue` | most recent conversation in cwd | newest-mtime `.jsonl` that `hasConversation` (store.go:636) | MATCH | TestContinue | — |
| `-r <id>` same-transcript resume | appends to same file, parentUuid chained to leaf | `sessionID=resumeID`, `SeedParent(leaf)` (main.go:434,924) | MATCH | TestResume | — |
| `-r` bare → picker | interactive `Resume session` UI | `PickSession`, non-TTY falls back to plain list | MATCH | TestResumePickerInteractive | — |
| `-r <file>.jsonl` | loads arbitrary transcript path | `LoadPath` + `.jsonl` suffix detection (main.go:430) | MATCH | covered via picker all-projects path | — |
| `-r` resolves custom/ai titles | real matches `customTitle`/`aiTitle` (exact/case-insensitive substring) when resolving a session arg | ID/path only; a title arg fails silently | GAP | none | medium |
| Picker session filter | entries need `customTitle‖aiTitle‖messages‖firstPrompt` (empty transcripts hidden) | lists every `*.jsonl` incl. init-only files | PARTIAL | none | low |
| Picker keys | `ctrl+a` all-projects/current-repo toggle, `space` preview, `ctrl+r` rename, type-to-search | `ctrl+b` bound to all-projects but footer advertises **Ctrl+A** (unbound) and a **Ctrl+B "only show current branch"** filter that doesn't exist; search requires `/`/`ctrl+f` first (no type-to-search) | PARTIAL (bug) | TestResumePickerInteractive | medium |
| Picker rename | `recordRename` → `{type:"custom-title"}` record + sidecar | `WriteTitle` writes record + `custom-title.json` — same files | MATCH | none | — |
| Picker live-bg mark | running bg sessions marked, resume refused | `running` map + "still running — attach" notice (picker.go:71,120) | MATCH | TestResumeLiveBgRefused | — |
| `--fork-session` (CLI flag) | copies the session **file** to `<newid>.jsonl` with per-record `forkedFrom` stamps (`Vpt(k,Me)` in mod_1346 `hvs`); new id keeps working | keeps new id, loads msgs in-memory, writes **only** `{system,fork}` record — history never lands in the new transcript; a later `-r` of the fork sees nothing | GAP | TestForkSession/TestFlagForkSession (asserts only the fork marker, not copied history) | **high** |
| `/fork` (in-REPL) | same copy semantics + `(Branch N)` title dedup via regex `\(Branch(?: (\d+))?\)$` | `session.Fork` copies records w/ forkedFrom ✓ but always titles `"(Branch)"` — no `N` dedup | PARTIAL | none | low |
| `--resume-session-at` | mid-chain resume at message uuid | `loadSessionAt[Path]` incl. uuid-prefix match (flags.go:578) | MATCH | TestResumeDropsTurn | — |
| `--resume-drops-turn` | `vg` guard: tail must be exactly the declared turn's records | `dropGuard` equivalent incl. promptless/compact-summary refusal | MATCH | TestResumeDropsTurn | — |
| `--rewind-files` | file-history-snapshot restore at user-msg uuid; refuse non-user uuid | `RewindFiles` identical error strings (rewind.go) | MATCH | TestRewindFiles | — |
| `summary` records | `{type:"summary",summary,leafUuid}` leaf markers | `LogSummary` same shape | MATCH | — | — |
| `custom-title` record + `<sid>/custom-title.json` sidecar | `$vn` joins dirname+sid+`custom-title.json` | identical (store.go:296-358) | MATCH | — | — |
| `ai-title` records | real auto-generates + persists `{type:"ai-title",aiTitle,sessionId}` | never written (no auto-titling) | GAP | none | medium |
| `ended-by-model` records | `{type:"ended-by-model",sessionId}` on model-ended sessions; picker filters them | never written | GAP | none | low |
| `pr-link` records | `{type:"pr-link",prNumber,prUrl,prRepository}` written on PR association | `sessionForPR` reads loosely but nothing writes `pr-link` | PARTIAL | none | low |
| `continued-in` records | `{type:"continued-in",continuedInSessionId}` on bg continuation | never written | GAP | none | low |
| `tag`/`agent-name`/`agent-color`/`agent-setting` records | written for teammate/agent sessions ("always" merge policy) | never written | GAP | none | low |
| Todos persistence | app-state only (`todos` in state store); `~/.claude/todos/` is legacy | in-memory `todosList` — same semantics | MATCH | — | — |
| `cleanupPeriodDays` retention | real prunes `.jsonl` transcripts older than N days (default 30, `pvt`), paused on settings errors | no retention cleanup at all | GAP | none | low |
| `history.jsonl` prompt history | `~/.claude/history.jsonl` | same file written/read (term.go:1455,1915) | MATCH | TestHistorySearch | — |
| Subagent transcripts | `<sid>/subagents/<agentId>.jsonl` + `.meta.json`, isSidechain stamps | `OpenSubagent` identical layout/mode 0600 (store.go:692) | MATCH | TestAgentToolSubagent | — |
| Init meta / permission-mode restore | `system/init` record; `--inherit-permission-mode` reads `permissionMode` back | `LogInitMeta`/`InitMode` same fields | MATCH | — | — |
| Away summary on resume | recap when last msg ≥5min (`awaySummaryEnabled`) | same env/setting + recap line (main.go:989) | MATCH | — | — |
| `--from-pr` resolution | finds session linked to PR | `sessionForPR` works but uses a **different slug rule** (`/`,`.`→`-` vs session `Slug()` `/`→`-`+realpath) — misses sessions whose cwd has dots | PARTIAL (bug) | none | low |
| bg-session fork cleanup | fork drops `sessionKind` marker | `delete(cp,"sessionKind")` (store.go:566) | MATCH | — | — |

## 2. Background sessions + daemon

| item | real + evidence | ours | status | e2e | severity |
|---|---|---|---|---|---|
| `--bg` lifecycle | spawns `bg-pty-host` worker, prints id + attach hint | `bg.Spawn` → `bg-host` subcommand, prints same info (main.go:346) | MATCH | TestBgSessionLifecycle | — |
| Job registry layout | `~/.claude/jobs/<short>/` w/ job record (pid, procStart, sessionId, rendezvousSock, ptySock, messagingSock, rvAuth, ptyAuth, replPid, workers, updatedAt), `.fleetview-heartbeat` (5s), `jobs/.order` lock, `jobs/settled/*.json` for finished jobs | `~/.claude/bg/jobs/<sid8>/job.json` (subset fields) + `$TMPDIR/claude-go-rv/<sid>.sock`; heartbeat written into job.json every 5s; no order lock; job dir deleted on exit | DIVERGENT (paths/protocol don't interop) | TestBgSessionLifecycle | medium |
| Attach protocol | **two** UDS: rendezvous (control/replies/claim, `rvAuth`, `claimAuth`) + pty socket (`ptyAuth`); framed JSON; claim handshake (`buildClaimFrame`) | **one** socket; custom `[1B op][4B len]` frames (hello/ready/data/resize/detach/exit/error); single auth token | DIVERGENT | TestBackgroundSlashDetach | medium |
| pty auth token | `CLAUDE_BG_PTY_AUTH` + `CLAUDE_BG_RV_AUTH` + `CLAUDE_BG_CLAIM_AUTH` + `CLAUDE_BG_SOCKET_TOKENS_PATH` | `CLAUDE_BG_PTY_AUTH` only, minted per spawn, refused on mismatch | PARTIAL | none | medium |
| Daemon supervisor | `CLAUDE_BG_BACKEND=daemon`; daemon dir under `~/.claude/daemon`; sweeps respawn dead workers; **no user-facing `claude daemon` command** | `daemon run/start/stop/status/install/uninstall` subcommands (invented surface — real has none); sweeps every 5s, daemon.json heartbeat 30s stale | PARTIAL (semantics match; surface diverges) | TestDaemonRespawn | low |
| Wedged-worker handling | TERM → escalate → respawn | TERM once, KILL after >2 sweeps, next sweep respawns (daemon.go:91) | MATCH | TestDaemonRespawn | — |
| Version-skew respawn | `respawnIfIdleStale` — recycles stale-version workers when idle/unattached | same: `Version != version && Attached==0 && idle>60s` → TERM+respawn (daemon.go:98) | MATCH | TestDaemonRespawnsVersionStale | — |
| Dead-worker respawn | new worker resumes same session transcript | `Spawn{ResumeFrom:j.SessionID}` + stamps new pid into job.json | MATCH | TestDaemonRespawn | — |
| `claude agents` | full fleetview TUI: peers via messaging bus, fold groups, PR-status batch, peek-reply, dead-epoch offers, rename (ctrl+r), find (ctrl+f), pin (ctrl+t), settled/past sessions | plain text: agent types + live bg jobs with `attached`/`↑v` marks; "local bus agents: none" hardcoded | PARTIAL | TestAgentsSubcommand, TestAgentsPaneListsBgTask | medium |
| `claude stop` | `stop` with `kill` alias; TERM worker pgrp | `stop` only (no `kill` alias) | PARTIAL | TestBgStop | low |
| `claude logs <id>` | prints session's recent terminal output (tail replay exists internally) | **absent** | GAP | none | medium |
| `claude kill` | alias of stop | **absent** | GAP | none | low |
| `claude respawn <id|--all>` | on-demand worker respawn | **absent** | GAP | none | medium |
| `claude rm <id>` | delete session/job records | **absent** | GAP | none | medium |
| `--bg-spare` spare worker | real pre-warms a spare pty host for fast claim | absent | GAP | none | low |
| `--attach-serve` | worker entrypoint serving attach | flag stubs to `RunHost` | PARTIAL | none | low |
| Ctrl+Z detach in attach | "Ctrl+Z to detach" | `0x1a` → opDetach + reattach hint | MATCH | TestBackgroundSlashDetach | — |
| Ctrl+Z suspend inside bg pty | inner session gets real job control | `childAttr` own session + controlling pty | MATCH | TestSuspendResume | — |
| stdin-EOF attach watch | attach outlives stdin EOF until opExit (scriptable wait) | implemented (attach.go:130) | MATCH | none | — |
| Live-resume guard | "this session is running in another terminal" | `bg.Lookup`+`Alive()` refusal (main.go:442) | MATCH | TestResumeLiveBgRefused | — |
| Tail replay on attach | new attacher gets pty scrollback snapshot | 64KB rolling tail replayed post-opReady | MATCH | none | — |
| Derived session names | `bg-<name>-<hex8>` / `Aa(cwd,sid)` naming for registry/fleet | registry `derivedSessionName` ✓; bg jobs keep raw uuid short | PARTIAL | none | low |

## 3. MCP

| item | real + evidence | ours | status | e2e | severity |
|---|---|---|---|---|---|
| stdio transport | child process, newline JSON-RPC | `connectStdio` identical | MATCH | — | — |
| Streamable HTTP | POST; `Mcp-Session-Id` capture/echo; `MCP-Protocol-Version` header; `Accept: application/json, text/event-stream`; JSON or SSE response bodies | `httpTransport` identical (transport_http.go) | MATCH | TestMCPHTTPToolCall | — |
| GET notification stream | optional GET SSE; **reconnects with max-attempt backoff** ("SSE GET-stream reconnection exhausted") | opened once, **no reconnect** | PARTIAL | none | medium |
| Legacy SSE transport | GET stream → `endpoint` event → POST channel | `sseTransport` same shape | MATCH | TestMCPSSEToolCall | — |
| `ws` transport | config type `ws` supported (add-json "stdio, SSE, HTTP, or WebSocket"; schema enum `["stdio","sse","http","ws","sdk","claudeai-proxy"]`) | `Connect` has no `ws` case — `--transport ws` writes config then fails "unsupported" | GAP | none | medium |
| `sdk`/`sse-ide`/`ws-ide`/`claudeai-proxy` types | sdk (in-proc), ide transports, claudeai-proxy connectors | claudeai-proxy + ws-ide (via `ConnectIDE`) present; no sdk | PARTIAL | — | low |
| OAuth: discovery | RFC 9728 protected-resource → 8414/OIDC; `resource_metadata` hint in WWW-Authenticate | `DiscoverOAuthMetadata`/`DiscoverAuthURL` same order | MATCH | TestMCPOAuthNeedsAuth | — |
| OAuth: flow | PKCE + DCR + browser + localhost callback | `RunOAuthFlow` same (oauth.go:367) | MATCH | — | — |
| OAuth: token persistence | `mcpOAuth` records inside the credential store (keychain/plaintext storageV5), keyed `Hl(name,url)`, holding `serverName/serverUrl/discoveryState/stepUpScope` | `~/.claude/.credentials-mcp.json` (separate file real never reads), keyed name only, no discoveryState/stepUp | DIVERGENT | none | medium |
| OAuth: client registration | persisted at `.claude.json mcpOAuthClientConfig[name]` | same file/key (saveClientConfig) | MATCH | — | — |
| OAuth: extras | `--client-id`, `--client-secret` (prompt/env `MCP_CLIENT_SECRET`), `--callback-port`, step-up retry on `insufficient_scope`, revoke on logout (`mcp_oauth_revoke`), hidden `--xaa` | none of these | GAP | none | medium |
| `mcp login`/`logout <name>` | real HAS these subcommands | both present (`RunOAuthFlow`/`DeleteServerAuth`; logout doesn't revoke) | PARTIAL | none | low |
| initialize capabilities | `{roots:{listChanged:true}, elicitation:{}, tasks:{requests:{elicitation:{create:{}}}}}` | `capabilities: {}` — servers never learn we answer roots/elicitation | GAP | TestMCPRootsList (server-driven anyway) | medium |
| resources surface | `resources/list|read|templates/list`, paginated | identical incl. cursor + `resources/directory/read` ext | MATCH | TestMCPResources | — |
| prompts surface | `prompts/list|get`, args, list_changed → rebuild slash cmds | identical + `OnPromptsChanged` | MATCH | TestMCPPromptSlash | — |
| elicitation | form mode + **url mode** (`elicitationId`, completion notifications) + `ElicitationResult` hooks | form mode only, decline default, no url mode/hooks | PARTIAL | TestMCPElicitation | medium |
| roots | `roots/list` answered w/ workspace roots + `roots/listChanged` advertise | answers when `Roots` set but not advertised (see above) | PARTIAL | TestMCPRootsList | low |
| notifications | `tools/prompts/resources list_changed`, `notifications/message`, `progress`, cancelled | same set handled (client.go:447) | MATCH | TestMCPListChanged, TestMCPNotificationMessage | — |
| Request timeouts | `MCP_TIMEOUT` (requests), `MCP_CONNECT_TIMEOUT_MS`, `MCP_TOOL_TIMEOUT`, **per-server `timeout` field** (≥1000ms, clamped to max ~30min); tool default ~5min | `MCP_TIMEOUT`=30s + `MCP_CONNECT_TIMEOUT_MS`=5s; `CallTool` hardcoded 120s; no `MCP_TOOL_TIMEOUT`; ignores per-server `timeout` | PARTIAL | none | medium |
| Reconnect | `/mcp reconnect <name|all>`; disabled-server skip; "reconnected during call" guard | `/mcp reconnect` is a **no-op** print | GAP | none | medium |
| Per-server enable/disable | server `enabled`/`disabled` state gates auto-connect ("enable before reconnect") | no enable flag in `ServerConfig` | GAP | none | low |
| `mcp add` surface | `-s/--scope` (local,user,project; default local), `-t/--transport` (stdio,sse,http + streamable-http alias, validated), `-e/--env`, `-H/--header`, `--client-id`, `--client-secret`, `--callback-port`, `--xaa`; transport/scope **validated** | long flags only (`--transport`,`--scope`,`-e/--env`,`--header`); **no validation** (any transport/scope string accepted); missing client-id/secret/callback-port/xaa | PARTIAL | TestMCPAddList | medium |
| `mcp add-json` | `-s/--scope`; JSON validated incl. `ws` type | `--scope` prefix only; no ws/type validation | PARTIAL | none | low |
| Scope semantics | `local` → `.claude.json projects["<cwd>"].mcpServers`; `user` → top-level `mcpServers`; `project` → `./.mcp.json` | local **and** user both written to top-level `mcpServers`; `LoadConfigs` never reads `projects[<cwd>]` — real's local-scope servers invisible; ours' "local" servers leak to every project in real's view | GAP | none | **high** |
| `mcp remove` | `-s/--scope`; per-scope "No MCP server named X in Y scope" errors | removes top-level only; can't remove project `.mcp.json` entries | PARTIAL | none | medium |
| `mcp serve` | "Start the Claude Code MCP server" (+`-d`,`--verbose`) | **absent** | GAP | none | medium |
| `mcp add-from-claude-desktop` | import Claude Desktop config (Mac/WSL) | **absent** | GAP | none | low |
| `mcp reset-project-choices` | clears approvals **then re-renders the disclosure/approval scan** | clears `enabled/disabledMcpjsonServers` only | PARTIAL | none | low |
| `mcp list/get` | connected-status view; "⏸ Pending approval" for unapproved project servers | prints config tuples; pending marker on list+get | PARTIAL | TestMCPListEmpty | low |
| `.mcp.json` trust flow | interactive approve dialog → writes `enabledMcpjsonServers`/`enableAllProjectMcpServers` to local settings | unapproved servers silently skipped forever (env `CLAUDE_GO_APPROVE_MCP` bypass only); pending shown but **no approval prompt anywhere** | GAP | none | medium |
| Plugin `.mcp.json` | enabled plugins contribute servers; `--plugin-dir-no-mcp` suppresses | same via `EnabledPluginEntries` + `NoMCP` | MATCH | TestPluginDirHooks | — |
| `--channels` allowlist | selects plugin-provided servers; `dev_*` exempt under `--dangerously-load-development-channels` | env-var equivalent incl. `dev_` exemption (client.go:142) | MATCH | none | — |
| `${VAR}`/`${user_config.X}` expansion | expands env + `user_config.` refs in url/headers/env | `os.ExpandEnv` only — `user_config` refs unsupported | PARTIAL | none | low |
| claude.ai connectors | `/v1/mcp_servers` discovery, proxy transport, eligibility gates, retries | `connectors.go` mirrors SLn incl. retry/budget | MATCH | connectors_test.go | — |
| IDE bridge | `~/.claude/ide/*.lock` → ws MCP | `ConnectIDE` same probe/match logic | MATCH | — | — |
| Server→client requests | ping, roots/list, elicitation/create, logging/setLevel answered; rest −32601 | same set incl. logging/setLevel | MATCH | — | — |
| Status registry | per-server connected/failed/needs-auth + authURL | `ServerStatus`/`recordStatus` same | MATCH | — | — |

## 4. Plugins

| item | real + evidence | ours | status | e2e | severity |
|---|---|---|---|---|---|
| Marketplace store | `~/.claude/plugins/marketplaces/<name>` checkout + `known_marketplaces.json` `{name:{source:{source,…,installLocation},autoUpdate}}` | same dir; registry `{name:{source:{source,path|url},autoUpdate}}` — no `installLocation`, thinner source typing | PARTIAL | TestMarketplaceListEmpty | low |
| Marketplace sources | github, git(+`ref`), url, local dir, **npm**, sparse checkout (`--sparse`) | github/URL/local only; no npm, ref, or `--sparse` | PARTIAL | none | medium |
| Install location | `~/.claude/plugins/cache/…` (`join(we(),"plugins","cache")`) | `~/.claude/plugins/synced/<mkt>/<plugin>` | DIVERGENT (path only; registry carries absolute installPath so it self-heals) | TestPluginList | low |
| `installed_plugins.json` | V2 `{version:2,plugins:{"<id>":[{scope,installPath,version,installedAt,lastUpdated,gitCommitSha}]}}` + `installed_plugins_v2.json` side-file + V1→V2 migration | `{plugins:{"name@mkt":[{version,scope,installPath,installedAt,enabled}]}}` — no `version:2`, no `lastUpdated`/`gitCommitSha`, extra `enabled` field (real tracks enable in settings instead) | PARTIAL | none | medium |
| `enabledPlugins` semantics | OR of `enabledPlugins` across **local + project + user** settings scopes (+ policy deny) | user settings `enabledPlugins`/`disabledPlugins` + installed-file `enabled` flag | PARTIAL | none | medium |
| `plugin enable/disable` | `claude plugin enable <p>` / `disable [p]` subcommands | **absent** — toggling needs manual settings edit | GAP | none | medium |
| Other plugin subcommands | `update <plugin>`, `configure <plugin>`, `validate <path>`, `install` alias `i`, `marketplace remove --scope`, `marketplace list --json/--available` | `install/i`, `remove`, `list`, `details`, `marketplace add/list/update` only | PARTIAL | TestPluginList | medium |
| `.claude-plugin/plugin.json` manifest | read for name/version + optional declared paths (skillsPath/skillsPaths etc.) | existence-check only for `--plugin-dir`; conventional `skills/`,`agents/`,`hooks/hooks.json`,`.mcp.json` dirs used | PARTIAL | TestPluginDirHooks | low |
| Contributions | skills, agents, hooks, commands, MCP servers, LSP | skills+agents+hooks+commands+.mcp.json loaded; **no plugin LSP servers** | PARTIAL | TestPluginDirHooks | low |
| `--plugin-dir <dir>` | value **is** a plugin root (has `.claude-plugin/plugin.json`); inline plugins for the session | `loadInstalled` treats value as a **parent** scanning subdirs for `*/.claude-plugin/plugin.json`; hooks loader treats value as the plugin root — inconsistent between loaders; also `filepath.SplitList` (plugins) vs `strings.Fields` (hooks) separators | PARTIAL (bug) | TestPluginDirHooks | medium |
| `--plugin-dir-no-mcp` | same roots minus MCP registration | `NoMCP` flag honored (same granularity issue) | PARTIAL | — | low |
| `--plugin-url <url>` | session-only ad-hoc plugin/marketplace | registers marketplace via `AddMarketplace` (persists — real is session-scoped) | PARTIAL | none | low |
| `--channels` | plugin channel allowlist + dev-channels flag | env-backed equivalent | MATCH | — | — |
| Plugin sync/update | `CLAUDE_CODE_SYNC_PLUGIN_INSTALL`, auto-update at startup, git SHA pinning | `marketplace update` manual `git pull --ff-only` only | PARTIAL | none | low |

## 5. Messenger / cross-session

| item | real + evidence | ours | status | e2e | severity |
|---|---|---|---|---|---|
| Session registry | `~/.claude/sessions/<pid>.json` (mode 0700), fields: sessionId,name,pid,kind,entrypoint,startedAt,peerProtocol,peerFeatures,procStart,messagingSocketPath,version,… | same dir/file/fields incl. `peerProtocol`,`peerFeatures`,`procStart` (services.go:586-800) | MATCH | messenger_test.go | — |
| Socket dir | `<XDG_RUNTIME_DIR‖CLAUDE_CODE_TMPDIR‖/tmp>/cc-socks/<pid>.sock`, per-uid fallback, dir vetting (ownership/mode/sticky) | identical incl. vetSocketDir + 103-byte sun_path guard | MATCH | messenger_test.go | — |
| Wire protocol | line-JSON frames; `{"type":"auth","token"}` first line then user/control frames; auth = windows-only (`OAn()`); `.key` files at `sessions/<pid>.<sha256(sock)>.key` | same constants: 1MB line cap, 30s first-line deadline, 5s write timeout, authRequired = windows-only | MATCH | messenger_test.go | — |
| `<cross-session-message>` envelope | `from="uds:…" from-session="<uuid>" hop-chain="<24-hex>" from-name="<name>" from-mode="prompting\|bypass"`, tag-breaking escaped (`CWe()`) | byte-identical envelope + escape + 24-hex hop (services.go:2233) | MATCH | none | — |
| Offline delivery | mailbox fallback when peer offline | `mailboxAppend` on failed send + `DrainMailbox` between turns | MATCH | messenger_test.go | — |
| Mailbox location | per-session mailbox files | `~/.claude/mailbox/<sid>.jsonl` | MATCH | messenger_test.go | — |
| Peer features | `["notify_idle","reply_across_default_dirs","artifact_yield"]` | same incl. peercred-gated middle entry | MATCH | — | — |
| `peer_message_status`/held-message flow | delivery status + held-message release | implemented (peerStatusWait 750ms, held-message strings at services.go:2189,3108) | MATCH | — | — |
| e2e coverage | — | unit tests only (messenger_test.go, coordinator_test.go); no e2e driving two live sessions | PARTIAL | none | low |

## Summary counts

| status | count |
|---|---|
| MATCH | 49 |
| PARTIAL | 34 |
| GAP | 20 |
| DIVERGENT | 4 |
| (107 rows) | |

## Top gaps (ranked)

1. **`--fork-session` never persists the copied transcript** — new session file gets only a `system/fork` record; re-resuming the fork loses all history (real copies the file w/ `forkedFrom`). HIGH.
2. **MCP `local` scope written to top-level `mcpServers`** instead of `.claude.json projects["<cwd>"].mcpServers` — real's local servers invisible to us; ours' local entries leak globally. HIGH.
3. **Missing bg subcommands** `logs`/`kill`/`respawn`/`rm` + `mcp serve`/`add-from-claude-desktop`/`reset-project-choices` re-scan + `plugin enable/disable/update/configure/validate`. MEDIUM.
4. **No interactive `.mcp.json` trust flow** — project servers stay "⏸ Pending approval" forever. MEDIUM.
5. **Picker keybinding bugs** — footer advertises Ctrl+A (unbound; Ctrl+B actually toggles all-projects) and a "current branch" filter that doesn't exist; no type-to-search. MEDIUM.
6. **MCP OAuth divergence** — separate `.credentials-mcp.json` file real ignores; no step-up auth, no `--client-id/--client-secret/--callback-port`, no revoke-on-logout. MEDIUM.
7. **`initialize` advertises `capabilities:{}`** — roots/elicitation never advertised; no URL-mode elicitation or ElicitationResult hooks. MEDIUM.
8. **Bg layout/protocol non-interop** — `~/.claude/bg/jobs` vs real `~/.claude/jobs`; single-socket custom framing vs real's rendezvous+pty dual-socket with rv/pty/claim auth; no settled-job records, no `--bg-spare`. MEDIUM (only matters for mixed real/ours fleets).
9. **Missing transcript record types** — `ai-title` (no auto-titling), `ended-by-model`, `pr-link` write, `continued-in`, `tag`/`agent-*`. LOW-MEDIUM.
10. **Timeout/env surface** — no `MCP_TOOL_TIMEOUT`, no per-server `timeout` field, `CallTool` fixed at 120s; SSE GET stream never reconnects; `/mcp reconnect` no-op. MEDIUM.
11. **Plugin registry drift** — `synced/` vs real `cache/`, installed-file missing `version:2`/`lastUpdated`/`gitCommitSha`, `enabledPlugins` evaluated at user scope only, `--plugin-dir` granularity+separator inconsistency. MEDIUM.
12. **No `cleanupPeriodDays` transcript retention.** LOW.
