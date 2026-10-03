// claude-go: a Go reimplementation of the Claude Code CLI.
package main

import (
	"bufio"
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
  --allowedTools "Bash,Read"    add permission allow rules
  --disallowedTools "Bash(rm*)" add permission deny rules
  --system-prompt <s>           extra system prompt text
  --system-prompt-file <f>      extra system prompt from file
  --session-id <id>             use a specific session id
  --fork-session                fork when resuming
  --mcp-config <file>           MCP servers config file
  --settings <file>             additional settings file
  --input-format stream-json    read JSONL messages from stdin
  --output-format text|json|stream-json
  --fallback-model <m>          model fallback (accepted, not used)
  --debug, --verbose            debug logging
  --mock                        run against the built-in mock API (no auth needed)
  --mock-port <port>            start a standalone mock server on a port and exit
  -v, --version                 print version
  -h, --help                    this help

Subcommands:
  mcp list|add|remove           manage MCP servers
  doctor                        diagnostics
  config get|set|list           settings.json
  update                        version info
`, version)
}

func main() {
	var (
		printMode   bool
		mockMode    bool
		mockPort    int
		model       string
		outFormat   = "text"
		maxTurns    int
		permMode    = "default"
		appendSys   string
		addDirs     []string
		cont        bool
		resume      string
		prompt      []string
		allowTools  string
		denyTools   string
		sysPrompt   string
		sysPromptF  string
		sessID      string
		forkSess    bool
		bareMode    bool
		agentsJSON  string
		toolsList   string
		workload    string
		fileArgs    []string
		_           = agentsJSON
		_           = workload
		debug       bool
		verbose     bool
		mcpConfig   string
		strictMCP   bool
		settingsF   string
		fallback    string
		partialMsgs bool
		inFormat    string
		jsonSchema  string
		agentName   string
	)
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "mcp":
			mcpSub(args[1:])
			return
		case "doctor":
			doctorSub()
			return
		case "config":
			configSub(args[1:])
			return
		case "update":
			fmt.Println("claude-go is built from source; update via git pull + go build")
			return
		case "login":
			authLoginSub()
			return
		case "auth":
			authSub(args[1:])
			return
		case "purge":
			purgeSub(args[1:])
			return
		case "setup-token":
			fmt.Println("token: uses ANTHROPIC_API_KEY or Claude Code keychain credentials")
			return
		}
	}
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
		case a == "--allowedTools" || a == "--allowed-tools":
			allowTools = next()
		case a == "--disallowedTools" || a == "--disallowed-tools":
			denyTools = next()
		case a == "--system-prompt":
			sysPrompt = next()
		case a == "--system-prompt-file":
			sysPromptF = next()
		case a == "--session-id":
			sessID = next()
		case a == "--fork-session":
			forkSess = true
		case a == "--debug" || a == "-d":
			debug = true
		case a == "--verbose":
			verbose = true
		case a == "--mcp-config":
			mcpConfig = next()
		case a == "--strict-mcp-config":
			strictMCP = true
		case a == "--settings":
			settingsF = next()
		case a == "--fallback-model":
			fallback = next()
		case a == "--include-partial-messages":
			partialMsgs = true
		case a == "--input-format":
			inFormat = next()
		case a == "--json-schema":
			jsonSchema = next()
		case a == "--bare":
			bareMode = true
		case a == "--file":
			fileArgs = append(fileArgs, next())
		case a == "--tools":
			toolsList = next()
		case a == "--workload":
			workload = next()
			os.Setenv("CLAUDE_GO_WORKLOAD", workload)
		case a == "--agents":
			agentsJSON = next()
		case a == "--agent":
			agentName = next()
		case a == "-c" || a == "--continue":
			cont = true
		case a == "-r" || a == "--resume":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				resume = next()
			} else {
				resume = "-"
			}
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

	if mockPort > 0 {
		runMockServer(mockPort)
		return
	}

	cwd, _ := os.Getwd()
	sessionID := newUUID()
	if sessID != "" {
		sessionID = sessID
	}

	client, cleanup := buildClient(mockMode, sessionID)
	defer cleanup()

	// connect MCP servers in background (like CC's async init)
	mcpClients := startMCP(cwd, mcpConfig, strictMCP)
	defer func() {
		for _, c := range mcpClients {
			c.Close()
		}
	}()

	if bareMode {
		os.Setenv("CLAUDE_CODE_SIMPLE", "1")
	}
	var promptAtt string
	loop := agent.NewLoop(client, cwd)
	if !mockMode {
		agent.QuotaCheck(context.Background(), client, model)
	}
	loop.Entrypoint = "sdk-cli"
	if model != "" {
		loop.Model = resolveModel(model)
	}
	loop.MaxTurns = maxTurns
	loop.PermMode = permMode
	loop.AppendSysP = appendSys
	loop.ToolCtx.Session = sessionID
	email, acct, dev := config.Account()
	loop.User = agent.UserConfig{Email: email, AccountUUID: acct, DeviceID: dev}
	loop.AddDirs = addDirs
	if sysPromptF != "" {
		if b, err := os.ReadFile(sysPromptF); err == nil {
			sysPrompt = string(b)
		}
	}
	if sysPrompt != "" {
		loop.AppendSysP = sysPrompt
	}
	for _, fp := range fileArgs {
		if b, err := os.ReadFile(fp); err == nil {
			promptAtt += fmt.Sprintf("\n<file path=%q>\n%s\n</file>\n", fp, string(b))
		}
	}
	loop.ToolCtx.OAuthToken = func() string { return clientCreds() }
	_ = partialMsgs
	_ = jsonSchema
	if agentName != "" {
		if p := agent.ResolveAgentPrompt(agentName, cwd); p != "" {
			loop.AppendSysP = p
		}
	}
	loop.FallbackModel = fallback
	st := config.Load(cwd)
	rules := map[string][]string{}
	if st.Permissions != nil {
		rules["allow"] = st.Permissions.Allow
		rules["deny"] = append(st.ManagedDeny, rules["deny"]...)
		rules["ask"] = append(st.ManagedAsk, rules["ask"]...)
		rules["deny"] = st.Permissions.Deny
		rules["ask"] = st.Permissions.Ask
		if st.Permissions.DefaultMode != "" {
			loop.PermMode = st.Permissions.DefaultMode
		}
	}
	if allowTools != "" {
		rules["allow"] = append(rules["allow"], strings.Split(allowTools, ",")...)
	}
	if toolsList != "" {
		loop.AllowedTools = strings.Split(toolsList, ",")
	}
	if denyTools != "" {
		rules["deny"] = append(rules["deny"], strings.Split(denyTools, ",")...)
	}
	if settingsF != "" {
		var sb []byte
		if strings.HasPrefix(strings.TrimSpace(settingsF), "{") {
			sb = []byte(settingsF) // --settings accepts a JSON string too
		} else {
			sb, _ = os.ReadFile(settingsF)
		}
		if len(sb) > 0 {
			var s2 config.Settings
			if json.Unmarshal(sb, &s2) == nil && s2.Permissions != nil {
				rules["allow"] = append(rules["allow"], s2.Permissions.Allow...)
				rules["deny"] = append(rules["deny"], s2.Permissions.Deny...)
				rules["ask"] = append(rules["ask"], s2.Permissions.Ask...)
			}
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
	loop.ToolCtx.Searcher = loop.ToolCtx.Agent.(*agent.Runner).WebSearch
	if st.Model != "" && model == "" {
		loop.Model = st.Model
	}

	// session transcript
	st2, _ := session.Open(cwd, sessionID)
	if st2 != nil {
		defer st2.Close()
		loop.Transcript = st2.Path
		loop.Events.OnCompact = func() {
			st2.LogCompactBoundary(0, len(loop.Messages))
		}
	}

	if cont || resume != "" {
		id := resume
		if cont {
			if l2, err := session.Latest(cwd); err == nil {
				id = l2
			}
		}
		if id == "-" {
			if id2 := repl.PickSession(cwd); id2 != "" {
				id = id2
			} else {
				return
			}
		}
		if id != "" {
			if msgs, err := session.Load(cwd, id); err == nil && len(msgs) > 0 {
				loop.Messages = msgs
				if forkSess && st2 != nil {
					st2.LogRecord(map[string]any{
						"type": "system", "subtype": "fork",
						"forkedFrom": map[string]string{"sessionId": id},
					})
					fmt.Fprintf(os.Stderr, "forked session %s → %s (%d messages)\n", id[:8], sessionID[:8], len(msgs))
				} else {
					fmt.Fprintf(os.Stderr, "resumed session %s (%d messages)\n", id, len(msgs))
				}
			}
		}
	}

	if debug || verbose {
		os.Setenv("CLAUDE_GO_DEBUG", "1")
	}
	if printMode {
		tools.Interactive = false
		if inFormat == "stream-json" {
			runPrintStreamJSON(loop)
			return
		}
		// non-interactive: honor rules, auto-deny what would need asking
		loop.ToolCtx.Permission = func(name string, input json.RawMessage, _ string) bool {
			return perms.Check(perms.Mode(loop.PermMode), &perms.Rules{
				Allow: rules["allow"], Deny: rules["deny"], Ask: rules["ask"],
			}, name, input, loop.ToolCtx.CWD) == perms.Allow
		}
		code := runPrint(loop, strings.Join(prompt, " ")+promptAtt, outFormat, st2, jsonSchema)
		os.Exit(code)
	}
	tools.Interactive = true
	loop.Client.Entrypoint = api.EntryCLI
	tools.Checkpoints.Init(sessionID)
	if !repl.TrustGate(cwd) {
		fmt.Println("not trusted — exiting")
		return
	}
	fireHook(loop, hooks.SessionStart)
	repl.RunWithStore(loop, st2)
	fireHook(loop, hooks.SessionEnd)
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
func startMCP(cwd, configFile string, strict bool) (clients []*mcp.Client) {
	var cfgs map[string]mcp.ServerConfig
	if configFile != "" {
		cfgs = map[string]mcp.ServerConfig{}
		if b, err := os.ReadFile(configFile); err == nil {
			var top struct {
				McpServers map[string]mcp.ServerConfig `json:"mcpServers"`
			}
			if json.Unmarshal(b, &top) == nil {
				cfgs = top.McpServers
			}
		}
	} else {
		cfgs = mcp.LoadConfigs(cwd)
	}
	_ = strict
	var wg sync.WaitGroup
	var mu sync.Mutex
	httpMCP := map[string]*mcp.HTTPClient{}
	_ = httpMCP
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
			tools.RegisterMCPServer(name, c)
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
	lastCreds = creds
	return api.NewClient(creds, sessionID, api.EntrySDKCLI), func() {}
}

var lastCreds *auth.Credentials

func clientCreds() string {
	if lastCreds != nil {
		return lastCreds.AccessToken
	}
	return ""
}

func runMockServer(port int) {
	srv, err := mock.StartOn(fmt.Sprintf(":%d", port))
	if err != nil {
		fatal("mock: %v", err)
	}
	fmt.Println("mock listening at", srv.Addr)
	select {}
}

func runPrint(l *agent.Loop, prompt, format string, st *session.Store, schema string) int {
	var out strings.Builder
	sj := format == "stream-json"
	if sj {
		toolNames := tools.Names()
		initEv, _ := json.Marshal(map[string]any{
			"type": "system", "subtype": "init",
			"session_id": l.Client.SessionID, "model": l.Model, "cwd": l.CWD,
			"tools": toolNames, "mcp_servers": []string{}, "permissionMode": l.PermMode,
			"slash_commands": repl.SlashCommandNames(), "apiKeySource": "none",
		})
		fmt.Println(string(initEv))
	}
	l.Events.OnTextDelta = func(t string) {
		switch format {
		case "text":
			fmt.Print(t)
		default:
			out.WriteString(t)
		}
	}
	turns := 0
	l.Events.OnToolUse = func(name string, input json.RawMessage) {
		if sj {
			ev, _ := json.Marshal(map[string]any{
				"type": "assistant",
				"message": map[string]any{"role": "assistant", "content": []map[string]any{{
					"type": "tool_use", "name": name, "input": json.RawMessage(input)}}},
				"session_id": l.Client.SessionID,
			})
			fmt.Println(string(ev))
		}
	}
	l.Events.OnToolResult = func(name string, tr tools.Result) {
		if sj {
			ev, _ := json.Marshal(map[string]any{
				"type": "user",
				"message": map[string]any{"role": "user", "content": []map[string]any{{
					"type": "tool_result", "content": tr.Content, "is_error": tr.IsError}}},
				"session_id": l.Client.SessionID,
			})
			fmt.Println(string(ev))
		}
	}
	ctx := context.Background()
	start := time.Now()
	resp, err := l.Ask(ctx, prompt)
	turns++
	if st != nil {
		for _, mm := range l.Messages {
			st.LogMessage(mm)
		}
	}
	if format == "text" {
		fmt.Println()
	}
	if err != nil {
		if sj {
			ev, _ := json.Marshal(map[string]any{
				"type": "result", "subtype": "error", "is_error": true,
				"error": err.Error(), "session_id": l.Client.SessionID,
				"duration_ms": time.Since(start).Milliseconds(), "num_turns": turns,
			})
			fmt.Println(string(ev))
		} else {
			fmt.Fprintln(os.Stderr, "Error:", err)
		}
		return 1
	}
	switch format {
	case "json", "stream-json":
		if schema != "" {
			// real wraps the result in structured output when a schema is given
			var v any
			if json.Unmarshal([]byte(out.String()), &v) != nil {
				out.Reset()
				b2, _ := json.Marshal(map[string]any{"result": out.String()})
				out.Write(b2)
			}
		}
		b, _ := json.MarshalIndent(map[string]any{
			"type":        "result",
			"subtype":     "success",
			"is_error":    false,
			"result":      out.String(),
			"session_id":  l.Client.SessionID,
			"duration_ms": time.Since(start).Milliseconds(),
			"num_turns":   turns,
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

// runPrintStreamJSON implements --input-format stream-json: reads
// newline-delimited user messages from stdin and answers each.
func runPrintStreamJSON(l *agent.Loop) {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m struct {
			Type    string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &m); err != nil || m.Type != "user" {
			continue
		}
		text := ""
		switch c := m.Message.Content.(type) {
		case string:
			text = c
		case []any:
			for _, b := range c {
				if bm, ok := b.(map[string]any); ok && bm["type"] == "text" {
					text += fmt.Sprint(bm["text"])
				}
			}
		}
		if text == "" {
			continue
		}
		var out strings.Builder
		l.Events.OnTextDelta = func(t string) { out.WriteString(t) }
		if _, err := l.Ask(context.Background(), text); err != nil {
			fmt.Fprintf(os.Stderr, `{"type":"error","error":%q}`+"\n", err.Error())
			continue
		}
		fmt.Printf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`+"\n", out.String())
	}
}

func authLoginSub() {
	urlStr, verifier, state := auth.LoginURL()
	fmt.Println("open in browser:", urlStr)
	auth.OpenBrowser(urlStr)
	fmt.Print("paste code (code#state): ")
	var code string
	fmt.Scanln(&code)
	creds, err := auth.ExchangeCode(code, verifier, state)
	if err != nil {
		fatal("login failed: %v", err)
	}
	if err := auth.WriteKeychain(creds); err != nil {
		fatal("keychain write failed: %v", err)
	}
	fmt.Println("logged in")
}

// resolveModel maps short aliases + env overrides to concrete model IDs,
// matching real resolution: sonnet/opus/haiku/default + ANTHROPIC_DEFAULT_*.
func resolveModel(m string) string {
	switch m {
	case "default", "":
		return ""
	case "sonnet":
		if v := os.Getenv("ANTHROPIC_DEFAULT_SONNET_MODEL"); v != "" {
			return v
		}
		return "claude-sonnet-4-6"
	case "opus":
		if v := os.Getenv("ANTHROPIC_DEFAULT_OPUS_MODEL"); v != "" {
			return v
		}
		return "claude-opus-4-6"
	case "haiku":
		if v := os.Getenv("ANTHROPIC_DEFAULT_HAIKU_MODEL"); v != "" {
			return v
		}
		if v := os.Getenv("ANTHROPIC_SMALL_FAST_MODEL"); v != "" {
			return v
		}
		return "claude-haiku-4-5"
	case "fable":
		if v := os.Getenv("ANTHROPIC_DEFAULT_FABLE_MODEL"); v != "" {
			return v
		}
		return "claude-fable-5-1"
	}
	return m // full id or [1m] suffix passes through
}
