// claude-go: a Go reimplementation of the Claude Code CLI.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"claude-go/internal/agent"
	"claude-go/internal/api"
	"claude-go/internal/auth"
	"claude-go/internal/config"
	"claude-go/internal/hooks"
	"claude-go/internal/mcp"
	"claude-go/internal/mock"
	"claude-go/internal/perms"
	"claude-go/internal/repl"
	"claude-go/internal/tools"

	"claude-go/internal/session"
)

var version = api.Version

func usage() {
	fmt.Fprintf(os.Stderr, `claude-go %s — Claude Code replica in Go

Usage:
  claude-go [flags]                 interactive session
  claude-go -p "prompt" [flags]     print mode (non-interactive)
  claude-go -c                      continue most recent session
  claude-go -r <session-id>         resume a session

Flags:
  -p, --print                   print response and exit
  --model <model>               model (default claude-fable-5-1)
  --output-format text|json     print-mode output format
  --max-turns <n>               cap agent loop turns
  --permission-mode <mode>      default|acceptEdits|plan|bypassPermissions|auto
  --dangerously-skip-permissions  bypass all permission checks
  --append-system-prompt <s>    append to the system prompt
  --add-dir <dir>               grant access to an additional directory
  --mock                        run against the built-in mock API (no auth needed)
  --mock-port <port>            start a standalone mock server on a port and exit
  -v, --version                 print version
  -h, --help                    this help
`, version)
}

func main() {
	var (
		printMode bool
		mockMode  bool
		mockPort  int
		model     string
		outFormat = "text"
		maxTurns  int
		permMode  = "auto"
		appendSys string
		addDirs   []string
		cont      bool
		resume    string
		prompt    []string
	)
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			i++
			if i >= len(args) {
				fatal("flag %s requires a value", a)
			}
			return args[i]
		}
		switch {
		case a == "-p" || a == "--print":
			printMode = true
		case a == "--mock":
			mockMode = true
		case a == "--mock-port":
			fmt.Sscanf(next(), "%d", &mockPort)
		case a == "--model":
			model = next()
		case a == "--output-format":
			outFormat = next()
		case a == "--max-turns":
			fmt.Sscanf(next(), "%d", &maxTurns)
		case a == "--permission-mode":
			permMode = next()
		case a == "--dangerously-skip-permissions":
			permMode = "bypassPermissions"
		case a == "--append-system-prompt":
			appendSys = next()
		case a == "--add-dir":
			addDirs = append(addDirs, next())
		case a == "-c" || a == "--continue":
			cont = true
		case a == "-r" || a == "--resume":
			resume = next()
		case a == "-v" || a == "--version":
			fmt.Println(version + " (Claude Code)")
			return
		case a == "-h" || a == "--help":
			usage()
			return
		case strings.HasPrefix(a, "-"):
			fatal("unknown flag: %s", a)
		default:
			prompt = append(prompt, a)
		}
	}
	_ = addDirs

	if mockPort > 0 {
		runMockServer(mockPort)
		return
	}

	cwd, _ := os.Getwd()
	sessionID := newUUID()

	client, cleanup := buildClient(mockMode, sessionID)
	defer cleanup()

	// connect MCP servers in background (like CC's async init)
	mcpClients := startMCP(cwd)
	defer func() {
		for _, c := range mcpClients {
			c.Close()
		}
	}()

	loop := agent.NewLoop(client, cwd)
	loop.Entrypoint = "sdk-cli"
	if model != "" {
		loop.Model = model
	}
	loop.MaxTurns = maxTurns
	loop.PermMode = permMode
	loop.AppendSysP = appendSys
	email, acct, dev := config.Account()
	loop.User = agent.UserConfig{Email: email, AccountUUID: acct, DeviceID: dev}
	st := config.Load(cwd)
	rules := map[string][]string{}
	if st.Permissions != nil {
		rules["allow"] = st.Permissions.Allow
		rules["deny"] = st.Permissions.Deny
		rules["ask"] = st.Permissions.Ask
		if permMode == "auto" && st.Permissions.DefaultMode != "" {
			loop.PermMode = st.Permissions.DefaultMode
		}
	}
	loop.PermRules = rules
	loop.Hooks = hooks.Load(cwd)
	fireHook(loop, hooks.SessionStart)
	loop.ToolCtx.Agent = &agent.Runner{
		Client: client, PermMode: loop.PermMode, Model: loop.Model,
		User: loop.User, PermRules: rules, Entrypoint: "sdk-cli",
	}
	loop.ToolCtx.Answerer = func(ctx context.Context, prompt, pageText string) (string, error) {
		sub := agent.NewLoop(client, cwd)
		sub.Model = "claude-haiku-4-5-20251001"
		sub.ToolCtx.Permission = func(string, json.RawMessage, string) bool { return false }
		resp, err := sub.Ask(ctx, prompt+"\n\nPage content:\n"+pageText)
		if err != nil {
			return "", err
		}
		var out string
		for _, b := range resp.Content {
			if b.Type == "text" {
				out += b.Text
			}
		}
		return out, nil
	}
	loop.ToolCtx.Searcher = func(ctx context.Context, q string, allow, block []string) (string, error) {
		return "", fmt.Errorf("web search backend not configured")
	}
	if st.Model != "" && model == "" {
		loop.Model = st.Model
	}

	// session transcript
	st2, _ := session.Open(cwd, sessionID)
	if st2 != nil {
		defer st2.Close()
		loop.Transcript = st2.Path
	}

	if cont || resume != "" {
		id := resume
		if cont {
			if l2, err := session.Latest(cwd); err == nil {
				id = l2
			}
		}
		if id != "" {
			if msgs, err := session.Load(cwd, id); err == nil && len(msgs) > 0 {
				loop.Messages = msgs
				fmt.Fprintf(os.Stderr, "resumed session %s (%d messages)\n", id, len(msgs))
			}
		}
	}

	if printMode {
		// non-interactive: honor rules, auto-deny what would need asking
		loop.ToolCtx.Permission = func(name string, input json.RawMessage, _ string) bool {
			return perms.Check(perms.Mode(loop.PermMode), &perms.Rules{
				Allow: rules["allow"], Deny: rules["deny"], Ask: rules["ask"],
			}, name, input, loop.ToolCtx.CWD) == perms.Allow
		}
		code := runPrint(loop, strings.Join(prompt, " "), outFormat, st2)
		os.Exit(code)
	}
	fireHook(loop, hooks.SessionEnd)
	repl.RunWithStore(loop, st2)
}

func fireHook(l *agent.Loop, ev hooks.Event) {
	hooks.Run(l.Hooks, ev, map[string]any{
		"session_id":      l.Client.SessionID,
		"transcript_path": l.Transcript,
		"cwd":             l.ToolCtx.CWD,
		"hook_event_name": string(ev),
		"permission_mode": l.PermMode,
	}, l.ToolCtx.CWD)
}

// startMCP connects configured MCP servers and registers their tools.
func startMCP(cwd string) (clients []*mcp.Client) {
	cfgs := mcp.LoadConfigs(cwd)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for name, cfg := range cfgs {
		if cfg.Command == "" {
			continue
		}
		wg.Add(1)
		go func(name string, cfg mcp.ServerConfig) {
			defer wg.Done()
			c, err := mcp.Connect(name, cfg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  [mcp] %s: %v\n", name, err)
				return
			}
			for _, t := range c.Tools {
				tools.RegisterMCP(name, c, t.Name, t.Description, t.InputSchema)
			}
			mu.Lock()
			clients = append(clients, c)
			mu.Unlock()
		}(name, cfg)
	}
	wg.Wait()
	return clients
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "claude-go: "+f+"\n", a...)
	os.Exit(2)
}

func buildClient(mockMode bool, sessionID string) (*api.Client, func()) {
	var creds *auth.Credentials
	if mockMode {
		srv, err := mock.Start()
		if err != nil {
			fatal("mock server: %v", err)
		}
		os.Setenv("ANTHROPIC_BASE_URL", srv.Addr)
		creds = &auth.Credentials{AccessToken: "mock-token", IsAPIKey: false}
		return api.NewClient(creds, sessionID, api.EntrySDKCLI), func() { srv.Close() }
	}
	var err error
	creds, err = auth.Load()
	if err != nil {
		fatal("%v", err)
	}
	return api.NewClient(creds, sessionID, api.EntrySDKCLI), func() {}
}

func runMockServer(port int) {
	srv, err := mock.StartOn(fmt.Sprintf(":%d", port))
	if err != nil {
		fatal("mock: %v", err)
	}
	fmt.Println("mock listening at", srv.Addr)
	select {}
}

func runPrint(l *agent.Loop, prompt, format string, st *session.Store) int {
	var out strings.Builder
	if format == "stream-json" {
		fmt.Printf(`{"type":"system","subtype":"init","session_id":%q,"model":%q,"cwd":%q}`+"\n",
			l.Client.SessionID, l.Model, l.CWD)
	}
	l.Events.OnTextDelta = func(t string) {
		switch format {
		case "text":
			fmt.Print(t)
		default:
			out.WriteString(t)
		}
	}
	l.Events.OnToolUse = func(name string, input json.RawMessage) {
		if format == "stream-json" {
			fmt.Printf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":%q,"input":%s}]}}`+"\n",
				name, string(input))
		}
	}
	ctx := context.Background()
	start := time.Now()
	resp, err := l.Ask(ctx, prompt)
	if st != nil {
		for _, mm := range l.Messages {
			st.LogMessage(mm)
		}
	}
	if format == "text" {
		fmt.Println()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return 1
	}
	switch format {
	case "json", "stream-json":
		b, _ := json.MarshalIndent(map[string]any{
			"type":        "result",
			"subtype":     "success",
			"result":      out.String(),
			"session_id":  l.Client.SessionID,
			"duration_ms": time.Since(start).Milliseconds(),
			"usage":       resp.Usage,
		}, "", "  ")
		fmt.Println(string(b))
	}
	return 0
}

func newUUID() string {
	f, _ := os.Open("/dev/urandom")
	var b [16]byte
	io.ReadFull(f, b[:])
	f.Close()
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
