package repl

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
	ed.vim = vimEnabled()
	if ed.vim {
		ed.insertRemaps = vimInsertRemaps()
	}
	ed.completer = slashCompleter
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
		fmt.Printf("\n%s⏵⏵ %s mode%s\n", yellow, l.PermMode, reset)
	}
	pr := &promptUI{}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		runPipe(l)
		return
	}
	ks, err := startKeys()
	if err == nil {
		ed.keys = ks
		ed.registerDefaultActions()
		ed.actions[2] = func(e *editor) *readResult { // ctrl+shift+b (app:toggleBrief)
			toggleBrief()
			return nil
		}
		ed.onKillAgents = func() {
			fmt.Println("\r\x1b[2K  killing background tasks…")
			n := l.ToolCtx.BgTasks.KillAll()
			fmt.Printf("  %d background tasks stopped\n", n)
		}
		ed.onToggleExpand = func() {
			// ctrl+o: full transcript view; second ctrl+o (or esc/q) returns
			showTranscript(l, ks)
			os.Stdout.WriteString("╭─╮\n")
			ed.redraw()
		}
		ed.onToggleTodos = func() {
			if s := tools.TodosText(); s != "" {
				fmt.Println("\r\x1b[2K" + dim + s + reset)
			} else {
				fmt.Println("\r\x1b[2K  (no todos)")
			}
		}
	}
	endSession := func() { fmt.Println(); os.Exit(0) }
	l.ToolCtx.SetPermMode = func(mode string) { l.PermMode = mode }
	l.ToolCtx.OnCwdChanged = func(nc string) {
		hooks.Run(l.Hooks, hooks.CwdChanged, map[string]any{
			"session_id": l.Client.SessionID, "cwd": nc, "transcript_path": l.Transcript,
		}, nc)
	}
	l.ToolCtx.OnFileChanged = func(p, kind string) {
		hooks.Run(l.Hooks, hooks.FileChanged, map[string]any{
			"session_id": l.Client.SessionID, "file_path": p, "kind": kind,
			"transcript_path": l.Transcript,
		}, l.ToolCtx.CWD)
	}
	l.ToolCtx.EndSession = endSession
	l.ToolCtx.PlanApprove = func() bool {
		return pr.ask(l, ks, "ExitPlanMode", "Approve the plan above?")
	}
	l.ToolCtx.AskStructured = func(header, question string, options []string, multi bool) ([]string, error) {
		return pr.askStructured(ks, header, question, options, multi)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "repl: raw mode:", err)
		return
	}
	defer ks.stop()
	var printedDot bool
	md := newMDRenderer()

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
			fmt.Printf("  %sdenied by permission rules%s\n", dim, reset)
			return false
		default:
			reason, cfg := askReason(l, name, input)
			ok, feedback := pr.askEx(l, ks, name, summary, reason, cfg)
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
			fmt.Print("\r\x1b[K⏺ ")
			printedDot = true
		}
		md.Write(t)
	}
	l.Events.OnThinking = func(t string) {
		// thinking display: dim italic
		fmt.Print(dim + t + reset)
	}
	l.Events.OnToolUse = func(name string, input json.RawMessage) {
		spinRef.stop()
		fmt.Printf("\r\x1b[K%s⏺%s %s%s%s\n", green, reset, bold+name+reset, summarize(name, input), "")
	}
	l.Events.OnToolResult = func(name string, r tools.Result) {
		lines := strings.Split(strings.TrimRight(r.Content, "\n"), "\n")
		max := 5
		if briefMode.Load() {
			max = 1 // brief mode: tool previews collapse to one line
		}
		for i, ln := range lines {
			if i >= max {
				fmt.Printf("  %s⎿%s  … +%d lines\n", dim, reset, len(lines)-max)
				break
			}
			fmt.Printf("  %s⎿%s  %s\n", dim, reset, trunc(ln, 120))
		}
	}
	l.Events.OnUsage = func(u api.Usage) {}

	fmt.Printf("%sclaude-go%s %s — %s\n", bold, reset, api.Version, l.CWD)
	fmt.Printf("%smodel: %s · /help for commands · esc to interrupt · ctrl-D to exit%s\n\n", dim, l.Model, reset)

	for {
		printStatus(l, ed)
		tools.UserActive.Store(true)
		res := ed.readLine(ks)
		if res.quit {
			fmt.Println()
			fireSessionEndHook(l)
			return
		}
		if res.rewind {
			runRewindSelector(l, ks, ed)
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
			if expanded := expandCustomSlash(line); expanded != "" {
				line = expanded
			} else {
				if handleSlashLocal(l, line, ed, ks) {
					continue
				}
				if handleSlash(l, line, ed, ks) {
					return
				}
				continue
			}
		}

		var queued []string
		var draftBack []rune
		runTurn := func(line string) {
			tools.UserActive.Store(false)
			defer tools.UserActive.Store(true)
			printedDot = false
			spin := newSpinner()
			*spinRef = *spin
			ctx, cancel := context.WithCancel(context.Background())
			start := time.Now()
			respDone := make(chan struct{})
			var queueBuf []rune
			var sawInput atomic.Bool // any keystroke during the turn = user present
			go func() {
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
						if k.b == 0x1b && k.seq == "" {
							cancel()
							// popQueuedIntoDraft: unfinished queued text returns to the editor
							if len(queueBuf) > 0 {
								draftBack = append(draftBack, queueBuf...)
							}
						} else if k.b == '\r' || k.b == '\n' {
							if len(queueBuf) > 0 {
								queued = append(queued, string(queueBuf))
								queuedPending.Add(1)
								queueBuf = nil
								fmt.Fprintf(os.Stderr, "\n  %s➤ queued%s\n", dim, reset)
							}
						} else if k.r != 0 {
							queueBuf = append(queueBuf, k.r)
						} else if k.b == 0x7f && len(queueBuf) > 0 {
							queueBuf = queueBuf[:len(queueBuf)-1]
						}
					}
				}
			}()
			_, err := l.Ask(ctx, expandMentions(l, line))
			spin.stop()
			fmt.Fprint(os.Stderr, "\r\x1b[K")
			fireStopHook(l)
			if store != nil {
				for _, m := range l.Messages {
					store.Log(m)
				}
			}
			md.Close()
			close(respDone)
			cancel()
			fmt.Println()
			// ping the notification channel when the turn ended with no
			// user keystrokes — the session was unattended (UserActive is
			// false for the whole turn, so key activity is the real signal)
			if !sawInput.Load() {
				notifyAttention(l.ToolCtx.CWD)
			}
			// context-low warning like real CC
			tok := l.UsageTotal.InputTokens + l.UsageTotal.CacheReadInputTokens + l.UsageTotal.CacheCreationInputTokens
			if tok > 150000 {
				fmt.Printf("%s  Context limit nearing · /compact or /clear to continue%s\n", yellow, reset)
			}
			if err != nil {
				if ctx.Err() == context.Canceled {
					fmt.Printf("%s⏺ Interrupted%s\n", yellow, reset)
				} else {
					fmt.Printf("%sError: %v%s\n", red, err, reset)
				}
			} else {
				fmt.Printf("%s· %s%s\n\n", dim, time.Since(start).Round(time.Millisecond), reset)
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
				default:
					goto drained
				}
			}
		drained:
			for _, nm := range tools.DrainMailbox(l.Client.SessionID) {
				fmt.Printf("%s  [message]%s %s\n", cyan, reset, nm)
				l.Messages = append(l.Messages, api.TextMessage("user",
					"<system-reminder>Message received while you were working:</system-reminder>\n"+nm))
			}
			tools.FireIdleNotifications(l.Client.SessionID)
			if len(draftBack) > 0 {
				ed.buf = draftBack
				ed.pos = len(ed.buf)
				draftBack = nil
			}
			if len(queued) == 0 {
				break
			}
			line = queued[0]
			queued = queued[1:]
			queuedPending.Add(-1)
		}
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
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
	ok, _ := p.askEx(l, ks, name, summary, "", "")
	return ok
}

// spinner shows "⠋ working… (esc to interrupt · Ns)" on stderr.
type spinner struct {
	done chan struct{}
}

func newSpinner() *spinner {
	s := &spinner{done: make(chan struct{})}
	go s.run()
	return s
}

func (s *spinner) run() {
	frames := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
	i := 0
	start := time.Now()
	t := time.NewTicker(80 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			el := time.Since(start).Round(time.Second)
			fmt.Fprintf(os.Stderr, "\r\x1b[K%s%c%s working… (esc to interrupt · %s)", dim, frames[i%len(frames)], reset, el)
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
			b = b[:100000]
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
	fmt.Println("  ⎿ ", strings.TrimSpace(res.Content))
	l.PushUser([]api.ContentBlock{{Type: "text",
		Text: "<bash-input>\n" + cmd + "\n</bash-input>\n\n<local-command-stdout>\n" + res.Content + "\n</local-command-stdout>"}})
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
		if _, err := l.Ask(context.Background(), expandMentions(l, line)); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
		}
		fmt.Println()
	}
}

func fireStopHook(l *agent.Loop) {
	if l.Hooks == nil {
		return
	}
	res := hooks.Run(l.Hooks, hooks.Stop, map[string]any{
		"session_id":      l.Client.SessionID,
		"transcript_path": l.Transcript,
		"cwd":             l.ToolCtx.CWD,
	}, l.ToolCtx.CWD)
	if res.SystemMessage != "" {
		fmt.Printf("  %s%s%s\n", dim, res.SystemMessage, reset)
	}
}

func vimEnabled() bool {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return false
	}
	return strings.Contains(string(b), `"keybindingMode":"vim"`) ||
		strings.Contains(string(b), `"editorMode":"vim"`)
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

func printStatus(l *agent.Loop, ed *editor) {
	if out := statusLineCmd(l); out != "" {
		fmt.Println(out)
	} else {
		w, _ := termWidth()
		left := "  " + l.ToolCtx.CWD
		right := l.Model + " │ " + l.PermMode + " │ ? for shortcuts"
		pad := w - len(left) - len(right)
		if pad < 1 {
			pad = 1
		}
		fmt.Printf("%s%s%s%s%s\n", dim, left, strings.Repeat(" ", pad), right, reset)
	}
	if b := permModeBanner(l.PermMode); b != "" {
		fmt.Println("  " + b)
	}
	if hints := footerHints(l, ed); len(hints) > 0 {
		fmt.Printf("%s  %s%s\n", dim, strings.Join(hints, " · "), reset)
	}
}

var slashCommands = []string{
	"add-dir", "agents", "alias", "bug", "clear", "color", "compact",
	"config", "context", "cost", "diff", "doctor", "export", "exit",
	"feedback", "files", "help", "hooks", "ide", "import", "init",
	"keybindings", "login", "logout", "mcp", "memory", "model", "output-style",
	"permissions", "plugin", "privacy-settings", "quit", "release-notes",
	"resume", "review", "status", "statusline", "tasks", "terminal-setup",
	"theme", "todos", "usage", "vim",
}

func termWidth() (int, error) {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	return w, err
}

// askStructured renders a numbered options menu (AskUserQuestion tool).
func (p *promptUI) askStructured(ks *keySource, header, question string, options []string, multi bool) ([]string, error) {
	fmt.Printf("\n%s%s%s\n", bold, question, reset)
	for i, o := range options {
		fmt.Printf("  %s%d.%s %s\n", bold, i+1, reset, o)
	}
	other := fmt.Sprintf("  %s%d.%s Other\n  ", bold, len(options)+1, reset)
	fmt.Print(other)
	var picked []string
	var custom strings.Builder
	for k := range ks.ch {
		c := k.b
		if c >= '1' && c <= '9' {
			idx := int(c - '1')
			if idx == len(options) {
				// Other: collect a line
				p.customLine = &custom
				continue
			}
			if idx >= 0 && idx < len(options) {
				fmt.Println(string(c), "·", options[idx])
				picked = append(picked, options[idx])
				if !multi {
					return picked, nil
				}
				continue
			}
		}
		if c == '\r' {
			if p.customLine != nil {
				picked = append(picked, custom.String())
				p.customLine = nil
				if !multi {
					return picked, nil
				}
				continue
			}
			if len(picked) > 0 {
				return picked, nil
			}
		}
		if c == 0x1b || c == 3 {
			return nil, fmt.Errorf("cancelled")
		}
		if p.customLine != nil && c >= 32 {
			custom.WriteByte(c)
			os.Stdout.WriteString(string(c))
		}
	}
	return nil, fmt.Errorf("cancelled")
}

func fireSessionEndHook(l *agent.Loop) {
	if l.Hooks == nil {
		return
	}
	hooks.Run(l.Hooks, hooks.SessionEnd, map[string]any{
		"session_id": l.Client.SessionID, "cwd": l.ToolCtx.CWD,
	}, l.ToolCtx.CWD)
}

// statusLineCmd runs the configured statusLine.command (like real CC) —
// the JSON payload goes on stdin, first stdout line becomes the status.
func statusLineCmd(l *agent.Loop) string {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		return ""
	}
	var st struct {
		StatusLine struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"statusLine"`
	}
	if json.Unmarshal(b, &st) != nil || st.StatusLine.Command == "" {
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
		"version":         "2.1.288",
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
	cmd := exec.Command(sh, "-c", st.StatusLine.Command)
	cmd.Dir = l.ToolCtx.CWD
	cmd.Stdin = strings.NewReader(string(payload))
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.SplitN(string(out), "\n", 2)[0]
}

// expandCustomSlash resolves /name args to a prompt if it's a custom command.
func expandCustomSlash(line string) string {
	fields := strings.Fields(line)
	name := strings.TrimPrefix(fields[0], "/")
	c, ok := customCommands[name]
	if !ok {
		return ""
	}
	arg := strings.TrimSpace(strings.TrimPrefix(line, "/"+name))
	fmt.Printf("  %s/%s%s\n", dim, name, reset)
	return expandCustomCommand(c, arg)
}

// quickMemory: "# text" appends to the project memory index (CC's # prefix).
func quickMemory(l *agent.Loop, text string) {
	home, _ := os.UserHomeDir()
	slug := strings.ReplaceAll(filepath.Clean(l.ToolCtx.CWD), "/", "-")
	dir := filepath.Join(home, ".claude", "projects", slug, "memory")
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "MEMORY.md")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Println("  memory write failed:", err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "- %s\n", text)
	fmt.Println("  saved to memory:", p)
}

func vimMode(l *agent.Loop) string {
	if vimEnabled() {
		return "normal"
	}
	return ""
}

func outputStyle(l *agent.Loop) string {
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(l.ToolCtx.CWD, ".claude", "settings.local.json"),
		filepath.Join(home, ".claude", "settings.json"),
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
		fmt.Println("  (nothing to rewind to)")
		return
	}
	fmt.Println("\n  Rewind to:")
	for i, m := range list {
		if i >= 12 {
			fmt.Printf("    … %d more\n", len(list)-12)
			break
		}
		fmt.Printf("    %d. %s\n", i+1, trunc(strings.ReplaceAll(m.text, "\n", " "), 80))
	}
	fmt.Print("  number to restore (esc cancels): ")
	sel := 0
	for k := range ks.ch {
		if k.b == 0x1b {
			fmt.Println()
			return
		}
		if k.b >= '1' && k.b <= '9' {
			sel = sel*10 + int(k.b-'0')
			fmt.Print(string(k.b))
			continue
		}
		if k.b == '\r' {
			fmt.Println()
			if sel < 1 || sel > len(list) {
				fmt.Println("  (invalid)")
				return
			}
			target := list[sel-1]
			l.Messages = l.Messages[:target.i]
			p, err := tools.Checkpoints.Rewind()
			restored := 0
			if err == nil && p != "" {
				restored = 1
			}
			fmt.Printf("  rewound to before %q (%d files restored)\n", trunc(target.text, 50), restored)
			return
		}
		if k.b == 3 {
			fmt.Println()
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
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(cwd, ".claude", "settings.json"),
		filepath.Join(home, ".claude", "settings.json"),
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
	return []string{"add-dir", "agents", "alias", "allowed-tools", "auto", "batch", "branch", "btw", "bug", "checkpoint", "checkup", "clear", "code-review", "color", "compact", "config", "context", "cost", "debug", "diff", "doctor", "effort", "exit", "export", "fast", "feedback", "fewer-permission-prompts", "files", "fork", "goal", "help", "high", "hooks", "ide", "import", "init", "insights", "keybindings", "keybindings-help", "login", "logout", "loop", "low", "max", "mcp", "medium", "memory", "memory-types", "model", "new", "output-style", "permissions", "plan", "plugin", "pr", "privacy-settings", "proactive", "quit", "release-notes", "rename", "reset", "resume", "review", "rewind", "run", "sandbox", "security-review", "settings", "skills", "stats", "status", "statusline", "subtask", "tasks", "terminal-setup", "theme", "todos", "undo", "update-config", "usage", "vim", "xhigh"}
}

// currentStore is set by RunWithStore so slash commands can write records.
var currentStore Store
