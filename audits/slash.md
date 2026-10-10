# Slash Command Audit — claude v2.1.294 (refsrc-294) vs claude-go replica

Scope: builtin slash commands only (user `.claude/commands/*.md` and MCP `mcp__*`
prompts are out of scope — both exist on each side).

## Sources (real)

- `refsrc-294/mod_0416.js` — the core command table (~95 `{type:"local-jsx"|"local"|"prompt",name:...}` objects; most commands have a `local-jsx` interactive variant and a `local` non-interactive variant).
- `refsrc-294/mod_0064.js` — the **canonical category map** `a` (`u_s(e)` buckets every builtin name into `config`/`action`/`info`/`agent`). This is the authoritative name list, including hidden/dev commands.
- `refsrc-294/mod_1204–1212.js`, `mod_2048.js`, `mod_0360.js`, `mod_1198.js` — lazy-loaded command objects (background, remote-control, daemon, recap, goal, brief, workflows, list-agents, install, web-setup, stop).
- `refsrc-294/mod_0683.js`, `mod_1752–1758.js`, `mod_1627.js`, `mod_0674/0678.js` — skill-backed commands registered via `Us({name,menuDescription,description,whenToUse,getPromptForCommand})` (prompt-type).
- Aliases resolved via `aliases:[...]` fields (nearest owning `name:`).

Ours: `internal/repl/repl.go` `builtinSlashCommands()` (130 names),
`internal/repl/commands.go` `handleSlash` switch (~105 case arms),
`internal/repl/dialogs.go`, `internal/repl/customcmd.go` (user .md commands).

## Definitely missing (real has, ours has neither registry entry nor dispatch)

User-visible command objects:

    /version /upgrade /tui /mobile /desktop /session /recap /list-agents
    /install /install-slack-app /passes /rate-limit-options /pro-trial-expired
    /auto-mode-setup /web-setup /workflows /daemon /skill-doctor /stickers
    /team-onboarding /loops /low-priority /limit-reset /claim-credit

Skill-backed commands (type:"prompt" / Us registration):

    /commit /commit-push-pr /verify /simplify /design /design-sync
    /explain-usage /claude-api /claude-code-docs /claude-in-chrome
    /cowork-plugin /run-skill-generator /setup-claude /workflow-authoring
    /artifact-design /artifact-diagramming /artifact-capabilities
    /artifact-components /artifact-pr-review /workshop /whiteboard
    /prototype /dataviz /doc (artifact doc-skill kind)

Hidden/dev commands (in real's category map, mostly flag- or env-gated):

    /channel /onboarding /autopilot /bugfix /dashboard /docs /investigate
    /thrash /input-debug /render-debug /mock-limits /perf-issue
    /reset-limits /simulate-usage /__remote-workflow /workflow-launch-exec
    /autofix-pr

Missing aliases (the parent command exists in ours, the alias doesn't):

    /continue→resume /share→feedback /app→desktop /name→rename
    /remote→session /bashes→tasks /tp→teleport /plugins+/marketplace→plugin
    /rc→remote-control /update→restart /peers→list-agents
    /ios+/android→mobile /setup-cowork→setup-claude

Phantom/invented in ours (no real counterpart, or real arg promoted to command):

    /auto /low /medium /high /xhigh /max — registered + advertised in
        SlashCommandNames but hit `default:` → "unknown command". In real
        these are /effort level arguments, never commands.
    /alias, /files, /todos — invented commands; nothing in real's tables
        (`name:"alias"`/`"files"` are shell-arg spec tables, `todos` is a
        lifecycle task name; none in the mod_0064 category map).

## Diff table (sorted by severity)

| /command | real behavior (evidence) | ours | status | e2e | severity |
|---|---|---|---|---|---|
| /low /medium /high /xhigh /max /auto | not commands — `/effort <level>` args; `N0t` builds hint `[low|...|auto]` (mod_0416 `Kwo`,`kHe`) | registered → "unknown command" | broken phantom | none | HIGH |
| /import | "Import config from another AI coding agent", args `[codex|gemini|cursor] [--dry-run]` (mod_0416) | reads a file into context instead | wrong feature | /import 1 | HIGH |
| /fork | "Spawn a background agent that inherits the full conversation" `<directive>` / `[prompt]` (mod_0416, gated `!ua()`) | transcript/session fork of current chat | divergent | /fork 1 | HIGH |
| /mcp | "Manage MCP servers", `[reconnect (<server>|all)|enable|disable [<server>|all]]`; local-jsx twin panel + local mgmt (mod_0416 `Fco`,`mcp local`) | list servers + reconnect stub only | partial | /mcp 19 | HIGH |
| /resume | local-jsx picker, `aliases:["continue"]`, argHint `[conversation id or search term]` (mod_0416) | picker always; arg ignored; no /continue | partial + missing alias | /resume 5 | HIGH |
| /code-review | skill `n6`, alias `/review`, effort arg `/code-review high`, `ultra` subcommand→ultrareview (mod_0683 `ho()`) | sends bundledCodeReview; arg treated as review target, no effort/ultra | arg semantics differ | /review 1, /code-review 0 | HIGH |
| /plugin | "Manage Claude Code plugins", aliases `plugins`,`marketplace` — full manager dialog (mod_0416 `Jbo`) | prints one line | stub | /plugin 1 | HIGH |
| /color | "Set the prompt bar color for this session" (local+jsx, mod_0416) | merged into `case "theme","color"` — sets UI theme | divergent | none | HIGH |
| /plan | "Enable plan mode or view the current session plan" `[open|<description>]` (mod_0416 `Hbo`) | only sets PermMode=plan, no args | partial | none | HIGH |
| /version | exists (local+jsx) but `isEnabled:()=>!1` in 294 — dead entry kept for analytics (mod_0416 `YBe`) | absent | missing (disabled in real) | none | MED |
| /tui | "Set the terminal UI renderer (default | fullscreen)" `[default|fullscreen]` (mod_0416 `Fbo`) | absent | missing | none | MED |
| /upgrade | "Upgrade to Max for higher rate limits and more Opus" (mod_0416) | absent | missing | none | MED |
| /mobile | "Show QR code to download the Claude mobile app", aliases ios/android (mod_0416 `Bco`) | absent | missing + aliases | none | MED |
| /desktop | "Continue the current session in Claude Desktop", alias app (mod_0416) | absent | missing + alias | none | MED |
| /session | "Show cloud session URL and QR code", alias remote, disabled 294 (mod_0416) | absent | missing (disabled in real) | none | MED |
| /list-agents | "List subagents, teammates, and other Claude sessions you can message", alias peers (mod_1206) | absent | missing + alias | none | MED |
| /install | "Install Claude Code native build" `[options]` (mod_2048) | absent | missing | none | MED |
| /install-slack-app | config-category command (mod_0064; obj in mod_0416) | absent | missing | none | MED |
| /passes | "Share a free week of Claude Code", gated `DGe()` (mod_0416 `jBe`) | absent | missing | none | MED |
| /rate-limit-options | "Manage usage limits and upgrade options" (mod_0416) | absent | missing | none | MED |
| /pro-trial-expired | "Options shown when the Pro plan trial has ended" (mod_0416) | absent | missing | none | MED |
| /auto-mode-setup | "Teach auto mode about your environment, plus optional rule tweaks" (local+jsx, mod_0416) | absent | missing | none | MED |
| /web-setup | "Set up cloud sessions with your GitHub account" (mod_0360) | absent | missing | none | MED |
| /workflows | "Browse running and completed workflows" (mod_1204) | absent | missing | none | MED |
| /daemon | "Manage background services and routines" (mod_1209) | absent | missing | none | MED |
| /skill-doctor | "Show which loaded skills are unused and costing context" (local+jsx, mod_0416 `NBe`,`ece`) | absent | missing | none | MED |
| /stickers | "Order Claude Code stickers" (local, mod_0416) | absent | missing | none | MED |
| /recap | "Generate a one-line session recap now" (local, mod_1210) | absent | missing | none | MED |
| /team-onboarding | type prompt "Help teammates ramp on Claude Code" (mod_0416) | absent | missing | none | MED |
| /loops | "List, create, and delete loops", `isEnabled:()=>!1` (mod_0416 `Gbo`) | absent | missing (disabled in real) | none | MED |
| /commit /commit-push-pr /pr | prompt skills `q5t`,`cso`,`K5t` — commit/PR creation prompts; ours has /pr only | /pr present; /commit, /commit-push-pr missing | missing | /pr 0 | MED |
| /verify /simplify | prompt skills `Qte`,`V5t` — verify fix / clean up changed code (mod_0303 names) | absent | missing | none | MED |
| /setup-claude | prompt skill `xls`, alias `setup-cowork` — guided setup (mod_1756) | absent | missing + alias | none | MED |
| /explain-usage | prompt skill `Rls` — "See where this session's tokens went, in plain words" | absent | missing | none | MED |
| /claude-api /claude-code-docs /claude-in-chrome | prompt skills (mod_0678, mod_1752, mod_0683) | absent | missing | none | MED |
| /design /design-sync /workflow-authoring /cowork-plugin /run-skill-generator | prompt skills `Grt`, design-sync, `DO`, `l0n`, run-skill-generator | absent | missing | none | MED |
| /artifact-* /workshop /whiteboard /prototype /dataviz /doc | artifact + workshop prompt skills (`Wq`,`Wrt`,`Lh`,`Tls`,`Sle`,`Als`,`i0n`,`a0n`, doc kind) | absent | missing | none | MED |
| /continue /share /app /name /remote /bashes /tp /plugins /marketplace /rc /update /peers /ios /android /setup-cowork | real aliases of resume/feedback/desktop/rename/session/tasks/teleport/plugin/remote-control/restart/list-agents/mobile/setup-claude | no aliases registered (handleSlash matches exact names only) | missing aliases | none | MED |
| /memory | "Edit CLAUDE.md files and memory settings" — memory editor menu (mod_0416 `Sco`) | prints memory dir listing | divergent shape | /memory 2 | MED |
| /context | "Visualize current context usage as a colored grid" `[all]` (mod_0416 `jFe`) | prints token counts | divergent shape | /context 6 | MED |
| /release-notes | release-notes viewer (local-jsx, mod_0416; info category) | prints "claude-go <ver>" | divergent | /release-notes 1 | MED |
| /keybindings | real `type:"local"` opens keybindings.json editor (mod_0416) | prints "edit ~/.claude/keybindings.json" | partial | /keybindings 2 | MED |
| /fast | `Toggle fast mode (<state>)` `[on|off]` (mod_0416 `Ubo`,`HBe`); real arg semantics unclear for no-arg | no-arg turns OFF | probably divergent | /fast 1 | MED |
| /pause-memory | real `isEnabled:()=>!1` stub in 294, aliases memory-pause/toggle-memory (mod_0416 `bco`) | working toggle implemented | ours ahead / divergent | /pause-memory 1 | MED |
| /btw | "Ask a quick side question…" `[question]`, `isEnabled:()=>!1` in 294 | implemented, prints usage | ours ahead / divergent | /btw 2 | MED |
| /bug /feedback | "Report a bug or share your conversation" `[report]`, alias share — report dialog | print URLs only | partial | /bug 1, /feedback 0 | MED |
| /teleport | "Send this session to the cloud, or resume one from claude.ai", alias tp | send-to-cloud only; resume-from-cloud unwired (comment) | partial | /teleport 1 | MED |
| /permissions | "Manage allow and deny tool permission rules", alias allowed-tools, control-request editor (mod_0416) | tabbed modal w/ search; alias ok | approx | /permissions 2 | LOW |
| /tasks | "View and manage everything running in the background", alias bashes (mod_0416 `Qco`) | lists bg tasks; no bashes alias | partial | /tasks 1 | LOW |
| /compact | `<optional custom summarization instructions>`, DISABLE_COMPACT gate (mod_0416 `mco`) | focus arg → AppendSysP, spinner, hooks | approx; no env gate | /compact 5 | LOW |
| /usage /cost /stats | "Show session cost, plan usage, and activity stats" dialog + non-interactive variant | sessionCostBlock + usageDisplay print | approx | covered | LOW |
| /status | status overlay: version/model/account/API/tools (mod_0416) | statusOverlay tabs | approx | /status 6 | LOW |
| /hooks | read-only hooks menu "View hook configurations" (mod_0416 `Wbo`) | hooksMenu | approx | /hooks 4 | LOW |
| /export | "Export conversation to a file or clipboard" `[filename]` | exportDialog + file write (no clipboard) | approx | /export 1 | LOW |
| /diff | toggle diff panel / view uncommitted changes (mod_0416 `GFe`) | diffCmdKS prints diff | approx | /diff 5 | LOW |
| /skills | "List available skills" panel | prints skill list / "no skills found" | approx | /skills 3 | LOW |
| /sandbox | dynamic-desc sandbox status/config object `gwo` (mod_0416) | prints "Sandbox Mode Overrides Config" | approx | /sandbox 1 | LOW |
| /model | local-jsx picker + local `<model>` arg (mod_0416) | arg + picker; 's' session-only flag | approx | /model 11 | LOW |
| /theme | theme picker dialog (mod_0416 `Nbo`) | runThemePicker + persist | approx | /theme 5 | LOW |
| /effort | `[level]` + slider (mod_0416 `Kwo`) | effortCmd + effortSlider | approx | /effort 2 | LOW |
| /config /settings | `[key=value]` / `key=value`; alias settings | overlay tab 2 / configCmd; alias ok | approx | /config 9 | LOW |
| /help | local-jsx fullscreen help (mod_0416 `wco`) | printHelp sections | approx | /help 2 | LOW |
| /ide | "Manage IDE integrations and show status" `[open]` | "no IDE detected" print | partial | /ide 1 | LOW |
| /vim | hidden flag-gated "Editor mode moved to /config" `uOt("vim",…)` | handled (prints moved msg), unregistered | approx (hidden both) | /vim 1 | LOW |
| /agents | hidden "(removed)" stub — prints pointer (mod_0416 `Ybo`) | prints same removal note | match | /agents 3 | LOW |
| /wellbeing | disabled stub + aliases breaks/break-reminder/downtime | same stub msg + aliases | match | /wellbeing 1 | LOW |
| /init /insights /statusline /doctor /security-review /batch /run /debug /update-config /fewer-permission-prompts /keybindings-help /memory-types | prompt/skill commands — inject a prompt and run a turn | each sends a bundled/agent prompt | approx | mixed | LOW |
| /loop /proactive /schedule /routines | prompt skills (`zrt` alias proactive; `c0n` alias routines) — cron scheduling, cloud routines | loopCmd schedules locally; scheduleCmd needs account | approx | /loop 10 | LOW |
| /exit /quit /clear /reset /new /cd /copy /subtask /goal /branch /rename /rewind /checkpoint /undo /restart /stop /background /bg /pause→aliases | session actions — quit, clear, cd, copy N, subagent, goal, branch, rename, checkpoint rewind, restart, stop bg session, bg detach | equivalents implemented w/ same aliases (minus restart's /update) | approx | mixed | LOW |
| /login /logout /oauth-refresh | Anthropic OAuth flows (local+jsx) | own OAuth impl | divergent but present | covered | LOW |
| /advisor /autocompact /scroll-speed /focus /fast /setup-bedrock /setup-vertex /privacy-settings /terminal-setup /chrome /cloud-plugins /usage-credits /extra-usage /remote-env /remote-control /design-consent /design-login /design-revoke /env /experiments /issue /radio /ant-trace /debug-tool-call /heapdump /output-style /voice /add-dir /import /btw /goal /effort /context /diff /export /memory /mcp /model /permissions /plugin /powerup /skills /status /tasks /theme /usage /voice | config/action/info commands — settings panels, toggles, dev tools | equivalents or prints | approx | most covered | LOW |
| /low-priority | "Continue now at lower priority after reaching your session limit" (`nge`, hidden) | absent | missing hidden | none | LOW |
| /limit-reset | "Reset your session limit now and keep working" (`XY`, hidden) | absent | missing hidden | none | LOW |
| /claim-credit | "Open the page of the offer shown at start-up" (`WVt`, hidden) | absent | missing hidden | none | LOW |
| /mock-limits /perf-issue /reset-limits /simulate-usage /thrash /input-debug /render-debug /onboarding /channel /autopilot /bugfix /dashboard /docs /investigate /__remote-workflow /workflow-launch-exec /autofix-pr | hidden/dev/agent commands in category map (mod_0064); no literal `name:` objects found — defined via imported vars or dead entries | absent | missing dev/hidden | none | LOW |
| /alias /files /todos | not real commands | implemented extras | invented | covered | LOW |

## Counts

- Real builtin command names: ~95 category-map entries + ~20 more command
  objects (version, tui, upgrade, mobile, desktop, session, install*, passes,
  rate-limit-options, pro-trial-expired, auto-mode-setup, web-setup, workflows,
  daemon, skill-doctor, stickers, recap, team-onboarding, loops, low-priority,
  limit-reset, claim-credit) + ~28 skill-backed prompt commands + ~24 aliases.
- Ours: 130 registry names + /vim dispatched.
- Missing real commands (excl. dev-only): **~50** (25 command objects +
  25 skill/prompt commands).
- Missing real aliases: **15**.
- Phantom registry entries hitting "unknown command": **6** (auto, low, medium,
  high, xhigh, max).
- Invented commands not in real: **3** (/alias, /files, /todos).
- E2e: TestSlashTable covers ~62 commands; TestSlashSmoke 6; dedicated tests
  for /copy, /advisor, /model picker, /resume(-r), /compact, /rewind, /loop.
  ~50 registry names have zero e2e coverage.

## Top gaps

1. **Phantom effort-level commands** — /low /medium /high /xhigh /max /auto
   are advertised (init event + completion candidates via SlashCommandNames)
   but dispatch to "unknown command". Remove from registry or dispatch to
   effortCmd.
2. **/import implements the wrong feature** — real imports config from
   codex/gemini/cursor; ours reads a file.
3. **/fork semantic mismatch** — real spawns a background agent inheriting
   context; ours forks the transcript.
4. **/mcp is read-only** — missing enable|disable|reconnect <server>|all
   arg handling.
5. **/resume ignores its arg** — real takes `[conversation id|search term]`;
   also missing /continue alias.
6. **/code-review arg parsing** — real parses effort level + `ultra`
   subcommand; ours treats arg as a review target.
7. **~25 missing visible commands** incl. /tui, /upgrade, /mobile, /desktop,
   /skill-doctor, /workflows, /daemon, /web-setup, /auto-mode-setup, /install.
8. **~25 missing skill-backed commands** (commit, commit-push-pr, verify,
   simplify, setup-claude, claude-api, explain-usage, artifact-*, workshop,
   whiteboard, prototype, dataviz, …).
9. **15 missing aliases** for commands we do implement (tp, bashes, plugins,
   marketplace, rc, update, name, remote, share, continue, peers, app,
   ios, android, setup-cowork).
10. **/color merged into /theme** — real sets the prompt-bar color, a
    different setting.
