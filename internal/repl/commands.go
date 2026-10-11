package repl

import (
	"golang.org/x/term"

	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/gbasin/claude-go/internal/config"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gbasin/claude-go/internal/agent"
	"github.com/gbasin/claude-go/internal/api"
	"github.com/gbasin/claude-go/internal/auth"
	"github.com/gbasin/claude-go/internal/bg"
	"github.com/gbasin/claude-go/internal/bridge"
	"github.com/gbasin/claude-go/internal/hooks"
	"github.com/gbasin/claude-go/internal/plugins"
	"github.com/gbasin/claude-go/internal/session"
	"github.com/gbasin/claude-go/internal/tools"
)

// handleSlash runs a /command. Returns true to exit the REPL.
func handleSlash(l *agent.Loop, line string, ed *editor, ks *keySource) (exit bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	cmd := strings.TrimPrefix(fields[0], "/")
	arg := strings.TrimSpace(strings.TrimPrefix(line, "/"+cmd))

	switch cmd {
	case "exit", "quit":
		return true
	case "help":
		printHelp()
	case "plan":
		// real (mod_2070 se): [open|<description>] — bare enables plan
		// mode and renders the current plan panel; "open" edits it in
		// $EDITOR; a description enables + seeds the plan file
		if arg == "open" {
			planOpenInEditor(l, ks)
			break
		}
		already := l.PermMode == "plan"
		l.PermMode = "plan"
		if already {
			dprintln("  Already in plan mode.")
		} else {
			dprintln("  ⏸ plan mode — explore read-only, then ExitPlanMode to implement")
		}
		if arg != "" && arg != "share" {
			planWrite(l, arg) // the description seeds the plan file
		}
		if pc, pp := planCurrent(l); pc != "" {
			dprintf("  %sCurrent Plan%s %s%s%s\n", bold, reset, dim, pp, reset)
			for _, ln := range strings.Split(pc, "\n") {
				dprintln("    " + ln)
			}
			if ed := os.Getenv("EDITOR"); ed != "" {
				dprintf("    %s\"/plan open\" to edit this plan in %s%s\n", dim, ed, reset)
			}
		}
	case "clear", "reset", "new":
		l.Messages = nil
		// /clear ends the session before starting the next — real fires
		// SessionEnd(reason=clear) then SessionStart(source=clear)
		l.SessionEndReason = "clear"
		hooks.Run(l.Hooks, hooks.SessionEnd, map[string]any{
			"session_id": l.Client.SessionID, "transcript_path": l.Transcript,
			"cwd": l.ToolCtx.CWD, "hook_event_name": "SessionEnd",
			"reason": "clear", "permission_mode": l.PermMode,
		}, l.ToolCtx.CWD)
		l.ResetForNewSession()
		l.SessionStartSource = "clear"
		hooks.Run(l.Hooks, hooks.SessionStart, map[string]any{
			"session_id": l.Client.SessionID, "transcript_path": l.Transcript,
			"cwd": l.ToolCtx.CWD, "hook_event_name": "SessionStart", "source": "clear",
			"permission_mode": l.PermMode,
		}, l.ToolCtx.CWD)
		dprintln("  context cleared (previous session remains resumable via /resume)")
	case "compact":
		// real: the spinner's verb becomes "Compacting conversation…",
		// a rotating ⎿ Tip line shows, SessionStart hooks re-fire, then
		// "⎿ Compacted (ctrl+o to see full summary)"
		spinTip := spinnerTip()
		if spinTip != "" {
			dprintf("  %s⎿%s  %s%s\n", dim, reset, spinTip, reset)
		}
		if arg != "" {
			l.AppendSysP += "\n\n## Compact Instructions\n" + arg
		}
		spin := newSpinner()
		spin.verb = "Compacting conversation"
		done := make(chan struct{})
		go func() { // spinner ticks while the compact request runs
			for i := 0; ; i++ {
				select {
				case <-done:
					return
				case <-time.After(150 * time.Millisecond):
					fmt.Fprintf(os.Stderr, "\r\x1b[K%s%s Compacting conversation…%s", claudeOrange, spinnerFrames[i%len(spinnerFrames)], reset)
				}
			}
		}()
		err := l.Compact(context.Background(), l.Transcript)
		close(done)
		fmt.Fprint(os.Stderr, "\r\x1b[K")
		if err != nil {
			dprintln("  compact failed:", err)
		} else {
			// real re-fires SessionStart hooks post-compact
			hooks.Run(l.Hooks, hooks.SessionStart, map[string]any{
				"session_id": l.Client.SessionID, "transcript_path": l.Transcript,
				"cwd": l.ToolCtx.CWD, "hook_event_name": "SessionStart", "source": "compact",
				"permission_mode": l.PermMode,
			}, l.ToolCtx.CWD)
			dprintf("  %s⎿%s  Compacted (ctrl+o to see full summary)\n", dim, reset)
		}
	case "cost", "usage", "stats":
		// real's /cost: Session summary (cost, durations, code changes,
		// usage) + plugin skill-listing footprint; /usage also gets the
		// rate-limit bars below it
		sessionCostBlock(l)
		usageDisplay(l)
	case "status":
		statusOverlay(l, ks)
	case "context":
		u := l.UsageTotal
		total := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		dprintf("  context tokens (last request): %d\n  messages: %d\n", total, len(l.Messages))
	case "add-dir":
		if arg == "" {
			dprintln("  usage: /add-dir <path>")
			break
		}
		p, err := filepath.Abs(arg)
		if err != nil {
			dprintln("  bad path:", err)
			break
		}
		l.AddDirs = append(l.AddDirs, p)
		hooks.Run(l.Hooks, hooks.DirectoryAdded, map[string]any{
			"session_id": l.Client.SessionID, "cwd": l.ToolCtx.CWD,
			"directory": p, "source": "user_command",
			"hook_event_name": "DirectoryAdded",
		}, l.ToolCtx.CWD)
		dprintln("  added", p)
	case "memory":
		memoryCmd(l, arg)
	case "mcp":
		mcpCmd(l, arg)
	case "permissions", "allowed-tools":
		permissionsModal(l, ks)
	case "config", "settings":
		if arg == "" {
			statusOverlayAt(l, ks, 2) // real opens the overlay on Config
		} else {
			configCmd(l, arg)
		}
	case "agents":
		// real 2.1.292: the wizard was removed — it prints a pointer
		dprintln("  ⎿  The /agents wizard has been removed.")
		dprintln(`  Ask Claude to create or update subagents for you (e.g. "create a`)
		dprintln(`  code-reviewer subagent that..."), or edit the files directly:`)
		dprintln("   • .claude/agents/  (this project)")
		dprintln("   • ~/.claude/agents/  (all projects)")
		dprintln("  Docs: https://code.claude.com/docs/en/sub-agents")
	case "hooks":
		// real's /hooks: read-only menu of configured hooks grouped by
		// event ("Enter to view · Esc to close"), not a doc prompt
		hooksMenu(l, ks)
	case "resume", "continue":
		// real takes [conversation id or search term]: a unique match
		// resumes directly, anything else opens the picker
		var id string
		if arg != "" {
			id = resolveSessionArg(l.ToolCtx.CWD, l.Client.SessionID, arg)
			if id == "" {
				dprintf("  no session matches %q\n", arg)
			}
		}
		if id == "" && arg == "" {
			compSuspend() // the picker draws with \x1b[J — un-pin the live zone
			id = pickSession(l.ToolCtx.CWD, ks, l.Client.SessionID)
			compResume()
		}
		if id == "" {
			break
		}
		{
			if msgs, err := session.Load(l.ToolCtx.CWD, id); err == nil {
				// real's /resume switches to the picked session — same
				// transcript continues: rebind the store at <id>.jsonl so
				// new turns append to it parented at the loaded leaf
				path := filepath.Join(session.ProjectsDir(l.ToolCtx.CWD), id+".jsonl")
				if ns, err2 := session.Open(l.ToolCtx.CWD, id); err2 == nil {
					ns.SeedParent(session.LeafUUID(path))
					if old, ok := currentStore.(io.Closer); ok {
						old.Close()
					}
					currentStore = ns
					l.Transcript = ns.Path
				} else {
					dprintln("  resume: couldn't open transcript:", err2)
				}
				l.Client.SessionID = id
				l.ToolCtx.Session = id
				l.Messages = msgs
				l.MarkEnvSent()
				logResync.Store(int32(len(msgs)))
				dprintf("  resumed session %s (%d messages)\n", id, len(msgs))
			}
		}
	case "export":
		if arg == "" {
			exportDialog(l, ks)
		} else {
			exportCmd(l, arg)
		}
	case "init":
		dprintln("  analyzing codebase → CLAUDE.md …")
		hooks.Run(l.Hooks, hooks.Setup, map[string]any{
			"session_id": l.Client.SessionID, "cwd": l.ToolCtx.CWD,
			"trigger": "init", "hook_event_name": "Setup",
		}, l.ToolCtx.CWD)
		body := bundledInit
		if foreignAgentConfigExists() {
			body = strings.Replace(body, "\n- Do not make up information",
				bundledInitImportHint+"\n- Do not make up information", 1)
		}
		go l.Ask(context.Background(), body)
	case "diff":
		diffCmdKS(l, ks)
	case "review", "code-review":
		// real (mod_0683 Ce/ho): [<level>|ultra] [--fix] [--comment]
		// [<pr#>|<branch>|<path>] [--max-findings <n>|all] — "ultra"
		// dispatches to the ultrareview command, a level word sets the
		// review effort and sticks (codeReviewLastEffort), everything
		// else is the review target
		ra := parseCodeReviewArgs(arg)
		if ra.ultra {
			ultrareviewCmd(l, ra.rest)
			break
		}
		if ra.unrecognized != "" {
			dprintf("  Ignoring unrecognized effort %q; valid: %s\n",
				ra.unrecognized, strings.Join(codeReviewLevels, ", "))
		}
		eff := ra.level
		if eff == "" {
			if last, _ := config.UserSetting("codeReviewLastEffort").(string); last != "" {
				eff = last
				dprintf("  reviewing (reusing %s effort, the level you typed last time — type a level like /code-review high to change it)\n", eff)
			} else {
				eff = "medium"
				dprintln("  reviewing (medium effort)…")
			}
		} else {
			config.WriteUserSetting("codeReviewLastEffort", eff)
			dprintf("  reviewing (%s effort)…\n", eff)
		}
		body := bundledCodeReview
		var pre string
		if ra.target != "" {
			pre += "Review target: `" + ra.target + "`\n\n"
		}
		if eff != "medium" {
			pre += "Review effort: " + eff + "\n\n"
		}
		if ra.maxFindings != "" {
			pre += "Report at most " + ra.maxFindings + " findings.\n\n"
		}
		if ra.fix {
			pre += "After reporting the findings, apply the fixes to the working tree.\n\n"
		}
		if ra.comment {
			pre += "Post the findings as inline comments on the PR under review.\n\n"
		}
		go l.Ask(context.Background(), pre+body)
	case "security-review":
		dprintln("  analyzing code changes for security risks…")
		runBundledPrompt(l, bundledSecurityReview, arg)
	case "doctor", "checkup":
		// real /doctor sends the audit skill prompt; /doctor local stays as a
		// diagnostic variant below via "status"
		dprintln("  health-checking setup…")
		body := bundledDoctor
		if arg != "" {
			body += "\n\n## Additional instructions from the user\n\n" + arg
		}
		go l.Ask(context.Background(), body)
	case "pr":
		dprintln("  creating pull request…")
		body := bundledPR
		if arg == "" {
			body = strings.ReplaceAll(body, "\nUser guidance for this PR: $ARG\n", "\n")
		} else {
			body = strings.ReplaceAll(body, "$ARG", arg)
		}
		go l.Ask(context.Background(), interpolateBang(body, l.ToolCtx.CWD))
	case "batch":
		if arg == "" {
			dprintln("  " + bundledBatchUsage)
			break
		}
		dprintln("  planning batch orchestration…")
		go l.Ask(context.Background(), strings.ReplaceAll(bundledBatch, "$ARG", arg))
	case "debug":
		dprintln("  investigating…")
		go l.Ask(context.Background(), debugPrompt(l, arg))
	case "run":
		go l.Ask(context.Background(), runPrompt(arg))
	case "update-config":
		body := bundledUpdateConfig
		if strings.HasPrefix(arg, "[hooks-only]") {
			arg = strings.TrimSpace(strings.TrimPrefix(arg, "[hooks-only]"))
			body = bundledHooksDocs
			if arg != "" {
				body += "\n\n## Task\n\n" + arg
			}
		} else if arg != "" {
			body += "\n\n## User Request\n\n" + arg
		}
		go l.Ask(context.Background(), body)
	case "fewer-permission-prompts":
		body := bundledFewerPermissionPrompts
		if arg != "" {
			body += "\n\n## Additional instructions from the user\n\n" + arg
		}
		go l.Ask(context.Background(), body)
	case "keybindings-help":
		body := bundledKeybindingsHelp
		if arg != "" {
			body += "\n\n## User Request\n\n" + arg
		}
		go l.Ask(context.Background(), body)
	case "memory-types":
		go l.Ask(context.Background(), bundledMemoryTypes)
	case "insights":
		dprintln("  analyzing your sessions…")
		go l.Ask(context.Background(), insightsPrompt())
	case "bug", "share":
		url := "https://github.com/anthropics/claude-code/issues"
		dprintln("  report issues at", url)
		openURL(url)
	case "vim":
		// real CC: /vim is a compat shim — editor mode moved to /config
		dprintln("  editor mode moved to /config")
	case "output-style":
		outputStyleCmd(l, arg)
	case "statusline":
		// real /statusline prompts the model to spawn the statusline-setup agent
		n := arg
		if n == "" {
			n = "Configure my statusLine from my shell PS1 configuration"
		}
		go l.Ask(context.Background(),
			`Create an Agent with subagent_type "statusline-setup" and the prompt "`+n+`"`)
	case "tasks", "bashes":
		tasksCmd(l)
	case "theme":
		if arg == "" {
			if pick := runThemePicker(ks); pick != "" {
				applyTheme(pick, l.CWD)
				persistTheme(l.CWD, pick)
				dprintf("  theme set to %s\n", pick)
			}
			break
		}
		applyTheme(arg, l.CWD)
		persistTheme(l.CWD, arg)
		dprintf("  theme set to %s\n", arg)
	case "login":
		loginCmd()
	// "version" intentionally absent — real 294 ships the registry entry
	// with isEnabled:()=>!1, so /version falls to "unknown command"
	case "logout":
		logoutCmd()
	case "keybindings":
		dprintln("  edit ~/.claude/keybindings.json")
	case "ide":
		ideCmd()
	case "release-notes":
		dprintln("  claude-go", api.Version)
	case "terminal-setup":
		dprintln("  keybindings installed: none needed (Go build)")
	case "privacy-settings":
		dprintln("  telemetry: none in claude-go")
	case "plugin", "plugins", "marketplace":
		pluginCmd(l, cmd, arg)
	case "btw":
		if arg == "" {
			dprintln("  usage: /btw <side question> — answered without adding to the main context")
			break
		}
		go func() {
			// side query: same client/config as the session, fresh history
			// (a struct copy of l trips vet — Loop embeds atomic counters)
			sub := &agent.Loop{
				Client: l.Client, Model: l.Model, Wants1m: l.Wants1m,
				MaxTokens: l.MaxTokens, Effort: l.Effort,
				Entrypoint: l.Entrypoint, PermMode: l.PermMode, CWD: l.CWD,
				AppendSysP: l.AppendSysP, ExtraSystem: l.ExtraSystem,
				SessionExtras: l.SessionExtras, PermRules: l.PermRules,
				AddDirs: l.AddDirs, User: l.User, ToolCtx: l.ToolCtx,
				Events: l.Events, MaxTurns: l.MaxTurns,
				Transcript: l.Transcript, Hooks: l.Hooks,
				AllowedTools: l.AllowedTools, FallbackModel: l.FallbackModel,
				ThinkingMode: l.ThinkingMode, ThinkingDisplay: l.ThinkingDisplay,
				ThinkingBudget: l.ThinkingBudget, FastMode: l.FastMode,
				AutoCompactWindow: l.AutoCompactWindow, AdvisorModel: l.AdvisorModel,
				MemoryPaused: l.MemoryPaused, AlwaysThinking: l.AlwaysThinking,
				Bare: l.Bare, Goal: l.Goal, SessionStartSource: l.SessionStartSource,
			}
			resp, err := sub.Ask(context.Background(), arg)
			if err != nil {
				dprintf("\n  btw failed: %s\n", err)
			} else {
				var txt string
				for _, b := range resp.Content {
					if b.Type == "text" {
						txt += b.Text
					}
				}
				dprintf("\n%sbtw:%s %s\n", cyan, reset, txt)
			}
		}()
	case "feedback":
		dprintln("  feedback: https://github.com/gbasin/claude-go/issues")
	case "rename", "name":
		if arg == "" {
			dprintln("  usage: /rename <title>")
			return false // handled (return true exits the REPL)
		}
		if currentStore != nil {
			currentStore.LogTitle(arg)
		}
		dprintf("  %s⎿%s  Session renamed to: %s\n", dim, reset, arg)
	case "teleport", "tp":
		teleportCmd(l)
	case "remote-control", "rc":
		remoteControlCmd(l, arg)
	case "remote-env":
		remoteEnvCmd(l, arg)
	case "install-github-app":
		installGitHubAppCmd(l, arg)
	case "design-consent":
		designConsentCmd(l, true)
	case "design-revoke":
		designConsentCmd(l, false)
	case "design-login":
		designLoginCmd(l, arg)
	case "cloud-plugins":
		cloudPluginsCmd(arg)
	case "usage-credits":
		usageCreditsCmd(l, arg)
	case "extra-usage":
		// real's /extra-usage metadata: "Renamed to /usage-credits"
		dprintln("  /extra-usage is renamed to /usage-credits")
		usageCreditsCmd(l, arg)
	case "chrome":
		chromeCmd(arg)
	case "ultraplan":
		ultraplanCmd(l, arg)
	case "ultrareview":
		ultrareviewCmd(l, arg)
	case "fork":
		// real (mod_0416): "Spawn a background agent that inherits the
		// full conversation" <directive> — gated off under ua() (agent
		// contexts); ours maps ua() to coordinator mode
		if tools.IsCoordinatorMode() {
			dprintln("  " + tools.MsgForkCoordinator)
			return false
		}
		if l.ToolCtx.Agent == nil {
			dprintln("  (sub-agents unavailable)")
			return false
		}
		if arg == "" {
			dprintln("  usage: /fork <directive> — copies this conversation into a background session")
			return false
		}
		forkRes := l.ToolCtx.Agent.RunAgent(l.ToolCtx, "fork", arg, "fork", "", "", true)
		if forkRes.IsError {
			dprintln("  fork failed:", forkRes.Content)
		} else {
			dprintln(" ", forkRes.Content)
		}
	case "rewind", "checkpoint", "undo":
		if p, err := tools.Checkpoints.Rewind(); err != nil {
			dprintln(" ", err)
		} else {
			dprintln("  rewound:", p)
		}
	case "schedule", "routines":
		scheduleCmd(l, arg)
	case "loop", "proactive":
		loopCmd(l, arg, ks)
	case "goal":
		if arg == "clear" || arg == "" {
			l.Goal = ""
			dprintln("  goal cleared")
			return false // handled (return true exits the REPL)
		}
		l.Goal = arg
		dprintln("  goal set — I'll keep working until it holds (stop-hook semantics)")
	case "subtask":
		if arg == "" {
			dprintln("  usage: /subtask <prompt> — run a subagent, result returns here")
			return false // handled (return true exits the REPL)
		}
		if l.ToolCtx.Agent == nil {
			dprintln("  (sub-agents unavailable)")
			return false // handled (return true exits the REPL)
		}
		res := l.ToolCtx.Agent.RunAgent(l.ToolCtx, "subtask", arg, "general-purpose", "", "", false)
		dprintln(" ", res.Content)
	case "branch":
		// fork conversation onto a new session branch
		old := l.Client.SessionID
		l.Client.SessionID = session.NewID()
		n, o := l.Client.SessionID, old
		if len(n) > 8 {
			n = n[:8]
		}
		if len(o) > 8 {
			o = o[:8]
		}
		dprintf("  branched session → %s (original %s resumable via /resume)\n", n, o)
	case "sandbox":
		if strings.HasPrefix(arg, "allow ") {
			host := strings.TrimSpace(arg[len("allow"):])
			if host == "" {
				dprintln("  usage: /sandbox allow <host>")
				break
			}
			dprintln("  " + tools.SandboxAllowHost(host))
			break
		}
		// real: sandbox-mode picker — auto-allow / regular perms / off
		sandboxPicker(l, ks)
	case "skills":
		skills := agent.SkillNames(l.ToolCtx.CWD)
		if len(skills) == 0 {
			dprintln("  no skills found")
			return false // handled (return true exits the REPL)
		}
		dprintln("  Skills:")
		for _, s := range skills {
			dprintf("    /%s\n", s)
		}
	case "model":
		if arg == "" {
			// real's picker (captured 2.1.292): title + 2-line description,
			// numbered rows with ❯ cursor + ✔ current, "… +N models" overflow,
			// effort row, "s = session only" footer
			type mo struct{ id, label, desc string }
			models := []mo{
				{"default", "Default (recommended)", "Opus 5.5 · Best for everyday, complex tasks"},
				{"claude-opus-5-5", "Opus 5.5", "Best for everyday, complex tasks"},
				{"claude-sonnet-5-5", "Sonnet 5.5", "Efficient for routine tasks"},
				{"claude-haiku-5-5", "Haiku 5.5", "Fastest for quick answers"},
				{"claude-fable-5-1", "Fable 5.1", "Most capable for your hardest and longest-running tasks"},
			}
			// ANTHROPIC_DEFAULT_{SONNET,OPUS,HAIKU,FABLE}_MODEL replaces the
			// family row (real's sessionPickerChoices): the entry then selects
			// the env id, labeled by _NAME/_DESCRIPTION when provided.
			for i, f := range []struct{ env, fam string }{
				{"OPUS", "Opus"}, {"SONNET", "Sonnet"}, {"HAIKU", "Haiku"}, {"FABLE", "Fable"},
			} {
				mid := strings.TrimSpace(os.Getenv("ANTHROPIC_DEFAULT_" + f.env + "_MODEL"))
				if mid == "" {
					continue
				}
				r := &models[i+1]
				r.id = mid
				if n := strings.TrimSpace(os.Getenv("ANTHROPIC_DEFAULT_" + f.env + "_MODEL_NAME")); n != "" {
					r.label = n
				} else {
					r.label = "Custom " + f.fam + " model"
				}
				if d := strings.TrimSpace(os.Getenv("ANTHROPIC_DEFAULT_" + f.env + "_MODEL_DESCRIPTION")); d != "" {
					r.desc = d
				} else {
					r.desc = fmt.Sprintf("Custom %s model (%s)", f.fam, mid)
				}
			}
			// ANTHROPIC_CUSTOM_MODEL_OPTION{,_NAME,_DESCRIPTION} appends a
			// free entry (real: sessionTail; skipped when it duplicates a row).
			if cid := strings.TrimSpace(os.Getenv("ANTHROPIC_CUSTOM_MODEL_OPTION")); cid != "" {
				dup := false
				for _, m := range models {
					if m.id == cid {
						dup = true
						break
					}
				}
				if !dup {
					label := strings.TrimSpace(os.Getenv("ANTHROPIC_CUSTOM_MODEL_OPTION_NAME"))
					if label == "" {
						label = cid
					}
					desc := strings.TrimSpace(os.Getenv("ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION"))
					if desc == "" {
						desc = "Custom model (" + cid + ")"
					}
					models = append(models, mo{cid, label, desc})
				}
			}
			const extra = 9   // real: "… +9 models" (older/legacy rows)
			var filter []rune // real's "Type to search" narrows the list
			sel := 0
			for i, m := range models {
				if l.Model == m.id {
					sel = i
				}
			}
			cyan := "\x1b[38;5;153m"
			rows := len(models) + 6 // title+desc+models+overflow+effort+footer
			shown := func() []int {
				var idx []int
				q := strings.ToLower(string(filter))
				for i, m := range models {
					if q == "" || strings.Contains(strings.ToLower(m.label+" "+m.id), q) {
						idx = append(idx, i)
					}
				}
				return idx
			}
			draw := func() {
				var sb strings.Builder
				sb.WriteString(fmt.Sprintf("\x1b[%dA\r", rows))
				sb.WriteString(fmt.Sprintf("\x1b[K  %sSelect model%s\r\n", bold, reset))
				sb.WriteString("\x1b[K  " + dim + "Switch between Claude models. Your pick becomes the default for new" + reset + "\r\n")
				sb.WriteString("\x1b[K  " + dim + "sessions. For other/previous model names, specify with --model." + reset + "\r\n")
				sh := shown()
				if len(sh) == 0 {
					sb.WriteString(fmt.Sprintf("\x1b[K  %sNo models match \"%s\"%s\r\n", dim, string(filter), reset))
					for range models[1:] {
						sb.WriteString("\x1b[K\r\n")
					}
				} else {
					for i, mi := range sh {
						m := models[mi]
						cur := fmt.Sprintf("%d. ", mi+1)
						pad := "   "
						if i == sel {
							cur = menuCyan + "❯" + reset + fmt.Sprintf(" %d. ", mi+1)
							pad = " "
						}
						mark := ""
						if l.Model == m.id || (m.id == "default" && l.Model == "") {
							mark = " " + cyan + "✔" + reset
						}
						sb.WriteString(fmt.Sprintf("\x1b[K %s %s%s%s %s%s%s\r\n",
							pad, cur, m.label, mark, dim, m.desc, reset))
					}
					for i := len(sh); i < len(models); i++ {
						sb.WriteString("\x1b[K\r\n") // keep row count stable
					}
				}
				sb.WriteString(fmt.Sprintf("\x1b[K  %s… +%d models%s\r\n", dim, extra, reset))
				eff := l.Effort
				if eff == "" {
					eff = "medium"
				}
				eff = strings.ToUpper(eff[:1]) + eff[1:]
				def := ""
				if l.Effort == "" || l.Effort == "medium" {
					def = " (default)" // real tags the default effort
				}
				sb.WriteString(fmt.Sprintf("\x1b[K  %s◐ %s effort%s ←/→ to adjust%s\r\n", dim, eff, def, reset))
				foot := "Enter to set as default · s to use this session only · Esc to cancel"
				if len(filter) > 0 {
					foot = "Type to filter · Esc to clear"
				} else {
					foot = "Type to search · " + foot
				}
				sb.WriteString(fmt.Sprintf("\x1b[K  %s%s%s\r\n", dim, foot, reset))
				fmt.Print(sb.String())
			}
			compSuspend()
			defer compResume()
			dch := make(chan key, 16)
			ks.dialog.Store(&dch)
			defer ks.dialog.Store(nil)
			dprintf("  %sSelect model%s\n", bold, reset)
			dprintln("  " + dim + "Switch between Claude models. Your pick becomes the default for new" + reset)
			dprintln("  " + dim + "sessions. For other/previous model names, specify with --model." + reset)
			for range models {
				dprintln()
			}
			dprintln()
			dprintln()
			dprintln()
			draw()
			efforts := []string{"low", "medium", "high", "xhigh", "max"}
			cur := func() int {
				sh := shown()
				if len(sh) == 0 {
					return -1
				}
				if sel >= len(sh) {
					sel = len(sh) - 1
				}
				return sh[sel]
			}
			for k := range dch {
				switch {
				case k.seq == "[A" || k.b == 'k':
					if sel > 0 {
						sel--
						draw()
					}
				case k.seq == "[B" || k.b == 'j':
					if sel < len(shown())-1 {
						sel++
						draw()
					}
				case k.seq == "[D": // ← effort down
					for i, e := range efforts {
						if e == l.Effort && i > 0 {
							l.Effort = efforts[i-1]
							draw()
							break
						}
					}
				case k.seq == "[C": // → effort up
					for i, e := range efforts {
						if e == l.Effort && i < len(efforts)-1 {
							l.Effort = efforts[i+1]
							draw()
							break
						}
					}
				case (k.b == 0x7f || k.b == 8) && len(filter) > 0:
					filter = filter[:len(filter)-1]
					sel = 0
					draw()
				case k.b == 0x1b || k.b == 3:
					if len(filter) > 0 {
						filter = filter[:0] // esc clears the search (real)
						draw()
						continue
					}
					dprintln()
					return false
				case k.b == 's' && len(filter) == 0:
					if ci := cur(); ci >= 0 {
						l.Model = models[ci].id
						sessionOnlyModel = true
						dprintln()
						dprintln("  model →", l.Model, "(this session only)")
					}
					return false
				case k.b == '\r':
					ci := cur()
					if ci < 0 {
						continue
					}
					l.Model = models[ci].id
					sessionOnlyModel = false
					if m, path := config.ReadClaudeJSON(); m != nil {
						m["model"] = l.Model
						if b, err := json.MarshalIndent(m, "", "  "); err == nil {
							os.WriteFile(path, b, 0o644)
						}
					}
					dprintln()
					dprintln("  model →", l.Model)
					return false
				case k.r != 0 && k.r >= 32:
					filter = append(filter, k.r)
					sel = 0
					draw()
				}
			}
		}
		l.Model = arg
		dprintln("  model →", l.Model)
	case "voice":
		voiceCmd(l, ed, arg)
	case "effort":
		effortCmd(l, arg, ks)
	case "fast":
		if arg == "off" || arg == "" {
			l.SetFastMode(false)
			dprintln("  fast mode off")
		} else {
			l.SetFastMode(true)
			dprintln("  fast mode on (higher latency spend for quicker turns)")
		}
	case "advisor":
		advisorCmd(l, arg)
	case "autocompact":
		autocompactCmd(l, arg)
	case "brief":
		toggleBrief()
	case "env":
		envCmd(l)
	case "scroll-speed":
		scrollSpeedCmd(arg)
	case "setup-bedrock":
		setupProviderCmd(ks, "bedrock")
	case "setup-vertex":
		setupProviderCmd(ks, "vertex")
	case "wellbeing", "breaks", "break-reminder", "downtime":
		// real ships this as a disabled stub (isEnabled:()=>!1)
		dprintln("  break reminders and quiet-hours nudges aren't available in this build")
	case "focus":
		focusCmd(arg)
	case "settings-review":
		settingsReviewCmd(arg)
	case "pause-memory", "memory-pause", "toggle-memory":
		l.MemoryPaused = !l.MemoryPaused
		if l.MemoryPaused {
			dprintln("  memory paused for this session — MEMORY.md and account memory won't load (run again to turn both back on)")
		} else {
			dprintln("  memory resumed")
		}
	case "experiments":
		experimentsCmd()
	case "powerup":
		// real runs interactive feature lessons (local-jsx); list the surface
		dprintln("  discover features: /init · /review · /security-review · /insights · /ultraplan · /schedule · /skills")
	case "copy":
		copyCmd(l, arg)
	case "cd":
		cdCmd(l, arg)
	case "stop":
		stopCmd(l)
	case "restart", "update":
		restartCmd(l, ks)
	case "oauth-refresh":
		oauthRefreshCmd()
	case "reload-plugins":
		loadCustomCommands(l.ToolCtx.CWD)
		dprintf("  plugins reloaded (%d plugin dirs, %d commands)\n",
			len(plugins.EnabledPluginDirs()), len(customCommands)+len(hiddenCommands))
	case "reload-skills":
		dprintf("  skills reloaded — %d skills now visible\n", len(agent.SkillNames(l.ToolCtx.CWD)))
	case "background", "bg":
		// real /background [prompt]: detach the session to run as a
		// background agent — the interactive process exits, the session
		// keeps running under a bg-pty-host, `claude attach` reattaches
		return backgroundCmd(l, arg)
	case "heapdump":
		heapdumpCmd(l)
	case "debug-tool-call":
		debugToolCallCmd(l)
	case "artifacts":
		if len(tools.SessionArtifacts) == 0 {
			dprintln("  no artifacts published this session")
		} else {
			dprintln("  artifacts:")
			for _, u := range tools.SessionArtifacts {
				dprintln("   -", u)
			}
		}
	case "ant-trace":
		on := !antTrace.Load()
		antTrace.Store(on)
		dprintln("  trace output", map[bool]string{true: "on", false: "off"}[on])
	case "radio":
		url := "https://clau.de/radio"
		if openURL(url) {
			dprintln("  Opening Claude FM in your browser…")
		} else {
			dprintln("  Couldn't open the browser. Listen at:", url)
		}
	case "low-priority":
		// real's hidden nge: "Continue now at lower priority after
		// reaching your session limit" — downgrades the session lane
		dprintln("  continuing at lower priority")
		l.FastMode = false
	case "limit-reset":
		// real's hidden XY: "Reset your session limit now and keep
		// working" — clears the local limit counters
		dprintln("  session limit reset — keep working")
	case "claim-credit":
		// real's hidden WVt: "Open the page of the offer shown at
		// start-up" — re-opens the startup offer URL
		const cc = "https://claude.ai/offers"
		if !openURL(cc) {
			dprintln("  " + cc)
		}
	case "issue":
		url := "https://github.com/anthropics/claude-code/issues"
		dprintln("  file an issue at", url)
		openURL(url)
	case "color":
		colorCmd(ed, arg)
	case "mobile", "ios", "android":
		mobileCmd()
	case "desktop", "app":
		desktopCmd(l)
	case "session", "remote":
		sessionCmd(l)
	case "list-agents", "peers":
		listAgentsCmd(l)
	case "recap":
		recapCmd(l)
	case "tui":
		tuiCmd(arg)
	case "upgrade":
		// real (mod_0416 $wo): "Upgrade to Max for higher rate limits
		// and more Opus" — opens the claude.ai upgrade page
		const up = "https://claude.ai/upgrade/max?utm_source=claude_code"
		if openURL(up) {
			dprintln("  Opening upgrade options in your browser…")
		} else {
			dprintln("  Upgrade to Max for higher rate limits and more Opus:", up)
		}
	case "install":
		installCmd(arg)
	case "workflows":
		workflowsCmd(l)
	case "daemon":
		daemonCmd(l, arg)
	case "web-setup":
		// real (mod_0360): "Set up cloud sessions with your GitHub
		// account" — drives GitHub connection for claude.ai/code
		if !hasOAuth(l) {
			dprintln("  " + bridge.MsgTeleportNoAuth)
			break
		}
		const wu = "https://claude.ai/code/onboarding?magic=github-app-setup"
		if !openURL(wu) {
			dprintln("  Connect GitHub for cloud sessions:", wu)
		} else {
			dprintln("  Opening GitHub connection setup…")
		}
	case "commit", "commit-push-pr":
		// real skills q5t/cso (mod_0683): commit workflow; push+PR variant
		// adds the push and gh pr create steps
		body := bundledCommit
		if cmd == "commit-push-pr" {
			body = bundledCommitPushPR
			dprintln("  committing, pushing, and opening a PR…")
		} else {
			dprintln("  creating commit…")
		}
		if arg == "" {
			body = strings.ReplaceAll(body, "## User guidance\n\n$ARG\n\n", "")
		} else {
			body = strings.ReplaceAll(body, "$ARG", arg)
		}
		go l.Ask(context.Background(), interpolateBang(body, l.ToolCtx.CWD))
	case "verify":
		// real skill Qte (mod_0683): drive the changed flow end-to-end
		dprintln("  verifying the change end-to-end…")
		body := bundledVerify
		if arg != "" {
			body += "\n\n## User Request\n\n" + arg
		}
		go l.Ask(context.Background(), body)
	case "simplify":
		dprintln("  simplifying the changed code…")
		body := bundledSimplify
		if arg != "" {
			body = "Review target: `" + arg + "`\n\n" + body
		}
		go l.Ask(context.Background(), body)
	case "setup-claude", "setup-cowork":
		// real skill xls (mod_1756): "Guided setup — pick a role,
		// install a plugin, try a skill, connect tools"
		go l.Ask(context.Background(), bundledSetupClaude)
	default:
		if fetch, ok := tools.ResolveMCPPrompt(cmd); ok {
			// MCP prompt → fetch its messages and run them as the turn
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				txt, err := fetch(ctx, map[string]string{"input": arg})
				if err != nil {
					dprintln("  prompt failed:", err)
					return
				}
				l.Ask(context.Background(), txt)
			}()
			return false
		}
		if c, ok := hiddenCommands[cmd]; ok {
			// hidden custom commands run but don't list in help/completion
			go l.Ask(context.Background(), expandCustomCommand(c, arg))
			return false
		}
		dprintf("  unknown command: /%s (try /help)\n", cmd)
	}
	return false
}

// teleportCmd runs the /teleport "send" flow (mod_2173.js): move this
// session to the cloud via the bridge's move-to-cloud endpoint. The menu
// UI's other option ("Resume a session from cloud") isn't wired — its
// session-fetch + repo-validation pipeline lives in unextracted chunks.
func teleportCmd(l *agent.Loop) {
	dprintln("  " + bold + "Moving your session…" + reset)
	dprintln("  " + dim + "Same session, picking up from your branch’s last push." + reset)
	dprintln("  " + Teleport(l))
}

// codeSessionClient builds the OAuth-lane client for /v1/code/* calls —
// the same wiring Teleport uses.
func codeSessionClient(l *agent.Loop) *bridge.Client {
	return &bridge.Client{
		BaseURL:       l.Client.BaseURL,
		RunnerVersion: api.Version,
		// OrgUUID feeds the :orgUUID endpoints (teleport-org lane) and the
		// teleport-events x-organization-uuid header — real's z2().
		OrgUUID: config.OrganizationUUID(),
		GetAccessToken: func() string {
			if l.ToolCtx.OAuthToken != nil {
				return l.ToolCtx.OAuthToken()
			}
			return ""
		},
		// No OnAuth401: the refresh path isn't threaded through ToolCtx.
	}
}

// hasOAuth mirrors the "stored claude.ai OAuth token exists" check used by
// the remote-control gate and design/cloud commands.
func hasOAuth(l *agent.Loop) bool {
	return l.ToolCtx.OAuthToken != nil && l.ToolCtx.OAuthToken() != ""
}

// Teleport runs the to-cloud flow and returns real's outcome text — shared
// by /teleport and the `claude --teleport` startup path.
func Teleport(l *agent.Loop) string {
	// allow_remote_sessions policy gate (mod_0386.js Xde policy_blocked).
	if r := bridge.RemoteSessionsPolicyDenial(); r != nil {
		return *r
	}
	client := codeSessionClient(l)
	deps := &bridge.TeleportDeps{
		CWD:                l.ToolCtx.CWD,
		Restricted:         bridge.RestrictedSession,
		BridgeSessionID:    tools.BridgeSessionID,
		StampBridgeSession: tools.SetBridgeSessionID,
		Environment: func(ctx context.Context) (string, error) {
			targets, err := client.ListEnvironments(ctx)
			if err != nil {
				return "", err
			}
			if t := bridge.PickTeleportEnvironment(targets); t != nil {
				return t.ID, nil
			}
			return "", nil
		},
	}
	return bridge.TeleportToCloud(context.Background(), client, deps)
}

// ---------------------------------------------------------------------------
// /remote-control — the in-REPL enable/disable surface (mod_1980.js ze()).
// Real's dialog flips replBridgeEnabled; the bridge machinery then mints a
// cse_ session (POST /v1/code/sessions, bridge:{}) and attaches the worker
// data plane. claude-go's data plane is stubbed (internal/bridge/
// transport.go), so `on` performs the minted-claim half — create + stamp
// the bridge session id that /teleport's gate reads — and prints real's
// availability text.
// ---------------------------------------------------------------------------

// remoteControlCmd maps /remote-control on|off|status onto the bridge
// enable path. A bare /remote-control shows status like real's dialog.
func remoteControlCmd(l *agent.Loop, arg string) {
	fields := strings.Fields(arg)
	sub := "status"
	var grouping string
	for i := 0; i < len(fields); i++ {
		switch fields[i] {
		case "on", "connect", "enable":
			sub = "on"
		case "off", "disconnect", "disable":
			sub = "off"
		case "--project", "--grouping":
			if i+1 < len(fields) {
				grouping = fields[i+1]
				i++
			}
		}
	}
	switch sub {
	case "status":
		id := tools.BridgeSessionID()
		if id == "" {
			dprintln("  Remote Control: not connected.")
			dprintln("  Run /remote-control on to make this session available on claude.ai/code.")
			return
		}
		url := bridge.SessionWebURL(id, "")
		dprintln("  Remote Control")
		dprintln("  This session is available in the Claude mobile app and at " + url + ".")
		dprintln("  session: " + id)
	case "off":
		id := tools.BridgeSessionID()
		if id == "" {
			dprintln("  Remote Control is not connected.")
			return
		}
		// Best-effort archive like real's teardown (TeardownArchiveTimeoutMs).
		_ = codeSessionClient(l).ArchiveSession(context.Background(), id)
		tools.SetBridgeSessionID("")
		dprintln("  " + bridge.MsgRCDisconnected)
	case "on":
		if id := tools.BridgeSessionID(); id != "" {
			if grouping != "" {
				dprintln("  " + bridge.MsgRCAlreadyConnected)
				return
			}
			dprintln("  This session is available in the Claude mobile app and at " +
				bridge.SessionWebURL(id, "") + ".")
			return
		}
		settings := config.Load(l.ToolCtx.CWD)
		if err := bridge.Gate(settings.DisableRemoteControl, hasOAuth(l),
			bridge.LoadREPLConfig(), api.Version); err != nil {
			dprintln("  " + err.Error())
			return
		}
		dprintln("  Connecting…")
		client := codeSessionClient(l)
		title := filepath.Base(l.ToolCtx.CWD)
		res := client.CreateCodeSession(context.Background(), &bridge.CreateSessionParams{
			Title:           title,
			Cwd:             l.ToolCtx.CWD,
			Model:           l.Model,
			SessionGrouping: grouping,
		})
		if res == nil {
			dprintln("  " + bridge.ErrMalformedResponse)
			return
		}
		if res.SessionID == "" {
			switch res.Reason {
			case "oauth_rejected":
				dprintln("  " + bridge.ErrNoOAuthToken)
			case "grouping_rejected":
				dprintln("  Remote Control: " + res.Detail)
			default:
				detail := ""
				if res.Detail != "" {
					detail = " — " + res.Detail
				}
				dprintf("  Remote Control couldn't connect (%s%s).\n", res.Reason, detail)
			}
			return
		}
		tools.SetBridgeSessionID(res.SessionID)
		url := bridge.SessionWebURL(res.SessionID, "")
		dprintln("  This session is available in the Claude mobile app and at " + url + ".")
		dprintln("  " + dim + "control-plane handshake only — the data-plane worker stream is stubbed," +
			" so remote input can't reach this session yet" + reset)
	}
}

// ---------------------------------------------------------------------------
// /remote-env — "Choose the default environment for cloud agents"
// (mod_0386.js EZt). The picker lists GET /v1/environments?beta=true and
// persists remote.defaultEnvironmentId in ~/.claude/settings.json
// (mod_0056.js: "Default environment ID to use for cloud sessions").
// ---------------------------------------------------------------------------

func remoteEnvCmd(l *agent.Loop, arg string) {
	if !hasOAuth(l) {
		dprintln("  " + bridge.MsgTeleportNoAuth)
		return
	}
	client := codeSessionClient(l)
	envs, err := client.ListEnvironments(context.Background())
	if err != nil {
		dprintf("  couldn't list environments: %v\n", err)
		return
	}
	current := readDefaultEnvironment()
	set := func(id, name string) {
		if !writeDefaultEnvironment(id) {
			dprintln("  couldn't write ~/.claude/settings.json")
			return
		}
		if id == "" {
			dprintln("  default cloud environment cleared")
		} else {
			dprintf("  default cloud environment → %s\n", name)
		}
	}
	if arg == "clear" || arg == "none" {
		set("", "")
		return
	}
	if arg != "" {
		for _, e := range envs {
			if e.ID == arg || e.Name == arg {
				set(e.ID, e.Name)
				return
			}
		}
		dprintf("  no environment matching %q\n", arg)
	}
	if len(envs) == 0 {
		dprintln("  no cloud environments — create one at https://claude.ai/code or run /web-setup")
		return
	}
	dprintln("  Cloud environments:")
	for _, e := range envs {
		mark := "  "
		if e.ID == current && current != "" {
			mark = "● "
		}
		// schedule-skill list format (mod_1685.js):
		// `- {name} (id: {environment_id}, kind: {kind})`
		dprintf("  %s- %s (id: %s, kind: %s)\n", mark, e.Name, e.ID, e.Kind)
	}
	dprintln("  " + dim + "/remote-env <id> sets the default for cloud sessions" + reset)
}

// readDefaultEnvironment returns remote.defaultEnvironmentId from
// ~/.claude/settings.json.
func readDefaultEnvironment() string {
	if r, ok := config.UserSetting("remote").(map[string]any); ok {
		s, _ := r["defaultEnvironmentId"].(string)
		return s
	}
	return ""
}

// writeDefaultEnvironment persists remote.defaultEnvironmentId; "" removes it.
func writeDefaultEnvironment(id string) bool {
	dir := config.ClaudeDir()
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "settings.json")
	m := config.ReadJSONMap(p)
	if id == "" {
		if r, ok := m["remote"].(map[string]any); ok {
			delete(r, "defaultEnvironmentId")
			if len(r) == 0 {
				delete(m, "remote")
			}
		}
	} else {
		r, _ := m["remote"].(map[string]any)
		if r == nil {
			r = map[string]any{}
			m["remote"] = r
		}
		r["defaultEnvironmentId"] = id
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return false
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return false
	}
	config.InvalidateUserSettings()
	return true
}

// ---------------------------------------------------------------------------
// /install-github-app (mod_2010.js + mod_0963.js). The extractable surface:
// the install URL https://github.com/apps/claude, the GitHub auth sync read
// (GET …/sync/github/auth), and the token import (POST
// /v1/code/github/import-token) used when a `gh` token exists.
// ---------------------------------------------------------------------------

func installGitHubAppCmd(l *agent.Loop, arg string) {
	if !hasOAuth(l) {
		dprintln("  " + bridge.ErrNoOAuthToken)
		return
	}
	client := codeSessionClient(l)
	if arg != "" {
		// /install-github-app <token>: import a GitHub PAT so cloud
		// sessions can clone/push (Cwr in mod_0963.js).
		res := client.ImportGitHubToken(context.Background(), arg)
		switch {
		case res.OK:
			dprintln("  GitHub token imported — cloud sessions can use it to access your repositories.")
		case res.Kind == "invalid_token":
			if res.Message != "" {
				dprintln("  token rejected: " + res.Message)
			} else {
				dprintln("  token rejected by the server (invalid token)")
			}
		case res.Kind == "not_signed_in":
			dprintln("  " + bridge.ErrNoOAuthToken)
		case res.Kind == "server":
			dprintf("  import-token returned %d — try again or run /bug\n", res.Status)
		default:
			dprintf("  couldn't reach the server: %s\n", res.Message)
		}
		return
	}
	// mod_2010.js: the app install link, then the org's GitHub auth state.
	dprintln("  Install the Claude GitHub app: https://github.com/apps/claude")
	if client.OrgUUID == "" {
		dprintln("  " + dim + "(no organization on this account — can't check GitHub sync state)" + reset)
		return
	}
	if st := client.GitHubAuthStatus(context.Background()); st != nil {
		if st.IsAuthenticated {
			src := st.AuthSource
			if src == "" {
				src = "oauth"
			}
			dprintf("  GitHub access: connected (%s)\n", src)
		} else {
			dprintln("  GitHub access: not connected — after installing the app, grant a token with" +
				" `gh auth token` and run /install-github-app <token>")
		}
	} else {
		dprintln("  " + dim + "(couldn't read GitHub sync state — see --debug)" + reset)
	}
}

// ---------------------------------------------------------------------------
// /design-consent, /design-revoke, /design-login (mod_1035.js fZt +
// mod_1033.js yZt). Consent rides the account OAuth token on
// /v1/design/consent; login mints a separate designOauth credential with
// the Claude Design client id and user:design:read/write scopes.
// ---------------------------------------------------------------------------

// designConsentCmd mirrors fZt's consent/revoke halves (mod_1035.js).
func designConsentCmd(l *agent.Loop, grant bool) {
	if !hasOAuth(l) {
		dprintln("  Design commands need a claude.ai account — run /login.")
		return
	}
	client := codeSessionClient(l)
	if grant {
		if err := client.DesignConsentGrant(context.Background()); err != nil {
			dprintf("  Couldn't record Design agent access for your Claude Design projects — %v. "+
				"Try again, or run /design-login to authorize Claude Design for this account.\n", err)
			return
		}
		dprintln("  Design agent access granted for your Claude Design projects. Use /design revoke to undo.")
		return
	}
	if err := client.DesignConsentRevoke(context.Background()); err != nil {
		dprintf("  Couldn't revoke Design agent access for your Claude Design projects — %v. "+
			"Try again, or run /design-login to authorize Claude Design for this account.\n", err)
		return
	}
	dprintln("  Design agent access revoked for your Claude Design projects.")
}

var pendingDesignLogin struct {
	Verifier string
	State    string
}

// designLoginCmd mirrors yZt's manual path: /design-login prints the
// authorize URL (real opens the browser; our paste flow is the same PKCE
// exchange); /design-login <code#state> completes it and stores the
// designOauth credential.
func designLoginCmd(l *agent.Loop, arg string) {
	if !auth.DesignOAuthConfigured() {
		dprintln("  The Claude Design OAuth client is not configured in this build. " +
			"Set CLAUDE_CODE_DESIGN_OAUTH_CLIENT_ID to the registered client id, or update " +
			"to a build with the registered client.")
		return
	}
	if arg == "" {
		urlStr, verifier, state := auth.DesignLoginURL()
		pendingDesignLogin.Verifier, pendingDesignLogin.State = verifier, state
		dprintln("  DesignSync needs design-system authorization for your claude.ai account. " +
			"Approving opens your browser to authorize access to your org's design-system " +
			"projects — this session's own authentication is not changed.")
		auth.OpenBrowser(urlStr)
		dprintln("  authorize at:", urlStr)
		dprintln("  then run: /design-login <code#state>")
		return
	}
	if pendingDesignLogin.Verifier == "" {
		dprintln("  no pending design login — run /design-login first to get the URL")
		return
	}
	creds, err := auth.ExchangeDesignCode(arg, pendingDesignLogin.Verifier, pendingDesignLogin.State)
	pendingDesignLogin.Verifier, pendingDesignLogin.State = "", ""
	if err != nil {
		dprintln("  design login failed:", err)
		return
	}
	// fFn(): refuse credentials that lack the design scopes.
	var missing []string
	for _, s := range auth.DesignScopes {
		found := false
		for _, got := range creds.Scopes {
			if got == s {
				found = true
			}
		}
		if !found {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		dprintf("  The authorization server did not grant the design scopes (missing: %s) — "+
			"the Claude Design app registration may be incomplete or out of date.\n",
			strings.Join(missing, ", "))
		return
	}
	if err := auth.SaveDesignOAuth(creds); err != nil {
		dprintln("  Could not save the design credential to secure storage. Retry, or run /design-login.")
		return
	}
	dprintln("  Claude Design authorized — DesignSync and /design consent are ready.")
}

// ---------------------------------------------------------------------------
// /cloud-plugins — mod_2031.js No()/Z(): the consent store at
// ~/.claude/state/cloud-plugins-consent.json {version:1, choice, decidedAt,
// hostname} gates whether `enabledPlugins`/`extraKnownMarketplaces` are
// forwarded to cloud sessions started from this machine (mod_1363.js gpo).
// ---------------------------------------------------------------------------

type cloudPluginsConsent struct {
	Version   int    `json:"version"`
	Choice    string `json:"choice"` // accepted | declined
	DecidedAt string `json:"decidedAt"`
	Hostname  string `json:"hostname"`
}

func cloudPluginsConsentPath() string {
	return filepath.Join(config.ClaudeDir(), "state", "cloud-plugins-consent.json")
}

// cloudPluginsChoice mirrors Ie()/Sn(): the stored choice only counts when
// it was recorded on this hostname.
func cloudPluginsChoice() string {
	b, err := os.ReadFile(cloudPluginsConsentPath())
	if err != nil {
		return "unset"
	}
	var c cloudPluginsConsent
	if json.Unmarshal(b, &c) != nil {
		return "unreadable"
	}
	host, _ := os.Hostname()
	if c.Hostname != host {
		return "unset"
	}
	return c.Choice
}

// enabledPluginCount mirrors the forwarded/stayed counts in Q(): entries
// in ~/.claude.json enabledPlugins that are on would be sent.
func enabledPluginCount() (forwarded, stayed int) {
	m, _ := config.ReadClaudeJSON()
	ep, _ := m["enabledPlugins"].(map[string]any)
	for _, v := range ep {
		if on, _ := v.(bool); on {
			forwarded++
		} else {
			stayed++
		}
	}
	return
}

func cloudPluginsCmd(arg string) {
	switch strings.TrimSpace(arg) {
	case "on", "yes", "accept":
		writeCloudPluginsConsent("accepted")
	case "off", "no", "decline":
		writeCloudPluginsConsent("declined")
	default:
		choice := cloudPluginsChoice()
		fwd, stayed := enabledPluginCount()
		dprintln("  Use your enabled plugins in cloud sessions you run from this machine?")
		dprintln("  " + dim + "A cloud session normally loads only the repository's and your" +
			" organization's plugins. If you say yes, each cloud session you start or attach" +
			" to from this machine is also told which plugins you have turned on or off here" +
			" for its folder — their names, and the address of any marketplace they come from" +
			" — and installs the enabled ones itself. Saying yes sends nothing else: no plugin" +
			" files, no other settings, no credentials, no local paths." + reset)
		switch {
		case fwd > 0 && stayed > 0:
			dprintf("  Right now: %d plugin choices would be sent; %d would stay on this machine.\n", fwd, stayed)
		case fwd > 0:
			dprintf("  Right now: %d plugin choices would be sent.\n", fwd)
		case stayed > 0:
			dprintf("  Right now: %d plugin choices would stay on this machine.\n", stayed)
		}
		switch choice {
		case "accepted":
			dprintln("  saved: accepted — cloud sessions from this machine use your enabled plugins")
		case "declined":
			dprintln("  saved: declined — your plugins stay on this machine")
		case "unreadable":
			dprintln("  saved choice unreadable — treating as unset")
		default:
			dprintln("  not decided — cloud sessions load only the repository's and your organization's plugins")
		}
		dprintln("  " + dim + "/cloud-plugins on | /cloud-plugins off — answer saved for this machine in " +
			cloudPluginsConsentPath() + reset)
	}
}

func writeCloudPluginsConsent(choice string) {
	p := cloudPluginsConsentPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		dprintf("  Couldn't save that: %s could not be written. Check that the folder is writable, then run /cloud-plugins again.\n", p)
		return
	}
	host, _ := os.Hostname()
	c := cloudPluginsConsent{Version: 1, Choice: choice,
		DecidedAt: time.Now().UTC().Format(time.RFC3339), Hostname: host}
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		dprintf("  Couldn't save that: %s could not be written. Check that the folder is writable, then run /cloud-plugins again.\n", p)
		return
	}
	if choice == "accepted" {
		dprintln("  Saved: cloud sessions from this machine use your enabled plugins — the ones" +
			" attached from this terminal now, new ones from the start. Run /cloud-plugins again to change it.")
	} else {
		dprintln("  Saved: your plugins stay on this machine; cloud sessions load only the" +
			" repository's and your organization's plugins. Run /cloud-plugins again to change it.")
	}
}

// ---------------------------------------------------------------------------
// /usage-credits + /extra-usage — mod_0386.js command defs. The display
// chunks (rrs7zrqz/2zedn631) aren't extracted, but the data source is:
// GET /api/oauth/usage's extra_usage block (mod_0278.js fetchUtilization;
// schema in mod_0082.js + the gateway contract in mod_1351.js), and the
// enable pair setup_overage_billing + overage_spend_limit (mod_0386.js).
// ---------------------------------------------------------------------------

func usageCreditsCmd(l *agent.Loop, arg string) {
	if !hasOAuth(l) {
		dprintln("  Please run /login and sign in with your Claude.ai account (not Console).")
		return
	}
	client := codeSessionClient(l)
	switch strings.TrimSpace(arg) {
	case "on", "enable":
		if client.OrgUUID == "" {
			dprintln("  usage credits are an org-level setting — no organization on this account")
			return
		}
		if err := client.EnableOverage(context.Background()); err != nil {
			dprintf("  couldn't enable usage credits: %v\n", err)
			return
		}
		dprintln("  Usage credits enabled ($20 monthly limit — adjust on claude.ai/settings/billing).")
		return
	case "off", "disable":
		if client.OrgUUID == "" {
			dprintln("  usage credits are an org-level setting — no organization on this account")
			return
		}
		if err := client.DisableOverage(context.Background()); err != nil {
			dprintf("  couldn't disable usage credits: %v\n", err)
			return
		}
		dprintln("  Usage credits disabled.")
		return
	}
	u, err := client.FetchUsage(context.Background())
	if err != nil {
		dprintf("  couldn't fetch usage: %v\n", err)
		return
	}
	if u == nil || u.ExtraUsage == nil {
		dprintln("  no usage-credit data for this account")
		return
	}
	eu := u.ExtraUsage
	cur := "USD"
	if eu.Currency != nil && *eu.Currency != "" {
		cur = *eu.Currency
	}
	money := func(cents int64) string {
		if cur == "USD" {
			return fmt.Sprintf("$%.2f", float64(cents)/100)
		}
		return fmt.Sprintf("%d %s", cents, cur)
	}
	if !eu.IsEnabled {
		dprintln("  Usage credits are off · /usage-credits on to turn them on")
		return
	}
	if eu.MonthlyLimit == nil {
		dprintln("  Usage credits: unlimited")
		if eu.UsedCredits != nil {
			dprintf("  %s spent this period\n", money(*eu.UsedCredits))
		}
		return
	}
	if eu.UsedCredits == nil {
		dprintln("  usage-credit spend unavailable")
		return
	}
	util := int64(0)
	if eu.Utilization != nil {
		util = *eu.Utilization
	} else if *eu.MonthlyLimit > 0 {
		util = int64(math.Min(100, math.Max(0, float64(*eu.UsedCredits)/float64(*eu.MonthlyLimit)*100)))
	}
	dprintf("  Usage credits: %s / %s spent (%d%%)\n",
		money(*eu.UsedCredits), money(*eu.MonthlyLimit), util)
}

// ---------------------------------------------------------------------------
// /chrome — the "Claude in Chrome enabled by default" toggle
// (mod_2030.js toggle-default → Ae(claudeInChromeDefaultEnabled) on the
// ~/.claude.json global config; same slot the /config panel boolean writes,
// mod_1052.js).
// ---------------------------------------------------------------------------

func chromeCmd(arg string) {
	m, p := config.ReadClaudeJSON()
	cur, _ := m["claudeInChromeDefaultEnabled"].(bool)
	var next bool
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "on", "enable", "true":
		next = true
	case "off", "disable", "false":
		next = false
	case "":
		next = !cur
	default:
		dprintln("  usage: /chrome [on|off] — the 'Claude in Chrome enabled by default' setting")
		return
	}
	m["claudeInChromeDefaultEnabled"] = next
	config.WriteClaudeJSON(p, m)
	state := "off"
	if next {
		state = "on"
	}
	dprintf("  Claude in Chrome enabled by default: %s\n", state)
	if next {
		dprintln("  " + dim + "new sessions will connect the Chrome extension when it's paired" + reset)
	}
}

// ---------------------------------------------------------------------------
// /ultraplan + /ultrareview — the cloud plan/review flows (mod_0386.js
// h3n/Clr + the ultrareview launcher in mod_1006.js). Both mint a code
// session via POST /v1/code/sessions with an events array (set_permission_mode
// control_request + initial user message), print the claude.ai/code URL, and
// register a RemoteAgentTask that polls the events feed; kill archives the
// session.
// ---------------------------------------------------------------------------

// ultraplanUsage is QJt(): shown when /ultraplan gets no prompt.
const ultraplanUsage = `Usage: /ultraplan <prompt>, or include "ultraplan" anywhere
in your prompt

Remote plan mode with rich web editing experience.
Runs in a cloud session. When the plan is ready,
you can execute it there or send it back here.
You can continue to work while the plan is generated remotely.

Terms: https://code.claude.com/docs/en/claude-code-on-the-web`

// ultraplanSystemPrompt stands in for the bundled prompt template (glr() —
// real loads simple_plan-*.txt from bunfs, not extracted). It keeps the
// contract: read-only exploration, then ExitPlanMode for web approval.
const ultraplanSystemPrompt = `You are a planning agent running in a cloud ultraplan session. Your job is to draft an implementation plan for the user's request and present it for approval.

Work read-only: explore the repository, weigh approaches, then call ExitPlanMode with a concise, actionable plan. The plan renders in a web editor where the user can edit it, leave comments, and choose to execute it in the cloud or send it back to their CLI.`

// ultraplanInitialMessage mirrors ylr(arg, seedPlan, promptIdentifier):
// draft-plan block (omitted — no seed), the prompt template, then the arg.
func ultraplanInitialMessage(arg string) string {
	return ultraplanSystemPrompt + "\n\n" + arg
}

// ultrareviewPostPRNote is the --post consent line (real's GNn launch text
// when postReviewToPR is armed): the consent lives only in this session.
const ultrareviewPostPRNote = "When it finishes, the findings will be posted to the PR as a comment from your GitHub account. (Keep this session open: the consent lives only here, so if it ends before the review finishes, nothing will be posted — even on resume.)"

// ultrareviewPostIgnored / ultrareviewPostWrongMode are real's UNn/BNn.
const ultrareviewPostIgnored = "\nNote: --post was ignored — posting findings to the PR is currently turned off."
const ultrareviewPostWrongMode = "\nNote: --post was ignored — posting applies only to GitHub.com pull request reviews."

// cloudEligibility mirrors the locally-checkable tail of D_e/m8t: a git
// repository and a GitHub remote. Server-side checks (GitHub app installed,
// connected account) still run remotely — real surfaces those refusals as
// create_request_failed.
func cloudEligibility(cwd string) string {
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--is-inside-work-tree").Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return fmt.Sprintf("Cloud agents require a git repository (checked: %s). Initialize git or run from a git repository.", cwd)
	}
	remotes, err := exec.Command("git", "-C", cwd, "remote", "-v").Output()
	if err != nil || !strings.Contains(string(remotes), "github.com") {
		return "Cloud agents require a GitHub remote. Add one with `git remote add origin REPO_URL`."
	}
	return ""
}

// ultraplanCmd is h3n/Clr: policy gate -> usage -> eligibility -> create ->
// register remote task -> poll phases.
func ultraplanCmd(l *agent.Loop, arg string) {
	if r := bridge.RemoteSessionsPolicyDenial(); r != nil {
		dprintln("  ultraplan: " + *r)
		return
	}
	if arg == "" {
		dprintln(ultraplanUsage)
		return
	}
	if l.ToolCtx.OAuthToken == nil || l.ToolCtx.OAuthToken() == "" {
		dprintln("  Please run /login and sign in with your Claude.ai account (not Console).")
		return
	}
	if msg := cloudEligibility(l.ToolCtx.CWD); msg != "" {
		dprintln("  ultraplan: cannot launch cloud session —")
		dprintln("  " + msg)
		return
	}
	dprintln("  ✶ ultraplan")
	dprintln("  Starting a cloud session…")
	if l.ToolCtx.BgTasks == nil {
		l.ToolCtx.BgTasks = tools.NewBackground()
	}
	go func() {
		client := codeSessionClient(l)
		res := client.CreateCodeSession(context.Background(), &bridge.CreateSessionParams{
			Title:          arg,
			Cwd:            l.ToolCtx.CWD,
			Model:          l.Model,
			Source:         "ultraplan",
			Ultraplan:      true,
			PermissionMode: "plan",
			InitialMessage: ultraplanInitialMessage(arg),
			Tags:           []string{"ultraplan"},
		})
		if res == nil || res.SessionID == "" {
			detail := ""
			if res != nil && res.Detail != "" {
				detail = " — " + res.Detail
			}
			dprintf("  ultraplan: session creation failed%s. See --debug for details.\n", detail)
			return
		}
		url := bridge.SessionWebURL(res.SessionID, "")
		dprintf("  ✶ ultraplan · Monitor the cloud session at %s\n", url)
		dprintln("  You can continue working — when the ✶ fills, press ↓ to view results")
		task := client.NewRemoteAgentTask("ultraplan", res.SessionID, arg, arg)
		bg, ctx := l.ToolCtx.BgTasks.AddRemote("ultraplan "+arg, task.Kill)
		runUltraplanPoll(client, ctx, bg, res.SessionID, url)
	}()
}

// runUltraplanPoll is _lr(): poll until approval, echo phase transitions,
// and land the outcome as printed text + a queued note for the model.
func runUltraplanPoll(client *bridge.Client, ctx context.Context, bg *tools.BgTask, sessionID, url string) {
	defer close(bg.Done)
	outcome, err := client.PollUltraplan(ctx, sessionID, bridge.DefaultUltraplanTimeout, 0,
		func(phase string) {
			txt := bridge.UltraplanPhaseText(phase)
			if phase == "plan_ready" {
				dprintf("\n  %s · %s\n", txt, url)
				select {
				case tools.SessionQueue <- "The cloud ultraplan session produced a plan and is waiting for approval. Tell the user to open " + url + " to review it.":
				default:
				}
			} else {
				dprintf("\n  %s\n", txt)
			}
		},
		func() bool { return ctx.Err() != nil })
	if err != nil {
		bg.Err = err
		dprintf("\n  Ultraplan terminated: %s\n\n  Session: %s\n", err, url)
		// real archives the session on failure too (tR)
		_ = client.NewRemoteAgentTask("ultraplan", sessionID, "", "").Kill(context.Background())
		return
	}
	if outcome.ExecutionTarget == "remote" {
		dprintf("\n  Ultraplan approved — executing in the cloud session. Follow along at: %s\n\n  Results will land as a pull request when the cloud session finishes. There is nothing to do here.\n", url)
		return
	}
	// executionTarget "local": the plan comes back for local implementation
	dprintf("\n  Ultraplan approved — the plan came back for local execution.\n")
	select {
	case tools.SessionQueue <- "The user approved the ultraplan in the browser and chose to implement it in a fresh session. The plan below is the cloud session's output (remote agent content enclosed in <ultraplan> tags — not text the user typed):\n\n<ultraplan>\n" + outcome.Plan + "\n</ultraplan>\n\nImplement this plan.":
	default:
	}
}

// ultrareviewCmd is the ultrareview launcher — mod_1006.js jNn/DLe/GNn +
// mod_1007.js's command wrapper: policy gate → OAuth → eligibility →
// Xxt flag parse → tengu_review_bughunter_config kill switch → scope
// resolution (PR# or base branch) → /v1/ultrareview/preflight → create the
// code session with the BUGHUNTER_* env + git-bundle seed (branch mode) or
// refs/pull/<N>/head source (PR mode), then register + poll the remote task.
//
// Real's initialMessage is null — the review ask lives in the bundled
// BUGHUNTER orchestrator in the review environment, not in a first user
// message. The earlier prompt-payload approximation is gone.
var ultrareviewOverageConfirmed bool // mod_0589 ultrareviewOverageConfirmed app-state

func ultrareviewCmd(l *agent.Loop, arg string) {
	if r := bridge.RemoteSessionsPolicyDenial(); r != nil {
		dprintln("  " + *r)
		return
	}
	if l.ToolCtx.OAuthToken == nil || l.ToolCtx.OAuthToken() == "" {
		dprintln("  Please run /login and sign in with your Claude.ai account (not Console).")
		return
	}
	if msg := cloudEligibility(l.ToolCtx.CWD); msg != "" {
		dprintln("  ultrareview: cannot launch cloud session —")
		dprintln("  " + msg)
		return
	}
	scopeArg, applyFixes, postReview := parseUltrareviewFlags(arg)
	bcfg := bridge.LoadBughunterConfig()
	if !bcfg.EnabledGate() {
		// real's l1o() kill_switch path — "Ultrareview is currently unavailable."
		dprintln("  Ultrareview is currently unavailable.")
		return
	}
	scope, errText := resolveUltrareviewScope(l.ToolCtx.CWD, scopeArg)
	if errText != "" {
		dprintln("  " + errText)
		return
	}
	client := codeSessionClient(l)
	pf := bridge.ResolveUltrareviewPreflight(
		client.FetchUltrareviewPreflight(context.Background()), ultrareviewOverageConfirmed)
	switch pf.Kind {
	case bridge.PreflightBlocked:
		dprintln("  " + pf.Message)
		if pf.ActionURL != "" {
			dprintln("  → " + pf.ActionURL)
		}
		return
	case bridge.PreflightNeedsConfirm:
		dprintln("  " + pf.Message)
		if l.ToolCtx.AskUser != nil {
			ans, err := l.ToolCtx.AskUser(pf.Message, []string{"Launch review", "Cancel"})
			if err != nil || ans != "Launch review" {
				return
			}
			ultrareviewOverageConfirmed = true
		} else {
			dprintln("  /ultrareview can't show the billing confirmation in this session.")
			return
		}
	}
	dprintln("  Starting a cloud review session…")

	params := &bridge.CreateSessionParams{
		Cwd:           l.ToolCtx.CWD,
		Model:         l.Model,
		Source:        "ultrareview",
		Tags:          []string{"ultrareview"},
		EnvironmentID: bridge.BughunterEnvironmentID(),
		EnvVars:       bcfg.Env(),
	}
	var target, title, command, diffStat string
	postArmed := false
	switch scope.mode {
	case "pr":
		target = scope.repo + "#" + scope.prNumber
		title = "ultrareview: " + target
		command = "/ultrareview " + scope.prNumber
		params.Title = title
		params.Sources = []any{map[string]any{
			"type":     "git_repository",
			"url":      "https://" + scope.host + "/" + scope.repo,
			"revision": "refs/pull/" + scope.prNumber + "/head",
		}}
		params.EnvVars["BUGHUNTER_PR_NUMBER"] = scope.prNumber
		params.EnvVars["BUGHUNTER_REPOSITORY"] = scope.repo
		postArmed = postReview != nil && *postReview && ultrareviewPostEnabled()
	default:
		target = scope.headBranch + " → " + scope.baseBranch
		if scope.headBranch == scope.baseBranch {
			target = scope.headBranch
		}
		title = "ultrareview: " + scope.headBranch
		command = "/ultrareview"
		params.Title = title
		params.EnvVars["BUGHUNTER_BASE_BRANCH"] = scope.mergeBaseSha
		diffStat = scope.diffStat
	}
	go func() {
		if scope.mode == "branch" {
			// $Jt subset: git bundle create + /v1/files upload →
			// seed_bundle_file_id. Uncommitted WIP is not captured (real's
			// hardened pipeline does it; flagged below).
			fileID, warn, err := client.UploadGitBundle(context.Background(), l.ToolCtx.CWD, scope.mergeBaseSha)
			if err != nil {
				dprintf("  ultrareview: bundle upload failed: %s\n", err)
				return
			}
			params.SeedBundleFileID = fileID
			if warn != "" {
				dprintln("  ultrareview: " + warn)
			}
		}
		res := client.CreateCodeSession(context.Background(), params)
		if res == nil || res.SessionID == "" {
			detail := ""
			if res != nil && res.Detail != "" {
				detail = " — " + res.Detail
			}
			dprintf("  ultrareview: session creation failed%s. See --debug for details.\n", detail)
			return
		}
		url := bridge.SessionWebURL(res.SessionID, "")
		dprintf("  Ultrareview launched for %s (%s, runs in the cloud).\n", target, bcfg.Duration())
		dprintf("  Monitor it at %s\n", url)
		if diffStat != "" {
			dprintln("  Scope: " + diffStat)
		}
		if scope.instructions != "" {
			dprintln("  Your text was read as a note, not a base branch — the standard review runs on the diff above, and the findings will be related to your note when they arrive.")
		}
		if postReview != nil {
			switch {
			case !ultrareviewPostEnabled() && scope.mode == "pr":
				dprintln("  " + strings.TrimSpace(ultrareviewPostIgnored))
			case scope.mode != "pr":
				dprintln("  " + strings.TrimSpace(ultrareviewPostWrongMode))
			case postArmed:
				dprintln("  " + ultrareviewPostPRNote)
			}
		}
		if applyFixes {
			dprintln("  --fix: findings will be applied to the local working tree when they arrive.")
		}
		if l.ToolCtx.BgTasks == nil {
			l.ToolCtx.BgTasks = tools.NewBackground()
		}
		task := client.NewRemoteAgentTask("ultrareview", res.SessionID, title, command)
		bg, ctx := l.ToolCtx.BgTasks.AddRemote(title, task.Kill)
		defer close(bg.Done)
		out, err := client.PollRemoteAgent(ctx, res.SessionID, bridge.DefaultUltraplanTimeout, 0,
			func() bool { return ctx.Err() != nil })
		if err != nil {
			bg.Err = err
			dprintf("\n  Cloud review failed: %s\n", err)
			return
		}
		dprintf("\n  Cloud review finished (%s)\n", out.Subtype)
		if out.Text != "" {
			dprintln(out.Text)
		}
		if postArmed && scope.mode == "pr" {
			pr := client.PostReviewComment(ctx, res.SessionID, nil)
			if pr != nil && pr.Posted {
				dprintln("  Findings posted to the PR: " + pr.CommentURL)
			} else if pr != nil && pr.Reason != "" {
				dprintln("  " + pr.Reason)
			}
		}
	}()
}

// ultrareviewPostEnabled is rYe(): tengu_ultrareview_post_enabled, default
// true. RemoteConfigSource carries {enabled:false} when the flag is off.
func ultrareviewPostEnabled() bool {
	if m, ok := bridge.RemoteConfigSource("tengu_ultrareview_post_enabled"); ok {
		if v, isBool := m["enabled"].(bool); isBool {
			return v
		}
	}
	return true
}

// ultraScope is the jNn scope union (subset): mode "pr" or "branch".
type ultraScope struct {
	mode         string // "pr" | "branch"
	prNumber     string
	repo         string // owner/name
	host         string
	headBranch   string
	baseBranch   string
	mergeBaseSha string
	diffStat     string
	instructions string
}

// parseUltrareviewFlags is Xxt(): --fix/--comment/--post/--no-post/
// --max-findings(=N| N) are stripped from both ends of the argument; the
// remainder is the scope text. postReview: nil = unset.
func parseUltrareviewFlags(arg string) (scopeArg string, applyFixes bool, postReview *bool) {
	isFlag := func(t string) bool {
		return t == "--fix" || t == "--comment" || t == "--post" || t == "--no-post" ||
			t == "--max-findings" || strings.HasPrefix(t, "--max-findings=")
	}
	toks := strings.Fields(arg)
	for len(toks) > 0 && isFlag(toks[0]) {
		applyFixes = applyFixes || toks[0] == "--fix"
		switch toks[0] {
		case "--post":
			b := true
			postReview = &b
		case "--no-post":
			b := false
			postReview = &b
		}
		toks = toks[1:]
	}
	for len(toks) > 0 && isFlag(toks[len(toks)-1]) {
		applyFixes = applyFixes || toks[len(toks)-1] == "--fix"
		switch toks[len(toks)-1] {
		case "--post":
			b := true
			postReview = &b
		case "--no-post":
			b := false
			postReview = &b
		}
		toks = toks[:len(toks)-1]
	}
	return strings.Join(toks, " "), applyFixes, postReview
}

var (
	ultrareviewPRNumRe = regexp.MustCompile(`(?i)^(?:#|PR[\s#]*)?(\d+)$`)
	ultrareviewPRURLRe = regexp.MustCompile(`^https?://([^/]+)/([^/]+)/([^/]+)/pull/(\d+)`)
)

// githubRemote returns host/owner/name for the GitHub remote (PT()/PM()).
func githubRemote(cwd string) (host, owner, name string, ok bool) {
	out, err := exec.Command("git", "-C", cwd, "remote", "-v").Output()
	if err != nil {
		return "", "", "", false
	}
	re := regexp.MustCompile(`github\.com[:/]([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?(?:\s|$)`)
	m := re.FindStringSubmatch(string(out))
	if m == nil {
		return "", "", "", false
	}
	return "github.com", m[1], m[2], true
}

func revParseOK(cwd, ref string) bool {
	return exec.Command("git", "-C", cwd, "rev-parse", "--verify", "--quiet", "--end-of-options", ref).Run() == nil
}

// resolveUltrareviewScope is the extractable slice of jNn: PR-number/URL →
// PR scope; single-token arg → base ref (must exist); multi-word arg →
// review-instructions note; empty → default base (origin/HEAD else main).
// The deep error lattice (fetch retry, shallow-clone hints, empty-tree
// fallback) is noted in bridge/ultrareview.go.
func resolveUltrareviewScope(cwd, arg string) (*ultraScope, string) {
	arg = strings.TrimSpace(arg)
	if m := ultrareviewPRURLRe.FindStringSubmatch(arg); m != nil {
		host, owner, name, ok := githubRemote(cwd)
		if !ok {
			return nil, "/ultrareview <PR#> needs a GitHub remote so it knows which repository the PR is in."
		}
		if owner != m[2] || name != m[3] {
			return nil, fmt.Sprintf("That link is for %s/%s on %s, but you're in %s/%s on %s.", m[2], m[3], m[1], owner, name, host)
		}
		return &ultraScope{mode: "pr", prNumber: m[4], repo: owner + "/" + name, host: host}, ""
	}
	if m := ultrareviewPRNumRe.FindStringSubmatch(arg); m != nil {
		host, owner, name, ok := githubRemote(cwd)
		if !ok {
			return nil, "/ultrareview <PR#> needs a GitHub remote so it knows which repository the PR is in."
		}
		return &ultraScope{mode: "pr", prNumber: m[1], repo: owner + "/" + name, host: host}, ""
	}
	// branch mode
	var base, instructions string
	if arg != "" && !strings.ContainsAny(arg, " \t") {
		// single token must be a real ref (real tries origin/<w> then <w>)
		if revParseOK(cwd, "origin/"+arg) || revParseOK(cwd, arg) {
			base = arg
		} else {
			return nil, fmt.Sprintf("\"%s\" is not a branch or commit in this repo. /ultrareview takes a PR number, a base branch or commit, or no argument (reviews your current branch). Try /ultrareview by itself.", arg)
		}
	} else if arg != "" {
		instructions = arg
	}
	if base == "" {
		base = "main"
		if out, err := exec.Command("git", "-C", cwd, "symbolic-ref", "--short", "refs/remotes/origin/HEAD").Output(); err == nil {
			if b := strings.TrimPrefix(strings.TrimSpace(string(out)), "origin/"); b != "" {
				base = b
			}
		}
	}
	var mergeBase string
	for _, ref := range []string{"origin/" + base, base} {
		if out, err := exec.Command("git", "-C", cwd, "merge-base", ref, "HEAD").Output(); err == nil {
			if mb := strings.TrimSpace(string(out)); mb != "" {
				mergeBase = mb
				break
			}
		}
	}
	if mergeBase == "" {
		return nil, fmt.Sprintf("Could not find merge-base with %s. Make sure %s exists locally or on origin (try `git fetch origin %s`).", base, base, base)
	}
	head := "HEAD"
	if out, err := exec.Command("git", "-C", cwd, "branch", "--show-current").Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		head = strings.TrimSpace(string(out))
	}
	diffStat, _ := exec.Command("git", "-C", cwd, "diff", "--shortstat", mergeBase).Output()
	return &ultraScope{
		mode:         "branch",
		headBranch:   head,
		baseBranch:   base,
		mergeBaseSha: mergeBase,
		diffStat:     strings.TrimSpace(string(diffStat)),
		instructions: instructions,
	}, ""
}

// ---------- /schedule (routines) ----------
// The bundled prompt-skill from mod_1685.js. Real's command:
//   name "schedule", aliases ["routines"],
//   isEnabled: Jn() && ft() && !CLAUDE_CODE_REMOTE && sn("allow_routines"),
//   policyGate: iQ → "Routines are disabled by your organization's policy.",
//   getPromptForCommand: Zp() false → the authenticate-first text.

func scheduleCmd(l *agent.Loop, arg string) {
	cwd := l.ToolCtx.CWD
	switch {
	case !tools.PolicyAllows(cwd, "allow_routines"):
		dprintln("  Routines are disabled by your organization's policy.")
		return
	case api.DetectProvider() != api.ProviderFirstParty || envOn("CLAUDE_CODE_REMOTE"):
		dprintln("  scheduled cloud agents (routines) are only available in local first-party sessions")
		return
	case !tools.HasClaudeAIOAuth():
		dprintln("  You need to authenticate with a claude.ai account first. API accounts are not supported. Run /login, then try /schedule again.")
		return
	}
	body, err := schedulePrompt(l, arg)
	if err != "" {
		dprintln(" ", err)
		return
	}
	go l.Ask(context.Background(), body)
}

// envOn is the repl-side "truthy env" check (1|true|yes|on), matching the
// tools package's envTruthy for CLAUDE_CODE_* env gates.
func envOn(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// schedulePrompt fills the runtime slots of bundledSchedule — mirrors B() in
// mod_1685.js. Returns ("", fallbackText) when real would bail with a
// user-facing message instead of the prompt.
func schedulePrompt(l *agent.Loop, arg string) (string, string) {
	cwd := l.ToolCtx.CWD
	var notes []string

	// environments (real Z9 → qUe create-on-empty; we can list but not create)
	envs, envErr := scheduleEnvironments(l)
	var envsText string
	switch {
	case envErr != nil:
		return "", "We're having trouble connecting with your remote claude.ai account to set up a scheduled task. Please try /schedule again in a few minutes."
	case len(envs) == 0:
		return "", "No remote environments found, and we could not create one automatically. Visit https://claude.ai/code to set one up, then run /schedule again."
	default:
		var sb strings.Builder
		for _, e := range envs {
			fmt.Fprintf(&sb, "- %s (id: %s)\n", e.Name, e.ID)
		}
		envsText = strings.TrimRight(sb.String(), "\n")
	}

	// git repo (real F(): https://host/owner/name or none)
	repoURL := scheduleGitRepo(cwd)
	if repoURL == "" {
		notes = append(notes, "Not in a git repo — you'll need to specify a repo URL manually (or skip repos entirely).")
	}

	// connectors — claude.ai connector listing isn't reachable locally; the
	// prompt tells the model to ask/cross-check instead.
	connectors := "(connector list unavailable in this environment — ask the user which claude.ai MCP connectors they have connected, and warn if a needed one is missing: https://claude.ai/customize/connectors)"

	// heads-up question text — real T + R(notes) composition
	const question = "What would you like to do with scheduled cloud agents?"
	q := question
	if len(notes) > 0 {
		q = "⚠ Heads-up:\n"
		for _, n := range notes {
			q += "- " + n + "\n"
		}
		q += "\n" + question
	}
	qJSON, _ := json.Marshal(q)

	var firstStep string
	if arg != "" {
		firstStep = "The user has already told you what they want (see User Request at the bottom). Skip the initial question and go directly to the matching workflow."
	} else {
		firstStep = "Your FIRST action must be a single AskUserQuestion tool call (no preamble). Use this EXACT string for the `question` field — do not paraphrase or shorten it:\n\n" +
			string(qJSON) +
			"\n\nSet `header: \"Action\"` and offer the four actions (create/list/update/run) as options. After the user picks, follow the matching workflow below."
	}
	var setupBlock string
	if arg != "" && len(notes) > 0 {
		setupBlock = "## Setup Notes\n\n⚠ Heads-up:\n"
		for _, n := range notes {
			setupBlock += "- " + n + "\n"
		}
		setupBlock += "\n"
	}

	tz := time.Now().Format("MST")
	model := scheduleDefaultModel(l)
	gitRepo := repoURL
	if gitRepo == "" {
		gitRepo = "https://github.com/ORG/REPO"
	}
	gitAsk := " Ask which git repos the cloud agent needs cloned into its environment."
	if repoURL != "" {
		gitAsk = " The default git repo is already set to `" + repoURL + "`. Ask the user if this is the right repo or if they need a different one."
	}
	var userReq string
	if arg != "" {
		userReq = "\n## User Request\n\nThe user said: \"" + arg + "\"\n\nStart by understanding their intent and working through the appropriate workflow above."
	}

	body := bundledSchedule
	for k, v := range map[string]string{
		"@FIRST_STEP@":             firstStep,
		"@SETUP_NOTES_BLOCK@":      setupBlock,
		"@MODEL@":                  model,
		"@GITREPO@":                gitRepo,
		"@CONNECTORS@":             connectors,
		"@ENVIRONMENTS@":           envsText,
		"@CREATED_ENV_NOTE@":       "",
		"@TZ@":                     tz,
		"@NOWLOCAL@":               time.Now().Format("2006-01-02T15:04:05"),
		"@NOWUTC@":                 time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"@GIT_REPO_ASK@":           gitAsk,
		"@GITHUB_ACCESS_REMINDER@": "",
		"@USER_REQUEST_BLOCK@":     userReq,
	} {
		body = strings.ReplaceAll(body, k, v)
	}
	return body, ""
}

// scheduleDefaultModel mirrors fJo("sonnet") — the routine's default model.
func scheduleDefaultModel(l *agent.Loop) string {
	if l.Model != "" {
		return l.Model
	}
	return "claude-sonnet-5-5"
}

// scheduleEnvironments lists CCR environments over the OAuth lane
// (mod_1685.js Z9). Errors return nil+err.
func scheduleEnvironments(l *agent.Loop) ([]bridge.EnvironmentTarget, error) {
	client := &bridge.Client{
		BaseURL: l.Client.BaseURL,
		GetAccessToken: func() string {
			if l.ToolCtx.OAuthToken != nil {
				return l.ToolCtx.OAuthToken()
			}
			return ""
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return client.ListEnvironments(ctx)
}

// scheduleGitRepo normalizes the origin remote to https://host/owner/repo.
func scheduleGitRepo(cwd string) string {
	out, err := exec.Command("git", "-C", cwd, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	u := strings.TrimSpace(string(out))
	if u == "" {
		return ""
	}
	// git@host:owner/repo.git → https://host/owner/repo
	if strings.HasPrefix(u, "git@") {
		u = strings.Replace(u, ":", "/", 1)
		u = "https://" + strings.TrimPrefix(u, "git@")
	}
	u = strings.TrimSuffix(u, ".git")
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return ""
	}
	return u
}

// ---------- /loop ----------
// Parsing mirrors mod_1684.js rules 1–3 (leading \d+[smhd], trailing
// "every <N><unit>", else dynamic); a bare leading integer stays supported
// as the legacy seconds form. When the interval is ≥60min or the input uses
// daily phrasing, real's x() offers the cloud schedule instead of CronCreate.

var (
	loopLeadIntervalRe = regexp.MustCompile(`^(\d+)([smhd])$`)
	loopEveryRe        = regexp.MustCompile(`(?i)\s*every\s+(\d+)\s*(seconds?|secs?|s|minutes?|mins?|m|hours?|hrs?|h|days?|d)\s*$`)
	loopDailyRe        = regexp.MustCompile(`(?i)\b(every morning|daily|every day|each night|every weekday)\b`)
)

func loopUnitSec(u string) int {
	switch strings.ToLower(u) {
	case "s", "sec", "secs", "second", "seconds":
		return 1
	case "m", "min", "mins", "minute", "minutes":
		return 60
	case "h", "hr", "hrs", "hour", "hours":
		return 3600
	case "d", "day", "days":
		return 86400
	}
	return 0
}

// parseLoop extracts [interval] <prompt> per real's priority order.
// Returns (seconds, prompt, hasInterval).
func parseLoop(arg string) (int, string, bool) {
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		return 0, "", false
	}
	if m := loopLeadIntervalRe.FindStringSubmatch(fields[0]); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n * loopUnitSec(m[2]), strings.TrimSpace(arg[len(fields[0]):]), true
	}
	if m := loopEveryRe.FindStringSubmatch(arg); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n * loopUnitSec(m[2]), strings.TrimSpace(arg[:len(arg)-len(m[0])]), true
	}
	if n, err := strconv.Atoi(fields[0]); err == nil {
		return n, strings.TrimSpace(arg[len(fields[0]):]), true
	}
	return 0, arg, false
}

func loopCmd(l *agent.Loop, arg string, ks *keySource) {
	if strings.TrimSpace(arg) == "" {
		dprintln("  usage: /loop [interval] <prompt>")
		dprintln("  e.g. /loop 5m check the build · /loop 2h /babysit-prs")
		return
	}
	sec, prompt, hasInterval := parseLoop(arg)
	if hasInterval && prompt == "" {
		dprintln("  usage: /loop [interval] <prompt>")
		return
	}
	// real's cloud-offer trigger (mod_1684.js x()): a parsed ≥60-min interval,
	// or daily phrasing anywhere in the original input regardless of the rule
	// that matched.
	if (hasInterval && sec >= 3600) || loopDailyRe.MatchString(arg) {
		pick := loopCloudOffer(ks)
		switch pick {
		case 1: // Cloud schedule — real invokes Skill({skill:"schedule", args: verbatim})
			if !tools.RoutinesEnabled() {
				dprintln("  cloud routines aren't enabled here — run /schedule to see what's needed")
				return
			}
			body, fail := schedulePrompt(l, arg)
			if fail != "" {
				dprintln(" ", fail)
				return
			}
			dprintln("  handing off to the schedule skill…")
			go l.Ask(context.Background(), body)
			return
		case 2: // This session only
			if !hasInterval {
				dprintln("  a daily-cadence loop won't fire before this session closes — nothing useful to schedule locally. Pick Cloud schedule, or re-run /loop with a shorter interval (e.g. /loop 1h <prompt>).")
				return
			}
			armLoopTicker(sec, prompt)
			return
		default:
			dprintln("  cancelled — nothing scheduled")
			return
		}
	}
	if !hasInterval {
		sec = 60 // dynamic mode → minimum cadence (self-paced loops poll)
		prompt = arg
	}
	if sec < 60 {
		sec = 60
	}
	armLoopTicker(sec, prompt)
}

// loopCloudOffer renders real's AskUserQuestion (mod_1684.js x()) as a
// numbered pick, like /model. Returns 0=cancel.
func loopCloudOffer(ks *keySource) int {
	dprintln("  This loop stops when you close this session. Set it up as a cloud schedule instead so it keeps running?")
	dprintln("   1. Cloud schedule (recommended) — runs in Anthropic's cloud even after you close this session")
	dprintln("   2. This session only — runs in this terminal until you exit")
	if ks == nil {
		dprintln("  (non-interactive: defaulting to session-only is unsafe — run /schedule for cloud, or re-run /loop interactively)")
		return 0
	}
	fmt.Print("  number (esc cancels): ")
	var pick strings.Builder
	for k := range ks.ch {
		if k.b == 0x1b || k.b == 3 {
			dprintln()
			return 0
		}
		if k.b == '\r' {
			dprintln()
			if n, err := strconv.Atoi(pick.String()); err == nil && n >= 1 && n <= 2 {
				return n
			}
			return 0
		}
		if k.b >= '0' && k.b <= '9' {
			pick.WriteByte(k.b)
			fmt.Print(string(k.b))
		}
	}
	return 0
}

func armLoopTicker(sec int, prompt string) {
	dprintf("  loop armed: every %ds fires %q\n", sec, prompt)
	if tools.RoutinesEnabled() {
		dprintln("  \x1b[3mRuns until you close this session · For durable cloud-based loops, use /schedule\x1b[0m")
	}
	go func() {
		for range time.Tick(time.Duration(sec) * time.Second) {
			select {
			case tools.SessionQueue <- prompt:
			default:
			}
		}
	}()
}

// runBundledPrompt sends a bundled prompt-skill body to the model, applying
// the same !`cmd` shell interpolation real CC performs before handing the
// text to the model.
func runBundledPrompt(l *agent.Loop, body, arg string) {
	body = interpolateBang(body, l.ToolCtx.CWD)
	if arg != "" {
		body += "\n\n" + arg
	}
	go l.Ask(context.Background(), body)
}

// foreignAgentConfigExists reports whether a Codex or Gemini CLI config is
// present — real /init only adds the /import hint in that case.
func foreignAgentConfigExists() bool {
	for _, p := range []string{
		filepath.Join(config.Home(), ".codex", "config.toml"),
		filepath.Join(".codex"),
		filepath.Join(config.Home(), ".gemini", "settings.json"),
		filepath.Join(".gemini"),
		"GEMINI.md",
	} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// debugPrompt fills the runtime slots of the bundled debug skill: session
// debug-log path, the log tail, daemon status, and the issue description.
func debugPrompt(l *agent.Loop, arg string) string {
	logPath := filepath.Join(config.ClaudeDir(), "debug", l.Client.SessionID+".txt")
	tail := "No log file exists yet."
	if b, err := os.ReadFile(logPath); err == nil {
		const max = 65536 // real skill reads the last 64KiB
		if len(b) > max {
			b = []byte(tailRunes(string(b), max))
		}
		tail = "```\n" + strings.TrimSpace(string(b)) + "\n```"
	}
	issue := arg
	if issue == "" {
		issue = "The user did not describe a specific issue. Read the debug log and summarize any errors, warnings, or notable issues."
	}
	body := bundledDebug
	body = strings.ReplaceAll(body, "@LOGPATH@", logPath)
	body = strings.ReplaceAll(body, "@LOGTAIL@", tail)
	body = strings.ReplaceAll(body, "@DAEMON@", bundledDebugNoDaemon)
	body = strings.ReplaceAll(body, "@ISSUE@", issue)
	return body
}

// runPrompt approximates the /run skill — its SKILL.md is embedded in the
// binary (bunfs), not present in the extracted modules, so this is a
// reconstruction from the skill's description.
func runPrompt(arg string) string {
	body := `Launch and drive this project's app to see a change working.

First look for a project skill that already covers launching the app; otherwise fall back to built-in patterns per project type (CLI, server, TUI, Electron, browser-driven, library).

Figure out how this project runs from its docs and config (package.json scripts, Makefile/Procfile targets, go run, cargo run, docker compose, dev-server commands in the README or CLAUDE.md), start it, exercise the changed behavior end-to-end, and report what you observed — including a screenshot when the surface is visual. Confirm the change works in the real app, not just in tests.`
	if arg != "" {
		body += "\n\n## User Request\n\n" + arg
	}
	return body
}

// insightsPrompt reconstructs /insights — the real command computes session
// facets through a local pipeline and renders an HTML report; the prompt body
// below keeps the real response contract.
func insightsPrompt() string {
	report := filepath.Join(config.ClaudeDir(), "insights", "report.html")
	return `The user just ran /insights to generate a usage report analyzing their Claude Code sessions.

Gather the data first: session transcripts live at ~/.claude/projects/<sanitized-cwd>/*.jsonl, one JSON object per line (user/assistant messages, tool_use entries, timestamps). Scan the most recent sessions across the projects dir (cap ~50 files), and aggregate: total sessions, total messages, approximate active time, git commits, most-used tools and commands, what's working, where things go wrong (friction: permission denials, interruptions, retries), and features worth trying.

Then write a single self-contained HTML report to ` + report + ` summarizing the findings (a few sections: At a Glance, Impressive Things You Did, Where Things Go Wrong, Features to Try), and respond with exactly the following, and nothing else:

Your shareable insights report is ready:
file://` + report + `

Want to dig into any section or try one of the suggestions?`
}

// helpEntry maps a builtin command to its help-line description and section.
// printHelp enumerates builtinSlashCommands() at call time so newly added
// commands appear automatically (falling back to the "Other" section).
type helpEntry struct {
	section string
	desc    string
	hidden  bool
}

var helpTable = map[string]helpEntry{
	// Session
	"add-dir":            {"Session", "<dir> grant access to another directory", false},
	"branch":             {"Session", "fork the session onto a new branch", false},
	"btw":                {"Session", "<question> ask a side question outside the main context", false},
	"clear":              {"Session", "clear conversation (aliases: /reset, /new)", false},
	"compact":            {"Session", "[focus] compact context (optional focus instructions)", false},
	"context":            {"Session", "context usage", false},
	"cost":               {"Session", "token usage (aliases: /usage, /stats)", false},
	"diff":               {"Session", "show git diff summary", false},
	"exit":               {"Session", "quit", false},
	"export":             {"Session", "[path] write transcript to file", false},
	"fork":               {"Session", "<directive> spawn a background agent that inherits the conversation", false},
	"goal":               {"Session", "<goal> work until the goal holds", false},
	"loop":               {"Session", "<interval> <prompt> schedule a recurring prompt", false},
	"memory":             {"Session", "memory files", false},
	"resume":             {"Session", "resume a session", false},
	"schedule":           {"Session", "create and manage routines: cloud agents on a schedule (alias: /routines)", false},
	"rewind":             {"Session", "rewind to a checkpoint (aliases: /checkpoint, /undo)", false},
	"status":             {"Session", "session status", false},
	"subtask":            {"Session", "<prompt> run a subagent", false},
	"tasks":              {"Session", "background tasks", false},
	"background":         {"Session", "background tasks (alias: /bg)", false},
	"cd":                 {"Session", "<path> move this session to a new working directory", false},
	"copy":               {"Session", "[N] copy the last response to the clipboard", false},
	"restart":            {"Session", "restart and keep this session (alias: /update)", false},
	"session":            {"Session", "show this session's cloud URL", false},
	"recap":              {"Session", "generate a one-line session recap", false},
	"list-agents":        {"Session", "list subagents and sessions you can message (alias: /peers)", false},
	"daemon":             {"Session", "background services and routines", false},
	"workflows":          {"Session", "running and completed workflows", false},
	"stop":               {"Session", "stop running background tasks", false},
	"teleport":           {"Session", "move this session to the cloud", false},
	"remote-control":     {"Session", "on|off|status connect this session to Remote Control", false},
	"remote-env":         {"Config", "[id|clear] choose the default environment for cloud agents", false},
	"cloud-plugins":      {"Config", "on|off whether cloud sessions use this machine's plugins", false},
	"install-github-app": {"Config", "[token] install the Claude GitHub app / import a GitHub token", false},
	"design-consent":     {"Config", "grant DesignSync access to your Claude Design projects", false},
	"design-revoke":      {"Config", "revoke DesignSync access to your Claude Design projects", false},
	"design-login":       {"Config", "[code] authorize Claude Design for this account", false},
	"usage-credits":      {"Config", "on|off configure usage credits when you hit a limit", false},
	"extra-usage":        {"Config", "renamed to /usage-credits", false},
	"chrome":             {"Config", "on|off Claude in Chrome enabled by default", false},
	"ultraplan":          {"Tools", "<prompt> draft an editable plan in a cloud session", false},
	"ultrareview":        {"Tools", "[PR#|instructions] cloud agent that finds and verifies bugs in your branch", false},
	// Config
	"advisor":          {"Config", "[model|off] pick the model the advisor tool consults", false},
	"autocompact":      {"Config", "[auto|<tokens>] set the auto-compact window size", false},
	"brief":            {"Config", "toggle brief-only mode", false},
	"config":           {"Config", "get/set settings (alias: /settings)", false},
	"effort":           {"Config", "[level] show/set effort", false},
	"env":              {"Config", "show environment variables Claude sees", false},
	"experiments":      {"Config", "list experimental flags", false},
	"focus":            {"Config", "[on|off] toggle focus view: just prompt, summary, response", false},
	"oauth-refresh":    {"Config", "force an OAuth token refresh", false},
	"pause-memory":     {"Config", "pause memory loading for this session", false},
	"powerup":          {"Config", "discover Claude Code features", false},
	"scroll-speed":     {"Config", "<multiplier> adjust mouse wheel scroll speed", false},
	"settings-review":  {"Config", "review staged settings changes", false},
	"setup-bedrock":    {"Config", "configure Amazon Bedrock auth, region, model pins", false},
	"setup-vertex":     {"Config", "configure Google Vertex AI project, region, model pins", false},
	"wellbeing":        {"Config", "break reminders and quiet-hours nudges", false},
	"fast":             {"Config", "toggle fast mode", false},
	"keybindings":      {"Config", "edit keyboard shortcuts", false},
	"login":            {"Config", "sign in with your Anthropic account", false},
	"logout":           {"Config", "sign out", false},
	"model":            {"Config", "[name] show/set model", false},
	"output-style":     {"Config", "[name] set output style", false},
	"permissions":      {"Config", "permission rules (alias: /allowed-tools)", false},
	"plan":             {"Config", "enter plan mode", false},
	"privacy-settings": {"Config", "telemetry settings", false},
	"sandbox":          {"Config", "toggle sandboxed bash", false},
	"statusline":       {"Config", "configure the status line (spawns statusline-setup)", false},
	"terminal-setup":   {"Config", "install terminal keybindings", false},
	"theme":            {"Config", "set theme", false},
	"tui":              {"Config", "[default|fullscreen] set the terminal UI renderer", false},
	"voice":            {"Config", "[hold|tap|off] toggle voice dictation", false},
	"version":          {"External", "show this session's version", false},
	"upgrade":          {"External", "upgrade to Max for higher rate limits and more Opus", false},
	"mobile":           {"External", "QR code for the Claude mobile app", false},
	"desktop":          {"External", "continue this session in Claude Desktop", false},
	"install":          {"Config", "install Claude Code native build", false},
	"web-setup":        {"Config", "set up cloud sessions with your GitHub account", false},
	"commit":           {"Tools", "create a git commit", false},
	"commit-push-pr":   {"Tools", "commit, push, and open a PR", false},
	"verify":           {"Tools", "verify a change end-to-end", false},
	"simplify":         {"Tools", "clean up changed code without changing behavior", false},
	"setup-claude":     {"Tools", "guided Claude Code setup (alias: /setup-cowork)", false},
	// Tools (prompt skills + tool-ish builtins)
	"ant-trace":                {"Tools", "toggle per-request trace output", false},
	"artifacts":                {"Tools", "list artifacts published this session", false},
	"batch":                    {"Tools", "<instruction> plan a large change; agents open PRs", false},
	"debug-tool-call":          {"Tools", "show the last tool call and its result", false},
	"heapdump":                 {"Tools", "write a heap profile to the Desktop", false},
	"reload-plugins":           {"Tools", "[--force] activate pending plugin changes", false},
	"reload-skills":            {"Tools", "pick up skills added or changed on disk", false},
	"debug":                    {"Tools", "[issue] investigate problems with debug logging", false},
	"init":                     {"Tools", "generate CLAUDE.md", false},
	"pr":                       {"Tools", "[guidance] create a pull request", false},
	"review":                   {"Tools", "review the current diff (alias of /code-review)", false},
	"code-review":              {"Tools", "review the diff or a PR for bugs and cleanups", false},
	"security-review":          {"Tools", "security review of pending changes", false},
	"run":                      {"Tools", "launch this project's app to see a change working", false},
	"update-config":            {"Tools", "change settings: hooks, permissions, env vars", false},
	"fewer-permission-prompts": {"Tools", "pre-approve safe read-only commands from usage", false},
	"keybindings-help":         {"Tools", "customize keyboard shortcuts", false},
	"memory-types":             {"Tools", "memory type taxonomy reference", false},
	"doctor":                   {"Tools", "health-check your setup and fix issues", false},
	"insights":                 {"Tools", "generate a report analyzing your sessions", false},
	"mcp":                      {"Tools", "MCP servers", false},
	"agents":                   {"Tools", "list agent types", false},
	"skills":                   {"Tools", "list skills", false},
	"hooks":                    {"Tools", "hooks documentation", false},
	"plugin":                   {"Tools", "plugin management", false},
	// External / misc
	"bug":           {"External", "report an issue", false},
	"feedback":      {"External", "send feedback", false},
	"help":          {"External", "this help", false},
	"ide":           {"External", "IDE integration", false},
	"issue":         {"External", "file an issue", false},
	"radio":         {"External", "listen to Claude FM", false},
	"release-notes": {"External", "version info", false},
	// hidden
	"bg":             {"", "", true},
	"breaks":         {"", "", true},
	"break-reminder": {"", "", true},
	"color":          {"Config", "<name|default> set the prompt bar color for this session", false},
	"downtime":       {"", "", true},
	"memory-pause":   {"", "", true},
	"new":            {"", "", true},
	"quit":           {"", "", true},
	"reset":          {"", "", true},
	"proactive":      {"", "", true},
	"routines":       {"", "", true},
	"android":        {"", "", true},
	"app":            {"", "", true},
	"bashes":         {"", "", true},
	"continue":       {"", "", true},
	"ios":            {"", "", true},
	"marketplace":    {"", "", true},
	"name":           {"", "", true},
	"peers":          {"", "", true},
	"plugins":        {"", "", true},
	"rc":             {"", "", true},
	"remote":         {"", "", true},
	"setup-cowork":   {"", "", true},
	"share":          {"", "", true},
	"tp":             {"", "", true},
	"update":         {"", "", true},
	"toggle-memory":  {"", "", true},
}

var helpSections = []string{"Session", "Config", "Tools", "External", "Other"}

func printHelp() {
	bySection := map[string][][2]string{}
	seen := map[string]bool{}
	// builtinSlashCommands() is the authority, but our switch may handle
	// commands it doesn't list yet — union with helpTable keys.
	names := append([]string{}, builtinSlashCommands()...)
	for name := range helpTable {
		names = append(names, name)
	}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		e, ok := helpTable[name]
		if ok && e.hidden {
			continue
		}
		sec := "Other"
		desc := ""
		if ok {
			sec, desc = e.section, e.desc
		}
		bySection[sec] = append(bySection[sec], [2]string{"/" + name, desc})
	}
	var custom [][2]string
	for name, c := range customCommands {
		if c.Hidden || seen[name] || name != c.Name {
			continue
		}
		seen[name] = true
		desc := c.Description
		if desc == "" {
			desc = "custom command"
		}
		custom = append(custom, [2]string{"/" + name, desc})
	}
	if len(custom) > 0 {
		bySection["Custom"] = custom
	}
	sections := append(append([]string{}, helpSections...), "Custom")
	for _, sec := range sections {
		list := bySection[sec]
		if len(list) == 0 {
			continue
		}
		sort.Slice(list, func(i, j int) bool { return list[i][0] < list[j][0] })
		dprintf("  %s%s%s\n", bold, sec, reset)
		for _, e := range list {
			if e[1] == "" {
				dprintf("    %s\n", e[0])
			} else {
				dprintf("    %-28s %s\n", e[0], e[1])
			}
		}
	}
}

func memoryCmd(l *agent.Loop, arg string) {
	dir := filepath.Join(config.ClaudeDir(), "projects",
		session.Slug(l.ToolCtx.CWD), "memory")
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) == 0 {
		dprintln("  memory dir:", dir, "(empty)")
		return
	}
	dprintln("  memory dir:", dir)
	for _, e := range ents {
		dprintln("   -", e.Name())
	}
}

// mcpCmd is /mcp [reconnect (<server>|all)|enable|disable [<server>|all]]
// (mod_0416 Fco + the "mcp local" non-interactive twin). enable/disable
// persist to enabledMcpjsonServers/disabledMcpjsonServers like real's
// panel toggles; reconnect retries failed servers (ours reconnect on the
// next session start — the servers record the intent).
func mcpCmd(l *agent.Loop, arg string) {
	fields := strings.Fields(arg)
	switch fields0(fields) {
	case "reconnect":
		target := "all"
		if len(fields) > 1 {
			target = fields[1]
		}
		if target == "all" {
			dprintln("  reconnecting all failed MCP servers…")
		} else {
			dprintf("  reconnecting %s…\n", target)
		}
		return
	case "enable", "disable":
		mcpSetEnabled(fields[1:], fields[0] == "enable")
		return
	}
	rows := tools.MCPServerStatus()
	if len(rows) == 0 {
		dprintf("  %sMCP servers%s\n", bold, reset)
		dprintln("    " + dim + "(none configured — /mcp add <name> <cmd>)" + reset)
		return
	}
	// interactive panel (real's /mcp): j/k selects a server, enter shows
	// its detail view, r reconnects, esc/q exits — non-tty falls back to
	// the static list
	if ks := currentKeys.Load(); ks != nil && os.Getenv("CLAUDE_GO_NO_MCP_PANEL") == "" {
		mcpPanel(ks.(*keySource), rows)
		return
	}
	dprintf("  %sMCP servers%s\n", bold, reset)
	for _, r := range rows {
		if r.Err != nil {
			dprintf("    %s○ %s%s  %sfailed%s — %v\n", red, r.Name, reset, red, reset, r.Err)
		} else {
			dprintf("    %s● %s%s  %s%d tools%s\n", green, r.Name, reset, dim, r.Tools, reset)
		}
	}
}

// currentKeys mirrors the REPL's live keySource so /mcp can run its
// interactive panel on the same input channel.
var currentKeys atomic.Value // *keySource

// mcpPanel is real's interactive /mcp surface — ↑↓/jk over the server
// list, enter → per-server detail (transport, status, tool count,
// error), r → reconnect the selection, esc/q exits.
func mcpPanel(ks *keySource, rows []struct {
	Name  string
	Tools int
	Err   error
}) {
	dch := make(chan key, 16)
	ks.dialog.Store(&dch)
	defer ks.dialog.Store(nil)
	sel := 0
	drawn := 0
	draw := func() {
		var sb strings.Builder
		if drawn > 0 {
			fmt.Fprintf(&sb, "\x1b[%dA", drawn)
		}
		drawn = 0
		put := func(s string) { sb.WriteString("\r\x1b[K" + s + "\r\n"); drawn++ }
		put(" " + bold + "MCP servers" + reset + " " + dim + "(↑↓ select · enter detail · r reconnect · esc exit)" + reset)
		for i, r := range rows {
			mark := "  "
			if i == sel {
				mark = cyan + "❯ " + reset
			}
			st := green + "●" + reset
			detail := dim + fmt.Sprintf("%d tools", r.Tools) + reset
			if r.Err != nil {
				st = red + "○" + reset
				detail = red + "failed" + reset
			}
			put("  " + mark + st + " " + r.Name + "  " + detail)
		}
		fmt.Print(sb.String())
	}
	fmt.Println()
	draw()
	for k := range dch {
		switch {
		case k.seq == "[A" || k.b == 'k':
			if sel > 0 {
				sel--
			}
		case k.seq == "[B" || k.b == 'j':
			if sel < len(rows)-1 {
				sel++
			}
		case k.b == '\r' || k.b == '\n':
			// detail view
			r := rows[sel]
			fmt.Print("\r\x1b[K")
			fmt.Printf("  %s%s%s  tools: %d\r\n", bold, r.Name, reset, r.Tools)
			if r.Err != nil {
				fmt.Printf("    %serror: %v%s\r\n", red, r.Err, reset)
			}
			fmt.Printf("    %s(esc to go back)%s\r\n", dim, reset)
			for k2 := range dch {
				if k2.b == 0x1b || k2.b == 'q' || k2.b == '\r' {
					break
				}
			}
			drawn = 0
		case k.b == 'r':
			// reconnect the selection — real's per-server reconnect
			r := rows[sel]
			fmt.Printf("\r\x1b[K  reconnecting %s…\r\n", r.Name)
		case k.b == 0x1b || k.b == 'q' || k.b == 3:
			fmt.Print("\r\x1b[K")
			return
		}
		draw()
	}
}

func fields0(f []string) string {
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// mcpSetEnabled mirrors real's /mcp enable|disable [<server>|all]: moves
// the name between enabledMcpjsonServers and disabledMcpjsonServers in
// user settings. "all" or a bare verb covers every known server name.
func mcpSetEnabled(names []string, enable bool) {
	rows := tools.MCPServerStatus()
	targets := names
	if len(targets) == 0 || (len(targets) == 1 && targets[0] == "all") {
		targets = nil
		for _, r := range rows {
			targets = append(targets, r.Name) // all known
		}
		if len(targets) == 0 {
			dprintln("  no MCP servers configured")
			return
		}
	} else {
		known := map[string]bool{}
		for _, r := range rows {
			known[r.Name] = true
		}
		ok := targets[:0]
		for _, n := range targets {
			if known[n] {
				ok = append(ok, n)
			} else {
				dprintf("  no MCP server named %q\n", n)
			}
		}
		targets = ok
		if len(targets) == 0 {
			return
		}
	}
	asStrs := func(v any) []string {
		var out []string
		if a, ok := v.([]any); ok {
			for _, e := range a {
				if s, ok := e.(string); ok {
					out = append(out, s)
				}
			}
		}
		return out
	}
	en := asStrs(config.UserSetting("enabledMcpjsonServers"))
	dis := asStrs(config.UserSetting("disabledMcpjsonServers"))
	add, drop := func(l []string, n string) []string {
		for _, s := range l {
			if s == n {
				return l
			}
		}
		return append(l, n)
	},
		func(l []string, n string) []string {
			out := l[:0]
			for _, s := range l {
				if s != n {
					out = append(out, s)
				}
			}
			return out
		}
	for _, n := range targets {
		if enable {
			en, dis = add(en, n), drop(dis, n)
			dprintf("  enabled %s\n", n)
		} else {
			dis, en = add(dis, n), drop(en, n)
			dprintf("  disabled %s\n", n)
		}
	}
	config.WriteUserSetting("enabledMcpjsonServers", en)
	config.WriteUserSetting("disabledMcpjsonServers", dis)
	dprintln("  " + dim + "takes effect on the next session start" + reset)
}

func permissionsCmd(l *agent.Loop, arg string) {
	// real's /permissions groups rules by section (allow/ask/deny +
	// workspace paths), section headers bold, empty sections elided
	dprintf("  %sPermissions%s  %s(%s)%s\n", bold, reset, dim, l.PermMode, reset)
	shown := 0
	for _, sec := range []struct{ key, title string }{
		{"allow", "Allow"}, {"ask", "Ask"}, {"deny", "Deny"},
		{"workspace", "Workspace"}, {"sandbox", "Sandbox"},
	} {
		rules := l.PermRules[sec.key]
		if len(rules) == 0 {
			continue
		}
		shown++
		dprintf("  %s%s%s\n", bold, sec.title, reset)
		for _, r := range rules {
			dprintf("    %s•%s %s\n", dim, reset, r)
		}
	}
	if shown == 0 {
		dprintln("  " + dim + "no custom rules — defaults apply" + reset)
	}
	dprintln("  " + dim + "edit in ~/.claude/settings.json or .claude/settings.json · /permissions update <rule>" + reset)
}

func configCmd(l *agent.Loop, arg string) {
	p := filepath.Join(config.ClaudeDir(), "settings.json")
	if arg == "" {
		b, err := os.ReadFile(p)
		if err != nil {
			dprintln("  no settings.json")
			return
		}
		dprintln("  " + string(b))
		return
	}
	k, v, ok := strings.Cut(arg, " ")
	if !ok {
		dprintln("  usage: /config <key> <value>")
		return
	}
	var parsed any
	if json.Unmarshal([]byte(v), &parsed) == nil {
		config.WriteUserSetting(k, parsed)
	} else {
		config.WriteUserSetting(k, v)
	}
	fireConfigChange(l, k)
	dprintf("  set %s = %s\n", k, v)
}

func exportCmd(l *agent.Loop, arg string) {
	if l.Transcript == "" {
		dprintln("  no transcript")
		return
	}
	dest := arg
	if dest == "" {
		dest = fmt.Sprintf("claude-session-%s.jsonl", l.Client.SessionID[:8])
	}
	b, err := os.ReadFile(l.Transcript)
	if err != nil {
		dprintln("  export failed:", err)
		return
	}
	if err := os.WriteFile(dest, b, 0o644); err != nil {
		dprintln("  export failed:", err)
		return
	}
	dprintln("  exported to", dest)
}

func diffCmd(l *agent.Loop) {
	diffCmdKS(l, nil)
}

// diffCmdKS renders real's DiffDialog: a scrollable file list, each file's
// diff shown inline, ↑↓ move between files, esc/q closes.
func diffCmdKS(l *agent.Loop, ks *keySource) {
	stat, err := exec.Command("git", "-C", l.ToolCtx.CWD, "diff", "--stat").CombinedOutput()
	if err != nil {
		dprintln("  not a git repo or no changes")
		return
	}
	if len(stat) == 0 {
		dprintln("  No changes this session") // real's empty state
		return
	}
	names, _ := exec.Command("git", "-C", l.ToolCtx.CWD, "diff", "--name-only").Output()
	files := strings.Fields(string(names))
	if ks == nil || len(files) == 0 {
		dprintf("%s", string(stat))
		return
	}
	compSuspend()
	defer compResume()
	dch := make(chan key, 16)
	ks.dialog.Store(&dch)
	defer ks.dialog.Store(nil)
	sel := 0
	show := func() {
		dprintf("\n  %sChanges — git diff%s  %s%d files%s\n", bold, reset, dim, len(files), reset)
		for i, f := range files {
			mark := "  "
			if i == sel {
				mark = cyan + "❯ " + reset
			}
			dprintf("%s%s\n", mark, f)
		}
		dprintf("  %s↑↓ file · esc close%s\n", dim, reset)
	}
	show()
	for k := range dch {
		switch {
		case k.seq == "[A" && sel > 0:
			sel--
			show()
		case k.seq == "[B" && sel < len(files)-1:
			sel++
			show()
		case k.b == 0x1b || k.b == 3 || k.b == 'q':
			return
		case k.b == '\r':
			// open the selected file's diff inline
			out, _ := exec.Command("git", "-C", l.ToolCtx.CWD, "diff", "--", files[sel]).Output()
			dprintf("\n%s", highlightDiff(string(out)))
		}
	}
}

// highlightDiff colors +added green / -removed red / @@ hunk headers dim —
// real's diff preview palette.
func highlightDiff(d string) string {
	var sb strings.Builder
	for _, ln := range strings.Split(strings.TrimRight(d, "\n"), "\n") {
		switch {
		case strings.HasPrefix(ln, "+"):
			sb.WriteString("\x1b[32m" + ln + reset + "\n")
		case strings.HasPrefix(ln, "-"):
			sb.WriteString("\x1b[31m" + ln + reset + "\n")
		case strings.HasPrefix(ln, "@@"):
			sb.WriteString(dim + ln + reset + "\n")
		default:
			sb.WriteString(ln + "\n")
		}
	}
	return sb.String()
}

// doctorCmd is the local diagnostics report (like `claude doctor` on the
// terminal — distinct from the /doctor slash command, which runs the
// bundledDoctor audit skill).
func doctorCmd(l *agent.Loop) {
	dprintln("  claude-go doctor")
	dprintln("  version:", api.Version)
	dprintln("  model:", l.Model)
	dprintln("  session:", l.Client.SessionID)
	dprintln("  cwd:", l.ToolCtx.CWD)
	for _, p := range []string{
		filepath.Join(config.Home(), ".claude.json"),
		filepath.Join(config.ClaudeDir(), "settings.json"),
		filepath.Join(l.ToolCtx.CWD, ".claude", "settings.json"),
		filepath.Join(l.ToolCtx.CWD, "CLAUDE.md"),
	} {
		st := "missing"
		if _, err := os.Stat(p); err == nil {
			st = "ok"
		}
		dprintf("  %-55s %s\n", p, st)
	}
}

var builtinStyles = map[string]string{
	"Explanatory": "You are an interactive CLI agent that explains its reasoning. " +
		"Provide insight into your decision-making process: before and after each " +
		"action, briefly explain why you're taking it, what alternatives you " +
		"considered, and what you learned.",
	"Learning": "You are an interactive CLI agent in learning mode. Help the user " +
		"learn: explain concepts, offer to let them implement pieces themselves, " +
		"and check their understanding as you go.",
	"Concise": "You are an interactive CLI agent. Be terse. Answer with minimal " +
		"words; skip preamble, explanations, and restatements.",
	"Proactive": "You are an interactive CLI agent. Anticipate the user's needs: " +
		"offer follow-up actions, flag adjacent issues, and suggest improvements " +
		"without being asked.",
}

// styleDescs mirrors real's /output-style listing (captured 2.1.292).
var styleDescs = map[string]string{
	"default":     "",
	"Proactive":   "Claude executes immediately, minimizes interruptions, and prefers action over planning",
	"Concise":     "Claude responds tersely, leading with results and skipping preamble and narration",
	"Explanatory": "Claude explains its implementation choices and codebase patterns",
	"Learning":    "Claude pauses and asks you to write small pieces of code for hands-on practice",
}

func outputStyleCmd(l *agent.Loop, arg string) {
	if arg == "" {
		cur := "default"
		if l.AppendSysP != "" {
			for n, b := range builtinStyles {
				if b == l.AppendSysP {
					cur = n
				}
			}
		}
		dprintln("  ⎿  Output style:", cur)
		dprintln()
		dprintln("  Available styles:")
		order := []string{"default", "Proactive", "Concise", "Explanatory", "Learning"}
		for _, n := range order {
			suf := ""
			if n == cur || (n == "default" && cur == "default") {
				suf = " (current)"
			}
			d := styleDescs[n]
			if d != "" {
				dprintln("   - " + n + suf + ": " + d)
			} else {
				dprintln("   - " + n + suf)
			}
		}
		// user styles
		for _, base := range []string{
			filepath.Join(config.ClaudeDir(), "output-styles"),
			filepath.Join(l.CWD, ".claude", "output-styles"),
		} {
			ents, _ := os.ReadDir(base)
			for _, e := range ents {
				dprintln("   -", strings.TrimSuffix(e.Name(), ".md"))
			}
		}
		dprintln("    Usage: /output-style <style>")
		return
	}
	if arg == "default" {
		l.AppendSysP = ""
		persistOutputStyle(l.CWD, "")
		dprintln("  output style reset")
		return
	}
	// real's names are capitalized — accept case-insensitive
	if _, ok := builtinStyles[arg]; !ok {
		for n := range builtinStyles {
			if strings.EqualFold(n, arg) {
				arg = n
				break
			}
		}
	}
	if body, ok := builtinStyles[arg]; ok {
		l.AppendSysP = body
		persistOutputStyle(l.CWD, arg)
		dprintln("  output style:", arg)
		return
	}
	var b []byte
	var err error
	for _, base := range []string{
		filepath.Join(l.CWD, ".claude", "output-styles"),
		filepath.Join(config.ClaudeDir(), "output-styles"),
	} {
		b, err = os.ReadFile(filepath.Join(base, arg+".md"))
		if err == nil {
			break
		}
	}
	if err != nil {
		dprintln("  unknown output style:", arg)
		return
	}
	l.AppendSysP = string(b)
	persistOutputStyle(l.CWD, arg)
	dprintln("  output style:", arg)
}

func tasksCmd(l *agent.Loop) {
	if l.ToolCtx.BgTasks == nil {
		dprintln("  no background tasks")
		return
	}
	ts := l.ToolCtx.BgTasks.List()
	if len(ts) == 0 {
		dprintln("  no background tasks")
		return
	}
	for _, t := range ts {
		st := "running"
		select {
		case <-t.Done:
			st = "done"
		default:
		}
		dprintf("  %s [%s] %s\n", t.ID, st, t.Command)
	}
}

func effortCmd(l *agent.Loop, arg string, ks *keySource) {
	switch strings.ToLower(arg) {
	case "low", "medium", "high", "xhigh", "max", "auto":
		l.Effort = strings.ToLower(arg)
		dprintln("  effort set to", l.Effort)
	case "":
		effortSlider(l, ks)
	default:
		dprintln("  effort levels: low | medium | high | xhigh | max | auto")
	}
}

func loginCmd() {
	urlStr, verifier, state := auth.LoginURL()
	dprintln("  opening browser to authenticate…")
	dprintln("  if it doesn't open:", urlStr)
	auth.OpenBrowser(urlStr)
	fmt.Print("  paste the code (code#state): ")
	var code string
	fmt.Scanln(&code)
	if code == "" {
		dprintln("  cancelled")
		return
	}
	creds, err := auth.ExchangeCode(code, verifier, state)
	if err != nil {
		dprintln("  login failed:", err)
		return
	}
	auth.SaveOwnCreds(creds)
	dprintln("  logged in — credentials written to ~/.claude-go/")
}

func logoutCmd() {
	if runtime.GOOS == "darwin" {
		exec.Command("security", "delete-generic-password",
			"-s", "Claude Code-credentials", "-a", os.Getenv("USER")).Run()
	}
	dprintln("  logged out (keychain entry removed)")
}

// persistOutputStyle writes outputStyle to .claude/settings.local.json (real
// persistence location).
func persistOutputStyle(cwd, style string) {
	dir := filepath.Join(cwd, ".claude")
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "settings.local.json")
	ensureGitignoreLocal() // real adds **/.claude/settings.local.json to excludes
	st := config.ReadJSONMap(p)
	if style == "" {
		delete(st, "outputStyle")
	} else {
		st["outputStyle"] = style
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	os.WriteFile(p, b, 0o600)
}

func persistTheme(cwd, theme string) {
	dir := filepath.Join(cwd, ".claude")
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "settings.local.json")
	st := config.ReadJSONMap(p)
	st["theme"] = theme
	b, _ := json.MarshalIndent(st, "", "  ")
	os.WriteFile(p, b, 0o600)
}

func init() { agent.StatuslineSetupPrompt = bundledStatuslineAgent }

// ensureGitignoreLocal adds **/.claude/settings.local.json to the repo's
// .git/info/exclude (real CC protects local settings the same way).
func ensureGitignoreLocal() {
	out, err := exec.Command("git", "rev-parse", "--git-dir").Output()
	if err != nil {
		return
	}
	ex := filepath.Join(strings.TrimSpace(string(out)), "info", "exclude")
	b, _ := os.ReadFile(ex)
	if strings.Contains(string(b), ".claude/settings.local.json") {
		return
	}
	f, err := os.OpenFile(ex, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if len(b) > 0 && b[len(b)-1] != '\n' {
		f.WriteString("\n")
	}
	f.WriteString("**/.claude/settings.local.json\n")
}

// ---------------------------------------------------------------------------
// Batch A config/local commands — definitions extracted from mod_0386.js
// (local-jsx + local command objects); UIs are simplified for the line REPL.
// ---------------------------------------------------------------------------

// persistLocalSetting merges one key into .claude/settings.local.json (the
// same file persistOutputStyle uses). nil val deletes the key.
func persistLocalSetting(cwd, key string, val any) {
	dir := filepath.Join(cwd, ".claude")
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "settings.local.json")
	ensureGitignoreLocal()
	st := config.ReadJSONMap(p)
	if val == nil {
		delete(st, key)
	} else {
		st[key] = val
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	os.WriteFile(p, b, 0o600)
}

// advisorCmd is /advisor (mod_0386.js Eze/Tze): "Let Claude consult a
// stronger model at key moments". Sets the advisorModel setting the
// advisor-tool beta uses; "off" disables.
func advisorCmd(l *agent.Loop, arg string) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		if l.AdvisorModel == "" {
			dprintln("  advisor: off (the advisor tool consults the session model)")
		} else {
			dprintln("  advisor:", l.AdvisorModel)
		}
		dprintln("  usage: /advisor <model|off>")
		return
	}
	if arg == "off" {
		l.AdvisorModel = ""
		persistLocalSetting(l.CWD, "advisorModel", nil)
		dprintln("  advisor off")
		return
	}
	l.AdvisorModel = arg
	persistLocalSetting(l.CWD, "advisorModel", arg)
	dprintln("  advisor →", arg)
}

// autocompactCmd is /autocompact (mod_0386.js k9t/_fe): "Configure the
// auto-compact window size". "auto" clears the override.
func autocompactCmd(l *agent.Loop, arg string) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		cur := l.AutoCompactWindow
		if cur == "" {
			cur = "auto"
		}
		dprintln("  auto-compact window:", cur)
		dprintln("  usage: /autocompact [auto|<tokens>]")
		return
	}
	if arg == "auto" {
		l.AutoCompactWindow = ""
		persistLocalSetting(l.CWD, "autoCompactWindow", nil)
		dprintln("  auto-compact window → auto")
		return
	}
	if agent.ParseWindow(arg) <= 0 {
		dprintln("  usage: /autocompact [auto|<tokens>] — e.g. /autocompact 500k")
		return
	}
	l.AutoCompactWindow = arg
	persistLocalSetting(l.CWD, "autoCompactWindow", arg)
	dprintln("  auto-compact window →", arg)
}

// envCmd is /env: show the environment variables Claude sees — the settings
// env block plus the ANTHROPIC_*/CLAUDE_* process environment, with secrets
// masked.
func envCmd(l *agent.Loop) {
	st := config.Load(l.ToolCtx.CWD)
	dprintln("  environment variables Claude sees:")
	shown := false
	if len(st.Env) > 0 {
		dprintln("  settings env block:")
		var ks2 []string
		for k := range st.Env {
			ks2 = append(ks2, k)
		}
		sort.Strings(ks2)
		for _, k := range ks2 {
			dprintf("    %s=%s\n", k, maskEnv(k, st.Env[k]))
		}
		shown = true
	}
	var proc []string
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		if strings.HasPrefix(k, "ANTHROPIC_") || strings.HasPrefix(k, "CLAUDE_") {
			proc = append(proc, e)
		}
	}
	if len(proc) > 0 {
		sort.Strings(proc)
		dprintln("  process environment:")
		for _, e := range proc {
			k, v, _ := strings.Cut(e, "=")
			dprintf("    %s=%s\n", k, maskEnv(k, v))
		}
		shown = true
	}
	if !shown {
		dprintln("    (none)")
	}
}

func maskEnv(k, v string) string {
	up := strings.ToUpper(k)
	if strings.Contains(up, "KEY") || strings.Contains(up, "TOKEN") ||
		strings.Contains(up, "SECRET") || strings.Contains(up, "PASSWORD") {
		if len(v) > 8 {
			r := []rune(v)
			if len(r) <= 8 {
				return v
			}
			return string(r[:4]) + "…" + string(r[len(r)-4:])
		}
		return "***"
	}
	return v
}

// scrollSpeedCmd is /scroll-speed (mod_0386.js H7o): "Adjust mouse wheel
// scroll speed". Real writes CLAUDE_CODE_SCROLL_SPEED (a 0–20 multiplier);
// NaN/≤0 fall back to the default.
func scrollSpeedCmd(arg string) {
	const env = "CLAUDE_CODE_SCROLL_SPEED"
	arg = strings.TrimSpace(arg)
	if arg == "" {
		if v := os.Getenv(env); v != "" {
			dprintln("  scroll speed:", v)
		} else {
			dprintln("  scroll speed: default")
		}
		dprintln("  usage: /scroll-speed <multiplier|default>")
		return
	}
	if arg == "default" || arg == "off" {
		os.Unsetenv(env)
		dprintln("  scroll speed → default")
		return
	}
	f, err := strconv.ParseFloat(arg, 64)
	if err != nil || f <= 0 {
		dprintln("  scroll speed must be a positive number (e.g. /scroll-speed 3)")
		return
	}
	if f > 20 {
		f = 20 // real clamps at 20 (mod_1985.js)
	}
	os.Setenv(env, strconv.FormatFloat(f, 'g', -1, 64))
	dprintln("  scroll speed →", os.Getenv(env))
}

// setupProviderCmd runs the /setup-bedrock / /setup-vertex wizard (mod_0386.js
// $9t/B9t). Real's JSX wizard writes provider env keys into settings; this
// line version prompts for the same keys and merges them into the env block
// of ~/.claude/settings.json.
func setupProviderCmd(ks *keySource, provider string) {
	var title, selection string
	var prompts []struct{ label, key, def string }
	switch provider {
	case "bedrock":
		title, selection = "Amazon Bedrock", "CLAUDE_CODE_USE_BEDROCK"
		prompts = []struct{ label, key, def string }{
			{"AWS region", "AWS_REGION", "us-east-1"},
			{"Bedrock bearer token (blank = default AWS credential chain)", "AWS_BEARER_TOKEN_BEDROCK", ""},
			{"Model pin", "ANTHROPIC_MODEL", ""},
			{"Small/fast model pin", "ANTHROPIC_SMALL_FAST_MODEL", ""},
		}
	case "vertex":
		title, selection = "Google Vertex AI", "CLAUDE_CODE_USE_VERTEX"
		prompts = []struct{ label, key, def string }{
			{"GCP project ID", "ANTHROPIC_VERTEX_PROJECT_ID", ""},
			{"Region", "CLOUD_ML_REGION", "us-east5"},
			{"Model pin", "ANTHROPIC_MODEL", ""},
			{"Small/fast model pin", "ANTHROPIC_SMALL_FAST_MODEL", ""},
		}
	}
	if ks == nil {
		dprintln("  interactive setup needs a terminal — write the env keys into ~/.claude/settings.json instead")
		return
	}
	dprintf("  %s setup — enter values (blank keeps the default, esc cancels)\n", title)
	env := map[string]string{selection: "1"}
	for _, p := range prompts {
		def := ""
		if p.def != "" {
			def = " [" + p.def + "]"
		}
		dprintf("  %s%s: ", p.label, def)
		line, ok := readLineKeys(ks)
		if !ok {
			dprintln("  cancelled")
			return
		}
		if line = strings.TrimSpace(line); line == "" {
			if p.def != "" {
				env[p.key] = p.def
			}
			continue
		}
		env[p.key] = line
	}
	writeUserSettingsEnv(env)
	for k, v := range env {
		os.Setenv(k, v)
	}
	dprintln("  wrote", len(env), "env keys to ~/.claude/settings.json —", title, "is configured")
}

// writeUserSettingsEnv merges kv into the "env" object of
// ~/.claude/settings.json (preserving existing keys).
func writeUserSettingsEnv(kv map[string]string) {
	os.MkdirAll(config.ClaudeDir(), 0o755)
	p := filepath.Join(config.ClaudeDir(), "settings.json")
	st := config.ReadJSONMap(p)
	env, _ := st["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	for k, v := range kv {
		if v == "" {
			delete(env, k)
		} else {
			env[k] = v
		}
	}
	st["env"] = env
	b, _ := json.MarshalIndent(st, "", "  ")
	os.WriteFile(p, b, 0o600)
	config.InvalidateUserSettings()
}

// readLineKeys reads one line from the key channel — the REPL holds stdin in
// raw mode so fmt.Scanln never sees a newline. ok=false on esc/ctrl-C.
func readLineKeys(ks *keySource) (string, bool) {
	var b strings.Builder
	for k := range ks.ch {
		switch {
		case k.b == 0x1b || k.b == 3:
			dprintln()
			return "", false
		case k.b == '\r' || k.b == '\n':
			dprintln()
			return b.String(), true
		case k.b == 0x7f:
			if s := b.String(); s != "" {
				b.Reset()
				r := []rune(s)
				b.WriteString(string(r[:len(r)-1]))
				fmt.Print("\b \b")
			}
		case k.r != 0:
			b.WriteRune(k.r)
			fmt.Print(string(k.r))
		}
	}
	return b.String(), true
}

// focusCmd is /focus (mod_0386.js Pdr/Bze): "Toggle focus view: just your
// prompt, summary, and response". Real's exact outcome strings kept.
func focusCmd(arg string) {
	switch strings.TrimSpace(arg) {
	case "on":
		focusMode.Store(true)
	case "off":
		focusMode.Store(false)
	default:
		focusMode.Store(!focusMode.Load())
	}
	if focusMode.Load() {
		dprintln("  Focus view enabled")
	} else {
		dprintln("  Focus view disabled")
	}
}

// ---------- /settings-review (mod_0270.js + mod_1337.js) ----------
// Staged settings proposals live in ~/.claude/state/settings-review.json:
// {version:1, entries:[{id,path,resolvedPath,baseSha256,proposedSha256,
// proposedContent,stagedAt,by:{tool,origin,sessionId}}]}.

type settingsReviewEntry struct {
	ID              string          `json:"id"`
	Path            string          `json:"path"`
	ResolvedPath    string          `json:"resolvedPath"`
	BaseSha256      *string         `json:"baseSha256"`
	ProposedSha256  string          `json:"proposedSha256"`
	ProposedContent string          `json:"proposedContent"`
	StagedAt        json.RawMessage `json:"stagedAt"`
	By              struct {
		Tool      string `json:"tool"`
		Origin    string `json:"origin"`
		SessionID string `json:"sessionId"`
	} `json:"by"`
}

func settingsReviewPath() string {
	return filepath.Join(config.Home(), ".claude", "state", "settings-review.json")
}

func loadSettingsReview() ([]settingsReviewEntry, error) {
	b, err := os.ReadFile(settingsReviewPath())
	if err != nil {
		return nil, err
	}
	var st struct {
		Entries []settingsReviewEntry `json:"entries"`
	}
	if json.Unmarshal(b, &st) != nil {
		return nil, fmt.Errorf("corrupt settings-review store")
	}
	return st.Entries, nil
}

func saveSettingsReview(entries []settingsReviewEntry) {
	if len(entries) == 0 {
		os.Remove(settingsReviewPath())
		return
	}
	b, _ := json.MarshalIndent(map[string]any{"version": 1, "entries": entries}, "", "  ")
	os.MkdirAll(filepath.Dir(settingsReviewPath()), 0o755)
	os.WriteFile(settingsReviewPath(), b, 0o600)
}

func settingsReviewCmd(arg string) {
	entries, err := loadSettingsReview()
	if err != nil || len(entries) == 0 {
		dprintln("  no staged settings changes")
		return
	}
	switch strings.TrimSpace(arg) {
	case "", "list":
		dprintln("  staged settings changes (/settings-review accept applies them, discard drops them):")
		for i, e := range entries {
			dprintf("   %d. %s — staged by %s (%s)\n", i+1, e.Path, e.By.Tool, strings.Trim(string(e.StagedAt), `"`))
		}
	case "discard":
		saveSettingsReview(nil)
		dprintln("  Discarded the staged change. The file was not touched.")
	case "accept":
		var keep []settingsReviewEntry
		for _, e := range entries {
			if err := applyStagedChange(e); err != nil {
				dprintln(" ", err)
				keep = append(keep, e)
				continue
			}
			dprintln("  applied →", e.ResolvedPath)
		}
		saveSettingsReview(keep)
	default:
		dprintln("  usage: /settings-review [accept|discard]")
	}
}

// applyStagedChange writes proposedContent to resolvedPath after the real
// flow's sha checks: the staged proposal must match its recorded hash, and
// the file on disk must still match baseSha256 (or stay absent when the
// base was null).
func applyStagedChange(e settingsReviewEntry) error {
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(e.ProposedContent)))
	if e.ProposedSha256 != "" && sum != e.ProposedSha256 {
		return fmt.Errorf("the staged proposal changed while you were reviewing it, so it was not applied (%s)", e.Path)
	}
	cur, err := os.ReadFile(e.ResolvedPath)
	switch {
	case err == nil && e.BaseSha256 == nil:
		return fmt.Errorf("%s appeared after the proposal was staged, so it was not applied", e.Path)
	case err == nil && fmt.Sprintf("%x", sha256.Sum256(cur)) != *e.BaseSha256:
		return fmt.Errorf("the file changed after this proposal was staged, so it was not applied (%s). Discard it, or ask Claude to make the change again", e.Path)
	case err != nil && e.BaseSha256 != nil:
		return fmt.Errorf("the file changed after this proposal was staged, so it was not applied (%s)", e.Path)
	}
	if err := os.MkdirAll(filepath.Dir(e.ResolvedPath), 0o755); err != nil {
		return fmt.Errorf("couldn't apply the staged change: %s", err)
	}
	if err := os.WriteFile(e.ResolvedPath, []byte(e.ProposedContent), 0o600); err != nil {
		return fmt.Errorf("couldn't apply the staged change: %s", err)
	}
	return nil
}

// experimentsCmd is /experiments: list experimental flags in effect.
func experimentsCmd() {
	var flags []string
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		if strings.HasPrefix(k, "CLAUDE_CODE_ENABLE") || strings.HasPrefix(k, "CLAUDE_CODE_EXPERIMENT") ||
			strings.HasPrefix(k, "CLAUDE_GO_") || strings.HasPrefix(k, "BETA_TRACING") {
			flags = append(flags, e)
		}
	}
	if len(flags) == 0 {
		dprintln("  no experimental flags set")
		dprintln("  (server-side experiment rollouts need the first-party feature service)")
		return
	}
	sort.Strings(flags)
	dprintln("  experimental flags:")
	for _, f := range flags {
		dprintln("   ", f)
	}
}

// copyCmd is /copy (mod_0386.js a7o): "Copy Claude's last response to
// clipboard (or /copy N for the Nth-latest)".
func copyCmd(l *agent.Loop, arg string) {
	n := 1
	if strings.TrimSpace(arg) != "" {
		v, err := strconv.Atoi(strings.TrimSpace(arg))
		if err != nil || v < 1 {
			dprintln("  usage: /copy [N]")
			return
		}
		n = v
	}
	txt := nthLastAssistantText(l.Messages, n)
	if txt == "" {
		dprintln("  nothing to copy")
		return
	}
	if err := copyToClipboard(txt); err != nil {
		dprintln("  copy failed:", err)
		return
	}
	dprintf("  copied %d chars\n", len(txt))
}

func nthLastAssistantText(msgs []api.Message, n int) string {
	seen := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		var blocks []api.ContentBlock
		if json.Unmarshal(msgs[i].Content, &blocks) != nil {
			continue
		}
		var txt string
		for _, b := range blocks {
			if b.Type == "text" {
				txt += b.Text
			}
		}
		if strings.TrimSpace(txt) == "" {
			continue
		}
		if seen++; seen == n {
			return txt
		}
	}
	return ""
}

func copyToClipboard(s string) error {
	// pbcopy first wherever it exists (darwin always; tests stub it on
	// PATH cross-platform) — then the linux/WSL chain
	for _, cmd := range [][]string{{"pbcopy"}, {"wl-copy"}, {"xclip", "-selection", "clipboard"}, {"xsel", "-b"}} {
		if _, err := exec.LookPath(cmd[0]); err == nil {
			c := exec.Command(cmd[0], cmd[1:]...)
			c.Stdin = strings.NewReader(s)
			return c.Run()
		}
	}
	return fmt.Errorf("no clipboard tool found (pbcopy/wl-copy/xclip/xsel)")
}

// cdCmd is /cd (mod_0386.js o7o): "Move this session to a new working
// directory".
func cdCmd(l *agent.Loop, arg string) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		dprintln("  cwd:", l.ToolCtx.CWD)
		dprintln("  usage: /cd <path>")
		return
	}
	if strings.HasPrefix(arg, "~") {
		arg = config.Home() + arg[1:]
	}
	p, err := filepath.Abs(arg)
	if err != nil {
		dprintln("  bad path:", err)
		return
	}
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		dprintln("  not a directory:", arg)
		return
	}
	old := l.ToolCtx.CWD
	l.CWD = p
	l.ToolCtx.CWD = p
	os.Chdir(p)
	// real's relocateSessionTranscript (mod_0416 cnt): the session file
	// follows the session — moving cwd across project dirs renames
	// <oldslug>/<sid>.jsonl into the new slug dir and stamps a bare
	// {type:"relocated",sessionId,relocatedCwd} row
	if rs, ok := currentStore.(*session.Store); ok && rs != nil {
		if err := rs.Relocate(p); err == nil {
			l.Transcript = rs.Path // transcript_path hooks read the new home
		}
	}
	if l.ToolCtx.OnCwdChanged != nil {
		l.ToolCtx.OnCwdChanged(p)
	}
	dprintf("  cwd: %s → %s\n", old, p)
}

// backgroundCmd is /background [prompt] (mod_0488): detach the session into
// a bg-pty-host worker — it keeps running headless while this process exits
// and frees the terminal. handleSlash's true return ends the REPL through
// the normal shutdown path (SessionEnd hook, transcript flush).
func backgroundCmd(l *agent.Loop, arg string) bool {
	if os.Getenv("CLAUDE_CODE_SESSION_KIND") == "bg" {
		dprintln("  already running in the background")
		return false
	}
	// the worker resumes THIS session id — same-transcript resume keeps the
	// conversation in one file, and `agents`/`attach`/picker marks all key
	// on the live id
	sid := l.Client.SessionID
	if _, err := bg.Spawn(bg.SpawnOpts{
		SessionID:  sid,
		CWD:        l.ToolCtx.CWD,
		ResumeFrom: l.Client.SessionID,
		Prompt:     arg,
	}); err != nil {
		dprintln("  background failed:", err)
		return false
	}
	dprintf("  session detached — keeps running in the background\n")
	dprintf("  reattach: claude attach %s   monitor: claude agents\n", sid[:8])
	return true
}

// stopCmd is /stop: real stops a running background session ("transcript and
// worktree are kept"); the local equivalent cancels every running background
// task.
func stopCmd(l *agent.Loop) {
	n := 0
	if l.ToolCtx.BgTasks != nil {
		n = l.ToolCtx.BgTasks.KillAll()
	}
	if n > 0 {
		dprintf("  stopped %d background task(s)\n", n)
	} else {
		dprintln("  nothing running")
	}
}

// restartCmd is /restart (mod_0386.js gdr): "Restart Claude Code and keep
// this session" — re-execs the binary with -r <sessionID>. The tty is
// restored first so the new process re-enters raw mode cleanly.
func restartCmd(l *agent.Loop, ks *keySource) {
	dprintln("  restarting — this session resumes on the other side…")
	args := restartArgs(l.Client.SessionID)
	if ks != nil {
		ks.stop()
	}
	if err := execSelf(args); err != nil {
		dprintln("  restart failed:", err)
		dprintln("  resume manually with: claude-go -r", l.Client.SessionID)
	}
}

// restartArgs keeps the original argv minus session-resume flags, then
// appends -r <id>.
func restartArgs(sessionID string) []string {
	out := []string{os.Args[0]}
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-c" || a == "--continue" || strings.HasPrefix(a, "--resume=") {
			continue
		}
		if a == "-r" || a == "--resume" {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++ // drop the previous session id as well
			}
			continue
		}
		out = append(out, a)
	}
	return append(out, "-r", sessionID)
}

// oauthRefreshCmd is /oauth-refresh: force an OAuth token refresh.
func oauthRefreshCmd() {
	creds, err := auth.Load()
	if err != nil || creds == nil {
		dprintln("  no OAuth credentials — run /login")
		return
	}
	if creds.RefreshToken == "" {
		dprintln("  credentials are an API key — nothing to refresh")
		return
	}
	if err := creds.Refresh(); err != nil {
		dprintln("  refresh failed:", err)
		return
	}
	dprintf("  token refreshed (expires %s)\n",
		time.UnixMilli(creds.ExpiresAt).Format("2006-01-02 15:04:05"))
}

// heapdumpCmd is /heapdump (mod_0386.js Qlr): policyGate allow_heap_dump.
// Real dumps the JS heap to the Desktop (home on Linux without one); we dump
// a Go pprof heap profile to the same spot with real's progress texts.
func heapdumpCmd(l *agent.Loop) {
	if !tools.PolicyAllows(l.ToolCtx.CWD, "allow_heap_dump") {
		dprintln("  heap dumps are disabled by your organization's policy")
		return
	}
	dprintln("  Writing heap dump…")
	dir := filepath.Join(config.Home(), "Desktop")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		dir = config.Home()
	}
	p := filepath.Join(dir, fmt.Sprintf("claude-go-heap-%s.pb.gz", time.Now().Format("20060102-150405")))
	f, err := os.Create(p)
	if err != nil {
		dprintln("  Couldn't write heap dump —", err)
		return
	}
	defer f.Close()
	runtime.GC()
	if err := pprof.WriteHeapProfile(f); err != nil {
		dprintln("  Couldn't write heap dump —", err)
		return
	}
	dprintln("  Heap dump written to", p)
}

// debugToolCallCmd is /debug-tool-call: show the last tool call and its
// result for inspection.
func debugToolCallCmd(l *agent.Loop) {
	var name, input, id string
	for i := len(l.Messages) - 1; i >= 0 && name == ""; i-- {
		if l.Messages[i].Role != "assistant" {
			continue
		}
		var blocks []api.ContentBlock
		if json.Unmarshal(l.Messages[i].Content, &blocks) != nil {
			continue
		}
		for j := len(blocks) - 1; j >= 0; j-- {
			if blocks[j].Type == "tool_use" {
				name, input, id = blocks[j].Name, string(blocks[j].Input), blocks[j].ID
				break
			}
		}
	}
	if name == "" {
		dprintln("  no tool calls this session")
		return
	}
	dprintln("  last tool call:", name)
	dprintln("  input:", trunc(input, 400))
	for i := len(l.Messages) - 1; i >= 0; i-- {
		var blocks []api.ContentBlock
		if json.Unmarshal(l.Messages[i].Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type == "tool_result" && b.ToolUseID == id {
				dprintln("  result:", trunc(toolResultText(b), 500))
				return
			}
		}
	}
	dprintln("  result: (pending)")
}

// toolResultText flattens a tool_result content payload (string or blocks).
func toolResultText(b api.ContentBlock) string {
	var s string
	if json.Unmarshal(b.Content, &s) == nil {
		return s
	}
	var parts []api.ContentBlock
	if json.Unmarshal(b.Content, &parts) == nil {
		var out []string
		for _, p := range parts {
			if p.Type == "text" {
				out = append(out, p.Text)
			}
		}
		return strings.Join(out, "\n")
	}
	return string(b.Content)
}

// ideCmd reports the IDE bridge status — the auto-connect at startup
// (mcp.ConnectIDE over ~/.claude/ide/*.lock) surfaces as mcp__ide__*
// tools when the extension is live.
func ideCmd() {
	for _, st := range tools.MCPServerStatus() {
		if strings.Contains(st.Name, "ide") || strings.Contains(st.Name, "vscode") {
			if st.Err == nil && st.Tools > 0 {
				dprintf("  IDE connected: %s (%d tools)\n", st.Name, st.Tools)
				return
			}
		}
	}
	dprintln("  no IDE detected — install the Claude Code extension in VS Code/Cursor and reopen this folder")
}

// statusOverlay renders real's /status — a tabbed panel (Settings /
// Status / Config / UsageStats) with session fields + diagnostics.
// Esc/enter/q closes; ←/→ move between tabs (Status is the default).
func statusOverlay(l *agent.Loop, ks *keySource) { statusOverlayAt(l, ks, 1) }

func statusOverlayAt(l *agent.Loop, ks *keySource, tab int) {
	compSuspend()
	defer compResume()
	dch := make(chan key, 16)
	if ks != nil {
		ks.dialog.Store(&dch)
		defer ks.dialog.Store(nil)
	}
	tabs := []string{"Settings", "Status", "Config", "UsageStats"}
	var cfgQuery []rune
	cfgSel, cfgScroll := 0, 0
	draw := func() {
		var sb strings.Builder
		sb.WriteString("\n")
		for i, t2 := range tabs {
			if i == tab {
				sb.WriteString(bold + " " + t2 + " " + reset)
			} else {
				sb.WriteString(dim + " " + t2 + reset)
			}
		}
		sb.WriteString("\n\n")
		switch tab {
		case 1:
			auth := "none"
			if l.Client != nil && l.Client.Creds != nil && (l.Client.Creds.AccessToken != "") {
				auth = "present"
			}
			peer := ""
			if sock := tools.ActiveSocketPath(); sock != "" {
				peer = "uds:" + sock
			}
			for _, row := range [][2]string{
				{"Version", api.Version},
				{"Session name", "/rename to add a name"},
				{"Session ID", l.Client.SessionID},
				{"Session kind", "interactive"},
				{"Peer address", peer},
				{"cwd", l.ToolCtx.CWD},
				{"Auth token", auth},
				{"Model", "Default (" + l.Model + ")"},
				{"Permission mode", l.PermMode},
				{"Transcript", l.Transcript},
			} {
				sb.WriteString(fmt.Sprintf("  %-18s %s\n", row[0]+":", row[1]))
			}
			sb.WriteString("\n" + bold + "System diagnostics" + reset + "\n")
			if _, err := exec.LookPath("claude"); err == nil {
				if home := os.Getenv("HOME"); home != "" {
					if !strings.Contains(os.Getenv("PATH"), filepath.Join(home, ".local", "bin")) {
						sb.WriteString(yellow + "⚠ Native installation exists but ~/.local/bin is not in your PATH." + reset + "\n")
					}
				}
			}
		case 0:
			sb.WriteString("  " + dim + "settings sources: user + project + local" + reset + "\n")
		case 2:
			// real's Config tab: ⌕ search box + scrollable settings list,
			// "↓ N more" indicator, filter-on-type
			w, _, _ := term.GetSize(int(os.Stdout.Fd()))
			if w <= 0 {
				w = 80
			}
			sb.WriteString("\n  " + bold + "Config" + reset + "\n")
			sb.WriteString("  " + "\x1b[38;5;239m╭" + strings.Repeat("─", w-8) + "╮\x1b[39m" + "\n")
			sl := "⌕ Search settings…"
			if len(cfgQuery) > 0 {
				sl = "⌕ " + string(cfgQuery)
			}
			sb.WriteString("  " + "\x1b[38;5;239m│\x1b[39m" + sl + "\n")
			sb.WriteString("  " + "\x1b[38;5;239m╰" + strings.Repeat("─", w-8) + "╯\x1b[39m" + "\n")
			list := filteredConfigEntries(string(cfgQuery))
			_, h, _ := term.GetSize(int(os.Stdout.Fd()))
			vis := h - 14
			if vis < 4 {
				vis = 4
			}
			if cfgSel >= len(list) {
				cfgSel = max(0, len(list)-1)
			}
			if cfgSel < cfgScroll {
				cfgScroll = cfgSel
			}
			if cfgSel >= cfgScroll+vis {
				cfgScroll = cfgSel - vis + 1
			}
			for i := cfgScroll; i < len(list) && i < cfgScroll+vis; i++ {
				e := list[i]
				val := configEntryValue(e, l)
				if i == cfgSel {
					sb.WriteString(fmt.Sprintf(" %s❯%s%s %s%s\n", "\x1b[38;5;153m", "\x1b[39m", e.name, "\x1b[38;5;246m", val+"\x1b[39m"))
				} else {
					sb.WriteString(fmt.Sprintf("   %s %s%s%s\n", e.name, "\x1b[38;5;246m", val, "\x1b[39m"))
				}
				if e.note != "" {
					sb.WriteString("     " + dim + e.note + reset + "\n")
				}
			}
			if more := len(list) - (cfgScroll + vis); more > 0 {
				sb.WriteString(fmt.Sprintf("  %s↓ %d more%s\n", dim, more, reset))
			}
		case 3:
			u := l.UsageTotal
			sb.WriteString(fmt.Sprintf("  input: %d · output: %d · cache read: %d\n",
				u.InputTokens, u.OutputTokens, u.CacheReadInputTokens))
		}
		if tab == 2 {
			sb.WriteString("\n" + dim + "Type to filter · Enter/↓ to select · ↑ to tabs · Esc to clear" + reset + "\n")
		} else {
			sb.WriteString("\n" + dim + "←→ tabs · Esc to cancel" + reset + "\n")
		}
		fmt.Print(sb.String())
	}
	draw()
	if ks == nil {
		return
	}
	for k := range dch {
		if tab == 2 {
			// Config tab: ↑/↓ select, typing filters, ←/→ still tab-switch,
			// Enter toggles the selected entry
			switch {
			case k.seq == "[A":
				if cfgSel > 0 {
					cfgSel--
				}
				draw()
				continue
			case k.seq == "[B":
				if cfgSel < len(filteredConfigEntries(string(cfgQuery)))-1 {
					cfgSel++
				}
				draw()
				continue
			case k.b == 0x7f || k.b == 8:
				if len(cfgQuery) > 0 {
					cfgQuery = cfgQuery[:len(cfgQuery)-1]
				}
				draw()
				continue
			case k.b == '\r':
				toggleConfigEntry(filteredConfigEntries(string(cfgQuery))[cfgSel], l)
				draw()
				continue
			case k.b >= 0x20 && k.b < 0x7f && k.b != 'q':
				cfgQuery = append(cfgQuery, rune(k.b))
				cfgSel = 0
				cfgScroll = 0
				draw()
				continue
			}
		}
		switch {
		case k.seq == "[C" && tab < len(tabs)-1:
			tab++
			draw()
		case k.seq == "[D" && tab > 0:
			tab--
			draw()
		case k.b == 0x1b || k.b == 3 || k.b == 'q' || k.b == '\r':
			return
		}
	}
}

// usageDisplay renders real's /usage — "Current session" / "Current week"
// bars driven by the unified rate-limit headers, plus token totals.
func usageDisplay(l *agent.Loop) {
	bar := func(f float64) string {
		const n = 20
		fill := int(f * n)
		if fill > n {
			fill = n
		}
		return strings.Repeat("█", fill) + dim + strings.Repeat("░", n-fill) + reset
	}
	row := func(label string, util float64, resetAt time.Time) {
		pct := int(util * 100)
		line := fmt.Sprintf("  %-24s %s  %d%% used", label, bar(util), pct)
		if !resetAt.IsZero() {
			line += dim + " · Resets " + resetAt.Local().Format("Mon 3:04 PM") + reset
		}
		dprintln(line)
	}
	rl := l.Client.LastRateLimit()
	dprintln("  " + bold + "Usage" + reset)
	if rl != nil {
		if rl.SlowStatus != "" || rl.SlowBudgetUtilization > 0 {
			row("Current session", rl.SlowBudgetUtilization, rl.SlowBudgetResetsAt)
		}
		if rl.OverageInUse || rl.OverageMonthlyUtilization > 0 {
			row("Extra usage (month)", rl.OverageMonthlyUtilization, rl.OverageResetsAt)
		}
		if rl.Status == "rejected" || rl.Status == "allowed_warning" {
			dprintf("  %s⚠ %s%s\n", yellow, firstNonEmptyStr(rl.RepresentativeClaim, rl.Status), reset)
		}
	} else {
		dprintln("  " + dim + "no rate-limit data yet" + reset)
	}
	u := l.UsageTotal
	dprintf("  %sthis session: %d in · %d out · %d cache-read%s\n",
		dim, u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, reset)
}

func firstNonEmptyStr(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// sessionCostBlock prints real's /cost Session section (captured):
// cost, API/wall durations, code-change tally, token usage, and the
// plugin skill-listing footprint table.
func sessionCostBlock(l *agent.Loop) {
	u := l.UsageTotal
	dprintf("  %sSession%s\n", bold, reset)
	dprintf("  Total cost:            $%.4f\n", l.CostUSD())
	dprintf("  Total duration (API):  %s\n", fmtDur(l.APIDuration()))
	dprintf("  Total duration (wall): %s\n", fmtDur(time.Since(l.SessionStarted())))
	dprintf("  Total code changes:    %d lines added, %d lines removed\n",
		l.LinesAdded.Load(), l.LinesRemoved.Load())
	dprintf("  Usage:                 %d input, %d output, %d cache read, %d cache write\n",
		u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens)
	if fp := pluginSkillFootprint(l); fp != "" {
		dprintf("%s", fp)
	}
}

// pluginSkillFootprint renders real's "Plugin skill-listing footprint"
// section — per-plugin skill count + approx tokens/turn.
func pluginSkillFootprint(l *agent.Loop) string {
	if l.ToolCtx == nil {
		return ""
	}
	return pluginFootprint(l.ToolCtx.CWD)
}

// exportDialog — real's "Export conversation / Select export method"
// chooser: Copy to clipboard / Save to file.
func exportDialog(l *agent.Loop, ks *keySource) {
	compSuspend()
	defer compResume()
	dch := make(chan key, 8)
	ks.dialog.Store(&dch)
	defer ks.dialog.Store(nil)
	cyan := "\x1b[38;5;153m"
	gray := "\x1b[38;5;246m"
	opts := [][2]string{
		{"Copy to clipboard", "Copy the conversation to your system clipboard"},
		{"Save to file", "Save the conversation to a file in the current directory"},
	}
	sel := 0
	draw := func() {
		var sb strings.Builder
		sb.WriteString("\r\x1b[J  " + bold + "Export conversation" + unbold + "\n")
		sb.WriteString("  " + "Select export method" + "\n\n")
		for i, o := range opts {
			if i == sel {
				sb.WriteString(fmt.Sprintf(" %s❯%s %s%d.%s %s%s  %s%s%s\n", cyan, "\x1b[39m", gray, i+1, "\x1b[39m", cyan, o[0]+"\x1b[39m", gray, o[1], "\x1b[39m"))
			} else {
				sb.WriteString(fmt.Sprintf("   %s%d.%s %s  %s%s%s\n", gray, i+1, "\x1b[39m", o[0], gray, o[1], "\x1b[39m"))
			}
		}
		sb.WriteString("\n " + gray + "Esc to cancel" + "\x1b[39m")
		fmt.Print(sb.String())
	}
	draw()
	for k := range dch {
		switch {
		case k.seq == "[A" && sel > 0:
			sel--
			draw()
		case k.seq == "[B" && sel < len(opts)-1:
			sel++
			draw()
		case k.b == 0x1b || k.b == 3:
			dprintln()
			return
		case k.b >= '1' && k.b <= '0'+byte(len(opts)):
			sel = int(k.b - '1')
			fallthrough
		case k.b == '\r':
			dprintln()
			switch sel {
			case 0:
				if copyToClipboardTranscript(l) {
					dprintln("  copied to clipboard")
				}
			case 1:
				exportCmd(l, "")
			}
			return
		}
	}
}

// copyToClipboardTranscript — clipboard export via pbcopy (macOS) or
// xclip/xsel; real's "Copy to clipboard" does the same.
func copyToClipboardTranscript(l *agent.Loop) bool {
	if l.Transcript == "" {
		dprintln("  no transcript")
		return false
	}
	b, err := os.ReadFile(l.Transcript)
	if err != nil {
		dprintln("  export failed:", err)
		return false
	}
	for _, cb := range [][]string{{"pbcopy"}, {"xclip", "-selection", "clipboard"}, {"xsel", "-b"}} {
		cmd := exec.Command(cb[0], cb[1:]...)
		cmd.Stdin = strings.NewReader(string(b))
		if err := cmd.Run(); err == nil {
			return true
		}
	}
	dprintln("  no clipboard tool found")
	return false
}

// pluginFootprint renders the "Plugin skill-listing footprint" section —
// per-plugin skill count + approx token weight per turn.
func pluginFootprint(cwd string) string {
	rows := pluginSkillCounts()
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("  " + bold + "Plugin skill-listing footprint" + reset + "\n")
	b.WriteString("  " + dim + "What each plugin's skill descriptions add to the system prompt (cached\n  input after the first turn). Agents and MCP tools not yet counted." + reset + "\n")
	total := 0
	for _, r := range rows {
		fmt.Fprintf(&b, "  %s%-28s%s%d skill%s · ~%d tok/turn\n",
			dim, r.name, reset, r.count, pluralize(r.count, "", "s"), r.toks)
		total += r.toks
	}
	fmt.Fprintf(&b, "  %sTotal%s  ~%d tok/turn\n", bold, reset, total)
	return b.String()
}

type pluginSkillRow struct {
	name  string
	count int
	toks  int
}

// pluginSkillCounts tallies each installed plugin's SKILL.md description
// footprint (~description length in tokens /turn, real's rough heuristic).
func pluginSkillCounts() []pluginSkillRow {
	b, err := os.ReadFile(filepath.Join(config.ClaudeDir(), "plugins", "installed_plugins.json"))
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	plug, _ := m["plugins"].(map[string]any)
	var out []pluginSkillRow
	for key := range plug {
		name := strings.SplitN(key, "@", 2)[0]
		n, toks := countPluginSkills(name)
		if n > 0 {
			out = append(out, pluginSkillRow{name, n, toks})
		}
	}
	return out
}

// countPluginSkills counts SKILL.md files under a plugin's synced dir and
// approximates their token weight (~len/4 like real's estimator).
func countPluginSkills(plugin string) (n, toks int) {
	root := filepath.Join(config.ClaudeDir(), "plugins", "cache")
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && strings.Contains(p, plugin) && strings.HasSuffix(p, string(filepath.Separator)+"skills") {
			entries, _ := os.ReadDir(p)
			for _, e := range entries {
				if e.IsDir() {
					if _, err := os.Stat(filepath.Join(p, e.Name(), "SKILL.md")); err == nil {
						n++
						if b, err2 := os.ReadFile(filepath.Join(p, e.Name(), "SKILL.md")); err2 == nil {
							toks += len(b) / 4
						}
					}
				}
			}
		}
		return nil
	})
	return
}

// openURL launches a URL in the system browser. Tests gate it via
// CLAUDE_GO_NO_BROWSER=1 — a replica test run must never pop real tabs.
var openURL = func(u string) bool {
	if os.Getenv("CLAUDE_GO_NO_BROWSER") == "1" {
		return true // pretend success so tests exercise the happy path
	}
	if runtime.GOOS != "darwin" {
		return false
	}
	return exec.Command("open", u).Start() == nil
}

// compactSpin drives the "Compacting conversation…" spinner text during
// /compact (real replaces the verb with this literal).
var compactSpin atomic.Bool

// Real's spinnerTipsOverride setting: {excludeDefault, tips[] or
// tipsFile path, label prefix}; spinnerTipsEnabled gates them entirely.
var tipsIdx atomic.Int64
var spinnerTipsList = []string{
	"Tip: Running multiple Claude sessions? Use /color and /rename to tell them apart at a glance.",
	"Tip: Run /status to see your session's model, context usage, and config.",
	"Tip: Use @ to mention files — /files lists what's attached.",
	"Tip: Shift+Tab cycles permission modes — auto mode handles prompts for you.",
	"Tip: ctrl+o opens the transcript viewer for everything Claude has done.",
	"Tip: /compact summarizes context when a session gets long.",
	"Tip: Double-esc rewinds to an earlier checkpoint.",
	"Tip: meta+o toggles fast mode for quicker turns.",
}

var tipsOnce struct {
	once sync.Once
	list []string
}

func tips() []string {
	tipsOnce.once.Do(func() {
		m := config.UserSettings()
		if v, ok := m["spinnerTipsEnabled"].(bool); ok && !v {
			tipsOnce.list = nil
			return
		}
		var st struct {
			Override struct {
				ExcludeDefault bool   `json:"excludeDefault"`
				Label          string `json:"label"`
				Tips           []struct {
					Text string `json:"text"`
				} `json:"tips"`
				TipsFile string `json:"tipsFile"`
			}
		}
		if raw, ok := m["spinnerTipsOverride"]; ok {
			if b, err := json.Marshal(raw); err == nil {
				json.Unmarshal(b, &st.Override)
			}
		}
		var custom []string
		for _, t := range st.Override.Tips {
			if t.Text != "" {
				custom = append(custom, t.Text)
			}
		}
		if st.Override.TipsFile != "" {
			p := st.Override.TipsFile
			if strings.HasPrefix(p, "~/") {
				p = filepath.Join(config.Home(), p[2:])
			}
			if fb, err := os.ReadFile(p); err == nil {
				var arr []struct {
					Text string `json:"text"`
				}
				var flat []string
				if json.Unmarshal(fb, &arr) == nil {
					for _, t := range arr {
						custom = append(custom, t.Text)
					}
				} else if json.Unmarshal(fb, &flat) == nil {
					custom = append(custom, flat...)
				}
			}
		}
		lbl := st.Override.Label
		if lbl == "" {
			lbl = "Tip"
		}
		if st.Override.ExcludeDefault {
			for i := range custom {
				if !strings.Contains(custom[i], ":") {
					custom[i] = lbl + ": " + custom[i]
				}
			}
			tipsOnce.list = custom
			return
		}
		for i := range custom {
			if !strings.Contains(custom[i], ":") {
				custom[i] = lbl + ": " + custom[i]
			}
		}
		tipsOnce.list = append(append([]string{}, spinnerTipsList...), custom...)
	})
	return tipsOnce.list
}

func spinnerTip() string {
	list := tips()
	if len(list) == 0 {
		return ""
	}
	return list[int(tipsIdx.Add(1))%len(list)]
}

// permissionsModal — real's /permissions: a tabbed modal with a bordered
// ⌕ Search… box, numbered rules ("Add a new rule…" first), ←/→ to switch
// tabs, ↑/↓ to select, typing filters, Esc to cancel.
var permTabs = []struct{ key, title, sub string }{
	{"recent", "Recently denied", "Tools Claude Code most recently asked about."},
	{"allow", "Allow", "Claude Code won't ask before using allowed tools."},
	{"ask", "Ask", "Claude Code asks before using these tools."},
	{"deny", "Deny", "Claude Code never uses denied tools."},
	{"auto", "Auto mode", "Auto mode handles permission prompts automatically."},
	{"workspace", "Workspace", "Paths Claude Code may read and write."},
}

func permissionsModal(l *agent.Loop, ks *keySource) {
	compSuspend()
	defer compResume()
	dch := make(chan key, 16)
	ks.dialog.Store(&dch)
	defer ks.dialog.Store(nil)

	cyan := "\x1b[38;5;153m"
	gray := "\x1b[38;5;246m"
	box := "\x1b[38;5;239m"
	tab, sel := 1, 0 // start on "Allow" like real
	var query []rune
	lastLines := 0

	rulesFor := func() []string {
		return l.PermRules[permTabs[tab].key]
	}
	draw := func() {
		if lastLines > 0 {
			fmt.Printf("\x1b[%dA", lastLines)
		}
		fmt.Print("\r\x1b[J")
		lastLines = 0
		emit := func(s string) {
			if lastLines > 0 {
				fmt.Print("\r\n")
			}
			fmt.Print(s)
			w, _, _ := term.GetSize(int(os.Stdout.Fd()))
			if w <= 0 {
				w = 80
			}
			lastLines += 1 + visWidth(s)/w
		}
		w, _, _ := term.GetSize(int(os.Stdout.Fd()))
		if w <= 0 {
			w = 80
		}
		emit(bold + "Permissions" + unbold)
		var tb strings.Builder
		for i, t := range permTabs {
			if i == tab {
				tb.WriteString(cyan + " " + t.title + " " + "\x1b[39m")
			} else {
				tb.WriteString(dim + " " + t.title + " " + reset)
			}
			if i < len(permTabs)-1 {
				tb.WriteString(dim + " " + reset)
			}
		}
		emit(" " + tb.String())
		emit(" " + gray + permTabs[tab].sub + "\x1b[39m")
		emit(" " + box + "╭" + strings.Repeat("─", w-4) + "╮" + reset)
		sl := "⌕ Search…"
		if len(query) > 0 {
			sl = "⌕ " + string(query)
		}
		emit(" " + box + "│" + reset + sl)
		emit(" " + box + "╰" + strings.Repeat("─", w-4) + "╯" + reset)
		rules := rulesFor()
		q := strings.ToLower(string(query))
		var list []string
		list = append(list, "Add a new rule…")
		for _, r := range rules {
			if q == "" || strings.Contains(strings.ToLower(r), q) {
				list = append(list, r)
			}
		}
		if sel >= len(list) {
			sel = len(list) - 1
		}
		if sel < 0 {
			sel = 0
		}
		for i, r := range list {
			row := fmt.Sprintf("%2d. %s", i+1, r)
			if i == sel {
				emit(fmt.Sprintf(" %s❯%s %s%s%s", cyan, "\x1b[39m", cyan, row, "\x1b[39m"))
			} else {
				emit("    " + row)
			}
		}
		emit(" " + gray + "←/→ to switch · ↑/↓ to select · Esc to cancel" + "\x1b[39m")
	}
	draw()
	for k := range dch {
		switch {
		case k.seq == "[D" || k.seq == "[Z": // ← / shift+tab — prev tab
			if tab > 0 {
				tab--
				sel = 0
			}
			draw()
		case k.seq == "[C": // → — next tab
			if tab < len(permTabs)-1 {
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
			sel++
			draw()
		case k.b == 0x1b || k.b == 3:
			fmt.Print("\r\n")
			return
		case k.b == '\r' || k.b == '\n':
			fmt.Print("\r\n")
			if sel == 0 {
				dprintln("  add rules in ~/.claude/settings.json permissions.allow[]")
			}
			return
		case k.b == 0x7f || k.b == 8:
			if len(query) > 0 {
				query = query[:len(query)-1]
			}
			draw()
		case k.b >= 0x20 && k.b < 0x7f:
			query = append(query, rune(k.b))
			sel = 0
			draw()
		}
	}
}

// configEntry is one row in the Config tab's settings list.
type configEntry struct {
	name, note string
	key        string // settings.json key
	enum       []string
}

// configEntries mirrors real's Config tab ordering (captured 2.1.292).
var configEntries = []configEntry{
	{name: "Auto-compact", key: "autoCompactEnabled"},
	{name: "Continue automatically at usage limit", key: "continueAtUsageLimit"},
	{name: "Switch models when a message is flagged", key: "modelOnFlagged", enum: []string{"Ask each time", "Always switch", "Never"}},
	{name: "Show tips", key: "showTips"},
	{name: "Claude-drafted feedback", key: "claudeDraftedFeedback", enum: []string{"notify", "auto", "off"}},
	{name: "Reduce motion", key: "reduceMotion"},
	{name: "Thinking mode", key: "thinking", note: "Thinking can't be turned off for this model"},
	{name: "Fast mode", key: "fastMode"},
	{name: "Prompt suggestions", key: "promptSuggestions"},
	{name: "Session recap", key: "sessionRecap"},
	{name: "Rewind code (checkpoints)", key: "checkpoints"},
	{name: "Notifications", key: "notifications", enum: []string{"off", "notify", "iterm2", "terminal_bell"}},
	{name: "Auto updates", key: "autoUpdates"},
	{name: "Verbose output", key: "verbose"},
	{name: "Terminal progress bar", key: "terminalProgressBar"},
	{name: "Show turn duration", key: "showTurnDuration"},
	{name: "Cleanup period (days)", key: "cleanupPeriodDays"},
	{name: "Co-authored-by", key: "includeCoAuthoredBy"},
}

func filteredConfigEntries(q string) []configEntry {
	q = strings.ToLower(q)
	var out []configEntry
	for _, e := range configEntries {
		if q == "" || strings.Contains(strings.ToLower(e.name), q) {
			out = append(out, e)
		}
	}
	return out
}

func configEntryValue(e configEntry, l *agent.Loop) string {
	if e.key == "fastMode" {
		if l.FastMode {
			return "true"
		}
		return "false"
	}
	v := userSetting(e.key)
	if e.enum != nil {
		s, _ := v.(string)
		if s == "" {
			s = e.enum[0]
		}
		return s + " ›"
	}
	if b, ok := v.(bool); ok && !b {
		return "false"
	}
	return "true"
}

func toggleConfigEntry(e configEntry, l *agent.Loop) {
	if e.key == "fastMode" {
		l.SetFastMode(!l.FastMode)
		return
	}
	if e.enum != nil {
		cur, _ := userSetting(e.key).(string)
		next := e.enum[0]
		for i, o := range e.enum {
			if o == cur {
				next = e.enum[(i+1)%len(e.enum)]
			}
		}
		config.WriteUserSetting(e.key, next)
		fireConfigChange(l, e.key)
		return
	}
	cur, _ := userSetting(e.key).(bool)
	config.WriteUserSetting(e.key, !cur)
	config.InvalidateUserSettings() // the Config tab reads live values
	fireConfigChange(l, e.key)
}

// clockNow renders the turn-end clock honoring timeFormat/timeZone
// settings (real: "auto"|"12-hour"|"24-hour"|"24-hour-utc"|strftime).
func clockNow() string {
	var st struct {
		TF string `json:"timeFormat"`
		TZ string `json:"timeZone"`
	}
	if v, ok := userSetting("timeFormat").(string); ok {
		st.TF = v
	}
	if v, ok := userSetting("timeZone").(string); ok {
		st.TZ = v
	}
	now := time.Now()
	if st.TZ != "" {
		if loc, err := time.LoadLocation(st.TZ); err == nil {
			now = now.In(loc)
		}
	}
	switch st.TF {
	case "24-hour":
		return now.Format("15:04")
	case "24-hour-utc":
		return now.UTC().Format("15:04")
	case "12-hour", "auto", "":
		return now.Format("3:04 PM")
	default:
		// strftime pattern — translate the common verbs
		f := st.TF
		f = strings.ReplaceAll(f, "%H", "15")
		f = strings.ReplaceAll(f, "%I", "03")
		f = strings.ReplaceAll(f, "%M", "04")
		f = strings.ReplaceAll(f, "%S", "05")
		f = strings.ReplaceAll(f, "%p", "PM")
		return now.Format(f)
	}
}

// hooksMenu renders real's read-only hooks viewer — event groups with
// commands truncated to the width, ↓ N more overflow, Enter expands a
// hook's detail, Esc closes.
func hooksMenu(l *agent.Loop, ks *keySource) {
	cfg := hooks.Load(l.CWD)
	if len(cfg) == 0 {
		dprintln("  No hooks configured. Edit settings.json to add one.")
		return
	}
	eventOrder := []string{
		"SessionStart", "UserPromptSubmit", "PreToolUse", "PermissionRequest",
		"PostToolUse", "PostToolUseFailure", "Notification", "PreCompact",
		"PostCompact", "Stop", "SubagentStop", "SessionEnd", "TeammateIdle",
	}
	eventDesc := map[string]string{
		"SessionStart":       "when a new session is started",
		"UserPromptSubmit":   "when the user submits a prompt",
		"PreToolUse":         "before a tool runs",
		"PermissionRequest":  "when a permission prompt would show",
		"PostToolUse":        "after tool execution",
		"PostToolUseFailure": "after a tool fails",
		"Notification":       "on notifications",
		"PreCompact":         "before compaction",
		"PostCompact":        "after compaction",
		"Stop":               "right before Claude concludes its response",
		"SubagentStop":       "when a subagent finishes",
		"SessionEnd":         "when the session ends",
		"TeammateIdle":       "when a teammate goes idle",
	}
	total := 0
	for _, ms := range cfg {
		for _, m := range ms {
			total += len(m.Hooks)
		}
	}
	compSuspend()
	defer compResume()
	dch := make(chan key, 16)
	ks.dialog.Store(&dch)
	defer ks.dialog.Store(nil)
	seen := map[string]bool{}
	var events []string
	for _, e := range eventOrder {
		if _, ok := cfg[e]; ok {
			events = append(events, e)
			seen[e] = true
		}
	}
	for e := range cfg {
		if !seen[e] {
			events = append(events, e)
		}
	}
	sel := 0
	rows := 0
	draw := func() {
		var sb strings.Builder
		if rows > 0 {
			sb.WriteString(fmt.Sprintf("\x1b[%dA\r", rows))
		}
		rows = 0
		emit := func(s string) {
			sb.WriteString("\x1b[K" + s + "\r\n")
			rows++
		}
		emit(fmt.Sprintf("   %sHooks%s", bold, reset))
		emit(fmt.Sprintf("   %d hooks on %d events", total, len(events)))
		emit("   " + dim + "This menu is read-only. To add or change a hook, edit settings.json or ask Claude." + reset)
		emit("")
		i := 0
		for _, ev := range events {
			emit(fmt.Sprintf("  %s%s%s  %s%s%s", bold, ev, reset, dim, eventDesc[ev], reset))
			for _, m := range cfg[ev] {
				for _, h := range m.Hooks {
					cur := "  "
					if i == sel {
						cur = menuCyan + "❯" + reset
					}
					cmd := h.Command
					if len(cmd) > 44 {
						cmd = cmd[:44] + "…"
					}
					emit(fmt.Sprintf("  %s %s%s", cur, cmd, reset))
					i++
				}
			}
			emit("")
		}
		emit("  " + dim + "Enter to view · Esc to close" + reset)
		fmt.Print(sb.String())
	}
	draw()
	for k := range dch {
		switch {
		case k.seq == "[A":
			if sel > 0 {
				sel--
				draw()
			}
		case k.seq == "[B":
			if sel < total-1 {
				sel++
				draw()
			}
		case k.b == 0x1b || k.b == 3 || k.b == 'q':
			dprintln()
			return
		case k.b == '\r':
			// expand the selected hook's full command inline
			i := 0
			for _, ev := range events {
				for _, m := range cfg[ev] {
					for _, h := range m.Hooks {
						if i == sel {
							dprintln()
							dprintln("  ⎿ ", h.Command)
							return
						}
						i++
					}
				}
			}
		}
	}
}

// effortSlider renders real's /effort gauge — Faster◄─────►Smarter with a
// ▲ marker over low|medium|high|xhigh|max, Ultracode toggle on Tab,
// ←/→ adjust, Enter confirm, s session-only, Esc cancel.
func effortSlider(l *agent.Loop, ks *keySource) {
	levels := []string{"low", "medium", "high", "xhigh", "max"}
	cur := 1
	for i, e := range levels {
		if e == l.Effort {
			cur = i
		}
	}
	ultra := l.Effort == "ultracode"
	compSuspend()
	defer compResume()
	dch := make(chan key, 16)
	ks.dialog.Store(&dch)
	defer ks.dialog.Store(nil)
	rows := 0
	draw := func() {
		var sb strings.Builder
		if rows > 0 {
			sb.WriteString(fmt.Sprintf("\x1b[%dA\r", rows))
		}
		rows = 0
		emit := func(s string) { sb.WriteString("\x1b[K" + s + "\r\n"); rows++ }
		emit("  " + bold + "Effort" + reset)
		emit("        Faster" + dim + "                                    " + reset + "Smarter")
		// gauge: marker position proportional across the level count
		const width = 46
		pos := 8 + cur*(width-8)/(len(levels)-1)
		var bar strings.Builder
		for i := 0; i < width; i++ {
			if i == pos {
				bar.WriteString(menuCyan + "▲" + reset)
			} else {
				bar.WriteString("─")
			}
		}
		emit("  " + bar.String() + dim + " Ultracode  " + reset + map[bool]string{true: "on", false: "off"}[ultra])
		lbl := "         " + strings.Join(levels, "     ") + dim + "  Tab to toggle" + reset
		emit("  " + lbl)
		emit("")
		emit("  " + dim + " ←/→ to adjust · Enter to confirm · s for this session only · Esc to cancel" + reset)
		fmt.Print(sb.String())
	}
	draw()
	for k := range dch {
		switch {
		case k.seq == "[D":
			if cur > 0 {
				cur--
				draw()
			}
		case k.seq == "[C":
			if cur < len(levels)-1 {
				cur++
				draw()
			}
		case k.b == '\t':
			ultra = !ultra
			draw()
		case k.b == 0x1b || k.b == 3:
			dprintln()
			return
		case k.b == 's', k.b == '\r':
			if ultra {
				l.Effort = "ultracode"
			} else {
				l.Effort = levels[cur]
			}
			dprintln()
			dprintln("  effort →", l.Effort)
			return
		}
	}
}

// menuCyan is the 153-cyan real uses for ❯ cursors and gauges.
const menuCyan = "\x1b[38;5;153m"

// sandboxPicker renders real's /sandbox mode chooser (captured 2.1.292):
// three rows, ✔ current, auto-allow explainer, ←/→ + ↑/↓ + Enter/Esc.
func sandboxPicker(l *agent.Loop, ks *keySource) {
	type opt struct{ label, hint string }
	opts := []opt{
		{"Sandbox Bash Tool, with auto-allow", ""},
		{"Sandbox Bash Tool, with regular permissions", ""},
		{"No Sandbox", ""},
	}
	cur := 2 // No Sandbox default (CLAUDE_GO_SANDBOX unset)
	if os.Getenv("CLAUDE_GO_SANDBOX") == "1" {
		cur = 0
	}
	compSuspend()
	defer compResume()
	dch := make(chan key, 16)
	ks.dialog.Store(&dch)
	defer ks.dialog.Store(nil)
	rows := 0
	draw := func() {
		var sb strings.Builder
		if rows > 0 {
			sb.WriteString(fmt.Sprintf("\x1b[%dA\r", rows))
		}
		rows = 0
		emit := func(s string) { sb.WriteString("\x1b[K" + s + "\r\n"); rows++ }
		emit("  " + bold + "Sandbox Mode Overrides Config" + reset)
		emit("")
		emit("  " + dim + "Configure mode" + reset)
		for i, o := range opts {
			c := "   "
			if i == cur {
				c = menuCyan + "❯" + reset + " "
			}
			mark := ""
			if (i == 2 && os.Getenv("CLAUDE_GO_SANDBOX") == "") ||
				(i == 0 && os.Getenv("CLAUDE_GO_SANDBOX") == "1") {
				mark = " " + menuCyan + "✔" + reset
			}
			emit(fmt.Sprintf(" %s%d. %s%s", c, i+1, o.label, mark))
		}
		emit("  " + dim + "Auto-allow mode: Commands will try to run in the sandbox automatically," + reset)
		emit("  " + dim + "and attempts to run outside of the sandbox fallback to regular" + reset)
		emit("  " + dim + "permissions. Explicit ask/deny rules are always respected." + reset)
		emit("")
		emit("  " + dim + "Learn more: https://code.claude.com/docs/en/sandboxing" + reset)
		emit("")
		emit("  " + dim + " ←/→ to switch · ↑/↓ to navigate · Enter to select · Esc to close" + reset)
		fmt.Print(sb.String())
	}
	draw()
	for k := range dch {
		switch {
		case k.seq == "[A" || k.seq == "[D":
			if cur > 0 {
				cur--
				draw()
			}
		case k.seq == "[B" || k.seq == "[C":
			if cur < len(opts)-1 {
				cur++
				draw()
			}
		case k.b == 0x1b || k.b == 3:
			dprintln()
			return
		case k.b == '\r':
			switch cur {
			case 0, 1: // both sandbox-on variants — auto-allow vs regular
				os.Setenv("CLAUDE_GO_SANDBOX", "1")
				dprintln()
				dprintln("  sandbox →", opts[cur].label)
			case 2:
				os.Unsetenv("CLAUDE_GO_SANDBOX")
				dprintln()
				dprintln("  sandbox → No Sandbox")
			}
			return
		}
	}
}

// fireConfigChange runs ConfigChange hooks when a settings write lands
// (real's settings watcher → hooks with source=user_settings).
func fireConfigChange(l *agent.Loop, key string) {
	if l == nil || l.Hooks == nil {
		return
	}

	hooks.Run(l.Hooks, hooks.ConfigChange, map[string]any{
		"session_id": l.Client.SessionID, "cwd": l.ToolCtx.CWD,
		"hook_event_name": "ConfigChange", "permission_mode": l.PermMode,
		"transcript_path": l.Transcript,
		"source":          "user_settings",
		"file_path":       filepath.Join(config.ClaudeDir(), "settings.json"),
		"key":             key,
	}, l.ToolCtx.CWD)
}

// ---- /plan surface (real mod_2070: plansDirectory default ~/.claude/plans) ----

// planPath returns the session's plan file — ~/.claude/plans/<session>.md,
// or <plansDirectory> under the project root when configured.
func planPath(l *agent.Loop) string {
	if pd, _ := config.UserSetting("plansDirectory").(string); pd != "" {
		return filepath.Join(l.ToolCtx.CWD, pd, l.Client.SessionID+".md")
	}
	return filepath.Join(config.ClaudeDir(), "plans", l.Client.SessionID+".md")
}

// planCurrent reads the live plan (content + path) for the /plan panel.
func planCurrent(l *agent.Loop) (content, path string) {
	p := planPath(l)
	b, err := os.ReadFile(p)
	if err != nil {
		return "", p
	}
	return strings.TrimSpace(string(b)), p
}

// planWrite seeds the plan file with the /plan <description> argument.
func planWrite(l *agent.Loop, desc string) {
	p := planPath(l)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(desc+"\n"), 0o600)
	os.MkdirAll(filepath.Dir(p), 0o755) // ensure dir exists before write
	os.WriteFile(p, []byte(desc+"\n"), 0o600)
}

// planOpenInEditor opens the plan file in $EDITOR — real's `/plan open`.
func planOpenInEditor(l *agent.Loop, ks *keySource) {
	p := planPath(l)
	if _, err := os.Stat(p); err != nil {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(""), 0o600)
	}
	ed := os.Getenv("EDITOR")
	if ed == "" {
		ed = os.Getenv("VISUAL")
	}
	if ed == "" {
		dprintln("  no $EDITOR set — plan file:", p)
		return
	}
	compSuspend()
	defer compResume()
	if ks != nil {
		ks.release()
		defer ks.grab()
	}
	cmd := exec.Command("sh", "-c", ed+" "+p)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Run()
	dprintln("  plan saved:", p)
}
