// claude-go: a Go reimplementation of the Claude Code CLI.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
  claude-go -w [name]               run inside a new git worktree

Flags:
  -p, --print                   print response and exit
  --exec, -m <prompt>           execute prompt non-interactively and exit
  --model <model>               model (default claude-fable-5-1)
  --effort <level>              reasoning effort: low|medium|high|xhigh|max|auto
  --agent <name>                run as a named agent type
  --agents <json>               inline agent definitions ({name:{description,prompt,tools}})
  --output-format text|json|stream-json
  --input-format stream-json    read JSONL messages from stdin
  --include-partial-messages    emit partial assistant stream events (stream-json)
  --include-hook-events         include hook events in output (accepted)
  --replay-user-messages        echo user messages back in stream-json input mode
  --max-turns <n>               cap agent loop turns
  --max-budget-usd <n>          cost ceiling (accepted; not enforced)
  --max-thinking-tokens <n>     thinking budget
  --thinking <mode>             enabled|disabled
  --thinking-display <mode>     updates|omitted
  --task-budget <n>             (accepted)
  --permission-mode <mode>      default|acceptEdits|plan|bypassPermissions|auto
  --dangerously-skip-permissions  bypass all permission checks
  --allow-dangerously-skip-permissions  same
  --enable-auto-mode            start in auto permission mode
  --plan-mode-required          start in plan mode
  --permission-prompt-tool <t>  (accepted)
  --inherit-permission-mode <m> (accepted)
  --append-system-prompt <s>    append to the system prompt
  --append-system-prompt-file <f>  append system prompt from file
  --system-prompt <s>           extra system prompt text
  --system-prompt-file <f>      extra system prompt from file
  --system-prompt-snapshot <s>  (accepted)
  --append-subagent-system-prompt(-file)  (accepted)
  --plan-mode-instructions <s>  (accepted)
  --add-dir <dir>               grant access to an additional directory
  --allowedTools "Bash,Read"    add permission allow rules
  --disallowedTools "Bash(rm*)" add permission deny rules
  --tools "Bash,Read"           restrict the tool surface
  --file <path>                 attach file contents to the prompt (repeatable)
  --betas <flags>               extra anthropic-beta header flags
  --session-id <id>             use a specific session id
  --name, -n <name>             name this session
  -c, --continue                continue most recent session
  -r, --resume [id]             resume a session (picker if no id)
  --resume-session-at <uuid>    resume from a specific message
  --resume-drops-turn <n>       (accepted)
  --fork-session                fork when resuming
  --no-session-persistence      don't write a transcript
  --rewind-files <mode>         (accepted)
  -w, --worktree [name]         create .claude/worktrees/<name> and cd into it
  --project <dir>               run in a different project directory
  --from-pr <ref>               (accepted)
  --tmux                        (accepted; tmux integration not supported)
  --ide, --desktop              (accepted; IDE bridge not supported)
  --chrome, --no-chrome         chrome tool toggle (env)
  --teleport, --cloud, --remote, --remote-control/--rc
                                (accepted; remote sessions not supported)
  --environment, --pool <name>  (accepted)
  --mcp-config <file>           MCP servers config file
  --strict-mcp-config           only use --mcp-config servers
  --settings <file|json>        additional settings
  --setting-sources <list>      (accepted)
  --managed-settings <file>     (accepted)
  --plugin-dir <dir>            (accepted)
  --plugin-dir-no-mcp <dir>     (accepted)
  --plugin-url <url>            (accepted)
  --channels <list>             (accepted)
  --dangerously-load-development-channels <list>  (accepted)
  --forward-home-settings <v>   (accepted)
  --project-config-root <dir>   (accepted)
  --sdk-url <url>               API base URL (sets ANTHROPIC_BASE_URL)
  --attach-serve <target>       (accepted)
  --init-only                   run onboarding/migrations then exit
  --autocompact <mode>          (accepted)
  --advisor <mode>              (accepted)
  --proactivity <level>         (accepted)
  --prompt-suggestions [mode]   (accepted)
  --prefill <text>              (accepted)
  --prefill-b64 <b64>           (accepted)
  --teammate-mode <mode>        (accepted)
  --messaging-socket-path <p>   (accepted)
  --team-name, --agent-id, --agent-name, --agent-color, --agent-type <v>
                                teammate identity fields (accepted)
  --parent-session-id <id>      (accepted)
  --remote-control-session-name-prefix <s>  (accepted)
  --deep-link-* <v>             deep link params (accepted)
  --handle-uri <uri>            (accepted)
  --watch-artifact*, --correlation-id, --ref, --on-branch, --routine <v>
                                (accepted)
  --workload <name>             workload tag (env CLAUDE_GO_WORKLOAD)
  --json-schema <json>          structured output schema (print mode)
  --fallback-model <m>          model fallback chain
  --mcp-config, --add-dir       repeatable
  --debug, -d, --verbose        debug logging
  --debug-file <path>           redirect debug output to file
  --bare                        minimal session (no auto-discovery)
  --mock                        run against the built-in mock API (no auth needed)
  --mock-port <port>            start a standalone mock server on a port and exit
  -v, -V, --version             print version
  -h, --help                    this help

Subcommands:
  agents                        list agent types + background tasks
  auth login|status|logout      credential management
  config get|set|list           settings.json
  doctor                        diagnostics
  import <file>                 import a transcript into this project
  install, update               version info (built from source)
  login                         OAuth login
  mcp list|add|get|remove       manage MCP servers
  plugin list|install|remove|marketplace
  purge [--dry-run] [--all]     delete local project state
  setup-token                   store a long-lived token
  gateway, ssh, connect, self-hosted-runner  (stubs; not supported)
`, version)
}

// flagReplayUser / flagPartial bridge parse-time flags into the print
// runners below.
var flagReplayUser, flagPartial bool

func main() {
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
		case "update", "install":
			fmt.Println("claude-go is built from source; update via git pull && go build")
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
		case "plugin":
			pluginSub(args[1:])
			return
		case "setup-token":
			setupTokenSub()
			return
		case "agents":
			agentsSub(args[1:])
			return
		case "import":
			importSub(args[1:])
			return
		case "gateway", "ssh", "connect", "self-hosted-runner":
			fmt.Printf("claude-go %s: not supported — %s needs hosted infrastructure claude-go doesn't have\n", args[0], args[0])
			return
		}
	}
	o := parseCLI(args)

	// env wiring that must precede client construction
	if o.sdkURL != "" {
		os.Setenv("ANTHROPIC_BASE_URL", o.sdkURL)
	}
	if o.betas != "" {
		applyBetaFlags(o.betas)
	}
	if o.hookEvents {
		os.Setenv("CLAUDE_GO_INCLUDE_HOOK_EVENTS", "1")
	}
	flagReplayUser = o.replayUser
	flagPartial = o.partialMsgs

	if o.mockPort > 0 {
		runMockServer(o.mockPort)
		return
	}

	cwd, _ := os.Getwd()
	if o.projectDir != "" {
		if abs, err := filepath.Abs(o.projectDir); err == nil {
			if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
				cwd = abs
				os.Chdir(abs)
			} else {
				fmt.Fprintf(os.Stderr, "--project: %s is not a directory\n", o.projectDir)
			}
		}
	}
	sessionID := newUUID()
	if o.sessID != "" {
		sessionID = o.sessID
	}
	if o.worktreeSet {
		cwd = setupWorktree(cwd, o.worktreeName, sessionID)
		os.Chdir(cwd)
	}

	migrateClaudeJSON()
	go checkForUpdate()
	maybeOnboard(!o.printMode)
	if o.initOnly {
		fmt.Println("claude-go initialized")
		return
	}
	for _, s := range o.stubs {
		fmt.Fprintf(os.Stderr, "note: %s accepted but not supported in claude-go\n", s)
	}
	agentCleanup := installCLIAgents(o.agentsJSON, cwd)
	if o.agentsJSON != "" {
		os.Setenv("CLAUDE_GO_AGENTS_JSON", o.agentsJSON)
	}
	defer agentCleanup() // note: os.Exit below skips defers; print mode calls it explicitly

	client, cleanup := buildClient(o.mockMode, sessionID)
	defer cleanup()

	// connect MCP servers in background (like CC's async init)
	mcpClients := startMCP(cwd, o.mcpConfig, o.strictMCP)
	defer func() {
		for _, c := range mcpClients {
			c.Close()
		}
	}()

	if o.bareMode {
		os.Setenv("CLAUDE_CODE_SIMPLE", "1")
	}
	var promptAtt string
	loop := agent.NewLoop(client, cwd)
	if !o.mockMode {
		agent.QuotaCheck(context.Background(), client, o.model)
	}
	loop.Entrypoint = "sdk-cli"
	if o.model != "" {
		loop.Model = resolveModel(o.model)
	}
	loop.MaxTurns = o.maxTurns
	loop.PermMode = o.permMode
	loop.AppendSysP = o.appendSys
	loop.ToolCtx.Session = sessionID
	email, acct, dev := config.Account()
	loop.User = agent.UserConfig{Email: email, AccountUUID: acct, DeviceID: dev}
	loop.AddDirs = o.addDirs
	if o.sysPromptF != "" {
		if b, err := os.ReadFile(o.sysPromptF); err == nil {
			o.sysPrompt = string(b)
		}
	}
	if o.sysPrompt != "" {
		loop.AppendSysP = o.sysPrompt
	}
	for _, fp := range o.fileArgs {
		if b, err := os.ReadFile(fp); err == nil {
			promptAtt += fmt.Sprintf("\n<file path=%q>\n%s\n</file>\n", fp, string(b))
		}
	}
	loop.ToolCtx.OAuthToken = func() string { return clientCreds() }
	if o.agentName != "" {
		if p := agent.ResolveAgentPrompt(o.agentName, cwd); p != "" {
			loop.AppendSysP = p
		}
	}
	loop.FallbackModel = o.fallback
	if o.effort != "" {
		loop.Effort = o.effort
	}
	loop.ThinkingMode = o.thinkingMode
	loop.ThinkingDisplay = o.thinkingDisp
	loop.ThinkingBudget = o.thinkingTok
	st := config.Load(cwd)
	st.ApplyEnv() // settings.env injects into the process + all children
	// ANTHROPIC_MODEL env beats settings + flags when set
	if em := os.Getenv("ANTHROPIC_MODEL"); em != "" && o.model == "" {
		loop.Model = em
	}
	if loop.Effort == "" && st.EffortLevel != "" {
		loop.Effort = st.EffortLevel
	}
	loop.AutoCompactWindow = st.AutoCompactWindow
	loop.AlwaysThinking = st.AlwaysThinkingEnabled
	if st.FastMode == "on" || st.FastMode == "true" {
		loop.FastMode = true
	}
	rules := map[string][]string{}
	if st.Permissions != nil {
		rules["allow"] = st.Permissions.Allow
		// managed deny/ask rules CANNOT be overridden — prepend, never replace
		rules["deny"] = append(append([]string{}, st.ManagedDeny...), st.Permissions.Deny...)
		rules["ask"] = append(append([]string{}, st.ManagedAsk...), st.Permissions.Ask...)
		rules["allow"] = append(rules["allow"], st.ManagedAllow...)
		if st.Permissions.DefaultMode != "" {
			loop.PermMode = st.Permissions.DefaultMode
		}
		if st.Permissions.DisableBypassPermissionsMode == "disable" && loop.PermMode == "bypassPermissions" {
			loop.PermMode = "default"
		}
		if st.Permissions.DisableAutoMode == "disable" && loop.PermMode == "auto" {
			loop.PermMode = "default"
		}
		// additionalDirectories widen the writeable workspace
		for _, d := range st.Permissions.AdditionalDirectories {
			if !filepath.IsAbs(d) {
				d = filepath.Join(cwd, d)
			}
			d = filepath.Clean(d)
			tools.ExtraAllowedDirs = append(tools.ExtraAllowedDirs, d)
			perms.AdditionalDirs = append(perms.AdditionalDirs, d)
		}
	}
	// persisted per-project allow rules from ~/.claude.json
	if b, err := os.ReadFile(filepath.Join(config.Home(), ".claude.json")); err == nil {
		var cfg struct {
			Projects map[string]struct {
				AllowedTools []string `json:"allowedTools"`
			} `json:"projects"`
		}
		if json.Unmarshal(b, &cfg) == nil {
			if proj, ok := cfg.Projects[cwd]; ok {
				rules["allow"] = append(rules["allow"], proj.AllowedTools...)
			}
		}
	}
	if o.allowTools != "" {
		rules["allow"] = append(rules["allow"], strings.Split(o.allowTools, ",")...)
	}
	if o.toolsList != "" {
		loop.AllowedTools = strings.Split(o.toolsList, ",")
	}
	if o.denyTools != "" {
		rules["deny"] = append(rules["deny"], strings.Split(o.denyTools, ",")...)
	}
	if o.settingsF != "" {
		var sb []byte
		if strings.HasPrefix(strings.TrimSpace(o.settingsF), "{") {
			sb = []byte(o.settingsF) // --settings accepts a JSON string too
		} else {
			sb, _ = os.ReadFile(o.settingsF)
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
	stampProjectState(cwd, sessionID, loop.Model, false)
	loop.Hooks = hooks.Load(cwd)
	loop.SessionStartSource = "startup"
	tools.RegisterSession(sessionID, cwd, "")
	defer tools.UnregisterSession(sessionID)
	loop.ToolCtx.Agent = &agent.Runner{Hooks: loop.Hooks,
		Client: client, PermMode: loop.PermMode, Model: loop.Model,
		User: loop.User, PermRules: rules, Entrypoint: "sdk-cli",
		Parent: loop,
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
	// prompt/agent-type hooks are judged by the small model
	hooks.Judge = func(prompt, ctxJSON string) (bool, string) {
		out, err := loop.ToolCtx.Answerer(context.Background(),
			`You are evaluating a hook decision for Claude Code. Reply with exactly "OK" if the action should proceed, or "BLOCK: <reason>" if it should not.`+"\n\n"+prompt, ctxJSON)
		if err != nil {
			return true, ""
		}
		out = strings.TrimSpace(out)
		if strings.HasPrefix(strings.ToUpper(out), "BLOCK") {
			reason := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(out, "BLOCK:"), "BLOCK"))
			return false, reason
		}
		return true, ""
	}
	if st.Model != "" && o.model == "" {
		loop.Model = st.Model
	}

	// session transcript
	var st2 *session.Store
	if !o.noPersist {
		st2, _ = session.Open(cwd, sessionID)
	}
	if st2 != nil {
		defer st2.Close()
		loop.Transcript = st2.Path
		loop.Events.OnCompact = func() {
			st2.LogCompactBoundary(0, len(loop.Messages))
		}
		if o.sessionName != "" {
			st2.LogRecord(map[string]any{"type": "summary", "summary": o.sessionName})
		}
	}

	if o.cont || o.resume != "" {
		loop.SessionStartSource = "resume"
		id := o.resume
		if o.cont {
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
			var msgs []api.Message
			var err error
			switch {
			case o.resumeAt != "":
				msgs, err = loadSessionAt(cwd, id, o.resumeAt)
			case strings.HasSuffix(id, ".jsonl"): // resume by transcript path (pickers return paths for foreign projects)
				msgs, err = session.LoadPath(id)
				id = filepath.Base(id)[:len(filepath.Base(id))-6]
			default:
				msgs, err = session.Load(cwd, id)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "resume %s: %v\n", id, err)
			} else if len(msgs) > 0 {
				loop.Messages = msgs
				if o.forkSess && st2 != nil {
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

	if o.debug || o.verbose {
		os.Setenv("CLAUDE_GO_DEBUG", "1")
	}
	if o.debugFile != "" {
		if f, err := os.OpenFile(o.debugFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			os.Stderr = f
		}
	}
	if o.printMode {
		tools.Interactive = false
		if o.inFormat == "stream-json" {
			runPrintStreamJSON(loop)
			return
		}
		// non-interactive: honor rules, auto-deny what would need asking
		loop.ToolCtx.Permission = func(name string, input json.RawMessage, _ string) bool {
			if name == "Bash" && tools.SandboxAutoAllow(cwd) {
				return true // sandboxed bash auto-allowed per settings
			}
			return perms.Check(perms.Mode(loop.PermMode), &perms.Rules{
				Allow: rules["allow"], Deny: rules["deny"], Ask: rules["ask"],
			}, name, input, loop.ToolCtx.CWD) == perms.Allow
		}
		code := runPrint(loop, strings.Join(o.prompt, " ")+promptAtt, o.outFormat, st2, o.jsonSchema)
		if o.maxBudget > 0 {
			fmt.Fprintf(os.Stderr, "note: --max-budget-usd %.2f (per-request cost tracking not yet enforced)\n", o.maxBudget)
		}
		agentCleanup() // os.Exit skips the deferred call above
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
	stampProjectState(cwd, sessionID, loop.Model, true)
}

func fireHook(l *agent.Loop, ev hooks.Event) {
	payload := map[string]any{
		"session_id":      l.Client.SessionID,
		"transcript_path": l.Transcript,
		"cwd":             l.ToolCtx.CWD,
		"hook_event_name": string(ev),
		"permission_mode": l.PermMode,
	}
	switch ev {
	case hooks.SessionStart:
		payload["source"] = l.SessionStartSource
	case hooks.SessionEnd:
		payload["reason"] = l.SessionEndReason
	}
	res := hooks.Run(l.Hooks, ev, payload, l.ToolCtx.CWD)
	// SessionStart hook stdout/additionalContext injects into the session
	if ev == hooks.SessionStart {
		if s := res.PlainText + res.AdditionalContext; s != "" {
			l.AppendSysP += "\n\n" + strings.TrimSpace(s)
		}
	}
	if res.NonBlockErr != "" {
		fmt.Fprintf(os.Stderr, "%s hook error: %s%s\n", "[31m", strings.TrimSpace(res.NonBlockErr), "[0m")
	}
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
		if cfg.Command == "" && cfg.URL == "" {
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
	os.Exit(1) // real CC exits 1 on CLI errors (2 is hook-block)
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
		if sj && flagPartial {
			ev, _ := json.Marshal(map[string]any{
				"type": "stream_event",
				"event": map[string]any{
					"type":  "content_block_delta",
					"index": 0,
					"delta": map[string]any{"type": "text_delta", "text": t},
				},
				"session_id": l.Client.SessionID,
			})
			fmt.Println(string(ev))
		}
	}
	l.Events.OnThinking = func(t string) {
		if sj && flagPartial {
			ev, _ := json.Marshal(map[string]any{
				"type": "stream_event",
				"event": map[string]any{
					"type":  "content_block_delta",
					"index": 0,
					"delta": map[string]any{"type": "thinking_delta", "thinking": t},
				},
				"session_id": l.Client.SessionID,
			})
			fmt.Println(string(ev))
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
		if flagReplayUser {
			// --replay-user-messages: echo input back into the output stream
			fmt.Println(string(line))
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

// stampProjectState records lastSessionId/lastUsedModel/lastGracefulShutdown
// under ~/.claude.json projects[cwd] (real fields), plus bumps numStartups.
func stampProjectState(cwd, sessionID, model string, gracefulEnd bool) {
	cfg, p := config.ReadClaudeJSON()
	projs, _ := cfg["projects"].(map[string]any)
	if projs == nil {
		projs = map[string]any{}
	}
	proj, _ := projs[cwd].(map[string]any)
	if proj == nil {
		proj = map[string]any{}
	}
	proj["lastSessionId"] = sessionID
	if model != "" {
		proj["lastUsedModel"] = model
	}
	proj["lastGracefulShutdown"] = gracefulEnd
	projs[cwd] = proj
	cfg["projects"] = projs
	if !gracefulEnd {
		n, _ := cfg["numStartups"].(float64)
		cfg["numStartups"] = n + 1
	}
	config.WriteClaudeJSON(p, cfg)
}
