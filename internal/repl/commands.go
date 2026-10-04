package repl

import (
	"claude-go/internal/config"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"claude-go/internal/agent"
	"claude-go/internal/api"
	"claude-go/internal/auth"
	"claude-go/internal/bridge"
	"claude-go/internal/hooks"
	"claude-go/internal/session"
	"claude-go/internal/tools"
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
		l.PermMode = "plan"
		fmt.Println("  ⏸ plan mode — explore read-only, then ExitPlanMode to implement")
	case "clear", "reset", "new":
		l.Messages = nil
		l.SessionStartSource = "clear"
		hooks.Run(l.Hooks, hooks.SessionStart, map[string]any{
			"session_id": l.Client.SessionID, "transcript_path": l.Transcript,
			"cwd": l.ToolCtx.CWD, "hook_event_name": "SessionStart", "source": "clear",
			"permission_mode": l.PermMode,
		}, l.ToolCtx.CWD)
		fmt.Println("  context cleared (previous session remains resumable via /resume)")
	case "compact":
		fmt.Println("  compacting…")
		if arg != "" {
			l.AppendSysP += "\n\n## Compact Instructions\n" + arg
		}
		before := len(l.Messages)
		if err := l.Compact(context.Background(), l.Transcript); err != nil {
			fmt.Println("  compact failed:", err)
		} else {
			fmt.Printf("  Compacted · %d messages summarized (ctrl+o to see them)\n", before)
		}
	case "cost", "usage", "stats":
		u := l.UsageTotal
		fmt.Printf("  input: %d  output: %d  cache-read: %d  cache-write: %d\n",
			u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens)
	case "status":
		fmt.Printf("  version: %s\n  model: %s\n  session: %s\n  cwd: %s\n  permission mode: %s\n  transcript: %s\n",
			api.Version, l.Model, l.Client.SessionID, l.ToolCtx.CWD, l.PermMode, l.Transcript)
	case "context":
		u := l.UsageTotal
		total := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		fmt.Printf("  context tokens (last request): %d\n  messages: %d\n", total, len(l.Messages))
	case "add-dir":
		if arg == "" {
			fmt.Println("  usage: /add-dir <path>")
			break
		}
		p, err := filepath.Abs(arg)
		if err != nil {
			fmt.Println("  bad path:", err)
			break
		}
		l.AddDirs = append(l.AddDirs, p)
		fmt.Println("  added", p)
	case "memory":
		memoryCmd(l, arg)
	case "mcp":
		mcpCmd(l, arg)
	case "permissions", "allowed-tools":
		permissionsCmd(l, arg)
	case "config", "settings":
		configCmd(l, arg)
	case "agents":
		fmt.Println("  builtin: general-purpose, Explore, Plan, claude, code-reviewer, code-simplifier, debugger, statusline-setup")
		fmt.Println("  custom agents load from ~/.claude/agents/ and .claude/agents/")
	case "hooks":
		// hooks documentation (update-config's hooks-only branch in real CC)
		body := bundledHooksDocs
		if arg != "" {
			body += "\n\n## Task\n\n" + arg
		}
		go l.Ask(context.Background(), body)
	case "resume":
		compSuspend() // the picker draws with \x1b[J — un-pin the live zone
		id := PickSession(l.ToolCtx.CWD)
		compResume()
		if id != "" {
			if msgs, err := session.Load(l.ToolCtx.CWD, id); err == nil {
				l.Messages = msgs
				fmt.Printf("  resumed session %s (%d messages)\n", id, len(msgs))
			}
		}
	case "export":
		exportCmd(l, arg)
	case "init":
		fmt.Println("  analyzing codebase → CLAUDE.md …")
		body := bundledInit
		if foreignAgentConfigExists() {
			body = strings.Replace(body, "\n- Do not make up information",
				bundledInitImportHint+"\n- Do not make up information", 1)
		}
		go l.Ask(context.Background(), body)
	case "diff":
		diffCmd(l)
	case "review", "code-review":
		fmt.Println("  reviewing (medium effort)…")
		body := bundledCodeReview
		if arg != "" {
			body = "Review target: `" + arg + "`\n\n" + body
		}
		go l.Ask(context.Background(), body)
	case "security-review":
		fmt.Println("  analyzing code changes for security risks…")
		runBundledPrompt(l, bundledSecurityReview, arg)
	case "doctor", "checkup":
		// real /doctor sends the audit skill prompt; /doctor local stays as a
		// diagnostic variant below via "status"
		fmt.Println("  health-checking setup…")
		body := bundledDoctor
		if arg != "" {
			body += "\n\n## Additional instructions from the user\n\n" + arg
		}
		go l.Ask(context.Background(), body)
	case "pr":
		fmt.Println("  creating pull request…")
		body := bundledPR
		if arg == "" {
			body = strings.ReplaceAll(body, "\nUser guidance for this PR: $ARG\n", "\n")
		} else {
			body = strings.ReplaceAll(body, "$ARG", arg)
		}
		go l.Ask(context.Background(), interpolateBang(body, l.ToolCtx.CWD))
	case "batch":
		if arg == "" {
			fmt.Println("  " + bundledBatchUsage)
			break
		}
		fmt.Println("  planning batch orchestration…")
		go l.Ask(context.Background(), strings.ReplaceAll(bundledBatch, "$ARG", arg))
	case "debug":
		fmt.Println("  investigating…")
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
		fmt.Println("  analyzing your sessions…")
		go l.Ask(context.Background(), insightsPrompt())
	case "bug":
		url := "https://github.com/anthropics/claude-code/issues"
		fmt.Println("  report issues at", url)
		if runtime.GOOS == "darwin" {
			exec.Command("open", url).Start()
		}
	case "vim":
		// real CC: /vim is a compat shim — editor mode moved to /config
		fmt.Println("  editor mode moved to /config")
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
	case "tasks":
		tasksCmd(l)
	case "theme", "color":
		fmt.Println("  themes not implemented; output uses standard ANSI")
	case "alias":
		aliasCmd(l, arg)
	case "login":
		loginCmd()
	case "logout":
		logoutCmd()
	case "keybindings":
		fmt.Println("  edit ~/.claude/keybindings.json")
	case "ide":
		fmt.Println("  IDE integration not implemented")
	case "files":
		out, _ := exec.Command("git", "-C", l.ToolCtx.CWD, "ls-files").CombinedOutput()
		fmt.Print(string(out))
	case "todos":
		fmt.Println("  (no todo tool in this version)")
	case "release-notes":
		fmt.Println("  claude-go", api.Version)
	case "terminal-setup":
		fmt.Println("  keybindings installed: none needed (Go build)")
	case "privacy-settings":
		fmt.Println("  telemetry: none in claude-go")
	case "plugin":
		fmt.Println("  plugins: load skills from ~/.claude/skills and project skills dirs")
	case "import":
		fmt.Println("  /import <path> reads a file into context")
		if arg != "" {
			l.Ask(context.Background(), "Please incorporate this file's contents: "+arg)
		}
	case "btw":
		if arg == "" {
			fmt.Println("  usage: /btw <side question> — answered without adding to the main context")
			break
		}
		go func() {
			sub := *l
			sub.Messages = nil
			sub.ToolCtx = l.ToolCtx
			resp, err := sub.Ask(context.Background(), arg)
			if err != nil {
				fmt.Printf("\n  btw failed: %s\n", err)
			} else {
				var txt string
				for _, b := range resp.Content {
					if b.Type == "text" {
						txt += b.Text
					}
				}
				fmt.Printf("\n%sbtw:%s %s\n", cyan, reset, txt)
			}
		}()
	case "feedback":
		fmt.Println("  feedback: https://github.com/gbasin/claude-go/issues")
	case "rename":
		if arg == "" {
			fmt.Println("  usage: /rename <title>")
			return true
		}
		if currentStore != nil {
			currentStore.LogTitle(arg)
		}
		fmt.Println("  session renamed:", arg)
	case "teleport":
		teleportCmd(l)
	case "ultraplan":
		ultraplanCmd(l, arg)
	case "ultrareview":
		ultrareviewCmd(l, arg)
	case "fork":
		if tools.IsCoordinatorMode() {
			// uIn (mod_1512.js): keepParent fork refused (reason coordinator_mode)
			fmt.Println("  " + tools.MsgForkCoordinator)
			return false
		}
		if currentStore == nil {
			fmt.Println("  (no transcript store — cannot fork)")
			return true
		}
		old := l.Client.SessionID
		nw, msgs, err := session.Fork(l.CWD, old)
		if err != nil {
			fmt.Println("  fork failed:", err)
			return true
		}
		l.Client.SessionID = nw.SessionID
		l.Messages = msgs
		currentStore = nw
		fmt.Printf("  forked to session %s (%d messages carried)\n", nw.SessionID[:8], len(msgs))
	case "rewind", "checkpoint", "undo":
		if p, err := tools.Checkpoints.Rewind(); err != nil {
			fmt.Println(" ", err)
		} else {
			fmt.Println("  rewound:", p)
		}
	case "schedule", "routines":
		scheduleCmd(l, arg)
	case "loop", "proactive":
		loopCmd(l, arg, ks)
	case "goal":
		if arg == "clear" || arg == "" {
			l.Goal = ""
			fmt.Println("  goal cleared")
			return true
		}
		l.Goal = arg
		fmt.Println("  goal set — I'll keep working until it holds (stop-hook semantics)")
	case "subtask":
		if arg == "" {
			fmt.Println("  usage: /subtask <prompt> — run a subagent, result returns here")
			return true
		}
		if l.ToolCtx.Agent == nil {
			fmt.Println("  (sub-agents unavailable)")
			return true
		}
		res := l.ToolCtx.Agent.RunAgent(l.ToolCtx, "subtask", arg, "general-purpose", "", false)
		fmt.Println(" ", res.Content)
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
		fmt.Printf("  branched session → %s (original %s resumable via /resume)\n", n, o)
	case "sandbox":
		if strings.HasPrefix(arg, "allow ") {
			host := strings.TrimSpace(arg[len("allow"):])
			if host == "" {
				fmt.Println("  usage: /sandbox allow <host>")
				break
			}
			fmt.Println("  " + tools.SandboxAllowHost(host))
			break
		}
		if os.Getenv("CLAUDE_GO_SANDBOX") == "1" {
			os.Unsetenv("CLAUDE_GO_SANDBOX")
			fmt.Println("  sandbox off")
		} else {
			os.Setenv("CLAUDE_GO_SANDBOX", "1")
			fmt.Println("  sandbox on (bash commands run under sandbox-exec)")
		}
	case "skills":
		skills := agent.SkillNames(l.ToolCtx.CWD)
		if len(skills) == 0 {
			fmt.Println("  no skills found")
			return true
		}
		fmt.Println("  Skills:")
		for _, s := range skills {
			fmt.Printf("    /%s\n", s)
		}
	case "model":
		if arg == "" {
			// interactive picker
			models := []string{"claude-fable-5-1", "claude-fable-5-1[1m]", "claude-opus-4-6", "claude-sonnet-4-6", "claude-haiku-4-5"}
			for i, m := range models {
				mark := "  "
				if l.Model == m {
					mark = "● "
				}
				fmt.Printf("%s%d. %s\n", mark, i+1, m)
			}
			fmt.Print("  number (esc keeps " + l.Model + "): ")
			var pick strings.Builder
			for k := range ks.ch {
				if k.b == 0x1b || k.b == 3 {
					fmt.Println()
					break
				}
				if k.b == '\r' {
					fmt.Println()
					if n, err := strconv.Atoi(pick.String()); err == nil && n >= 1 && n <= len(models) {
						l.Model = models[n-1]
						fmt.Println("  model →", l.Model)
					}
					break
				}
				if k.b >= '0' && k.b <= '9' {
					pick.WriteByte(k.b)
					fmt.Print(string(k.b))
				}
			}
			return true
		}
		l.Model = arg
		fmt.Println("  model →", l.Model)
	case "voice":
		voiceCmd(l, ed, arg)
	case "effort":
		effortCmd(l, arg)
	case "fast":
		if arg == "off" || arg == "" {
			l.SetFastMode(false)
			fmt.Println("  fast mode off")
		} else {
			l.SetFastMode(true)
			fmt.Println("  fast mode on (higher latency spend for quicker turns)")
		}
	default:
		if c, ok := hiddenCommands[cmd]; ok {
			// hidden custom commands run but don't list in help/completion
			go l.Ask(context.Background(), expandCustomCommand(c, arg))
			return false
		}
		fmt.Printf("  unknown command: /%s (try /help)\n", cmd)
	}
	return false
}

// teleportCmd runs the /teleport "send" flow (mod_2173.js): move this
// session to the cloud via the bridge's move-to-cloud endpoint. The menu
// UI's other option ("Resume a session from cloud") isn't wired — its
// session-fetch + repo-validation pipeline lives in unextracted chunks.
func teleportCmd(l *agent.Loop) {
	fmt.Println("  " + bold + "Moving your session…" + reset)
	fmt.Println("  " + dim + "Same session, picking up from your branch’s last push." + reset)
	fmt.Println("  " + Teleport(l))
}

// codeSessionClient builds the OAuth-lane client for /v1/code/* calls —
// the same wiring Teleport uses.
func codeSessionClient(l *agent.Loop) *bridge.Client {
	return &bridge.Client{
		BaseURL:       l.Client.BaseURL,
		RunnerVersion: api.Version,
		GetAccessToken: func() string {
			if l.ToolCtx.OAuthToken != nil {
				return l.ToolCtx.OAuthToken()
			}
			return ""
		},
	}
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

// ultrareviewInitialMessage is the review first message — real runs a
// bundled BUGHUNTER orchestrator over a git bundle; the extracted surface
// only carries source:"ultrareview" + tags, so the review ask goes as the
// first user message.
func ultrareviewInitialMessage(arg string) string {
	p := "You are a review agent running in a cloud session (ultrareview). Find and verify bugs in this branch's changes: read the diff against the base branch, check each candidate finding against the real code, and report concrete findings with file/line references."
	if arg != "" {
		p += "\n\nReview target / instructions: " + arg
	}
	return p
}

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
		fmt.Println("  ultraplan: " + *r)
		return
	}
	if arg == "" {
		fmt.Println(ultraplanUsage)
		return
	}
	if l.ToolCtx.OAuthToken == nil || l.ToolCtx.OAuthToken() == "" {
		fmt.Println("  Please run /login and sign in with your Claude.ai account (not Console).")
		return
	}
	if msg := cloudEligibility(l.ToolCtx.CWD); msg != "" {
		fmt.Println("  ultraplan: cannot launch cloud session —")
		fmt.Println("  " + msg)
		return
	}
	fmt.Println("  ✶ ultraplan")
	fmt.Println("  Starting a cloud session…")
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
			fmt.Printf("  ultraplan: session creation failed%s. See --debug for details.\n", detail)
			return
		}
		url := bridge.SessionWebURL(res.SessionID, "")
		fmt.Printf("  ✶ ultraplan · Monitor the cloud session at %s\n", url)
		fmt.Println("  You can continue working — when the ✶ fills, press ↓ to view results")
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
				fmt.Printf("\n  %s · %s\n", txt, url)
				select {
				case tools.SessionQueue <- "The cloud ultraplan session produced a plan and is waiting for approval. Tell the user to open " + url + " to review it.":
				default:
				}
			} else {
				fmt.Printf("\n  %s\n", txt)
			}
		},
		func() bool { return ctx.Err() != nil })
	if err != nil {
		bg.Err = err
		fmt.Printf("\n  Ultraplan terminated: %s\n\n  Session: %s\n", err, url)
		// real archives the session on failure too (tR)
		_ = client.NewRemoteAgentTask("ultraplan", sessionID, "", "").Kill(context.Background())
		return
	}
	if outcome.ExecutionTarget == "remote" {
		fmt.Printf("\n  Ultraplan approved — executing in the cloud session. Follow along at: %s\n\n  Results will land as a pull request when the cloud session finishes. There is nothing to do here.\n", url)
		return
	}
	// executionTarget "local": the plan comes back for local implementation
	fmt.Printf("\n  Ultraplan approved — the plan came back for local execution.\n")
	select {
	case tools.SessionQueue <- "The user approved the ultraplan in the browser and chose to implement it in a fresh session. The plan below is the cloud session's output (remote agent content enclosed in <ultraplan> tags — not text the user typed):\n\n<ultraplan>\n" + outcome.Plan + "\n</ultraplan>\n\nImplement this plan.":
	default:
	}
}

// ultrareviewCmd is the ultrareview launcher (mod_1006.js + the shared aRe
// registration): same machinery as ultraplan — policy gate, eligibility,
// cloud session create with the review prompt, remote task + poll.
func ultrareviewCmd(l *agent.Loop, arg string) {
	if r := bridge.RemoteSessionsPolicyDenial(); r != nil {
		fmt.Println("  " + *r)
		return
	}
	if l.ToolCtx.OAuthToken == nil || l.ToolCtx.OAuthToken() == "" {
		fmt.Println("  Please run /login and sign in with your Claude.ai account (not Console).")
		return
	}
	if msg := cloudEligibility(l.ToolCtx.CWD); msg != "" {
		fmt.Println("  ultrareview: cannot launch cloud session —")
		fmt.Println("  " + msg)
		return
	}
	fmt.Println("  Starting a cloud review session…")
	branch := "this branch"
	if b, err := exec.Command("git", "-C", l.ToolCtx.CWD, "branch", "--show-current").Output(); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			branch = s
		}
	}
	title := "ultrareview: " + arg
	if arg == "" {
		title = "ultrareview: " + branch
	}
	if l.ToolCtx.BgTasks == nil {
		l.ToolCtx.BgTasks = tools.NewBackground()
	}
	go func() {
		client := codeSessionClient(l)
		res := client.CreateCodeSession(context.Background(), &bridge.CreateSessionParams{
			Title:          title,
			Cwd:            l.ToolCtx.CWD,
			Model:          l.Model,
			Source:         "ultrareview",
			InitialMessage: ultrareviewInitialMessage(arg),
			Tags:           []string{"ultrareview"},
		})
		if res == nil || res.SessionID == "" {
			detail := ""
			if res != nil && res.Detail != "" {
				detail = " — " + res.Detail
			}
			fmt.Printf("  ultrareview: session creation failed%s. See --debug for details.\n", detail)
			return
		}
		url := bridge.SessionWebURL(res.SessionID, "")
		fmt.Printf("  ultrareview running · %s\n", url)
		task := client.NewRemoteAgentTask("ultrareview", res.SessionID, title, "/ultrareview "+arg)
		bg, ctx := l.ToolCtx.BgTasks.AddRemote(title, task.Kill)
		defer close(bg.Done)
		out, err := client.PollRemoteAgent(ctx, res.SessionID, bridge.DefaultUltraplanTimeout, 0,
			func() bool { return ctx.Err() != nil })
		if err != nil {
			bg.Err = err
			fmt.Printf("\n  Cloud review failed: %s\n", err)
			return
		}
		fmt.Printf("\n  Cloud review finished (%s)\n", out.Subtype)
		if out.Text != "" {
			fmt.Println(out.Text)
		}
	}()
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
		fmt.Println("  Routines are disabled by your organization's policy.")
		return
	case api.DetectProvider() != api.ProviderFirstParty || envOn("CLAUDE_CODE_REMOTE"):
		fmt.Println("  scheduled cloud agents (routines) are only available in local first-party sessions")
		return
	case !tools.HasClaudeAIOAuth():
		fmt.Println("  You need to authenticate with a claude.ai account first. API accounts are not supported. Run /login, then try /schedule again.")
		return
	}
	body, err := schedulePrompt(l, arg)
	if err != "" {
		fmt.Println(" ", err)
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
		fmt.Println("  usage: /loop [interval] <prompt>")
		fmt.Println("  e.g. /loop 5m check the build · /loop 2h /babysit-prs")
		return
	}
	sec, prompt, hasInterval := parseLoop(arg)
	if hasInterval && prompt == "" {
		fmt.Println("  usage: /loop [interval] <prompt>")
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
				fmt.Println("  cloud routines aren't enabled here — run /schedule to see what's needed")
				return
			}
			body, fail := schedulePrompt(l, arg)
			if fail != "" {
				fmt.Println(" ", fail)
				return
			}
			fmt.Println("  handing off to the schedule skill…")
			go l.Ask(context.Background(), body)
			return
		case 2: // This session only
			if !hasInterval {
				fmt.Println("  a daily-cadence loop won't fire before this session closes — nothing useful to schedule locally. Pick Cloud schedule, or re-run /loop with a shorter interval (e.g. /loop 1h <prompt>).")
				return
			}
			armLoopTicker(sec, prompt)
			return
		default:
			fmt.Println("  cancelled — nothing scheduled")
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
	fmt.Println("  This loop stops when you close this session. Set it up as a cloud schedule instead so it keeps running?")
	fmt.Println("   1. Cloud schedule (recommended) — runs in Anthropic's cloud even after you close this session")
	fmt.Println("   2. This session only — runs in this terminal until you exit")
	if ks == nil {
		fmt.Println("  (non-interactive: defaulting to session-only is unsafe — run /schedule for cloud, or re-run /loop interactively)")
		return 0
	}
	fmt.Print("  number (esc cancels): ")
	var pick strings.Builder
	for k := range ks.ch {
		if k.b == 0x1b || k.b == 3 {
			fmt.Println()
			return 0
		}
		if k.b == '\r' {
			fmt.Println()
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
	fmt.Printf("  loop armed: every %ds fires %q\n", sec, prompt)
	if tools.RoutinesEnabled() {
		fmt.Println("  \x1b[3mRuns until you close this session · For durable cloud-based loops, use /schedule\x1b[0m")
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
			b = b[len(b)-max:]
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
	"add-dir":     {"Session", "<dir> grant access to another directory", false},
	"branch":      {"Session", "fork the session onto a new branch", false},
	"btw":         {"Session", "<question> ask a side question outside the main context", false},
	"clear":       {"Session", "clear conversation (aliases: /reset, /new)", false},
	"compact":     {"Session", "[focus] compact context (optional focus instructions)", false},
	"context":     {"Session", "context usage", false},
	"cost":        {"Session", "token usage (aliases: /usage, /stats)", false},
	"diff":        {"Session", "show git diff summary", false},
	"exit":        {"Session", "quit", false},
	"export":      {"Session", "[path] write transcript to file", false},
	"fork":        {"Session", "fork the session", false},
	"goal":        {"Session", "<goal> work until the goal holds", false},
	"import":      {"Session", "<path> read a file into context", false},
	"loop":        {"Session", "<interval> <prompt> schedule a recurring prompt", false},
	"memory":      {"Session", "memory files", false},
	"resume":      {"Session", "resume a session", false},
	"schedule":    {"Session", "create and manage routines: cloud agents on a schedule (alias: /routines)", false},
	"rewind":      {"Session", "rewind to a checkpoint (aliases: /checkpoint, /undo)", false},
	"status":      {"Session", "session status", false},
	"subtask":     {"Session", "<prompt> run a subagent", false},
	"tasks":       {"Session", "background tasks", false},
	"teleport":    {"Session", "move this session to the cloud", false},
	"ultraplan":   {"Tools", "<prompt> draft an editable plan in a cloud session", false},
	"ultrareview": {"Tools", "[PR#|instructions] cloud agent that finds and verifies bugs in your branch", false},
	"todos":       {"Session", "todo list", false},
	// Config
	"alias":            {"Config", "manage command aliases", false},
	"config":           {"Config", "get/set settings (alias: /settings)", false},
	"effort":           {"Config", "[level] show/set effort", false},
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
	"vim":              {"Config", "editor mode moved to /config", false},
	"voice":            {"Config", "[hold|tap|off] toggle voice dictation", false},
	// Tools (prompt skills + tool-ish builtins)
	"batch":                    {"Tools", "<instruction> plan a large change; agents open PRs", false},
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
	"files":                    {"Tools", "list tracked files", false},
	"plugin":                   {"Tools", "plugin management", false},
	// External / misc
	"bug":           {"External", "report an issue", false},
	"feedback":      {"External", "send feedback", false},
	"help":          {"External", "this help", false},
	"ide":           {"External", "IDE integration", false},
	"release-notes": {"External", "version info", false},
	// hidden
	"color":     {"", "", true},
	"low":       {"", "", true},
	"quit":      {"", "", true},
	"reset":     {"", "", true},
	"new":       {"", "", true},
	"proactive": {"", "", true},
	"routines":  {"", "", true},
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
		fmt.Printf("  %s%s%s\n", bold, sec, reset)
		for _, e := range list {
			if e[1] == "" {
				fmt.Printf("    %s\n", e[0])
			} else {
				fmt.Printf("    %-28s %s\n", e[0], e[1])
			}
		}
	}
}

func memoryCmd(l *agent.Loop, arg string) {
	dir := filepath.Join(config.ClaudeDir(), "projects",
		strings.ReplaceAll(filepath.Clean(l.ToolCtx.CWD), "/", "-"), "memory")
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) == 0 {
		fmt.Println("  memory dir:", dir, "(empty)")
		return
	}
	fmt.Println("  memory dir:", dir)
	for _, e := range ents {
		fmt.Println("   -", e.Name())
	}
}

func mcpCmd(l *agent.Loop, arg string) {
	fmt.Println("  configured MCP servers (from ~/.claude.json, .mcp.json):")
	// The tool registry already holds mcp__ entries; count them.
	n := 0
	for _, t := range tools.Defs() {
		if strings.HasPrefix(t.Name, "mcp__") {
			n++
		}
	}
	fmt.Printf("  %d mcp tools registered\n", n)
}

func permissionsCmd(l *agent.Loop, arg string) {
	fmt.Println("  mode:", l.PermMode)
	for k, rules := range l.PermRules {
		if len(rules) == 0 {
			continue
		}
		fmt.Printf("  %s:\n", k)
		for _, r := range rules {
			fmt.Println("   -", r)
		}
	}
	fmt.Println("  edit rules in ~/.claude/settings.json or .claude/settings.json")
}

func configCmd(l *agent.Loop, arg string) {
	p := filepath.Join(config.ClaudeDir(), "settings.json")
	if arg == "" {
		b, err := os.ReadFile(p)
		if err != nil {
			fmt.Println("  no settings.json")
			return
		}
		fmt.Println("  " + string(b))
		return
	}
	k, v, ok := strings.Cut(arg, " ")
	if !ok {
		fmt.Println("  usage: /config <key> <value>")
		return
	}
	var m map[string]any
	b, _ := os.ReadFile(p)
	json.Unmarshal(b, &m)
	if m == nil {
		m = map[string]any{}
	}
	var parsed any
	if json.Unmarshal([]byte(v), &parsed) == nil {
		m[k] = parsed
	} else {
		m[k] = v
	}
	out, _ := json.MarshalIndent(m, "", "  ")
	os.WriteFile(p, out, 0o644)
	fmt.Printf("  set %s = %s\n", k, v)
}

func exportCmd(l *agent.Loop, arg string) {
	if l.Transcript == "" {
		fmt.Println("  no transcript")
		return
	}
	dest := arg
	if dest == "" {
		dest = fmt.Sprintf("claude-session-%s.jsonl", l.Client.SessionID[:8])
	}
	b, err := os.ReadFile(l.Transcript)
	if err != nil {
		fmt.Println("  export failed:", err)
		return
	}
	if err := os.WriteFile(dest, b, 0o644); err != nil {
		fmt.Println("  export failed:", err)
		return
	}
	fmt.Println("  exported to", dest)
}

func diffCmd(l *agent.Loop) {
	out, err := exec.Command("git", "-C", l.ToolCtx.CWD, "diff", "--stat").CombinedOutput()
	if err != nil {
		fmt.Println("  not a git repo or no changes")
		return
	}
	if len(out) == 0 {
		fmt.Println("  clean working tree")
		return
	}
	fmt.Print(string(out))
}

// doctorCmd is the local diagnostics report (like `claude doctor` on the
// terminal — distinct from the /doctor slash command, which runs the
// bundledDoctor audit skill).
func doctorCmd(l *agent.Loop) {
	fmt.Println("  claude-go doctor")
	fmt.Println("  version:", api.Version)
	fmt.Println("  model:", l.Model)
	fmt.Println("  session:", l.Client.SessionID)
	fmt.Println("  cwd:", l.ToolCtx.CWD)
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
		fmt.Printf("  %-55s %s\n", p, st)
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

func outputStyleCmd(l *agent.Loop, arg string) {
	if arg == "" {
		fmt.Println("  usage: /output-style <name|default> — this changes how Claude communicates")
		fmt.Println("   - default")
		for n := range builtinStyles {
			fmt.Println("   -", n)
		}
		for _, base := range []string{
			filepath.Join(config.ClaudeDir(), "output-styles"),
			filepath.Join(l.CWD, ".claude", "output-styles"),
		} {
			ents, _ := os.ReadDir(base)
			for _, e := range ents {
				fmt.Println("   -", strings.TrimSuffix(e.Name(), ".md"))
			}
		}
		return
	}
	if arg == "default" {
		l.AppendSysP = ""
		persistOutputStyle(l.CWD, "")
		fmt.Println("  output style reset")
		return
	}
	if body, ok := builtinStyles[arg]; ok {
		l.AppendSysP = body
		persistOutputStyle(l.CWD, arg)
		fmt.Println("  output style:", arg)
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
		fmt.Println("  unknown output style:", arg)
		return
	}
	l.AppendSysP = string(b)
	persistOutputStyle(l.CWD, arg)
	fmt.Println("  output style:", arg)
}

func tasksCmd(l *agent.Loop) {
	if l.ToolCtx.BgTasks == nil {
		fmt.Println("  no background tasks")
		return
	}
	ts := l.ToolCtx.BgTasks.List()
	if len(ts) == 0 {
		fmt.Println("  no background tasks")
		return
	}
	for _, t := range ts {
		st := "running"
		select {
		case <-t.Done:
			st = "done"
		default:
		}
		fmt.Printf("  %s [%s] %s\n", t.ID, st, t.Command)
	}
}

func aliasCmd(l *agent.Loop, arg string) {
	p := filepath.Join(config.ClaudeDir(), "aliases.json")
	var m map[string]string
	b, _ := os.ReadFile(p)
	json.Unmarshal(b, &m)
	if m == nil {
		m = map[string]string{}
	}
	if arg == "" {
		for k, v := range m {
			fmt.Printf("  /%s -> %s\n", k, v)
		}
		return
	}
	name, target, ok := strings.Cut(arg, "=")
	if !ok {
		fmt.Println(`  usage: /alias name="/real command"`)
		return
	}
	m[strings.TrimSpace(name)] = strings.Trim(strings.TrimSpace(target), `"`)
	out, _ := json.MarshalIndent(m, "", "  ")
	os.WriteFile(p, out, 0o644)
	fmt.Printf("  alias /%s -> %s\n", strings.TrimSpace(name), m[strings.TrimSpace(name)])
}

func effortCmd(l *agent.Loop, arg string) {
	switch strings.ToLower(arg) {
	case "low", "medium", "high", "xhigh", "max", "auto":
		l.Effort = strings.ToLower(arg)
		fmt.Println("  effort set to", l.Effort)
	case "":
		fmt.Println("  current effort:", l.Effort)
		fmt.Println("  levels: low | medium | high | xhigh | max | auto")
	default:
		fmt.Println("  effort levels: low | medium | high | xhigh | max | auto")
	}
}

func loginCmd() {
	urlStr, verifier, state := auth.LoginURL()
	fmt.Println("  opening browser to authenticate…")
	fmt.Println("  if it doesn't open:", urlStr)
	auth.OpenBrowser(urlStr)
	fmt.Print("  paste the code (code#state): ")
	var code string
	fmt.Scanln(&code)
	if code == "" {
		fmt.Println("  cancelled")
		return
	}
	creds, err := auth.ExchangeCode(code, verifier, state)
	if err != nil {
		fmt.Println("  login failed:", err)
		return
	}
	if err := auth.WriteKeychain(creds); err != nil {
		fmt.Println("  token received but keychain write failed:", err)
		return
	}
	fmt.Println("  logged in — credentials written to keychain")
}

func logoutCmd() {
	if runtime.GOOS == "darwin" {
		exec.Command("security", "delete-generic-password",
			"-s", "Claude Code-credentials", "-a", os.Getenv("USER")).Run()
	}
	fmt.Println("  logged out (keychain entry removed)")
}

// persistOutputStyle writes outputStyle to .claude/settings.local.json (real
// persistence location).
func persistOutputStyle(cwd, style string) {
	dir := filepath.Join(cwd, ".claude")
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "settings.local.json")
	ensureGitignoreLocal() // real adds **/.claude/settings.local.json to excludes
	var st map[string]any
	if b, err := os.ReadFile(p); err == nil {
		json.Unmarshal(b, &st)
	}
	if st == nil {
		st = map[string]any{}
	}
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
	var st map[string]any
	if b, err := os.ReadFile(p); err == nil {
		json.Unmarshal(b, &st)
	}
	if st == nil {
		st = map[string]any{}
	}
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
