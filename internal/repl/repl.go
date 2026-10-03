package repl

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"claude-go/internal/agent"
	"claude-go/internal/api"
	"claude-go/internal/hooks"
	"claude-go/internal/perms"
	"claude-go/internal/tools"

	"golang.org/x/term"
)

const (
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
	Log(m api.Message) error
}

// Run drives the interactive session.
func Run(l *agent.Loop) {
	RunWithStore(l, nil)
}

// RunWithStore is Run with session transcript logging.
func RunWithStore(l *agent.Loop, store Store) {
	l.Entrypoint = "cli"
	ed := newEditor()
	ed.vim = vimEnabled()
	ed.completer = slashCompleter
	ed.cycleMode = func() {
		switch l.PermMode {
		case "auto":
			l.PermMode = "plan"
		case "plan":
			l.PermMode = "bypassPermissions"
		default:
			l.PermMode = "auto"
		}
		fmt.Printf("\n%s⏵⏵ %s mode%s\n", yellow, l.PermMode, reset)
	}
	pr := &promptUI{}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		runPipe(l)
		return
	}
	ks, err := startKeys()
	endSession := func() { fmt.Println(); os.Exit(0) }
	l.ToolCtx.SetPermMode = func(mode string) { l.PermMode = mode }
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
		switch perms.Check(perms.Mode(l.PermMode), &perms.Rules{
			Allow: l.PermRules["allow"], Deny: l.PermRules["deny"], Ask: l.PermRules["ask"],
		}, name, input, l.ToolCtx.CWD) {
		case perms.Allow:
			return true
		case perms.Deny:
			fmt.Printf("  %sdenied by permission rules%s\n", dim, reset)
			return false
		default:
			return pr.ask(l, ks, name, summary)
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
		printStatus(l)
		res := ed.readLine(ks)
		if res.quit {
			fmt.Println()
			fireSessionEndHook(l)
			return
		}
		if !res.ok {
			continue
		}
		line := strings.TrimSpace(res.line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "!") {
			runBang(l, line[1:])
			continue
		}
		if strings.HasPrefix(line, "/") {
			if handleSlash(l, line, ed, ks) {
				return
			}
			continue
		}

		printedDot = false
		spin := newSpinner()
		*spinRef = *spin
		ctx, cancel := context.WithCancel(context.Background())
		start := time.Now()
		respDone := make(chan struct{})
		go func() {
			// while the agent runs, drain keys watching for esc
			for {
				select {
				case <-respDone:
					return
				case k, ok := <-ks.ch:
					if !ok {
						return
					}
					if k.b == 0x1b && k.seq == "" {
						cancel()
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

func (p *promptUI) ask(l *agent.Loop, ks *keySource, name, summary string) bool {
	if p.always == nil {
		p.always = map[string]bool{}
	}
	if p.always[name] {
		return true
	}
	if l.Hooks != nil {
		hooks.Run(l.Hooks, hooks.Notification, map[string]any{
			"session_id": l.Client.SessionID, "cwd": l.ToolCtx.CWD,
			"message": "Claude needs permission to use " + name,
		}, l.ToolCtx.CWD)
	}
	fmt.Printf("\n%sDo you want to proceed?%s\n  %s%s\n  %s%s%s\n",
		bold, reset, dim, name, reset, trunc(summary, 200), reset)
	fmt.Printf("  %s1.%s Yes\n  %s2.%s Yes, and don't ask again for %s in %s\n  %s3.%s No\n  ",
		bold, reset, bold, reset, name, l.ToolCtx.CWD, bold, reset)
	for k := range ks.ch {
		switch k.b {
		case '1', 'y', 'Y', '\r':
			fmt.Println("1")
			return true
		case '2', 'a', 'A':
			fmt.Println("2")
			p.always[name] = true
			return true
		case '3', 'n', 'N', 0x1b, 3:
			fmt.Println("3")
			return false
		}
	}
	return false
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
		Text: "<bash-mode>\n" + cmd + "\n</bash-mode>\n\n<bash-output>\n" + res.Content + "\n</bash-output>"}})
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
	for _, c := range slashCommands {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func printStatus(l *agent.Loop) {
	if out := statusLineCmd(l); out != "" {
		fmt.Println(out)
		return
	}
	w, _ := termWidth()
	left := "  " + l.ToolCtx.CWD
	right := l.Model + " │ " + l.PermMode + " │ ? for shortcuts"
	pad := w - len(left) - len(right)
	if pad < 1 {
		pad = 1
	}
	fmt.Printf("%s%s%s%s%s\n", dim, left, strings.Repeat(" ", pad), right, reset)
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
	payload, _ := json.Marshal(map[string]any{
		"session_id":      l.Client.SessionID,
		"transcript_path": l.Transcript,
		"cwd":             l.ToolCtx.CWD,
		"model":           map[string]string{"id": l.Model, "display_name": l.Model},
		"workspace":       map[string]any{"current_dir": l.ToolCtx.CWD, "project_dir": l.ToolCtx.CWD},
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
