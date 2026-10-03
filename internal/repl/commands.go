package repl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"claude-go/internal/agent"
	"claude-go/internal/api"
	"claude-go/internal/auth"
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
	case "model":
		if arg == "" {
			fmt.Println("  model:", l.Model)
		} else {
			l.Model = arg
			fmt.Println("  model set to", arg)
		}
	case "clear":
		l.Messages = nil
		fmt.Println("  context cleared")
	case "compact":
		fmt.Println("  compacting…")
		if arg != "" {
			l.AppendSysP += "\n\n## Compact Instructions\n" + arg
		}
		if err := l.Compact(context.Background(), l.Transcript); err != nil {
			fmt.Println("  compact failed:", err)
		} else {
			fmt.Println("  context compacted")
		}
	case "cost", "usage":
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
	case "permissions":
		permissionsCmd(l, arg)
	case "config":
		configCmd(l, arg)
	case "agents":
		fmt.Println("  builtin: general-purpose, Explore, Plan, claude, code-reviewer, code-simplifier, debugger, statusline-setup")
		fmt.Println("  custom agents load from ~/.claude/agents/ and .claude/agents/")
	case "hooks":
		fmt.Println("  hooks configured in settings.json fire on PreToolUse/PostToolUse/UserPromptSubmit/SessionStart/SessionEnd/etc.")
	case "resume":
		if id := PickSession(l.ToolCtx.CWD); id != "" {
			if msgs, err := session.Load(l.ToolCtx.CWD, id); err == nil {
				l.Messages = msgs
				fmt.Printf("  resumed session %s (%d messages)\n", id, len(msgs))
			}
		}
	case "export":
		exportCmd(l, arg)
	case "init":
		fmt.Println("  analyzing codebase → CLAUDE.md …")
		go l.Ask(context.Background(),
			"Analyze this codebase and create a CLAUDE.md file with: build/test/lint commands, "+
				"architecture overview, key directories, and conventions to follow. "+
				"Write it to CLAUDE.md in the project root.")
	case "diff":
		diffCmd(l)
	case "review":
		go l.Ask(context.Background(),
			"Review the current uncommitted changes for bugs, style issues, and missed edge cases. Report findings.")
	case "doctor":
		doctorCmd(l)
	case "bug":
		url := "https://github.com/anthropics/claude-code/issues"
		fmt.Println("  report issues at", url)
		if runtime.GOOS == "darwin" {
			exec.Command("open", url).Start()
		}
	case "vim":
		ed.vim = !ed.vim
		ed.mode = "insert"
		fmt.Printf("  vim mode %s\n", map[bool]string{true: "on", false: "off"}[ed.vim])
	case "output-style":
		outputStyleCmd(l, arg)
	case "statusline":
		fmt.Println("  set statusLine.command in ~/.claude/settings.json")
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
	case "fork":
		fmt.Println("  (session forking not implemented)")
	case "rewind":
		if p, err := tools.Checkpoints.Rewind(); err != nil {
			fmt.Println(" ", err)
		} else {
			fmt.Println("  rewound:", p)
		}
	case "effort":
		effortCmd(l, arg)
	case "fast":
		if arg == "off" || arg == "" {
			fmt.Println("  fast mode off")
		} else {
			fmt.Println("  fast mode toggles speed/quality tradeoff")
		}
	default:
		fmt.Printf("  unknown command: /%s (try /help)\n", cmd)
	}
	return false
}

func printHelp() {
	fmt.Println(`  /add-dir <dir>      grant access to another directory
  /agents            list agent types
  /bug               report an issue
  /clear             clear conversation
  /compact [focus]   compact context (optional focus instructions)
  /config            get/set settings
  /context           context usage
  /cost, /usage      token usage
  /diff              show git diff summary
  /doctor            diagnostics
  /export [path]     write transcript to file
  /init              generate CLAUDE.md
  /memory            memory files
  /mcp               MCP servers
  /model [name]      show/set model
  /permissions       permission rules
  /resume            resume a session
  /review            review current changes
  /status            session status
  /tasks             background tasks
  /vim               toggle vim mode
  /exit              quit`)
}

func memoryCmd(l *agent.Loop, arg string) {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".claude", "projects",
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
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".claude", "settings.json")
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

func doctorCmd(l *agent.Loop) {
	fmt.Println("  claude-go doctor")
	fmt.Println("  version:", api.Version)
	fmt.Println("  model:", l.Model)
	fmt.Println("  session:", l.Client.SessionID)
	fmt.Println("  cwd:", l.ToolCtx.CWD)
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".claude", "settings.json"),
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

func outputStyleCmd(l *agent.Loop, arg string) {
	home, _ := os.UserHomeDir()
	if arg == "" {
		fmt.Println("  usage: /output-style <name|default>")
		ents, _ := os.ReadDir(filepath.Join(home, ".claude", "output-styles"))
		for _, e := range ents {
			fmt.Println("   -", strings.TrimSuffix(e.Name(), ".md"))
		}
		return
	}
	if arg == "default" {
		l.AppendSysP = ""
		fmt.Println("  output style reset")
		return
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude", "output-styles", arg+".md"))
	if err != nil {
		fmt.Println("  unknown output style:", arg)
		return
	}
	l.AppendSysP = string(b)
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
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".claude", "aliases.json")
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
	case "low", "medium", "high":
		fmt.Println("  effort:", arg, "(applies to output_config on next request)")
	default:
		fmt.Println("  effort levels: low | medium | high — set in settings or per-request")
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
