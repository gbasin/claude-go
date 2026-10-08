# TUI surface audit — claude 2.1.294 (refsrc-294/) vs claude-go replica

Scope: keybindings, prompt editor, rendering, dialogs/pickers, terminal
protocols. Real evidence cites `refsrc-294/mod_NNNN.js` (byte offset or
symbol). Ours cites `internal/repl/*`. e2e column cites `e2e/*_test.go`
(✓) or unit tests in `internal/repl/*_test.go` (u). Severity:
**input-losing** > crashy > visibly wrong > cosmetic.

## 1. Keybindings

Real's table: `mod_0263.js` `h1` (26 context blocks) + action registry
(171 `ctx:action` names). keybindings.json validation: `mod_0263.js`
(`Je()` — `invalid_context`, `invalid_action` "this binding is ignored"
with "Did you mean?" suggestions, `parse_error` with modifier hints;
`Unknown context "X"`, `Valid contexts: …`). Live file watcher reloads
keybindings.json (`mod_0263.js` ~33221 `Watching for …keybindings`).

Ours: `internal/repl/keybindings.go` — defaults table + chord parser +
27-context allowlist + Chat/Global live dispatch + `command:` actions.

| surface | real behavior + evidence | ours | status | e2e | severity |
|---|---|---|---|---|---|
| Global ctx | `ctrl+c`→app:interrupt, `ctrl+d`→app:exit, `ctrl+t`→toggleTodos, `ctrl+o`→toggleTranscript, `ctrl+shift+b`→toggleBrief, `ctrl+r`→history:search, `ctrl+up/down`+`meta+up/down`→diffFileListUp/Down, `ctrl+]`→openArtifact (mod_0263 h1) | all bound; diff-file-list + openArtifact bound but inert (keybindings.go:141-148) | parity | TestKeybindingCommandRemap ✓ | — |
| Chat ctx | escape→cancel, ctrl+l→clearInput, cmd+k→clearScreen, ctrl+x ctrl+k→killAgents, shift+tab(or meta+m)→cycleMode, meta+p→modelPicker, meta+o→fastMode, **ctrl+y→defaultToNewerModel**, meta+t→thinkingToggle, meta+w→workflowKeywordToggle, enter→submit, ctrl+x enter→queueSubmit, ctrl+x ctrl+s/ctrl+enter→sendNow, ctrl+j→newline, up/down→history:prev/next, ctrl+_/-(±shift)→undo, ctrl+x ctrl+e + ctrl+g→externalEditor, ctrl+x ctrl+a→abovePrompt:toggle, ctrl+x tab→abovePrompt:focus, ctrl+s→stash, ctrl+v/alt+v→imagePaste, space→voice:pushToTalk | all bound except abovePrompt:* (missing); **ctrl+y bound to invented `chat:yank`** → pops draft-stash, not real's action (keybindings.go:97,133) | divergent | TestStashYank ✓ (but pins non-real semantics) | visibly wrong |
| abovePrompt ctx | `ctrl+x ctrl+a` toggle, `ctrl+x tab` focus (mod_0263 h1) | absent — no AbovePrompt surface | missing | — | cosmetic |
| Chat: ctrl+y | `chat:defaultToNewerModel` handler returns false when kill-ring non-empty so editor yank still works (mod_1600:840889 `if(H9e(k.state)!=="")return!1`) | kbActions `chat:yank` pops ctrl+s stash (term.go:1998) | divergent | ✓ | visibly wrong |
| Chat: ctrl+s stash | toggle: empty input + stashed → popStash; non-empty → stash (mod_1600:826000 `M.stash()/M.popStash("input")`) | stash only; pop lives on ctrl+y — ctrl+s on empty input is a no-op (term.go:1986-1995) | divergent | ✓ | visibly wrong |
| Non-prompt contexts | real defaults for Autocomplete, Settings, Confirmation, Tabs, Transcript, HistorySearch, Task, ThemePicker, Scroll, Help, Attachments, Footer, AbovePrompt{,Input,Select}, Pane, PaneField, DiffDialog, ModelPicker, EffortSlider, Select, Plugin, Agents, MessageSelector — user remaps in these contexts take effect in those dialogs | contexts accepted+stored but never dispatched — every dialog hardcodes keys (dialogs.go draw loop, transcript.go, picker.go); remapping e.g. transcript j/k does nothing | missing | — | visibly wrong |
| Context count | 27 valid contexts | 27 — same list (keybindings.go:43) | parity | — | — |
| Chords | space-separated, prefix waits for next stroke | same, 3s timeout (keybindings.go:400+) | parity | — | — |
| Unknown action | validation error `invalid_action` — "this binding is ignored" (binding dropped → default key behavior survives) | stored silently; consumes the key dead (keybindings.go:373-380) | divergent | TestKeybindingInertAction asserts ours | cosmetic (misconfig path) |
| command:<name> | executes slash command | same (keybindings.go:429) | parity | ✓ | — |
| Validation UX | per-binding typed diagnostics + suggestions, dismissible notice, live file watcher | single stderr summary line; load-once, no watcher (keybindings.go:360) | divergent | TestKeybindingWarn ✓ | cosmetic |
| Reserved keys | ctrl+m/i/h/[ aliases etc. warned | kbReserved warns (keybindings.go:65) | parity | — | — |

## 2. Editor semantics

Real editor: `EMt`/`mi`/`Ot` in `mod_0263.js` (cursor+measuredText classes,
kill-ring store `se` via `T1e()`, `EMt` = useTextInput), vim state
machine `dr()`/`or()`/`No()` in `mod_0795.js`.

| surface | real behavior + evidence | ours | status | e2e | severity |
|---|---|---|---|---|---|
| Emacs layer | ctrl: a b c d e f h k n p u w y (`Kt` map); meta: b f d y(yank-pop `Ht`); kill-ring with yank-pop, kill→register shared with vim; super+left/right → line start/end; meta|ctrl+backspace → backwardKillWord; super/meta+delete → kill-to-end (mod_0263 `Kt`,`Gt`,`Vt`,`Yt`) | ctrl: a e k u w only. **ctrl+b/f/n/p dead** (fall through insertKey switch). meta+b/f/d dead (alt-prefixed letters never reach a word op). ctrl+y hijacked by stash-pop (see §1). No kill-ring/yank-pop. meta/super+backspace → plain 1-char delete | divergent | TestUndoKey, TestClearInput ✓ | **input-losing** (dead keys) |
| Escape on non-empty input | 1st esc → notification "Esc again to clear" only; 2nd within timeout → clears + **saves draft to history incl. pastedContents** (`Wt`=`jz(first,confirm)`, `aae(ee,Qe,{submitMode:k})`, mod_0263) | 1st esc clears immediately (cancelKey, term.go:1317); no hint; cleared text lost — no history save, no undo push | divergent | — | **input-losing** |
| Esc-esc on empty | opens message selector / rewind (jz+`openMessageSelector`, mod_1600:826000) | esc-esc within 400ms → rewind (term.go:1327) | parity | TestEscEscRewindPicker ✓ | — |
| ctrl+c | non-empty → clear; pending arm → "Press {key} again to exit"; 2nd → exit (jz 3-cb, mod_0263 `Mt`) | same (interruptKey, term.go:1289) — but hint hardcodes "Ctrl-C" vs real's resolved key name | parity | TestInterruptMidTurn ✓ | cosmetic |
| ctrl+d | non-empty → delete-forward; empty → arm "again to exit"; 2nd → exit (`It()`→`ut("Ctrl-D")` through jz) | empty → exits on FIRST press (exitKey, term.go:1306) | divergent | — | visibly wrong (surprise exit) |
| Cleared-draft recovery | ctrl+c'd draft re-enters history, Up restores it (changelog mod_0685 "a cleared prompt now stays reachable with Up") | interruptKey clears without pushUndo/history → gone for good | divergent | — | **input-losing** |
| Up/down in multiline | up on line 0 → history, else moves cursor between wrapped/logical lines (`mt`,`ht` in EMt) | `[A`→histPrev unconditionally; `[B`→histNext — cursor cannot move within a multiline draft | divergent | — | visibly wrong |
| shift+enter | kitty alt-key reporting → inserts `\n` (`jt`: `u.meta||u.shift→insert("\n")`) | `CSI 13;2u`→k{b:'\r',mod:shift} fails `mod==0` submit guard, hits no case → **swallowed dead** (input.go decodeCSI + term.go:529) | divergent | — | input-losing (no-op key) |
| Vim: modes | INSERT/NORMAL/VISUAL/VISUAL LINE (mod_0795 `oo`,`ne`) | insert/normal/visual — **no visual-line `V`** | divergent | TestVimModeEditor, TestVimBadge ✓ | visibly wrong |
| Vim: motions | h l sp bksp j k gj gk w b e W B E 0 ^ $ G gg f F t T ; , counts≤1e4 (`Xs`, `un`, `cn`, `vi`) | same minus **gj/gk**; counts capped 1e4 (term.go:756) | divergent | ✓ smoke only | cosmetic |
| Vim: operators | d c y + linewise dd/cc/yy + `>`/`<` indent + f/t operatorFind + text objects (`Ot`,`pn`,`mn`) | d c y > <, dd/cc/yy/>>/<<, f/t-op via vimFind+op (term.go:694-738) — **no i/a text objects** (iw/aw/i"/a(/i{ etc. — real `Zn` set has 17 objs) | divergent | — | visibly wrong |
| Vim: other cmds | i I a A o O x s S? r(replace) ~ D C Y p P J u . ? / (`Fe` change-type table: insert, x, substitute, replace, toggleCase, indent, join, openLine, paste) | i a A I o O x X d c y D C Y s S r p u . ~ J ? / (term.go:783-976) — **no `P`** (paste before); `.` replays last normal-mode **key** only — real replays the whole recorded change incl. inserted text (`recordChange`,`onDotRepeat`) | divergent | — | cosmetic |
| Vim: visual ops | all motions + counts + finds; d/c/y, r, ~-case ops, p paste-over, J join, >/< indent, i/a select-range, o swap-anchor (`No` visual dispatch) | h l sp w W b B e E 0 $ o d/x/y/c — **no j/k/G/gg/f/counts/r/>/</J/p** (visualKey, term.go:1864) | divergent | — | visibly wrong (can't select vertically) |
| Vim: `/`,`?` | `/`→history search, `?`→help overlay (mod_0795 rt idle branch) | `/`→histSearch, `?`→noop w/ comment (term.go:967) | parity | — | — |
| Vim insert remaps | `vimInsertModeRemaps` pairs → Esc, with pending-char timeout `Lo` (mod_0795 `P0t`,`ee`) | pairs → Esc, **no timing window** — pair works regardless of gap (term.go:596) | divergent | — | cosmetic |
| Vim registers | one unnamed register + linewise flag; `registerIsLinewise` (mod_0795 `io`) | one `lastDel` + linewise via `\n` sniff — same effective surface (term.go:1622) | parity | — | — |
| Vim undo/redo | `u`→onUndo checkpoint stack; no redo key found | u→undoStack; no redo (term.go:1595) | parity | — | — |
| Undo grouping | checkpoints pushed before destructive ops (`Mu(...)` before insert/external-edit/newline); insert bursts grouped via `insertedText` tracking | pushUndo at insert-burst start (`insGroup`) + each destructive op (term.go:614-624,1595) — stack cap 50 | parity | TestUndoKey ✓ | — |
| Bracketed paste | `?2004h`/`200~…201~`; paste≥len 800 (`z3`) OR newlines>min(rows-10,2) (≈2) → `[Pasted text #N]`/`#N +M lines` where **M=newline count** (mod_0304 `zee`,`a1`; mod_1600:822387). Drag/dropped file paths → image sniffing → `[Image #N]` (mod_0263 `R()` paste path). "…Truncated text #N +M lines…" for overflow | placeholder at nl>0 OR len≥200 → `[Pasted text #N +M lines]` where M=nl+1 (term.go:301-310) — wrong for 1-line long paste (real omits `+lines`) and off-by-one vs real's newline count; paste marker split across read() chunks can break (input.go:104-116); no dragged-image-path handling | divergent | TestBracketedPaste ✓ | cosmetic→visibly wrong |
| Image paste | ctrl+v/alt+v → clipboard image → `[Image #N]` + pastedContents entry (mod_1600 `QTe`); "No image found in clipboard…" hint incl. SSH variant | pngpaste/osascript → scratchpad `[Image #N]` (imagePaste, term.go:1380+) | parity-ish | TestImagePasteNoImage ✓ | cosmetic |
| History file | `~/.claude/history.jsonl` records incl. cleared-draft saves w/ pastedContents map + submitMode | same file, schema {display,timestamp,project,pastedContents,sessionId} (term.go:1915) — but no cleared-draft entries | divergent | — | cosmetic |
| @-completion | @token → fuzzy file picker (mod_1600 `tengu_ext_at_mentioned`, `H0t` watcher); submit expands to file blocks | acAt completion + expandMentions→`Contents of <p>:` block (term.go:1060-1138, repl.go:1160) | parity-ish | TestAtMentionAutocomplete ✓ | cosmetic |
| `#` memory | `#` prefix → memory write flow; input renders `#` with "remember" color + memoryBackgroundColor badge (mod_1551:118097) | `#` → append bullet to projects/<slug>/memory/MEMORY.md (repl.go:1691); **no input-time `#`/`!` mode badge** | divergent | — | cosmetic |
| `!` bash | `!cmd` → bash mode (highlighted `!`), output wrapped as local-command-stdout | runBang → Bash dispatch + `<bash-input>/<local-command-stdout>` wire block (repl.go:1189); respondToBashCommands honored | parity-ish | TestBangInterpolation ✓ (different feature) | — |
| Backslash-return | `\`+enter → newline (mod_0263 `jt`) | same (submitKey, term.go:1337) | parity | TestSubmitKeyBackslashContinuation u | — |
| `?` on empty input | shortcuts overlay | showShortcuts list (term.go:2017) | parity | TestShortcutsOverlay ✓ | — |
| Left-arrow on empty | ← → "Press ← again" arm → agents pane/history (mod_0263 left-arrow-again FSM: fire/arm/absorb) | leftArm 1.5s double-press + running-agents gate (term.go:492-513) | parity-ish | TestAgentsPane ✓ | cosmetic |
| Ghost text / inline suggestions | `inlineGhostText` + `editRelay` recording (mod_0263 EMt props) | none | missing | — | cosmetic |
| Selection model | shift+arrows extend selection, ctrl+shift+c/cmd+c copy via clipboard stack w/ OSC52+tmux fallbacks (Scroll ctx bindings, `M$e` copyOnSelect in mod_0795) | none | missing | — | cosmetic |

## 3. Rendering

| surface | real behavior + evidence | ours | status | e2e | severity |
|---|---|---|---|---|---|
| Composer chrome | bordered prompt in live region + hint rows; Ink static/live split | ╭─╮/│/╰─╯ box via compositor zone; `─` rule fallback off-zone (term.go:119-256) | parity | TestStartupChrome, TestAltScreenProtocol ✓ | — |
| Tool-call row | `⏺ ToolName` bold + summarized input; result `⎿` lines | same shape (repl.go:263,267-280) | parity | TestInteractiveToolCall ✓ | — |
| Result fold | above-the-fold = **3 wrapped lines** (col-aware), 4-line grace case; `… +N lines` + `(<resolved-binding> to expand)` where binding resolves through keymap (mod_0397 `d=3,u=10`, `Y_("app:toggleTranscript","Global","ctrl+o")`) | 5 raw lines, `trunc(ln,113)`; hint hardcodes "(ctrl+o to expand)" — stale if user remaps (repl.go:269-279) | divergent | — | visibly wrong |
| Thinking | `✻ Thinking…` dim **italic**; body shows API-side *summary* text (mod_1551:16527 `dimColor+italic`, `latestThinkingSummary`), gated by showThinkingSummaries; `Thought`/`thinking for Ns` elapsed variants | `✻ Thinking…` dim (no italic) + **raw thinking stream** in dim (repl.go:242-251) | divergent | TestThinkToggle, TestNarrowThink40 ✓ | visibly wrong |
| Spinner | `·✢✳✶✻✽` bounce ~150ms + shimmer verb band + elapsed `Ns` + `↓ N tokens` + `esc to interrupt` + retry `· next try in … · attempt N/M` + tips after 30s/30min + "running tool for Ns"/"thought for" sub-status (mod_1425) | same frame set + shimmer + `(esc to interrupt · ← for agents · Ns · ↓ Nk tokens)` + hook progress `(running X hook… n/m · Ns)` + rotating `⎿ Tip` (repl.go:1093-1141) — **no retry/attempt suffix, no timed tip thresholds** | parity-ish | TestSpinnerDuringToolExec, TestHookStatusInSpinner ✓ | cosmetic |
| Verb pool | 189 gerunds, settings `spinnerVerbs{mode,verbs}` (mod_0767 `v`, `hMt`) | identical 189 + settings hook (repl.go:813-1057) — verified list-diff: equal modulo \xE9 escaping | parity | — | — |
| Turn-end footer | `✻ <PastTense> for Ns · done H:MM` (ours comment claims captured; real renders elapsed+clock variants) | `pastTense(verb) + for Ns + done H:MM` (repl.go:641) | parity-ish | TestTurnEndLine ✓ | cosmetic |
| Queued msgs | pending sends render under composer + `N queued` in status/task rows (`${Q} queued` mod_1600:401103) | `❯ <queued>` dim row + `N queued` footer badge (repl.go:515, statusline.go:20) | parity-ish | TestQueuedMessage, TestQueuedHint ✓ | cosmetic |
| Markdown | streamed markdown: bold/italic/code/links(osc8)/tables/fences+builtin syntax hl/headers/lists incl. **numbered**; resize re-wraps transcript (changelog: "numbered replies… resize and re-open in transcript") | bold/italic/`code`→cyan/osc8 links/tables→box-drawn/fences→highlightLine/`#`→bold/`- `→`•`/`> `→`▎ + proseW lazy wrap (render.go); **no numbered-list handling, no strikethrough** | parity-ish | render_test.go u ×5 | cosmetic |
| Status line | configurable statusLine command + contextual hint rows (vim badge, mode pill, queued, tasks, proactivity) | statusline.go footerHints + `vim:<mode>` + permModeBanner pills (⏸/⏵⏵) + `? for shortcuts` + `← for agents` + custom statusLine cmd (statusline.go, term.go:2080) | parity-ish | TestStatusOverlay, TestStatusLineCommand, TestVimBadge ✓ | cosmetic |
| Dividers/footer | `─` rules + FooterHintLine with resolved keybinding chords (`ide`, `kQ`, mod_1600:7615xx: `ctrl+t todos · ctrl+_ undo · ctrl+s stash…` via `qa()` resolver) | boxRule `─` + footerText + fixed hint strings (term.go:2054-2088) — hints not binding-resolved | parity-ish | — | cosmetic |
| Queued-draft restore | unfinished queued text pops back into draft on interrupt (`popQueuedIntoDraft`) | same (repl.go:487) | parity | — | — |
| Interrupted | `⏺ Interrupted` yellow | same (repl.go:623) | parity | ✓ | — |
| File-path paste | dropped file path → image sniff → `[Image #N]` or text; screencaptureui handling (mod_0263 `R()`) | raw text insert only | divergent | — | cosmetic |

## 4. Dialogs / pickers

Real dialog contexts: Confirmation (enter/esc/↑↓/tab nextField/space toggle/shift+tab cycleMode), Select (j/k/ctrl+n/p/home/end/pageup/down/enter/esc), ModelPicker (←/→ effort, s session-only), ThemePicker (ctrl+t syntax, ctrl+e custom), Agents pane (ctrl+s/t/f/r, group nav).

| surface | real behavior + evidence | ours | status | e2e | severity |
|---|---|---|---|---|---|
| Permission prompt shape | boxed card; options: `Yes` (input row, placeholder "and tell Claude what to do next"), `Yes, and don't ask again for <rule> in <dir>` (editable-prefix input row `type:"input"`, placeholder "command prefix (e.g., npm run *)"), `Yes, and enable auto mode` (conditional), `No` (input row "and tell Claude what to do differently") (mod_1600:1172100-1173100); separate deny variant "Deny, and tell Claude what to do differently (esc)" (mod_1600:1094876); tab = nextField (moves between input rows), NOT amend | static numbered rows: `Yes` / `Yes, and switch to accept edits…` / `Yes, and always allow X from this project` / `Yes, and switch to auto mode` / `No`; `Esc to cancel · Tab to amend` (amend edits tool arg); number keys select (dialogs.go:196-336) | divergent | TestPermDialogWrite/Edit, TestPermissionPrompt/Deny ✓ | visibly wrong |
| Perm option order | yes → don't-ask → auto-mode → no | yes → acceptedits|always+automode → no — similar order, different wording/persistence semantics | divergent | ✓ | cosmetic |
| Perm digits | select rows are key-nav; numbers work in select lists | digits select (dialogs.go:316) | parity-ish | ✓ | — |
| Perm hooks | PermissionRequest can rewrite options (`askUser` deny/allow + suggestions); PermissionDenied hook event | both hooks fired (dialogs.go:88-100,256) | parity-ish | hook_matrix tests ✓ | — |
| Model picker | "Select model" + model rows + effort slider ←/→ + `s` session-only + overflow `… +N models` (ModelPicker ctx) | same shape hardcoded 5 models + `… +9 models` + effort ←/→ + s (commands.go:509-615) | parity-ish | TestModelPickerChrome, TestMetaModelPicker ✓ | cosmetic |
| Session picker | search, rename, preview, all-projects, bg marks | pickSession implements all (picker.go:30+) | parity | TestResumePickerInteractive ✓ | — |
| Theme picker | themes + syntax-preview + `ctrl+t` toggle highlighting + `ctrl+e` edit custom (ThemePicker ctx); j/k nav (Select ctx) | theme list + preview + ctrl+t flag (toggle no visual effect — `_ = syntaxOn`); **no ctrl+e, no j/k** (onboard.go:27-122) | divergent | TestThemePersist ✓ | cosmetic |
| /mcp panel | interactive panel: per-server detail/auth/tool views, reconnect flows | static ●/○ list (commands.go:2493) | divergent | — | cosmetic |
| /agents | Agents pane (bg-agent mgmt: ctrl+s switchView, ctrl+t pin, ctrl+f find, ctrl+r rename, group nav) — slash /agents wizard indeed removed | `/agents` prints pointer text (correct); agents pane via ← gesture exists (agentsPane result, TestAgentsPane*) — pane depth vs real's Agents ctx unclear | parity-ish | TestAgentsPane{,ListsBgTask} ✓ | cosmetic |
| Transcript (ctrl+o) | overlay w/ Transcript ctx: ctrl+e toggle detail, esc/q/ctrl+c exit, j/k, ctrl+n/p, ctrl+u/d half-pg, ctrl+b/f + space/b full-pg, g/shift+g, home/end, ↑↓ | overlay: esc/q/ctrl+o/ctrl+c exit, j/k+ctrl+n/p+↑↓, ctrl+u/d half-pg, ctrl+b/f+pgup/dn, g/G, ctrl+e — **missing space=pgdn, home/end** (transcript.go:35-95) | parity-ish | TestTranscriptOverlay, TestTranscriptView ✓ | cosmetic |
| Plan-mode approval | "Exit plan mode?" planMode-colored dialog, options Yes/(context variants like resume-auto-mode/keep-context)/No (mod_1600:1203941) | generic askEx "Approve the plan above?" (repl.go:188) | divergent | TestExitPlanModeApproval ✓ | visibly wrong |
| Exit flow | double-press gate "Press {resolved-key} again to exit" for ctrl+c AND ctrl+d (jz+exitMessage, mod_1470 `kC`) | ctrl+c double-tap ✓; ctrl+d exits on first press; hint text hardcodes Ctrl-C | divergent | — | visibly wrong |
| Rewind/message selector | esc-esc → message selector w/ restore-code+conversation options | rewind via readResult.rewind (term.go:1328) | parity | TestEscEscRewindPicker, TestRewindRestoresFile ✓ | — |
| Trust dialog | onboarding trust prompt | present (trust.go) | parity | TestTrustDialog ✓ | — |
| AskUserQuestion tool | multi-question dialog w/ options | dialogs exist | parity | TestAskUserQuestion{Dialog,Wizard} ✓ | — |

## 5. Terminal protocols

Real: mode manager `w()` in mod_0465 — modes: bracketedPaste `?2004h/l`,
themeReports `?2031h/l`, focusEvents `?1004h/l`, mouse full
`?1000h+?1002h+?1003h+?1006h` / scroll `?1000h+?1006h` / pixels `?1016` /
off, extendedKeys = kitty `pop <u` + push `>5u` (or `>1u` legacyKitty) +
xterm modifyOtherKeys `>4;2m` (off: `>4m`), altScreen = `?1049h` + `2J` +
`H`, altScreenKeys, WIN32_INPUT_MODE `?9001`. Sync output `?2026` gated
by probe. Capability probing: DECRPM `CSI ?<mode>$p` (`RPr`), kitty
keyboard query `CSI ?u` (`dPo`), XTVERSION `CSI >q`, kitty graphics
`_Gi=…` query, cell-pixel `CSI 16t`-family, OSC11 — heuristics seed
"likely" guesses that probes confirm (`mod_0463` `s={extendedKeys:false,
kittyKeyboard:true,…}` + `d()` env table).

Ours: `term.MakeRaw` + `?2004h` always + kitty `>1u` iff TERM_PROGRAM/TERM
matches kitty|ghostty|wezterm|iterm (input.go:41-58); altScreenEnter =
title `]0;✳ Claude Code\a` + `?1049h`+`2J`+`H` + `?1000h?1002h?1003h
?1006h` + `?2031h` + `?1004h`; exit reverses all incl `?2004l` (term.go:
2040-2070). release/grab pair restores cooked+modes for external
editor/suspend.

| surface | real | ours | status | e2e | severity |
|---|---|---|---|---|---|
| Kitty push flags | `>5u` (disambiguate+report-alternate-keys), `>1u` only for legacyKitty | always `>1u` — no alternate-key reporting (shift+enter etc. arrive unshifted) | divergent | TestAltScreenProtocol ✓ | input-losing (modifier fidelity) |
| modifyOtherKeys | `>4;2m` push, `>4m` pop — covers non-kitty xterm family | never emitted | missing | — | input-losing on xterm-family |
| Capability probes | DECRPM `$p`, `CSI ?u`, XTVERSION `>q`, kitty `Gi=` graphics, cell-size, OSC11 — results settle into `GL()` gating kitty/mouse/sync | none — env substring heuristic only; misdetects foot/contour/alacritty/vscode/mintty/wezterm-TERM etc.; can push `>1u` where unsupported | divergent | — | cosmetic→input-losing |
| Bracketed paste | `?2004h`/`l` | same | parity | TestBracketedPaste ✓ | — |
| Alt screen | `?1049h`+`2J`+`H`, `?1049l` restore | same + title OSC | parity | ✓ | — |
| Mouse | tiers full/scroll/pixels/off per surface | always 1000+1002+1003+1006 | divergent | — | cosmetic |
| Focus events | `?1004h/l` | same in enter/exit strings | parity | TestTmuxFocusWarn ✓ | — |
| Theme notify | `?2031h/l` | `?2031h` on, `?2031l` off | parity | — | — |
| Sync output | `?2026` begin/end around repaints when supported (tmux-aware gate) | absent | missing | — | cosmetic (flicker) |
| WIN32 input | `?9001` off on Windows | n/a (unix build) | n/a | — | — |
| Input parse | CSI-u full mod matrix, legacy CSI ~, kitty `>…u` replies, focus `[I/[O` filtered (`R()` tail-strip), mouse `[M`/`[<`, paste framing | CSI-u decode + ~ + letters + paste + alt-byte (input.go:97-215); `[I`/`[O` decode to dead keys — harmless | parity-ish | pty_test.go ✓ | — |
| Suspend | kitty/paste off + termios restore, resume reapplies | release/grab does same (input.go:67-91, suspend_unix.go) | parity | suspend_e2e ✓ | — |

## Summary counts

| status | count |
|---|---|
| parity / parity-ish | ~21 |
| divergent | ~19 |
| missing | ~6 |
| untested-of-the-above | most divergent rows lack e2e (see col) |

### Top gaps (by severity)

1. **input-losing** — single `esc` clears a non-empty draft (real: esc-esc
   clears + saves draft to history). Ours also can't recover a ctrl+c/esc-
   cleared draft via Up/ctrl+_. term.go:1289-1334 vs mod_0263 `Wt`/`Mt`.
2. **input-losing** — dead emacs keys: ctrl+b/f/n/p, meta+b/f/d/y,
   meta+backspace→backward-kill-word all do nothing or delete 1 char.
   term.go insertKey switch vs mod_0263 `Kt`/`Gt`.
3. **input-losing** — up/down in a multiline draft always page history;
   real moves the cursor between lines first. term.go:483-488 vs
   mod_0263 `mt`/`ht`.
4. **input-losing** — shift+enter swallowed on kitty terminals (ours
   pushes `>1u` then drops `13;2u`); missing `>4;2m` modifyOtherKeys.
   input.go:51,192-215 vs mod_0465 `cut()`.
5. **visibly wrong** — ctrl+d exits on first press (real: double-press
   gate, "Press {key} again to exit" with resolved key name).
   term.go:1306 vs mod_0263 `It`+`jz`, mod_1470 `kC`.
6. **visibly wrong** — permission dialog option labels/persistence differ
   from 294's ("Yes, and don't ask again for X in dir" vs ours "always
   allow X from this project"); real's options are editable input rows;
   our Tab-amend doesn't exist in real (tab=nextField).
   dialogs.go:184-336 vs mod_1600:1172100+.
7. **visibly wrong** — ctrl+y is a nonexistent `chat:yank` action that
   pops the draft stash; real ctrl+y = defaultToNewerModel (kill-ring
   yank lives in the editor layer and real ctrl+s pops the stash when
   input is empty). keybindings.go:97,133 vs mod_0263 h1 +
   mod_1600:826000.
8. **visibly wrong** — vim: no VISUAL-LINE (`V`), no i/a text objects,
   visual mode lacks j/k/G/gg/finds/counts/r/>/</J/p; `.` replays one
   key not the recorded change. term.go:632-976,1864 vs mod_0795.
9. **visibly wrong** — result fold shows 5 raw lines; real shows 3
   wrapped lines + grace case; "(ctrl+o to expand)" hint not
   binding-resolved. repl.go:269-279 vs mod_0397.
10. **visibly wrong** — paste placeholder `+N lines` off-by-one
    (real counts newlines) and threshold 200ch vs real 800ch-or->2-lines;
    no dragged-file→image handling. term.go:301 vs mod_0304 `zee`,
    mod_1600:822387.
11. **divergent** — history search: ours enter→accept-into-buffer /
    esc→cancel; real enter→**execute**, esc/tab→accept, ctrl+r cycles
    next match (ours ctrl+r in-search is a no-op redraw), ctrl+s scope.
    term.go:1530-1560 vs mod_0263 HistorySearch ctx.
12. **divergent** — no terminal capability probing (DECRPM/`CSI ?u`/
    XTVERSION/`Gi=`/cell-px/OSC11); kitty detection by env substring;
    no `?2026` sync output; no mouse tiering. input.go vs
    mod_0463/0465/0452.
