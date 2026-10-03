package repl

import (
	"claude-go/internal/config"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"claude-go/internal/agent"
	"claude-go/internal/api"
	"claude-go/internal/auth"
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
	case "fork":
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
	case "loop", "proactive":
		if arg == "" {
			fmt.Println("  usage: /loop <interval-or-dynamic> <prompt> — ScheduleWakeup drives self-paced loops")
			fmt.Println("  e.g. /loop 300 check the build")
			return true
		}
		fields := strings.Fields(arg)
		if len(fields) < 2 {
			fmt.Println("  usage: /loop <interval-seconds> <prompt>")
			return true
		}
		sec, _ := strconv.Atoi(fields[0])
		if sec < 60 {
			sec = 60
		}
		prompt := strings.TrimSpace(strings.TrimPrefix(arg, fields[0]))
		fmt.Printf("  loop armed: every %ds fires %q\n", sec, prompt)
		go func() {
			for range time.Tick(time.Duration(sec) * time.Second) {
				select {
				case tools.SessionQueue <- prompt:
				default:
				}
			}
		}()
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
	"add-dir": {"Session", "<dir> grant access to another directory", false},
	"branch":  {"Session", "fork the session onto a new branch", false},
	"btw":     {"Session", "<question> ask a side question outside the main context", false},
	"clear":   {"Session", "clear conversation (aliases: /reset, /new)", false},
	"compact": {"Session", "[focus] compact context (optional focus instructions)", false},
	"context": {"Session", "context usage", false},
	"cost":    {"Session", "token usage (aliases: /usage, /stats)", false},
	"diff":    {"Session", "show git diff summary", false},
	"exit":    {"Session", "quit", false},
	"export":  {"Session", "[path] write transcript to file", false},
	"fork":    {"Session", "fork the session", false},
	"goal":    {"Session", "<goal> work until the goal holds", false},
	"import":  {"Session", "<path> read a file into context", false},
	"loop":    {"Session", "<interval> <prompt> schedule a recurring prompt", false},
	"memory":  {"Session", "memory files", false},
	"resume":  {"Session", "resume a session", false},
	"rewind":  {"Session", "rewind to a checkpoint (aliases: /checkpoint, /undo)", false},
	"status":  {"Session", "session status", false},
	"subtask": {"Session", "<prompt> run a subagent", false},
	"tasks":   {"Session", "background tasks", false},
	"todos":   {"Session", "todo list", false},
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
