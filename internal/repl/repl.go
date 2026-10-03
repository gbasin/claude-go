package repl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"claude-go/internal/agent"
	"claude-go/internal/api"
	"claude-go/internal/perms"
	"claude-go/internal/tools"
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
	ed := newEditor()
	pr := &promptUI{}
	ks, err := startKeys()
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
			fmt.Print("⏺ ")
			printedDot = true
		}
		md.Write(t)
	}
	l.Events.OnThinking = func(t string) {
		// thinking display: dim italic
		fmt.Print(dim + t + reset)
	}
	l.Events.OnToolUse = func(name string, input json.RawMessage) {
		fmt.Printf("\n%s⏺%s %s%s%s\n", green, reset, bold+name+reset, summarize(name, input), "")
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
		line, ok, intr := ed.readLine(ks)
		if intr {
			fmt.Println("^C (press again to exit)")
			continue
		}
		if !ok {
			fmt.Println()
			return
		}
		line = strings.TrimSpace(line)
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

func handleSlash(l *agent.Loop, line string, ed *editor, ks *keySource) (exit bool) {
	fields := strings.Fields(line)
	cmd := strings.TrimPrefix(fields[0], "/")
	arg := ""
	if len(fields) > 1 {
		arg = strings.Join(fields[1:], " ")
	}
	switch cmd {
	case "exit", "quit":
		return true
	case "help":
		fmt.Println("  /model [name]  show or set model")
		fmt.Println("  /clear         clear conversation context")
		fmt.Println("  /compact       compact the conversation (not yet implemented)")
		fmt.Println("  /cost          session usage (not yet implemented)")
		fmt.Println("  /exit          quit")
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
		ctx := context.Background()
		if err := l.Compact(ctx, l.Transcript); err != nil {
			fmt.Println("  compact failed:", err)
		} else {
			fmt.Println("  context compacted")
		}
	case "cost", "usage":
		u := l.UsageTotal
		fmt.Printf("  input: %d  output: %d  cache-read: %d  cache-write: %d\n",
			u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens)
	case "status":
		fmt.Printf("  version: %s\n  model: %s\n  session: %s\n  cwd: %s\n  permission mode: %s\n",
			api.Version, l.Model, l.Client.SessionID, l.ToolCtx.CWD, l.PermMode)
	case "add-dir":
		fmt.Println("  additional dirs are accepted via --add-dir (grant tracked for this session)")
	case "memory":
		fmt.Println("  memory dir:", l.CWD)
	case "mcp":
		fmt.Println("  MCP servers are loaded from ~/.claude.json and .mcp.json at startup")
	case "permissions":
		fmt.Println("  permission mode:", l.PermMode)
	case "config", "agents", "review", "diff", "resume", "login", "logout",
		"init", "doctor", "bug", "vim", "hooks", "output-style", "export":
		fmt.Printf("  /%s is not yet implemented in claude-go\n", cmd)
	default:
		fmt.Printf("  unknown command: /%s (try /help)\n", cmd)
	}
	return false
}

// promptUI renders the y/n/always permission prompt.
type promptUI struct {
	always map[string]bool
}

func (p *promptUI) ask(l *agent.Loop, ks *keySource, name, summary string) bool {
	if p.always == nil {
		p.always = map[string]bool{}
	}
	if p.always[name] {
		return true
	}
	fmt.Printf("\n%s!%s %s wants to run:\n  %s%s%s\n", yellow, reset, name, bold, trunc(summary, 200), reset)
	fmt.Printf("  %s[y]%s yes  %s[n]%s no  %s[a]%s always allow %s  ", bold, reset, bold, reset, bold, reset, name)
	for k := range ks.ch {
		switch k.b {
		case 'y', 'Y', '\r', '\n':
			fmt.Println("yes")
			return true
		case 'a', 'A':
			fmt.Println("always")
			p.always[name] = true
			return true
		case 'n', 'N', 0x1b, 3:
			fmt.Println("no")
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
			fmt.Fprintf(os.Stderr, "\r\x1b[K")
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
