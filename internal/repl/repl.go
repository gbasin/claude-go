package repl

import (
	"bufio"
	"claude-go/internal/config"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"claude-go/internal/agent"
	"claude-go/internal/api"
	"claude-go/internal/hooks"
	"claude-go/internal/perms"
	"claude-go/internal/tools"

	"golang.org/x/term"
)

var (
	reset  = "\x1b[0m"
	dim    = "\x1b[2m"
	bold   = "\x1b[1m"
	blue   = "\x1b[34m"
	cyan   = "\x1b[36m"
	green  = "\x1b[32m"
	yellow = "\x1b[33m"
	red    = "\x1b[31m"
)

// Store abstracts transcript logging.
type Store interface {
	LogTitle(title string) error
	Log(m api.Message) error
	LogRecord(ev map[string]any) error
}

// Run drives the interactive session.
func Run(l *agent.Loop) {
	RunWithStore(l, nil)
}

// RunWithStore is Run with session transcript logging.
func RunWithStore(l *agent.Loop, store Store) {
	applyTheme(themeSetting(l.CWD), l.CWD)
	currentStore = store
	l.Entrypoint = "cli"
	ed := newEditor()
	// viewMode: default|verbose|focus — real's startup transcript mode
	switch strings.ToLower(readUserSettingStr("viewMode")) {
	case "focus":
		focusMode.Store(true)
	case "verbose":
		briefMode.Store(false)
		verboseMode.Store(true)
	}
	ed.voiceBase = l.Client.BaseURL
	ed.voiceCreds = l.Client.Creds
	ed.voiceCWD = l.ToolCtx.CWD
	ed.vim = vimEnabled()
	if ed.vim {
		ed.insertRemaps = vimInsertRemaps()
	}
	ed.completer = slashCompleter
	ed.acDesc = func(name string) string {
		if e2, ok := helpTable[name]; ok {
			return e2.desc
		}
		if c2, ok := customCommands[name]; ok {
			return c2.Description
		}
		return ""
	}
	loadCustomCommands(l.ToolCtx.CWD)
	ed.cycleMode = func() {
		order := []string{"default", "acceptEdits", "plan", "auto"}
		idx := 0
		for i, m := range order {
			if l.PermMode == m {
				idx = i
				break
			}
		}
		l.PermMode = order[(idx+1)%len(order)]
		compInline(fmt.Sprintf("\r\n%s⏵⏵ %s mode%s\r\n", yellow, l.PermMode, reset))
	}
	var wantQuit atomic.Bool
	pr := &promptUI{}
	compInit()
	defer compClose()
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		runPipe(l)
		return
	}
	ks, err := startKeys()
	if err == nil {
		// MCP elicitation: a server asks the user mid-flow — real shows a
		// form; we prompt inline + fire Elicitation/ElicitationResult
		tools.SetElicitHandler(func(req *tools.ElicitRequest) tools.ElicitAnswer {
			return elicitDialog(l, ks, req)
		})
		// TeammateIdle: a registered agent went idle/completed
		tools.OnAgentIdle = func(a *tools.AgentRecord) {
			hooks.Run(l.Hooks, hooks.TeammateIdle, map[string]any{
				"session_id": l.Client.SessionID, "cwd": l.ToolCtx.CWD,
				"hook_event_name": "TeammateIdle", "permission_mode": l.PermMode,
				"transcript_path": l.Transcript,
				"teammate_name":   a.Name, "teammate_id": a.AgentID,
			}, l.ToolCtx.CWD)
		}
	}
	if err == nil {
		ed.keys = ks
		ed.registerDefaultActions()
		ed.actions[2] = func(e *editor) *readResult { // ctrl+shift+b (app:toggleBrief)
			toggleBrief()
			return nil
		}
		ed.runningAgents = func() int { return runningAgents(l) }
		ed.onFastMode = func() {
			l.SetFastMode(!l.FastMode)
		}
		ed.onKillAgents = func() {
			compInline("\r\x1b[2K  killing background tasks…\r\n")
			n := l.ToolCtx.BgTasks.KillAll()
			compInline(fmt.Sprintf("  %d background tasks stopped\r\n", n))
		}
		ed.onToggleExpand = func() {
			// ctrl+o: full transcript view; second ctrl+o (or esc/q) returns
			if ed.zoneEdit {
				// the box lives in the zone — hand the screen to the
				// transcript dump; compResume reinstalls box+status
				compSuspend()
				showTranscript(l, ks)
				compResume()
				return
			}
			showTranscript(l, ks)
			os.Stdout.WriteString(boxRule() + "\n")
			ed.redraw()
		}
		ed.onToggleTodos = func() {
			if s := tools.TodosText(); s != "" {
				compInline("\r\x1b[2K" + dim + s + reset + "\r\n")
			} else {
				compInline("\r\x1b[2K  (no todos)\r\n")
			}
		}
	}
	// EndConversation mid-turn → flag normal shutdown (raw-mode restore,
	// unregister, MCP close, SessionEnd all run via the quit path).
	endSession := func() { wantQuit.Store(true) }
	_ = endSession
	if l.SessionEndReason == "" {
		l.SessionEndReason = "other"
	}
	l.ToolCtx.SetPermMode = func(mode string) {
		l.PermMode = mode
		if currentStore != nil {
			currentStore.LogRecord(map[string]any{
				"type": "system", "subtype": "permission-mode", "mode": mode,
			})
		}
	}
	l.ToolCtx.OnCwdChanged = func(nc string) {
		hooks.Run(l.Hooks, hooks.CwdChanged, map[string]any{
			"session_id": l.Client.SessionID, "old_cwd": l.ToolCtx.CWD, "new_cwd": nc,
			"transcript_path": l.Transcript, "hook_event_name": "CwdChanged",
			"permission_mode": l.PermMode,
		}, nc)
	}
	l.ToolCtx.OnFileChanged = func(p, kind string) {
		hooks.Run(l.Hooks, hooks.FileChanged, map[string]any{
			"session_id": l.Client.SessionID, "file_path": p, "event": kind,
			"transcript_path": l.Transcript, "hook_event_name": "FileChanged",
			"permission_mode": l.PermMode,
		}, l.ToolCtx.CWD)
	}
	l.ToolCtx.EndSession = endSession
	l.ToolCtx.PlanApprove = func() bool {
		return pr.ask(l, ks, "ExitPlanMode", "Approve the plan above?")
	}
	l.ToolCtx.AskStructured = func(header, question string, options []tools.AskOption, multi bool) ([]string, error) {
		return pr.askStructured(ks, header, question, options, multi)
	}
	l.ToolCtx.AskStructuredMulti = func(qs []tools.AskQuestion) (map[string][]string, error) {
		return pr.askStructuredMulti(ks, qs)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "repl: raw mode:", err)
		return
	}
	defer ks.stop()
	var printedDot bool
	var thinkingSeen bool
	md := newMDRenderer()
	if mw, _ := userSetting("maxProseWidth").(float64); mw > 0 {
		md.proseW = int(mw) // real's prose cap (default: terminal width)
	}
	md.out = &turnWriter{}

	// MessageDisplay hook lane (real's jlo transform queue): when
	// MessageDisplay hooks are configured, stream deltas through the
	// batching transform — displayContent rewrites what's rendered
	// without touching the stored message.
	var msgDisp *msgDisplay
	if len(l.Hooks[string(hooks.MessageDisplay)]) > 0 {
		msgDisp = newMsgDisplay(
			func(p map[string]any) hooks.Result {
				return l.RunHookEvent(hooks.MessageDisplay, p)
			},
			func(t string) { md.Write(t) },
		)
	}

	l.ToolCtx.Permission = func(name string, input json.RawMessage, summary string) bool {
		if name == "Bash" && tools.SandboxAutoAllow(l.ToolCtx.CWD) {
			return true // sandboxed bash auto-allowed per settings
		}
		switch perms.Check(perms.Mode(l.PermMode), &perms.Rules{
			Allow: l.PermRules["allow"], Deny: l.PermRules["deny"], Ask: l.PermRules["ask"],
		}, name, input, l.ToolCtx.CWD) {
		case perms.Allow:
			return true
		case perms.Deny:
			dprintf("  %sdenied by permission rules%s\n", dim, reset)
			return false
		default:
			reason, cfg := askReason(l, name, input)
			ok, feedback := pr.askEx(l, ks, name, summary, reason, cfg, input)
			if !ok && feedback != "" {
				queuePermFeedback(feedback)
			}
			return ok
		}
	}
	spinRef := &spinner{}
	l.Events.OnTextDelta = func(t string) {
		spinRef.stop()
		if !printedDot {
			// tracked write — a raw print here left the hardware and
			// stream cursors disagreeing, so the first md.Write chunk
			// re-positioned over the ⏺ (verified live at 60 cols)
			streamFresh("\r\x1b[K⏺ ")
			printedDot = true
		}
		if msgDisp != nil {
			msgDisp.Delta(t)
		} else {
			md.Write(t)
		}
	}
	l.Events.OnThinking = func(t string) {
		if thinkingHidden.Load() {
			return // meta+t collapsed (real hides thinking by default display)
		}
		if !thinkingSeen {
			streamPrint(dim + "✻ Thinking…" + reset)
			thinkingSeen = true
		}
		streamWrite(dim + t + reset)
	}
	l.Events.OnToolUse = func(name string, input json.RawMessage) {
		// real's spinner keeps animating through tool execution — the
		// tool card streams above the pinned footer row; stopping here
		// killed the "(running X hook)" status AND esc-to-interrupt for
		// the entire tool phase
		if focusMode.Load() {
			return // focus view hides tool lines (real's viewMode:"focus")
		}
		// the next text block is a new message — it gets its own ⏺
		if msgDisp != nil {
			msgDisp.Finalize()
		}
		printedDot = false
		thinkingSeen = false
		streamPrint(fmt.Sprintf("%s⏺%s %s%s%s", green, reset, bold+name+reset, summarize(name, input), ""))
	}
	l.Events.OnToolResult = func(name string, r tools.Result) {
		if focusMode.Load() {
			return
		}
		lines := strings.Split(strings.TrimRight(r.Content, "\n"), "\n")
		max := 5
		if briefMode.Load() {
			max = 1 // brief mode: tool previews collapse to one line
		}
		for i, ln := range lines {
			if i >= max {
				streamPrint(fmt.Sprintf("  %s⎿%s  … +%d lines (ctrl+o to expand)", dim, reset, len(lines)-max))
				break
			}
			streamPrint(fmt.Sprintf("  %s⎿%s  %s", dim, reset, trunc(ln, 113)))
		}
	}
	historySessionID = l.Client.SessionID
	l.Events.OnUsage = func(u api.Usage) {
		spinTokens.Store(int64(u.OutputTokens)) // spinner's ↓ token counter
		if antTrace.Load() {
			dprintf("%s  trace %s in=%d out=%d cache-r=%d cache-w=%d%s\n", dim,
				l.Model, u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens, reset)
		}
	}

	// real 2.1.291 banner: compact 3-line mascot — "Model · Plan" on
	// line 2 (effort moved to the ◐ pill above the composer)
	plan := "API Usage Billing"
	if l.Client != nil && l.Client.Creds != nil && l.Client.Creds.SubscriptionType != "" {
		switch l.Client.Creds.SubscriptionType {
		case "max":
			plan = "Claude Max"
		case "pro":
			plan = "Claude Pro"
		case "team":
			plan = "Claude Team"
		case "enterprise":
			plan = "Claude Enterprise"
		}
	}
	eff := l.Effort
	if eff == "" {
		eff = "medium"
	}
	curEffort.Store(eff)
	// real runs the session UI on the alternate screen with mouse
	// tracking + a ✳ window title (entered post-trust/onboarding)
	if os.Getenv("TMUX") != "" {
		if out, err := exec.Command("tmux", "show", "-g", "focus-events").Output(); err == nil &&
			!strings.Contains(string(out), " on") {
			fmt.Fprintf(os.Stderr, "tmux focus-events off · add 'set -g focus-events on' to ~/.tmux.conf\n")
		}
	}
	enterAltScreen()
	defer exitAltScreen()
	compactBanner(api.Version, agent.ModelDisplay(l.Model), "", plan, l.CWD, l.Client.SessionID)
	printAutoModeNotice(l.PermMode)

	// transcript append cursor — session-scoped so each turn logs only
	// new messages; starts past any resumed/loaded history, which is
	// already in the transcript file
	loggedTo := len(l.Messages)

	for {
		// real's weekly-limit banner: "You've used N% of your weekly
		// limit · resets <time>" — once per session at ≥80%
		if rl := l.Client.LastRateLimit(); rl != nil && !rateBannerShown.Load() {
			util := rl.OverageMonthlyUtilization
			if rl.SlowBudgetUtilization > util {
				util = rl.SlowBudgetUtilization
			}
			if util >= 0.8 {
				rateBannerShown.Store(true)
				resets := rl.ResetsAt
				if resets.IsZero() {
					resets = rl.OverageResetsAt
				}
				dprintf("  %sYou've used %d%% of your weekly limit · resets %s%s\n",
					"\x1b[38;5;220m", int(util*100), resets.Local().Format("3:04pm"), reset)
			}
		}
		// drain background-task notifications above the status line —
		// real shows "⎿ Task b1 finished: <cmd>" between turns
		if l.ToolCtx.BgTasks != nil {
			for _, n := range l.ToolCtx.BgTasks.DrainNotifications() {
				// real renders the completion as a ⏺ line
				dprintf("%s⏺%s %s\n", green, reset, n)
			}
		}
		printStatus(l, ed)
		tools.UserActive.Store(true)
		res := ed.readLine(ks)
		if res.quit || wantQuit.Load() {
			if ed.voice != nil && ed.voice.ctl != nil {
				ed.voice.ctl.Cancel() // drop any live capture+stream on exit
			}
			l.SessionEndReason = "prompt_input_exit"
			dprintln()
			fireSessionEndHook(l)
			return
		}
		if res.rewind {
			runRewindSelector(l, ks, ed)
			continue
		}
		if res.modelPicker {
			handleSlash(l, "/model", ed, ks)
			continue
		}
		if res.agentsPane {
			showAgentsPane(l, ks)
			continue
		}
		if !res.ok {
			continue
		}
		line := strings.TrimSpace(res.line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			quickMemory(l, strings.TrimSpace(line[1:]))
			continue
		}
		if strings.HasPrefix(line, "!") {
			runBang(l, line[1:])
			continue
		}
		if strings.HasPrefix(line, "/") {
			if expanded := expandCustomSlash(l, line); expanded != "" {
				line = expanded
			} else {
				if handleSlashLocal(l, line, ed, ks) {
					continue
				}
				if handleSlash(l, line, ed, ks) {
					l.SessionEndReason = "prompt_input_exit"
					return
				}
				continue
			}
		}

		// dispatchedByKeyword (mod_0386.js): "ultraplan" anywhere in the
		// prompt routes it to the cloud ultraplan flow.
		if strings.Contains(line, "ultraplan") {
			ultraplanCmd(l, line)
			continue
		}

		var queued []string
		var draftBack []rune
		if store != nil {
			// mid-turn compaction replaces history inside Ask — persist
			// the not-yet-logged tail of the replaced span so the last
			// turn's messages land before the boundary record
			l.Events.OnCompactSpan = func(span []api.Message) {
				if loggedTo > len(span) {
					return // whole span already logged
				}
				for _, m := range span[loggedTo:] {
					currentStore.Log(m)
				}
			}
		}
		// one-time "Fast mode is now available · /fast to turn on"
		// announcement (captured in real's 2.1.292 startup)
		if m, _ := config.ReadClaudeJSON(); m["fastModeNoticeShown"] != true {
			m["fastModeNoticeShown"] = true
			if b, e := json.MarshalIndent(m, "", "  "); e == nil {
				os.WriteFile(filepath.Join(config.Home(), ".claude.json"), b, 0o644)
			}
			streamPrint(dim + "Fast mode is now available · /fast to turn on" + reset)
		}
		// real announces auto-compact inline when it fires
		l.Events.OnAutoCompact = func(tokens int) {
			streamPrint(dim + "Compacted · auto-compact summarized history" + reset)
		}
		runTurn := func(line string) {
			tools.UserActive.Store(false)
			defer tools.UserActive.Store(true)
			if msgDisp != nil {
				msgDisp.NewTurn()
				defer msgDisp.Finalize()
			}
			printedDot = false
			thinkingSeen = false
			spin := newSpinner()
			spin.agents = func() int { return runningAgents(l) }
			// real shows a rotating ⎿ Tip line under the spinner
			// periodically (spinnerTipsOverride-able)
			if tip := spinnerTip(); tip != "" && tipNth.Add(1)%3 == 0 {
				streamPrint(dim + "  ⎿  " + tip + reset)
			}
			*spinRef = *spin
			tools.SetPeerSessionBusy(true) // held-queue peer messages mid-turn
			ctx, cancel := context.WithCancel(context.Background())
			start := time.Now()
			respDone := make(chan struct{})
			busyExited := make(chan struct{})
			var queueBuf []rune
			var sawInput atomic.Bool // any keystroke during the turn = user present
			go func() {
				defer close(busyExited)
				// while the agent runs: esc interrupts, other keys queue a message
				for {
					select {
					case <-respDone:
						return
					case k, ok := <-ks.ch:
						if !ok {
							return
						}
						sawInput.Store(true)
						if ks.forward(k) {
							continue // modal dialog owns the key
						}
						if suspendSupported && isSuspendKey(k) {
							// ctrl+z mid-turn: stop the process; compResume
							// repaints spinner/draft on fg
							suspendToShell(ks, nil)
							continue
						}
						if (k.b == 0x1b || k.b == 3) && k.seq == "" {
							cancel()
							// popQueuedIntoDraft: unfinished queued text returns to the editor
							if len(queueBuf) > 0 {
								draftBack = append(draftBack, queueBuf...)
							}
						} else {
							act := ed.busyAction(k)
							if act != "" && act != "chat:sendNow" {
								// a bound command (meta+o, ctrl+x ctrl+k) pressed
								// mid-turn — defer the ACTION (key replay would
								// lose chord context: the ctrl+x prefix is gone
								// by the time ctrl+k resolves)
								ed.deferredActions = append(ed.deferredActions, act)
							} else if k.b == '\r' || k.b == '\n' || act == "chat:sendNow" {
								if len(queueBuf) > 0 {
									if act == "chat:sendNow" {
										// sendNow jumps ahead of queued messages
										queued = append([]string{string(queueBuf)}, queued...)
									} else {
										queued = append(queued, string(queueBuf))
									}
									queuedPending.Add(1)
									printStatus(l, ed) // refresh "N queued" badge
									queueBuf = nil
									compQueueDraft("")
									// tracked write — a raw stderr print lands
									// at the hardware cursor but leaves
									// compStreamCol stale, so the next stream
									// write repositions and overwrites it
									streamPrint("  " + dim + "❯ " + trunc(string(queueBufLast(queued)), 90) + reset)
								}
							} else if k.r != 0 {
								queueBuf = append(queueBuf, k.r)
								compQueueDraft(string(queueBuf))
							} else if k.b >= 32 && k.b < 127 {
								// printable ASCII arrives as b-only (r unset)
								queueBuf = append(queueBuf, rune(k.b))
								compQueueDraft(string(queueBuf))
							} else if k.b == 0x7f && len(queueBuf) > 0 {
								queueBuf = queueBuf[:len(queueBuf)-1]
								compQueueDraft(string(queueBuf))
							}
						}
					}
				}
			}()
			compStreamBegin()
			_, err := l.Ask(ctx, expandMentions(l, line))
			// capture cancellation BEFORE cleanup cancel() — after that
			// call ctx.Err() is always Canceled and would mask real errors
			wasCancelled := ctx.Err() == context.Canceled
			compStreamEnd()
			spin.stop()
			tools.SetPeerSessionBusy(false)
			tools.FlushHeldPeerMessages() // release held peer msgs at the boundary
			compClearFooter()
			if !compActive.Load() {
				// zone mode leaves the cursor at the response row — the raw
				// erase would wipe it (verified: streamed text vanished)
				fmt.Fprint(os.Stderr, "\r\x1b[K")
			}
			fireStopHook(l)
			if store != nil {
				// log only NEW messages per turn (was O(n²) re-log + a git
				// fork per record). currentStore, not the param — /resume
				// and /fork swap it mid-session.
				if kept := l.TakeCompactPending(); kept >= 0 && len(l.Messages) > 0 {
					// compaction rewrote history: Messages[0] is the new
					// isCompactSummary record, indices 1..kept are the
					// retained tail already in the transcript
					currentStore.Log(l.Messages[0])
					loggedTo = 1 + kept
				} else if loggedTo > len(l.Messages) {
					loggedTo = 0 // /clear wiped history
				}
				if v := logResync.Load(); v >= 0 {
					loggedTo = int(v) // /resume|/fork swapped store+messages
					logResync.Store(-1)
				}
				for _, m := range l.Messages[loggedTo:] {
					currentStore.Log(m)
				}
				loggedTo = len(l.Messages)
			}
			md.Close()
			close(respDone)
			cancel()
			<-busyExited // join — a late key append must not race the drain
			// text typed mid-turn but never submitted returns to the editor
			if len(queueBuf) > 0 {
				draftBack = append(draftBack, queueBuf...)
				compQueueDraft("")
			}
			// drain buffered interrupt keys from the turn — a stale Esc
			// would otherwise fire cancel() instantly on the NEXT turn's
			// loop. Printable keys stay queued for the editor.
			for {
				select {
				case k, ok := <-ks.ch:
					if !ok {
						goto drained
					}
					isBareEsc := k.b == 0x1b && k.seq == ""
					isCtrlC := k.b == 3
					if !isBareEsc && !isCtrlC {
						// replay channel — re-pushing to ks.ch reorders
						// behind keys still buffered. Arrow keys carry
						// b==0x1b + a seq — they're NOT stale interrupts.
						select {
						case ks.replay <- k:
						default:
						}
						goto drained
					}
				default:
					goto drained
				}
			}
		drained:
			dprintln()
			// ping the notification channel when the turn ended with no
			// user keystrokes — the session was unattended (UserActive is
			// false for the whole turn, so key activity is the real signal)
			if !sawInput.Load() {
				notifyAttention(l.ToolCtx.CWD)
			}
			// context-low warning like real CC
			tok := l.UsageTotal.InputTokens + l.UsageTotal.CacheReadInputTokens + l.UsageTotal.CacheCreationInputTokens
			if tok > l.ContextWarnThreshold() {
				suffix := ""
				if os.Getenv("DISABLE_COMPACT") != "" {
					suffix = " · /clear to continue"
				}
				dprintf("%s  Context limit reached · /compact or /clear to continue%s%s\n", red, suffix, reset)
			}
			if err != nil {
				if wasCancelled {
					dprintf("%s⏺ Interrupted%s\n", yellow, reset)
				} else {
					dprintf("%sError: %v%s\n", red, err, reset)
				}
			} else {
				// real always prints "✻ Churned for 0s · done 9:01 AM"
				// (past-tense verb + elapsed + clock) — even sub-second
				v, _ := spinVerb.v.Load().(string)
				if v == "" {
					v = "Working"
				}
				// real appends "· N shell still running" when bg tasks live on
				tail := ""
				if n := runningAgents(l); n > 0 {
					tail = fmt.Sprintf(" · %d shell still running", n)
				}
				// showTurnDuration (default on) gates the footer line
				if config.UserSetting("showTurnDuration") != false {
					dprintf("%s✻ %s for %s · done %s%s%s\n\n", dim, pastTense(v),
						fmtDur(time.Since(start)), clockNow(), tail, reset)
				}
			}
		}
		for {
			runTurn(line)
			// drain session-injected prompts (cron, wakeup, monitor events)
			for {
				select {
				case p := <-tools.SessionQueue:
					queued = append(queued, p)
					queuedPending.Add(1)
					printStatus(l, ed)
				default:
					goto drained
				}
			}
		drained:
			for _, nm := range tools.DrainMailbox(l.Client.SessionID) {
				dprintf("%s  [message]%s %s\n", cyan, reset, nm)
				l.Messages = append(l.Messages, api.TextMessage("user",
					"<system-reminder>Message received while you were working:</system-reminder>\n"+nm))
			}
			tools.FireIdleNotifications(l.Client.SessionID)
			if len(draftBack) > 0 {
				ed.buf = draftBack
				ed.pos = len(ed.buf)
				draftBack = nil
			}
		nextQueued:
			if len(queued) == 0 {
				break
			}
			line = queued[0]
			queued = queued[1:]
			queuedPending.Add(-1)
			printStatus(l, ed)
			// queued lines get the same local dispatch as typed ones —
			// a "/clear" submitted mid-turn is a command, not a prompt
			switch {
			case strings.HasPrefix(line, "#"):
				quickMemory(l, strings.TrimSpace(line[1:]))
				goto nextQueued
			case strings.HasPrefix(line, "!"):
				runBang(l, line[1:])
				goto nextQueued
			case strings.HasPrefix(line, "/"):
				if expanded := expandCustomSlash(l, line); expanded != "" {
					line = expanded
				} else {
					if handleSlashLocal(l, line, ed, ks) {
						goto nextQueued
					}
					if handleSlash(l, line, ed, ks) {
						l.SessionEndReason = "prompt_input_exit"
						return
					}
					goto nextQueued
				}
			}
		}
		// EndConversation (or anything that set wantQuit mid-turn) takes
		// effect at turn end — gating it on the next submitted line would
		// wait for input that never comes.
		if wantQuit.Load() {
			l.SessionEndReason = "prompt_input_exit"
			dprintln()
			fireSessionEndHook(l)
			return
		}
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// cut on a rune boundary — a raw byte slice can split multi-byte
	// UTF-8 and emits U+FFFD garbage at narrow widths
	if r := []rune(s); len(r) <= n {
		return s
	} else {
		return string(r[:n]) + "…"
	}
}

func summarize(name string, input json.RawMessage) string {
	var m map[string]any
	json.Unmarshal(input, &m)
	get := func(k string) string {
		if v, ok := m[k]; ok {
			return fmt.Sprint(v)
		}
		return ""
	}
	var s string
	switch name {
	case "Bash", "Monitor":
		s = get("command")
	case "Read":
		s = get("file_path")
	case "Write":
		s = get("file_path")
	case "Edit":
		s = get("file_path")
	case "NotebookEdit":
		s = get("notebook_path")
	case "WebFetch":
		s = get("url")
	case "WebSearch":
		s = get("query")
	case "Agent":
		s = get("description")
	case "Skill":
		s = get("skill")
	case "EnterWorktree", "ExitWorktree":
		s = get("name")
	default:
		s = string(input)
	}
	return "(" + trunc(s, 90) + ")"
}

// promptUI renders the y/n/always permission prompt.
type promptUI struct {
	always     map[string]bool
	customLine *strings.Builder
}

// ask is the simple yes/no form (plan approval, legacy call sites); the
// full permission dialog with reason line, auto-deny and deny-with-feedback
// is askEx in dialogs.go.
func (p *promptUI) ask(l *agent.Loop, ks *keySource, name, summary string) bool {
	ok, _ := p.askEx(l, ks, name, summary, "", "", nil)
	return ok
}

// spinner: real CC's `<glyph> <Verb>… (Ns · ↓ Nk tokens)` — bounce frames
// ·✢✳✶✻✽ at 150ms, claude-orange verb, whimsical gerund pool.
type spinner struct {
	done   chan struct{}
	verb   string
	agents func() int // running bg-agent count for the busy footer
}

// spinVerb exposes the active verb for the past-tense footer.
var verboseMode atomic.Bool
var tipNth atomic.Int64

var spinVerb struct{ v atomic.Value }

// pastTense converts a spinner gerund ("Churning") to past ("Churned") —
// mirrors real's completion verb forms loosely.
func pastTense(g string) string {
	w := strings.TrimSuffix(g, "ing")
	if w == "" || w == g {
		return "Worked"
	}
	// double consonant where needed ("Skedaddl"+"ed" is fine; "Spin"+"ned")
	if strings.HasSuffix(g, "ning") || strings.HasSuffix(g, "ming") {
		return w + "ned"
	}
	if strings.HasSuffix(w, "'") { // "Beboppin'" → "Bebopped"
		return strings.TrimSuffix(w, "'") + "ed"
	}
	return w + "ed"
}

// bounce order up-then-down, like real's [...at, ...at.toReversed()]
var spinnerFrames = []string{"·", "✢", "✳", "✶", "✻", "✽", "✻", "✶", "✳", "✢"}

var spinnerVerbs = []string{
	"Accomplishing",
	"Actioning",
	"Actualizing",
	"Architecting",
	"Baking",
	"Beaming",
	"Beboppin'",
	"Befuddling",
	"Billowing",
	"Blanching",
	"Bloviating",
	"Boogieing",
	"Boondoggling",
	"Booping",
	"Bootstrapping",
	"Brewing",
	"Bunning",
	"Burrowing",
	"Calculating",
	"Canoodling",
	"Caramelizing",
	"Cascading",
	"Catapulting",
	"Cerebrating",
	"Channeling",
	"Choreographing",
	"Churning",
	"Clauding",
	"Coalescing",
	"Cogitating",
	"Combobulating",
	"Composing",
	"Computing",
	"Concocting",
	"Considering",
	"Contemplating",
	"Cooking",
	"Crafting",
	"Creating",
	"Crunching",
	"Crystallizing",
	"Cultivating",
	"Deciphering",
	"Deliberating",
	"Determining",
	"Dilly-dallying",
	"Discombobulating",
	"Doing",
	"Doodling",
	"Drizzling",
	"Ebbing",
	"Effecting",
	"Elucidating",
	"Embellishing",
	"Enchanting",
	"Envisioning",
	"Fermenting",
	"Fiddle-faddling",
	"Finagling",
	"Flambéing",
	"Flibbertigibbeting",
	"Flowing",
	"Flummoxing",
	"Fluttering",
	"Forging",
	"Forming",
	"Frolicking",
	"Frosting",
	"Gallivanting",
	"Galloping",
	"Garnishing",
	"Generating",
	"Gesticulating",
	"Germinating",
	"Gitifying",
	"Grooving",
	"Gusting",
	"Harmonizing",
	"Hashing",
	"Hatching",
	"Herding",
	"Honking",
	"Hullaballooing",
	"Hyperspacing",
	"Ideating",
	"Imagining",
	"Improvising",
	"Incubating",
	"Inferring",
	"Infusing",
	"Ionizing",
	"Jitterbugging",
	"Julienning",
	"Kerfuffling",
	"Kneading",
	"Leavening",
	"Levitating",
	"Lollygagging",
	"Manifesting",
	"Marinating",
	"Meandering",
	"Metamorphosing",
	"Misting",
	"Moonwalking",
	"Moseying",
	"Mulling",
	"Mustering",
	"Musing",
	"Nebulizing",
	"Nesting",
	"Newspapering",
	"Noodling",
	"Nucleating",
	"Onioning",
	"Orbiting",
	"Orchestrating",
	"Osmosing",
	"Perambulating",
	"Percolating",
	"Perusing",
	"Philosophizing",
	"Photosynthesizing",
	"Polishing",
	"Pollinating",
	"Pondering",
	"Pontificating",
	"Pouncing",
	"Precipitating",
	"Prestidigitating",
	"Processing",
	"Proofing",
	"Propagating",
	"Puttering",
	"Puzzling",
	"Quantumizing",
	"Razzle-dazzling",
	"Razzmatazzing",
	"Recombobulating",
	"Reticulating",
	"Roosting",
	"Ruminating",
	"Sautéing",
	"Scampering",
	"Schlepping",
	"Scurrying",
	"Seasoning",
	"Shenaniganing",
	"Shimmying",
	"Simmering",
	"Skedaddling",
	"Sketching",
	"Slithering",
	"Smooshing",
	"Sock-hopping",
	"Spelunking",
	"Spinning",
	"Sprouting",
	"Stewing",
	"Sublimating",
	"Swirling",
	"Swooping",
	"Symbioting",
	"Synthesizing",
	"Tempering",
	"Thinking",
	"Thundering",
	"Tinkering",
	"Tomfoolering",
	"Topsy-turvying",
	"Transfiguring",
	"Transmogrifying",
	"Transmuting",
	"Twisting",
	"Undulating",
	"Unfurling",
	"Unraveling",
	"Vibing",
	"Waddling",
	"Wandering",
	"Warping",
	"Whatchamacalliting",
	"Whirlpooling",
	"Whirring",
	"Whisking",
	"Wibbling",
	"Working",
	"Wrangling",
	"Zesting",
	"Zigzagging",
}

// claudeOrange is the brand accent (real claude token = rgb(215,119,87)).
const claudeOrange = "[38;2;215;119;87m"

// claudeShimmer is real's claudeShimmer token = rgb(245,149,117).
const claudeShimmer = "[38;2;245;149;117m"

// shimmerVerb renders the verb dim-orange with a bright band sweeping
// across it — real's Mst() splits text at glimmerIndex into
// before(dim)/shimmer(bright)/after(dim); band width 5 wraps through
// len+5 positions.
func shimmerVerb(verb string, tick int) string {
	r := []rune(verb)
	const band = 5
	pos := tick % (len(r) + band)
	var b strings.Builder
	b.WriteString(claudeOrange)
	for j, ch := range r {
		glow := j >= pos-band && j < pos
		if glow {
			b.WriteString(claudeShimmer)
		}
		b.WriteRune(ch)
		if glow {
			b.WriteString(claudeOrange)
		}
	}
	b.WriteString(reset)
	return b.String()
}

// settingsVerbs applies settings.spinnerVerbs {mode:"replace"|"append",verbs}
func settingsVerbs() []string {
	for _, p := range []string{".claude/settings.json",
		filepath.Join(config.ClaudeDir(), "settings.json")} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var st struct {
			SpinnerVerbs struct {
				Mode  string   `json:"mode"`
				Verbs []string `json:"verbs"`
			} `json:"spinnerVerbs"`
		}
		if json.Unmarshal(b, &st) != nil || len(st.SpinnerVerbs.Verbs) == 0 {
			continue
		}
		if st.SpinnerVerbs.Mode == "replace" {
			return st.SpinnerVerbs.Verbs
		}
		return append(append([]string{}, spinnerVerbs...), st.SpinnerVerbs.Verbs...)
	}
	return spinnerVerbs
}

func newSpinner() *spinner {
	// /compact overrides the verb with the literal "Compacting conversation"
	if compactSpin.Load() {
		return &spinner{done: make(chan struct{}), verb: "Compacting conversation"}
	}
	verbs := settingsVerbs()
	s := &spinner{done: make(chan struct{}),
		verb: verbs[time.Now().UnixNano()%int64(len(verbs))]}
	spinVerb.v.Store(s.verb)
	spinTokens.Store(0)
	go s.run()
	return s
}

func fmtDur(d time.Duration) string {
	s := int(d.Seconds())
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	if s < 3600 {
		return fmt.Sprintf("%dm %ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh %dm %ds", s/3600, s/60%60, s%60)
}

func compactTokens(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}

var spinTokens atomic.Int64 // ↓ tokens counter fed by OnUsage

func (s *spinner) run() {
	i := 0
	start := time.Now()
	t := time.NewTicker(150 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			el := fmtDur(time.Since(start))
			toks := ""
			if n := spinTokens.Load(); n > 0 {
				toks = " · ↓ " + compactTokens(int(n)) + " tokens"
			}
			// "(running <hook> hook · Ns)" — real's hook-exec status
			hookTxt := ""
			if hn, el := agent.RunningHook(); hn != "" {
				// real: "(running X hook… 0/2 · Ns)" — done/total + ellipsis
				p := hooks.Progress.Load()
				tot := int32(p >> 32)
				done := int32(p & 0xffffffff)
				if tot > 1 {
					hookTxt = fmt.Sprintf(" · (running %s hook… %d/%d · %ds)", hn, done, tot, int(el.Seconds()))
				} else {
					hookTxt = fmt.Sprintf(" · (running %s hook · %ds)", hn, int(el.Seconds()))
				}
			}
			// real sweeps a bright band across the dim verb text —
			// glimmerIndex advances ~every 90ms, band wraps over len+pad
			agentsHint := "← for agents"
			if s.agents != nil {
				if n := s.agents(); n > 0 {
					agentsHint = fmt.Sprintf("← %d agents · ↑↓ to manage", n)
				}
			}
			txt := fmt.Sprintf("%s%s %s… %s(esc to interrupt · %s · %s%s)",
				claudeOrange, spinnerFrames[i%len(spinnerFrames)],
				shimmerVerb(s.verb, i), claudeOrange, agentsHint, el, toks+hookTxt)
			if compActive.Load() {
				compFooter(txt) // pinned bottom row — doesn't scroll
			} else {
				fmt.Fprintf(os.Stderr, "\r\x1b[K%s", txt)
			}
			i++
		}
	}
}

func (s *spinner) stop() {
	if s == nil || s.done == nil {
		return
	}
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

func (s *spinner) stopIf() {
	if s != nil && s.done != nil {
		s.stop()
	}
}

// expandMentions resolves @path mentions into file attachments appended to
// the prompt, matching CC's @-mention behavior.
func expandMentions(l *agent.Loop, line string) string {
	var attach strings.Builder
	for _, tok := range strings.Fields(line) {
		if !strings.HasPrefix(tok, "@") || len(tok) < 2 {
			continue
		}
		p := tok[1:]
		if !filepath.IsAbs(p) {
			p = filepath.Join(l.ToolCtx.CWD, p)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if len(b) > 100000 {
			b = []byte(truncRunes(string(b), 100000))
		}
		fmt.Fprintf(&attach, "\n\nContents of %s:\n%s", p, string(b))
	}
	if attach.Len() > 0 {
		return line + attach.String()
	}
	return line
}

// runBang executes !command directly through the Bash tool and shows output,
// matching CC's bash-mode prefix.
func runBang(l *agent.Loop, cmd string) {
	raw, _ := json.Marshal(map[string]any{"command": cmd, "description": "run in bash mode"})
	res := tools.Dispatch(l.ToolCtx, "Bash", raw)
	dprintln("  ⎿ ", strings.TrimSpace(res.Content))
	msg := "<bash-input>\n" + cmd + "\n</bash-input>\n\n<local-command-stdout>\n" + res.Content + "\n</local-command-stdout>"
	if respondToBash(l) {
		// respondToBashCommands (default true): Claude responds to !cmd —
		// the bash-input block becomes this turn's user message
		if _, err := l.Ask(context.Background(), msg); err != nil {
			dprintf("\n  Error: %v\n", err)
		}
		return
	}
	l.PushUser([]api.ContentBlock{{Type: "text", Text: msg}})
}

// respondToBash — settings.respondToBashCommands, real default true.
func respondToBash(l *agent.Loop) bool {
	if v, ok := userSetting("respondToBashCommands").(bool); ok {
		return v
	}
	return true
}

// runPipe handles non-TTY stdin: each line is a prompt (like `claude < in.txt`).
func runPipe(l *agent.Loop) {
	l.Events.OnTextDelta = func(t string) { fmt.Print(t) }
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// dispatchedByKeyword (mod_0386.js): "ultraplan" anywhere routes to
		// the cloud ultraplan flow — same as the interactive path.
		if strings.Contains(line, "ultraplan") {
			ultraplanCmd(l, line)
			continue
		}
		if _, err := l.Ask(context.Background(), expandMentions(l, line)); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
		}
		dprintln()
	}
}

var stopHookActive atomic.Bool

// lastAssistantText returns the most recent assistant text for the Stop payload.
func lastAssistantText(l *agent.Loop) string {
	for i := len(l.Messages) - 1; i >= 0; i-- {
		if l.Messages[i].Role != "assistant" {
			continue
		}
		var m struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(l.Messages[i].Content, &m) == nil {
			for _, blk := range m.Content {
				if blk.Type == "text" && blk.Text != "" {
					return blk.Text
				}
			}
		}
	}
	return ""
}

func fireStopHook(l *agent.Loop) {
	if l.Hooks == nil {
		return
	}
	res := hooks.Run(l.Hooks, hooks.Stop, map[string]any{
		"session_id":      l.Client.SessionID,
		"transcript_path": l.Transcript,
		"cwd":             l.ToolCtx.CWD,
		"hook_event_name": "Stop", "permission_mode": l.PermMode,
		"stop_hook_active":       stopHookActive.Load(),
		"last_assistant_message": lastAssistantText(l),
	}, l.ToolCtx.CWD)
	if res.SystemMessage != "" {
		dprintf("  %s%s%s\n", dim, res.SystemMessage, reset)
	}
	if res.Block && res.Reason != "" && !stopHookActive.Swap(true) {
		// continue:false / decision:block → the model sees the reason and
		// the turn continues (real preventContinuation). One-shot guard
		// prevents infinite stop-hook recursion.
		tools.QueuePrompt(res.Reason)
	}
}

func vimEnabled() bool {
	b, err := os.ReadFile(filepath.Join(config.Home(), ".claude.json"))
	if err != nil {
		return false
	}
	var st map[string]any
	if json.Unmarshal(b, &st) != nil {
		return false
	}
	return st["keybindingMode"] == "vim" || st["editorMode"] == "vim"
}

func slashCompleter(prefix string) []string {
	var out []string
	for c := range customCommands {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	for _, c := range slashCommands {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// sessionGitBranch shells out once per status draw — real shows the branch.
func sessionGitBranch(cwd string) string {
	out, err := exec.Command("git", "-C", cwd, "branch", "--show-current").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// curEffort tracks the session effort for the ◐ pill (set at REPL start,
// updated by /effort and the model picker).
var rateBannerShown atomic.Bool

var curEffort atomic.Value // stores string

// effortLabel is real's "◐ medium · /effort" right-aligned pill.
func effortLabel() string {
	e, _ := curEffort.Load().(string)
	if e == "" {
		e = "medium"
	}
	return "◐ " + e + " · /effort"
}

func printStatus(l *agent.Loop, ed *editor) {
	var status string
	if out := statusLineCmd(l); out != "" {
		status = out
	} else {
		// real's footer: cwd+branch left, mode pill + hints right
		w, _ := termWidth()
		if w <= 0 {
			w = 80
		}
		cwdDisp := l.ToolCtx.CWD
		if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(cwdDisp, home) {
			cwdDisp = "~" + cwdDisp[len(home):] // real abbreviates $HOME
		}
		left := "  " + cwdDisp
		if b := sessionGitBranch(l.ToolCtx.CWD); b != "" {
			left += "  " + b // real shows the branch as text
		}
		right := strings.TrimSpace(permModeBanner(l.PermMode))
		if n := runningAgents(l); n > 0 {
			// real: "← N agents" when background agents are running
			right = strings.Replace(right, "← for agents", fmt.Sprintf("← %d agents", n), 1)
			right = strings.Replace(right, "? for shortcuts", "? for shortcuts · ← "+fmt.Sprint(n)+" agents", 1)
			if !strings.Contains(right, " agents") {
				// bypass mode's right side has neither needle — append
				right = right + " · ← " + fmt.Sprint(n) + " agents"
			}
		}
		if right == "" {
			right = "? for shortcuts"
		}
		if hints := footerHints(l, ed); len(hints) > 0 {
			right = strings.Join(hints, " · ") + " · " + right
		}
		pad := w - visWidth(left) - visWidth(right)
		if pad < 1 {
			// narrow: shed hints before clamping mid-word
			if i := strings.Index(right, " (shift+tab"); i > 0 {
				right = right[:i] + "\x1b[39m"
			}
			if pad = w - visWidth(left) - visWidth(right); pad < 1 {
				left = "  " + filepath.Base(cwdDisp)
				pad = w - visWidth(left) - visWidth(right)
			}
		}
		if pad < 1 {
			pad = 1
		}
		status = dim + left + strings.Repeat(" ", pad) + reset + right
	}
	if compActive.Load() {
		compSetStatus(status) // pinned bottom row — real keeps it in the live region
	} else {
		ed.footer = status // rendered inside the box frame (below the rules)
		ed.redraw()
	}
}

var slashCommands = []string{
	"add-dir", "agents", "alias", "bug", "clear", "color", "compact",
	"config", "context", "cost", "diff", "doctor", "export", "exit",
	"feedback", "files", "help", "hooks", "ide", "import", "init",
	"keybindings", "login", "logout", "mcp", "memory", "model", "output-style",
	"permissions", "plugin", "privacy-settings", "quit", "radio", "release-notes",
	"resume", "review", "status", "statusline", "tasks", "terminal-setup",
	"theme", "todos", "usage", "voice",
}

func termWidth() (int, error) {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	return w, err
}

// askStructured renders a numbered options menu (AskUserQuestion tool).
func (p *promptUI) askStructured(ks *keySource, header, question string, options []tools.AskOption, multi bool) ([]string, error) {
	compSuspend() // modal menu — un-pin the live zone while it owns the screen
	defer compResume()
	dch := make(chan key, 16)
	ks.dialog.Store(&dch)
	defer ks.dialog.Store(nil)
	// real 2.1.292's AskUserQuestion (captured bytes):
	//   black-16 on cyan-153 " ☐ <header> " chip · blank · bold-231
	//   question · numbered options with gray-246 description rows ·
	//   builtin "N+1. Type something." · ╌ rule · "N+2. Chat about
	//   this" below the rule · "Enter to select · ↑/↓ to navigate ·
	//   Esc to cancel" footer
	cyan := "\x1b[38;5;153m"
	gray := "\x1b[38;5;246m"
	white := "\x1b[38;5;231m"
	if header == "" {
		header = map[bool]string{true: "Pick any", false: "Pick one"}[multi]
	}
	type row struct{ label, desc string }
	opts := make([]row, 0, len(options)+2)
	for _, o := range options {
		opts = append(opts, row{o.Label, o.Description})
	}
	nBase := len(opts)
	opts = append(opts, row{"Type something.", ""}, row{"Chat about this", ""})

	sel := 0
	toggled := map[int]bool{}
	var custom strings.Builder
	customMode := false
	drawnRows := 0
	width, _, _ := term.GetSize(int(os.Stdout.Fd()))
	if width <= 0 {
		width = 80
	}
	draw := func() {
		var sb strings.Builder
		if drawnRows > 0 {
			fmt.Fprintf(&sb, "\x1b[%dA", drawnRows)
		}
		drawnRows = 0
		put := func(s string) {
			sb.WriteString("\r\x1b[K" + s + "\r\n")
			drawnRows++
		}
		put("") // leading blank under the composer rule
		put(" \x1b[48;5;153m\x1b[38;5;16m ☐ " + header + " \x1b[49m")
		put("")
		put(" " + white + bold + question + reset + "\x1b[39m")
		put("")
		for i, o := range opts {
			if i == nBase+1 {
				// "Chat about this" sits below the box rule
				put(" " + gray + strings.Repeat("─", width) + "\x1b[39m")
			}
			num := fmt.Sprintf("%d.", i+1)
			if i == sel {
				put(fmt.Sprintf(" %s❯%s %s%s%s %s%s", cyan, "\x1b[39m", gray, num, "\x1b[39m", cyan, o.label+"\x1b[39m"))
			} else {
				check := ""
				if multi && i < nBase {
					check = "○ "
					if toggled[i] {
						check = "● "
					}
				}
				lbl := o.label
				if customMode && i == nBase {
					lbl = "Type something: " + custom.String()
				}
				if customMode && i == nBase+1 && sel == nBase+1 {
					lbl = "Chat about this: " + custom.String()
				}
				put(fmt.Sprintf("   %s%s%s %s%s", gray, num, "\x1b[39m", check, lbl))
			}
			if o.desc != "" {
				put("      " + gray + o.desc + "\x1b[39m")
			}
		}
		put("")
		put(" " + gray + "Enter to select · ↑/↓ to navigate · Esc to cancel" + "\x1b[39m")
		fmt.Print(sb.String())
	}
	draw()
	var picked []string
	for k := range dch {
		switch {
		case k.seq == "[A" && !customMode:
			if sel > 0 {
				sel--
				draw()
			}
		case k.seq == "[B" && !customMode:
			if sel < len(opts)-1 {
				sel++
				draw()
			}
		case k.b == ' ' && multi && !customMode && sel < nBase:
			toggled[sel] = !toggled[sel]
			draw()
		case k.b == 0x1b || k.b == 3:
			return nil, fmt.Errorf("cancelled")
		case k.b == '\r':
			if sel == nBase && !customMode {
				customMode = true // Type something: free text on the row
				draw()
				continue
			}
			if sel == nBase+1 && !customMode {
				// "Chat about this": free-form reply instead of picking —
				// real sends the typed text back as the answer
				customMode = true
				custom.Reset()
				draw()
				continue
			}
			if customMode {
				if s := strings.TrimSpace(custom.String()); s != "" {
					picked = append(picked, s)
				}
				return picked, nil
			}
			if multi && sel < nBase {
				for i := range toggled {
					picked = append(picked, opts[i].label)
				}
				return picked, nil
			}
			picked = append(picked, opts[sel].label)
			return picked, nil
		case customMode:
			switch k.b {
			case 0x7f, 8:
				s := []rune(custom.String())
				if len(s) > 0 {
					custom.Reset()
					custom.WriteString(string(s[:len(s)-1]))
				}
			default:
				if k.r != 0 {
					custom.WriteRune(k.r)
				}
			}
			draw()
		}
	}
	return nil, fmt.Errorf("cancelled")
}

func fireSessionEndHook(l *agent.Loop) {
	if l.Hooks == nil {
		return
	}
	if l.SessionEndFired {
		return // once — the REPL quit path and main both reach this
	}
	l.SessionEndFired = true
	hooks.Run(l.Hooks, hooks.SessionEnd, map[string]any{
		"session_id": l.Client.SessionID, "cwd": l.ToolCtx.CWD,
		"hook_event_name": "SessionEnd", "permission_mode": l.PermMode,
		"reason":          l.SessionEndReason,
		"transcript_path": l.Transcript,
	}, l.ToolCtx.CWD)
}

// statusLineCmd runs the configured statusLine.command (like real CC) —
// the JSON payload goes on stdin, first stdout line becomes the status.
var slCache struct {
	at  time.Time
	out string
}

func statusLineCmd(l *agent.Loop) string {
	if time.Since(slCache.at) < 500*time.Millisecond {
		return slCache.out
	}
	out := statusLineCmdReal(l)
	slCache.at, slCache.out = time.Now(), out
	return out
}

func statusLineCmdReal(l *agent.Loop) string {
	var st struct {
		Command string `json:"command"`
	}
	if raw := config.UserSetting("statusLine"); raw != nil {
		if jb, err := json.Marshal(raw); err == nil {
			json.Unmarshal(jb, &st)
		}
	}
	if st.Command == "" {
		return ""
	}
	u := l.UsageTotal
	tok := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	cwSize := int64(200000)
	if strings.Contains(l.Model, "[1m]") {
		cwSize = 1000000
	}
	pct := float64(0)
	if cwSize > 0 {
		pct = float64(tok) / float64(cwSize) * 100
	}
	payload, _ := json.Marshal(map[string]any{
		"session_id":      l.Client.SessionID,
		"transcript_path": l.Transcript,
		"cwd":             l.ToolCtx.CWD,
		"version":         "2.1.292",
		"model":           map[string]string{"id": l.Model, "display_name": l.Model},
		"workspace":       map[string]any{"current_dir": l.ToolCtx.CWD, "project_dir": l.ToolCtx.CWD},
		"output_style":    map[string]string{"name": outputStyle(l)},
		"effort":          map[string]string{"level": l.Effort},
		"thinking":        map[string]bool{"enabled": true},
		"vim":             map[string]string{"mode": vimMode(l)},
		"context_window": map[string]any{
			"total_input_tokens":    u.InputTokens,
			"total_output_tokens":   u.OutputTokens,
			"context_window_size":   cwSize,
			"current_usage":         tok,
			"used_percentage":       pct,
			"remaining_percentage":  100 - pct,
			"cache_creation_tokens": u.CacheCreationInputTokens,
			"cache_read_tokens":     u.CacheReadInputTokens,
		},
	})
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/bash"
	}
	cmd := exec.Command(sh, "-c", st.Command)
	cmd.Dir = l.ToolCtx.CWD
	cmd.Stdin = strings.NewReader(string(payload))
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.SplitN(string(out), "\n", 2)[0]
}

// expandCustomSlash resolves /name args to a prompt if it's a custom command.
func expandCustomSlash(l *agent.Loop, line string) string {
	fields := strings.Fields(line)
	name := strings.TrimPrefix(fields[0], "/")
	c, ok := customCommands[name]
	if !ok {
		return ""
	}
	arg := strings.TrimSpace(strings.TrimPrefix(line, "/"+name))
	if c.mcp != nil {
		dprintf("  %s/%s%s\n", dim, name, reset)
		return expandMCPPrompt(c, arg)
	}
	dprintf("  %s/%s%s\n", dim, name, reset)
	// real fires UserPromptExpansion when a custom command expands
	hooks.Run(l.Hooks, hooks.UserPromptExpansion, map[string]any{
		"session_id": l.Client.SessionID, "cwd": l.ToolCtx.CWD,
		"command_name": name, "hook_event_name": "UserPromptExpansion",
	}, l.ToolCtx.CWD)
	return expandCustomCommand(c, arg)
}

// expandMCPPrompt resolves an MCP server prompt: /mcp__s__p a=b c=d —
// args pass as key=value pairs; bare words join positionally.
func expandMCPPrompt(c customCmd, arg string) string {
	args := map[string]string{}
	var bare []string
	for _, f := range strings.Fields(arg) {
		if k, v, ok := strings.Cut(f, "="); ok {
			args[k] = v
		} else {
			bare = append(bare, f)
		}
	}
	if len(bare) > 0 {
		args["arguments"] = strings.Join(bare, " ")
	}
	out, err := c.mcp(args)
	if err != nil {
		dprintf("  %s/%s: %v%s\n", red, c.Name, err, reset)
		return "" // a failed prompt must not submit — callers skip empty
	}
	return out
}

// quickMemory: "# text" appends to the project memory index (CC's # prefix).
func quickMemory(l *agent.Loop, text string) {
	slug := strings.ReplaceAll(filepath.Clean(l.ToolCtx.CWD), "/", "-")
	dir := filepath.Join(config.ClaudeDir(), "projects", slug, "memory")
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "MEMORY.md")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		dprintln("  memory write failed:", err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "- %s\n", text)
	dprintln("  saved to memory:", p)
}

func vimMode(l *agent.Loop) string {
	if vimEnabled() {
		return "normal"
	}
	return ""
}

func outputStyle(l *agent.Loop) string {
	for _, p := range []string{
		filepath.Join(l.ToolCtx.CWD, ".claude", "settings.local.json"),
		filepath.Join(config.ClaudeDir(), "settings.json"),
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var st struct {
			OutputStyle string `json:"outputStyle"`
		}
		if json.Unmarshal(b, &st) == nil && st.OutputStyle != "" {
			return st.OutputStyle
		}
	}
	return "default"
}

// ruleSuggestion builds the "Tool(spec)" rule real CC persists on
// "don't ask again" — Bash gets a command prefix, others use their spec.
func ruleSuggestion(name, summary string) string {
	s := strings.Trim(summary, "()")
	s = strings.TrimSpace(strings.TrimSuffix(s, "…"))
	switch name {
	case "Bash", "Monitor":
		fields := strings.Fields(s)
		if len(fields) == 0 {
			return name
		}
		return fmt.Sprintf("%s(%s:*)", name, fields[0])
	case "WebFetch":
		if u, err := url.Parse(s); err == nil && u.Host != "" {
			return fmt.Sprintf("%s(domain:%s)", name, u.Hostname())
		}
		return name
	case "Read", "Edit", "Write", "NotebookEdit":
		if s == "" {
			return name
		}
		return fmt.Sprintf("%s(%s)", name, s)
	}
	return name
}

// addLocalPermissionRule appends a rule to .claude/settings.local.json.
func addLocalPermissionRule(cwd, rule string) {
	dir := filepath.Join(cwd, ".claude")
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "settings.local.json")
	var st map[string]any
	if b, err := os.ReadFile(p); err == nil {
		json.Unmarshal(b, &st)
	}
	if st == nil {
		st = map[string]any{}
	}
	perms, _ := st["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
		st["permissions"] = perms
	}
	allow, _ := perms["allow"].([]any)
	allow = append(allow, rule)
	perms["allow"] = allow
	b, _ := json.MarshalIndent(st, "", "  ")
	os.WriteFile(p, b, 0o600)
	// real also mirrors into ~/.claude.json projects[dir].allowedTools
	mirrorAllowRule(cwd, rule)
}

func mirrorAllowRule(cwd, rule string) {
	cfg, p := config.ReadClaudeJSON()
	if len(cfg) == 0 {
		return // don't create the file just for this
	}
	projects, _ := cfg["projects"].(map[string]any)
	if projects == nil {
		return
	}
	proj, _ := projects[cwd].(map[string]any)
	if proj == nil {
		return
	}
	allowed, _ := proj["allowedTools"].([]any)
	allowed = append(allowed, rule)
	proj["allowedTools"] = allowed
	config.WriteClaudeJSON(p, cfg)
}

// runRewindSelector lists past user messages; picking one truncates the
// conversation to before it (conversation-only rewind — CC's three-mode
// selector offers "both"/"conversation"/"code"; we do conversation + code
// since we checkpoint every write).
func runRewindSelector(l *agent.Loop, ks *keySource, ed *editor) {
	// collect user messages with their positions
	type um struct {
		i    int
		text string
	}
	var list []um
	for i, m := range l.Messages {
		if m.Role != "user" {
			continue
		}
		var txt string
		var blocks []map[string]any
		if json.Unmarshal(m.Content, &blocks) == nil {
			for _, b := range blocks {
				if b["type"] == "text" {
					txt, _ = b["text"].(string)
					break
				}
			}
		}
		if txt == "" {
			var s string
			if json.Unmarshal(m.Content, &s) == nil {
				txt = s
			}
		}
		if txt != "" {
			list = append(list, um{i, txt})
		}
	}
	if len(list) == 0 {
		dprintln("  (nothing to rewind to)")
		return
	}
	// real's MessageSelector — cursor list of past user messages,
	// newest selected; ↑↓ navigate, enter rewinds, esc cancels
	compSuspend()
	defer compResume()
	dch := make(chan key, 16)
	ks.dialog.Store(&dch)
	defer ks.dialog.Store(nil)
	sel := len(list) - 1
	show := min(len(list), 12)
	rows := show + 2
	dprintf("\n  %sRewind to:%s\n", bold, reset)
	for range list[len(list)-show:] {
		dprintln()
	}
	dprintf("  %senter to rewind · esc to cancel%s\n", dim, reset)
	draw := func() {
		var sb strings.Builder
		fmt.Fprintf(&sb, "\x1b[%dA", rows)
		for i := len(list) - show; i < len(list); i++ {
			m := list[i]
			cur := "   "
			if i == sel {
				cur = cyan + "❯" + reset + " "
			}
			txt := trunc(strings.ReplaceAll(m.text, "\n", " "), 76)
			sb.WriteString(fmt.Sprintf("\x1b[K %s%d. %s\r\n", cur, i+1, txt))
		}
		fmt.Print(sb.String())
	}
	draw()
	for k := range dch {
		switch {
		case k.seq == "[A":
			if sel > len(list)-show {
				sel--
				draw()
			}
		case k.seq == "[B":
			if sel < len(list)-1 {
				sel++
				draw()
			}
		case k.b == 0x1b || k.b == 3:
			dprintln()
			return
		case k.b == '\r':
			target := list[sel]
			l.Messages = l.Messages[:target.i]
			p, err := tools.Checkpoints.Rewind()
			restored := 0
			if err == nil && p != "" {
				restored = 1
			}
			dprintln()
			dprintf("  rewound to before %q (%d files restored)\n", trunc(target.text, 50), restored)
			return
		}
	}
}

// notifyAttention emits the preferredNotifChannel signal — real channels:
// iterm2/iterm2_with_bell/terminal_bell/kitty/ghostty/notifications_disabled.
func notifyAttention(cwd string) {
	ch := notifChannel(cwd)
	switch ch {
	case "notifications_disabled", "none", "":
		return
	case "iterm2", "iterm2_with_bell":
		fmt.Print("\x1b]9;Claude needs your attention\x07")
		if ch == "iterm2_with_bell" {
			fmt.Print("\a")
		}
	case "kitty":
		fmt.Print("\x1b]99;i=1:d=0;Claude needs your attention\x1b\\")
	case "ghostty":
		fmt.Print("\x1b]777;notify;Claude;needs your attention\x07")
	default: // terminal_bell / auto
		fmt.Print("\a")
	}
}

func notifChannel(cwd string) string {
	for _, p := range []string{
		filepath.Join(cwd, ".claude", "settings.json"),
		filepath.Join(config.ClaudeDir(), "settings.json"),
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var st struct {
			PreferredNotifChannel string `json:"preferredNotifChannel"`
		}
		if json.Unmarshal(b, &st) == nil && st.PreferredNotifChannel != "" {
			return st.PreferredNotifChannel
		}
	}
	return "terminal_bell"
}

// SlashCommandNames lists available slash commands for the init event.
func SlashCommandNames() []string {
	out := builtinSlashCommands()
	for n := range customCommands {
		out = append(out, n)
	}
	return out
}

func builtinSlashCommands() []string {
	return []string{"add-dir", "advisor", "agents", "alias", "allowed-tools", "ant-trace", "artifacts", "auto", "autocompact", "background", "batch", "bg", "branch", "break-reminder", "breaks", "brief", "btw", "bug", "cd", "checkpoint", "checkup", "chrome", "clear", "cloud-plugins", "code-review", "color", "compact", "config", "context", "copy", "cost", "debug", "debug-tool-call", "design-consent", "design-login", "design-revoke", "diff", "doctor", "downtime", "effort", "env", "exit", "experiments", "export", "extra-usage", "fast", "feedback", "fewer-permission-prompts", "files", "focus", "fork", "goal", "heapdump", "help", "high", "hooks", "ide", "import", "init", "insights", "install-github-app", "issue", "keybindings", "keybindings-help", "login", "logout", "loop", "low", "max", "mcp", "medium", "memory", "memory-pause", "memory-types", "model", "new", "oauth-refresh", "output-style", "pause-memory", "permissions", "plan", "plugin", "powerup", "pr", "privacy-settings", "proactive", "quit", "radio", "release-notes", "reload-plugins", "reload-skills", "remote-control", "remote-env", "rename", "reset", "restart", "resume", "review", "rewind", "routines", "run", "sandbox", "schedule", "scroll-speed", "security-review", "settings", "settings-review", "setup-bedrock", "setup-vertex", "skills", "stats", "status", "statusline", "stop", "subtask", "tasks", "teleport", "terminal-setup", "theme", "todos", "toggle-memory", "ultraplan", "ultrareview", "undo", "update-config", "usage", "usage-credits", "voice", "wellbeing", "xhigh"}
}

// currentStore is set by RunWithStore so slash commands can write records.
var currentStore Store

// logResync carries a new logging start index across a mid-session store
// swap (/resume, /fork): the loaded history is already in the target file —
// re-logging it would duplicate the chain. -1 = keep counting.
var logResync atomic.Int32

func init() { logResync.Store(-1) }

var sessionOnlyModel bool // /model "s" key — applies the pick without persisting

// showAgentsPane is real's ←← agents overlay — lists background tasks
// with live status; esc/←/q close.
func showAgentsPane(l *agent.Loop, ks *keySource) {
	dch := make(chan key, 16)
	ks.dialog.Store(&dch)
	defer ks.dialog.Store(nil)
	var tasks []*tools.BgTask
	if l.ToolCtx.BgTasks != nil {
		tasks = l.ToolCtx.BgTasks.List()
	}
	// real's agents view (captured 2.1.292): banner, "N awaiting input ·
	// M working · K completed" counts, Needs input + Completed sections,
	// a "describe a task for a new session" input row, mode-pill footer
	var running, done int
	for _, t := range tasks {
		select {
		case <-t.Done:
			done++
		default:
			running++
		}
	}
	dprintf("\n  %d awaiting input · %d working · %d completed\n", running, 0, done)
	dprintf("  %sYour conversation moved to the background — enter opens it · esc returns to it · ctrl+c twice quits%s\n", dim, reset)
	if running > 0 {
		dprintf("\n  %sNeeds input%s\n", bold, reset)
		for _, t := range tasks {
			select {
			case <-t.Done:
				continue
			default:
			}
			dprintf("  %s✻%s %-36s %s%s%s  %s%s\n", claudeOrange, reset,
				truncRunes(t.Command, 36), dim, "", reset, relAgo(t.Started), reset)
		}
	}
	if done > 0 {
		dprintf("\n  %sCompleted%s\n", bold, reset)
		for _, t := range tasks {
			select {
			case <-t.Done:
			default:
				continue
			}
			st := "stopped"
			if t.Err == nil && !t.TimedOut {
				st = "done"
			}
			dprintf("  %s∙%s %-36s %s%s%s  %s%s\n", dim, reset,
				truncRunes(t.Command, 36), dim, st, reset, relAgo(t.Started), reset)
		}
	}
	dprintf("\n  %s❯%s %sdescribe a task for a new session%s\n", "\x1b[38;5;153m", reset, dim, reset)
	dprintf("  %senter to return · space to reply · esc to close%s\n", dim, reset)
	for k := range dch {
		if k.b == 0x1b || k.b == 3 || k.b == 'q' || k.seq == "[D" || k.seq == "[C" {
			return
		}
	}
}

// runningAgents counts live background tasks (real's "← N agents").
func runningAgents(l *agent.Loop) int {
	if l.ToolCtx == nil || l.ToolCtx.BgTasks == nil {
		return 0
	}
	n := 0
	for _, t := range l.ToolCtx.BgTasks.List() {
		select {
		case <-t.Done:
		default:
			n++
		}
	}
	return n
}

// truncRunes/tailRunes clamp a string to n runes from the start/end —
// byte slicing splits multi-byte UTF-8 (the TestNarrowTurn50 flake class).
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func tailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// askStructuredMulti renders real's multi-question wizard: a tab row of
// questions (←/→ or Tab to move) ending in a "✓ Submit" tab, per-question
// option lists, space toggles multiSelect, Enter answers + advances.
func (p *promptUI) askStructuredMulti(ks *keySource, qs []tools.AskQuestion) (map[string][]string, error) {
	compSuspend()
	defer compResume()
	dch := make(chan key, 16)
	ks.dialog.Store(&dch)
	defer ks.dialog.Store(nil)

	const (
		chipBG = "\x1b[48;5;153m\x1b[38;5;16m"
		cyan   = "\x1b[38;5;153m"
		gray   = "\x1b[38;5;246m"
		boldT  = "\x1b[38;5;231m\x1b[1m"
		unbold = "\x1b[22m"
	)
	answers := make(map[string][]string)
	toggled := make([]map[int]bool, len(qs))
	custom := make([][]rune, len(qs))
	customOn := make([]bool, len(qs)) // "Type something" editing per tab
	for i := range toggled {
		toggled[i] = map[int]bool{}
	}
	tab, sel := 0, 0
	width, _, _ := term.GetSize(int(os.Stdout.Fd()))
	if width <= 0 {
		width = 80
	}

	draw := func() {
		var sb strings.Builder
		sb.WriteString("\r\x1b[J")
		var q tools.AskQuestion
		if tab < len(qs) {
			q = qs[tab]
		}
		sb.WriteString(" " + chipBG + "  " + " " + "\x1b[49m\x1b[39m\n")
		hdr := q.Header
		if tab == len(qs) {
			hdr = "Review"
		}
		sb.WriteString(" " + chipBG + " " + hdr + " " + "\x1b[49m\x1b[39m\n")
		sb.WriteString(" " + boldT + q.Text + unbold + "\x1b[39m\n\n")
		// question tabs — "← Q1 Q2 … ✓ Submit →" like real's tab bar
		if len(qs) > 1 {
			var tb strings.Builder
			for i, qq := range qs {
				lbl := qq.Header
				if lbl == "" {
					lbl = fmt.Sprintf("Q%d", i+1)
				}
				if len(answers[fmt.Sprint(i)]) > 0 {
					lbl = "✓ " + lbl // answered marker
				}
				if i == tab {
					tb.WriteString(" " + cyan + lbl + "\x1b[39m")
				} else {
					tb.WriteString(" " + gray + lbl + "\x1b[39m")
				}
			}
			// submit tab
			sub := " ✓ Submit "
			if tab == len(qs) {
				sb.WriteString("  ← " + tb.String() + " " + cyan + sub + "\x1b[39m" + " →\n")
			} else {
				sb.WriteString("  ← " + tb.String() + " " + gray + sub + "\x1b[39m" + " →\n")
			}
			sb.WriteString("\n")
		}
		if tab == len(qs) {
			// submit tab: summary of answers
			sb.WriteString(" " + gray + "Review your answers, then submit." + "\x1b[39m\n\n")
			for i, qq := range qs {
				ans := answers[fmt.Sprint(i)]
				lbl := qq.Header
				if lbl == "" {
					lbl = fmt.Sprintf("Q%d", i+1)
				}
				sb.WriteString(fmt.Sprintf("  %s%s%s  %s%s%s\n", gray, lbl, "\x1b[39m", cyan, strings.Join(ans, ", "), "\x1b[39m"))
			}
		} else {
			base := len(q.Options)
			rows := base + 1 // + "Type something."
			for i := 0; i < rows; i++ {
				var lbl string
				isCustomRow := i == base
				if isCustomRow {
					lbl = "Type something."
					if customOn[tab] {
						lbl = "Type something: " + string(custom[tab])
					}
				} else {
					lbl = q.Options[i].Label
				}
				num := fmt.Sprintf("%d.", i+1)
				if i == sel && !customOn[tab] {
					sb.WriteString(fmt.Sprintf(" %s❯%s %s%s%s %s%s\n", cyan, "\x1b[39m", gray, num, "\x1b[39m", cyan, lbl+"\x1b[39m"))
				} else if customOn[tab] && isCustomRow {
					sb.WriteString(fmt.Sprintf(" %s❯%s %s%s%s %s█\n", cyan, "\x1b[39m", gray, num, "\x1b[39m", lbl))
				} else {
					check := ""
					if q.MultiSelect && !isCustomRow {
						check = "○ "
						if toggled[tab][i] {
							check = "● "
						}
					}
					sb.WriteString(fmt.Sprintf("   %s%s%s %s%s\n", gray, num, "\x1b[39m", check, lbl))
				}
				if !isCustomRow && q.Options[i].Description != "" {
					sb.WriteString("      " + gray + q.Options[i].Description + "\x1b[39m\n")
				}
			}
			sb.WriteString(" " + gray + strings.Repeat("─", width) + "\x1b[39m\n")
			sb.WriteString(fmt.Sprintf(" %s%d.%s Chat about this\n", gray, rows+1, "\x1b[39m"))
		}
		sb.WriteString("\n " + gray + "Enter to select · ↑/↓ to navigate" + "\x1b[39m")
		if len(qs) > 1 {
			sb.WriteString(gray + " · ←/→ questions · Esc to cancel" + "\x1b[39m")
		} else {
			sb.WriteString(gray + " · Esc to cancel" + "\x1b[39m")
		}
		fmt.Print(sb.String())
	}
	draw()
	take := func() map[string][]string {
		for i := range qs {
			key := fmt.Sprint(i)
			if s := strings.TrimSpace(string(custom[i])); s != "" && !slices.Contains(answers[key], s) {
				answers[key] = append(answers[key], s)
			}
		}
		return answers
	}
	for k := range dch {
		if tab < len(qs) && customOn[tab] {
			// typing into "Type something"
			switch {
			case k.b == '\r' || k.b == 0x1b || k.b == 3:
				customOn[tab] = false
				draw()
			case k.b == 0x7f || k.b == 8:
				if len(custom[tab]) > 0 {
					custom[tab] = custom[tab][:len(custom[tab])-1]
				}
				draw()
			case k.b >= 0x20:
				custom[tab] = append(custom[tab], rune(k.b))
				draw()
			}
			continue
		}
		switch {
		case k.seq == "[D": // ← prev question
			if tab > 0 {
				tab--
				sel = 0
			}
			draw()
		case k.seq == "[C" || k.b == '\t': // → / Tab next question (Submit last)
			if tab < len(qs) {
				tab++
				sel = 0
			}
			draw()
		case k.seq == "[A":
			if sel > 0 {
				sel--
			}
			draw()
		case k.seq == "[B":
			tabMax := len(qs[tab].Options)
			if tab < len(qs) && sel < tabMax {
				sel++
			}
			draw()
		case k.b == 0x1b || k.b == 3:
			fmt.Print("\r\n")
			return nil, fmt.Errorf("cancelled")
		case k.b == ' ' && tab < len(qs) && qs[tab].MultiSelect && sel < len(qs[tab].Options):
			toggled[tab][sel] = !toggled[tab][sel]
			draw()
		case k.b == '\r':
			if tab == len(qs) {
				fmt.Print("\r\n")
				return take(), nil
			}
			q := qs[tab]
			if sel == len(q.Options) {
				customOn[tab] = true // Type something
				draw()
				continue
			}
			if sel == len(q.Options)+1 {
				// Chat about this → deny + clarify feedback (real's path)
				fmt.Print("\r\n")
				return nil, tools.ErrChatAboutThis
			}
			if q.MultiSelect {
				// enter = confirm current toggles for this question
				var sel2 []string
				for i, on := range toggled[tab] {
					if on {
						sel2 = append(sel2, q.Options[i].Label)
					}
				}
				if len(sel2) == 0 {
					sel2 = []string{q.Options[sel].Label}
				}
				answers[fmt.Sprint(tab)] = sel2
			} else {
				answers[fmt.Sprint(tab)] = []string{q.Options[sel].Label}
			}
			if len(qs) == 1 {
				// single question: answering submits immediately (no Submit tab)
				fmt.Print("\r\n")
				return take(), nil
			}
			tab++
			sel = 0
			draw()
		}
	}
	return nil, fmt.Errorf("cancelled")
}

func readUserSettingStr(key string) string {
	s, _ := userSetting(key).(string)
	return s
}

// userSetting reads a key from ~/.claude/settings.json via config's cache —
// real reads it once at startup too (restart to pick up edits).
func userSetting(key string) any { return config.UserSetting(key) }

func queueBufLast(q []string) string {
	if len(q) == 0 {
		return ""
	}
	return q[len(q)-1]
}

// elicitDialog renders the elicitation/create prompt: server name +
// message + accept/decline (real's form renders schema fields; we take
// a single text answer when fields exist).
func elicitDialog(l *agent.Loop, ks *keySource, req *tools.ElicitRequest) tools.ElicitAnswer {
	hooks.Run(l.Hooks, hooks.Elicitation, map[string]any{
		"session_id": l.Client.SessionID, "cwd": l.ToolCtx.CWD,
		"hook_event_name": "Elicitation", "permission_mode": l.PermMode,
		"transcript_path": l.Transcript, "mcp_server_name": req.Server,
	}, l.ToolCtx.CWD)
	compInline("\r\n" + "\x1b[38;5;246m" + "┌ MCP " + req.Server + " is asking:\x1b[39m\r\n")
	compInline("  " + req.Message + "\r\n")
	// requestedSchema fields → prompt for each string property
	var schema struct {
		Properties map[string]struct {
			Type string `json:"type"`
			Desc string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	_ = json.Unmarshal(req.Schema, &schema)
	content := map[string]any{}
	action := "decline"
	if len(schema.Properties) == 0 {
		compInline("  " + "\x1b[38;5;246m" + "y to accept, n to decline\x1b[39m\r\n")
		k := <-ks.ch
		if k.r == 'y' || k.r == 'Y' || k.b == '\r' {
			action = "accept"
		}
	} else {
		for name, prop := range schema.Properties {
			if prop.Desc != "" {
				compInline(fmt.Sprintf("  %s%s%s: ", dim, prop.Desc, reset))
			} else {
				compInline(fmt.Sprintf("  %s%s%s: ", dim, name, reset))
			}
			var line strings.Builder
			for {
				k := <-ks.ch
				if k.b == '\r' || k.b == '\n' {
					break
				}
				if k.b == 0x1b || k.b == 3 {
					line.Reset()
					break
				}
				if k.b == 0x7f && line.Len() > 0 {
					s := []rune(line.String())
					line.Reset()
					line.WriteString(string(s[:len(s)-1]))
					continue
				}
				if k.r != 0 {
					line.WriteRune(k.r)
				}
			}
			if s := strings.TrimSpace(line.String()); s != "" {
				content[name] = s
			}
		}
		action = "accept"
	}
	hooks.Run(l.Hooks, hooks.ElicitationResult, map[string]any{
		"session_id": l.Client.SessionID, "cwd": l.ToolCtx.CWD,
		"hook_event_name": "ElicitationResult", "permission_mode": l.PermMode,
		"transcript_path": l.Transcript, "mcp_server_name": req.Server,
		"action": action,
	}, l.ToolCtx.CWD)
	return tools.ElicitAnswer{Action: action, Content: content}
}
