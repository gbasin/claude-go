// claude-go: a Go reimplementation of the Claude Code CLI.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"claude-go/internal/agent"
	"claude-go/internal/api"
	"claude-go/internal/auth"
	"claude-go/internal/bridge"
	"claude-go/internal/browsermcp"
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
  --claude-in-chrome-mcp        run the Claude in Chrome stdio MCP server (no REPL)
  --chrome-native-host          run the Chrome native-messaging host (extension-facing)
  --teleport [session]          resume a cloud session locally (with <session>),
                                or move this session to the cloud via /teleport
  --cloud, --remote, --remote-control/--rc
                                (accepted; use the remote-control subcommand)
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
  sandbox trust                 trust the sandbox MITM CA (macOS keychain)
  setup-token                   store a long-lived token
  remote-control, rc            Remote Control server (control-plane skeleton)
  gateway, ssh, connect, self-hosted-runner  (stubs; not supported)
`, version)
}

// flagReplayUser / flagPartial bridge parse-time flags into the print
// runners below.
var flagReplayUser, flagPartial bool

func main() {
	args := os.Args[1:]
	// argv[1] fast-path modes (real: mod_0006.js process.argv[2] checks) —
	// these are whole-process modes, not REPL flags.
	if len(args) > 0 {
		switch args[0] {
		case "--claude-in-chrome-mcp":
			os.Exit(browsermcp.RunServer())
		case "--chrome-native-host":
			os.Exit(browsermcp.RunNativeHost(os.Stdin, os.Stdout))
		}
	}
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
			selfUpdate()
			return
		case "login":
			authLoginSub()
			return
		case "auth":
			authSub(args[1:])
			return
		case "sandbox":
			sandboxSub(args[1:])
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
		case "remote-control", "rc":
			remoteControlSub(args[1:])
			return
		case "gateway", "ssh", "connect", "self-hosted-runner":
			fmt.Printf("claude-go %s: not supported — %s needs hosted infrastructure claude-go doesn't have\n", args[0], args[0])
			return
		}
	}
	// `claude <root flags> remote-control`: real refuses root options that
	// wouldn't carry into spawned sessions (mod_0690.js k()/D()).
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if vi := bridge.FindVerb(args); vi > 0 {
			if refused := bridge.RefusedRootOptions(args[:vi]); len(refused) > 0 {
				fmt.Fprintln(os.Stderr, bridge.RootOptionsRefusedMessage(refused))
				os.Exit(1)
			}
			remoteControlSub(args[vi+1:])
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

	// GB flag eval — real fires POST /api/eval/sdk-<key> near startup;
	// the feature map merges into cachedGrowthBookFeatures. Success/fail
	// selects real's flag-default header+beta set on later requests.
	if !o.mockMode {
		go func() {
			feats, err := client.EvalCheck(context.Background(), "")
			// empty-feature 200 => real's fallback header/beta set;
			// transport errors keep the cached-flag production shape
			client.MarkEvalFailed(err == nil && feats != nil && len(feats) == 0)
		}()
	}

	// connect MCP servers in background (like CC's async init)
	mcp.SetSessionID(sessionID)
	chromeSysP, chromeMCP := chromeSessionSetup(o, lastCreds)
	mcpClients := startMCP(cwd, o.mcpConfig, o.strictMCP, chromeMCP)
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
	}
	loop.Entrypoint = "sdk-cli"
	if o.model != "" {
		loop.Model = resolveModel(o.model)
	}
	loop.Wants1m = wants1m
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
	if chromeSysP != "" {
		// real splices the chrome/browser block into the sys[3] session
		// block (captured: end of the session array entry), not sys[2]
		loop.SessionExtras = append(loop.SessionExtras, chromeSysP)
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
		loop.Model = resolveModel(em)
		loop.Wants1m = wants1m
	}
	if loop.Effort == "" && st.EffortLevel != "" {
		loop.Effort = st.EffortLevel
	}
	loop.AutoCompactWindow = st.AutoCompactWindow
	loop.AdvisorModel = st.AdvisorModel
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
		// explicit --permission-mode beats settings defaultMode
		if st.Permissions.DefaultMode != "" && !o.permModeSet {
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
	var sb2 []byte
	if o.settingsF != "" {
		var sb []byte
		if strings.HasPrefix(strings.TrimSpace(o.settingsF), "{") {
			sb = []byte(o.settingsF) // --settings accepts a JSON string too
		} else {
			sb, _ = os.ReadFile(o.settingsF)
		}
		sb2 = sb
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
	// --settings JSON also carries hooks (real applies the whole file)
	if o.settingsF != "" && len(sb2) > 0 {
		var top struct {
			Hooks hooks.Config `json:"hooks"`
		}
		if json.Unmarshal(sb2, &top) == nil {
			for ev, ms := range top.Hooks {
				loop.Hooks[ev] = append(loop.Hooks[ev], ms...)
			}
		}
	}
	loop.SessionStartSource = "startup"
	tools.SetPeerPermMode(func() string { return loop.PermMode })
	tools.RegisterSession(sessionID, cwd, "")
	// durable cron (tengu_kairos_cron_durable): take the project scheduler
	// lock, surface missed one-shots, and re-arm .claude/scheduled_tasks.json
	tools.StartScheduledTasks(cwd, sessionID)
	defer tools.StopScheduledTasks()
	messengerCleanup := tools.StartMessenger(sessionID)
	defer messengerCleanup()
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
		loop.Model = resolveModel(st.Model)
		loop.Wants1m = wants1m
	}

	// session transcript
	var st2 *session.Store
	if !o.noPersist {
		st2, _ = session.Open(cwd, sessionID)
	}
	if st2 != nil {
		defer st2.Close()
		loop.Transcript = st2.Path
		st2.LogInit(loop.Model) // system/init record at session start (real)
		tools.OnSnapshot = func(path, backup string, ver int) {
			st2.LogRecord(map[string]any{
				"type": "file-history-snapshot", "isSnapshotUpdate": true,
				"snapshot": map[string]any{"trackedFileBackups": map[string]any{
					path: map[string]any{"backupFileName": filepath.Base(backup),
						"version": ver, "backupTime": time.Now().UTC().Format(time.RFC3339)},
				}},
			})
		}
		if ar, ok := loop.ToolCtx.Agent.(*agent.Runner); ok {
			ar.Transcript = st2.Path
		}
		loop.Events.OnCompact = func() {
			_, trig := loop.CompactMeta()
			st2.LogCompactBoundaryTrig(loop.LastInputTokens(), len(loop.Messages), trig)
		}
		if o.printMode {
			// print mode logs messages only after Ask returns — a
			// mid-turn compaction would drop the replaced span from the
			// transcript entirely. Log it as it's replaced, ahead of the
			// boundary record (interactive REPL logs incrementally and
			// wires its own span handler in RunWithStore).
			loop.Events.OnCompactSpan = func(span []api.Message) {
				for _, m := range span {
					st2.Log(m)
				}
			}
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
			// normalize: -r accepts a transcript path, not just an id —
			// resolve it to (id, loadPath) before branching on resumeAt
			loadPath := ""
			if strings.HasSuffix(id, ".jsonl") {
				loadPath = id
				id = strings.TrimSuffix(filepath.Base(id), ".jsonl")
			}
			switch {
			case o.resumeAt != "" && loadPath != "":
				msgs, err = loadSessionAtPath(loadPath, o.resumeAt)
			case o.resumeAt != "":
				msgs, err = loadSessionAt(cwd, id, o.resumeAt)
			case loadPath != "":
				msgs, err = session.LoadPath(loadPath)
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
	if o.teleport {
		// claude --teleport: real's flag resumes a session FROM the cloud
		// (mod_0686.js): <session> = direct resume via
		// teleportWithProgress/cRe (Bx -> p3n -> uss -> WNt -> jNt);
		// bare flag opens the cloud-session picker (Pi) fed by
		// GET /v1/code/sessions.
		if o.teleportArg != "" {
			if err := teleportResume(loop, o.teleportArg); err != nil {
				var ue *bridge.UserError
				msg := err.Error()
				if errors.As(err, &ue) && ue.UX != "" {
					msg = ue.UX
				}
				fmt.Fprintln(os.Stderr, msg)
				os.Exit(1)
			}
		} else if id := teleportPick(loop); id != "" {
			if err := teleportResume(loop, id); err != nil {
				var ue *bridge.UserError
				msg := err.Error()
				if errors.As(err, &ue) && ue.UX != "" {
					msg = ue.UX
				}
				fmt.Fprintln(os.Stderr, msg)
				os.Exit(1)
			}
		}
	}
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
// dynamic carries host-side additions (real's scope:"dynamic" entries —
// gme() puts `claude-in-chrome` there), merged last so a configured server
// of the same name wins, like real's explicit mcpConfigs beat dynamic.
func startMCP(cwd, configFile string, strict bool, dynamic map[string]mcp.ServerConfig) (clients []*mcp.Client) {
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
	if cfgs == nil {
		cfgs = map[string]mcp.ServerConfig{}
	}
	for name, c := range dynamic {
		if _, taken := cfgs[name]; !taken {
			cfgs[name] = c
		}
	}
	// claude.ai account connectors — real's yke()/SLn() (mod_0386.js):
	// the connector directory auto-mounts as scope:"claudeai" servers whose
	// tools appear as mcp__claude_ai_<Connector>__<tool>. Session OAuth
	// (user:mcp_servers scope) is the bearer for both discovery and calls.
	if lastCreds != nil {
		mcp.SetClaudeAICredentials(lastCreds)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		for n, c := range mcp.FetchClaudeAIConfigs(ctx, lastCreds, cwd) {
			if _, taken := cfgs[n]; taken {
				continue
			}
			// ODt(): connectors the control plane marks ineligible stay out
			// of the connect list unless previously connected.
			if c.Eligible != nil && !*c.Eligible && !mcp.ClaudeAIEverConnected(n) {
				continue
			}
			cfgs[n] = c
		}
		cancel()
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
				tools.NoteMCPServerStatus(name, 0, err)
				fmt.Fprintf(os.Stderr, "  [mcp] %s: %v\n", name, err)
				return
			}
			tools.NoteMCPServerStatus(name, len(c.Tools), nil)
			// tool prefix is the normalized server name (real's vn()):
			// "claude.ai Claude Docs" → mcp__claude_ai_Claude_Docs__<tool>
			prefix := mcp.NormalizeServerName(name)
			for _, t := range c.Tools {
				tools.RegisterMCP(prefix, c, t.Name, t.Description, t.InputSchema)
			}
			tools.RegisterMCPServer(name, c)
			mu.Lock()
			clients = append(clients, c)
			mu.Unlock()
		}(name, cfg)
	}
	wg.Wait()
	// IDE bridge: ~/.claude/ide/*.lock → MCP-over-WS (VSCode/Cursor extension)
	if ic, err := mcp.ConnectIDE(cwd, config.ClaudeDir()); err == nil && ic != nil {
		for _, t := range ic.Tools {
			tools.RegisterMCP(mcp.NormalizeServerName(ic.Name), ic, t.Name, t.Description, t.InputSchema)
		}
		tools.RegisterMCPServer(ic.Name, ic)
		clients = append(clients, ic)
	}
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

// cloudSessionClient builds the OAuth-lane bridge client for the
// --teleport resume path (same wiring as repl's codeSessionClient).
func cloudSessionClient(l *agent.Loop) *bridge.Client {
	return &bridge.Client{
		BaseURL:        l.Client.BaseURL,
		RunnerVersion:  api.Version,
		OrgUUID:        config.OrganizationUUID(),
		GetAccessToken: clientCreds,
	}
}

// teleportResume runs the direct `--teleport <session>` resume
// (teleportWithProgress → cRe in mod_0686.js/chunk-acxptg39.js): fetch the
// cloud session, validate the repo, fetch the log, checkout the outcome
// branch, then load the messages into this session.
func teleportResume(l *agent.Loop, id string) error {
	c := cloudSessionClient(l)
	res, err := c.ResumeFromCloud(context.Background(), id, &bridge.ResumeDeps{CWD: l.ToolCtx.CWD})
	if err != nil {
		return err
	}
	l.Messages = res.Messages
	fmt.Println(bridge.ResumeNotice(res.BranchError))
	if res.EnvironmentKind == "bridge" {
		// jNt's bridge-environment warning ($t(...,"warning")).
		fmt.Println("This terminal now has its own copy of the session: new work here stays local " +
			"and will not appear in the Claude app. To continue on your phone later, " +
			"run /remote-control in this session.")
	}
	return nil
}

// teleportPick is the bare `--teleport` picker (Pi): list resumable cloud
// sessions and read a number from stdin. Non-interactive stdin prints the
// list plus the direct form and returns "".
func teleportPick(l *agent.Loop) string {
	c := cloudSessionClient(l)
	sessions, err := c.ListCodeSessions(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "teleport: couldn't list cloud sessions:", err)
		return ""
	}
	var open []*bridge.CloudSession
	for _, s := range sessions {
		if s != nil && s.SessionStatus != "archived" {
			open = append(open, s)
		}
	}
	if len(open) == 0 {
		fmt.Fprintln(os.Stderr, "teleport: no cloud sessions found")
		return ""
	}
	fmt.Println("cloud sessions:")
	for i, s := range open {
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Printf("  %d. %s  %s  %s\n", i+1, title, s.ID, s.UpdatedAt)
	}
	st, _ := os.Stdin.Stat()
	if st.Mode()&os.ModeCharDevice == 0 {
		fmt.Println("run `claude --teleport <session-id>` to resume one")
		return ""
	}
	fmt.Print("number to resume (blank cancels): ")
	var pick string
	fmt.Scanln(&pick)
	if pick == "" {
		return ""
	}
	var n int
	if _, err := fmt.Sscanf(pick, "%d", &n); err != nil || n < 1 || n > len(open) {
		fmt.Fprintln(os.Stderr, "teleport: cancelled")
		return ""
	}
	return open[n-1].ID
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
		emitStreamJSONInit(l)
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
	emitStreamJSONInit(l)
	start := time.Now()
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	turns := 0
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
		l.Events.OnTextDelta = func(t string) {
			out.WriteString(t)
			if flagPartial {
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
		turns++
		if _, err := l.Ask(context.Background(), text); err != nil {
			ev, _ := json.Marshal(map[string]any{
				"type": "result", "subtype": "error", "is_error": true,
				"error": err.Error(), "session_id": l.Client.SessionID,
				"duration_ms": time.Since(start).Milliseconds(), "num_turns": turns,
			})
			fmt.Println(string(ev))
			continue
		}
		fmt.Printf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":%q}]},"session_id":%q}`+"\n",
			out.String(), l.Client.SessionID)
	}
	// stream-json protocol always ends with a result event
	ev, _ := json.Marshal(map[string]any{
		"type": "result", "subtype": "success", "is_error": false,
		"result": "", "session_id": l.Client.SessionID,
		"duration_ms": time.Since(start).Milliseconds(), "num_turns": turns,
	})
	fmt.Println(string(ev))
}

// emitStreamJSONInit writes the system/init event real emits first on
// --output-format stream-json.
func emitStreamJSONInit(l *agent.Loop) {
	toolNames := tools.Names()
	initFields := map[string]any{
		"type": "system", "subtype": "init",
		"session_id": l.Client.SessionID, "model": l.Model, "cwd": l.CWD,
		"tools": toolNames, "mcp_servers": []string{}, "permissionMode": l.PermMode,
		"slash_commands": repl.SlashCommandNames(), "apiKeySource": "none",
	}
	// mod_0659.js: startup payloads carry is_coordinator only when true
	if tools.IsCoordinatorMode() {
		initFields["is_coordinator"] = true
	}
	initEv, _ := json.Marshal(initFields)
	fmt.Println(string(initEv))
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
	auth.SaveOwnCreds(creds)
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
		return "claude-sonnet-5-5"
	case "opus":
		if v := os.Getenv("ANTHROPIC_DEFAULT_OPUS_MODEL"); v != "" {
			return v
		}
		return "claude-opus-5-5"
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
	// <alias|id>[1m] — real's 1M-context shorthand: resolves the base and
	// marks the request for the context-1m beta (the suffix never reaches
	// the wire — verified: claude-opus-4-8[1m] → not_found_error)
	if strings.HasSuffix(m, "[1m]") {
		wants1m = true
		return resolveModel(strings.TrimSuffix(m, "[1m]"))
	}
	return m // full id passes through
}

// wants1m is set when the configured model used the [1m] suffix — the
// request then carries the context-1m beta (client.go).
var wants1m bool

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
	// real keeps per-model usage under lastModelUsage; stamp minimal + keep
	// unknown keys untouched
	if model != "" {
		lmu, _ := proj["lastModelUsage"].(map[string]any)
		if lmu == nil {
			lmu = map[string]any{}
		}
		lmu[model] = map[string]any{"lastUsed": time.Now().UTC().Format(time.RFC3339)}
		proj["lastModelUsage"] = lmu
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

// selfUpdate rebuilds from the source checkout that produced this binary —
// claude-go ships from source, so "update" = git pull + go build + swap.
func selfUpdate() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Println("update: can't locate binary:", err)
		return
	}
	exe, _ = filepath.EvalSymlinks(exe)
	// find the repo root above the binary
	dir := filepath.Dir(exe)
	repo := ""
	for d := dir; ; d = filepath.Dir(d) {
		if st, err := os.Stat(filepath.Join(d, "go.mod")); err == nil && !st.IsDir() {
			b, _ := os.ReadFile(filepath.Join(d, "go.mod"))
			if strings.Contains(string(b), "module claude-go") {
				repo = d
				break
			}
		}
		if d == "/" || d == "." {
			break
		}
	}
	if repo == "" {
		fmt.Println("update: binary isn't inside a claude-go source checkout — rebuild with `go install github.com/gbasin/claude-go@latest`")
		return
	}
	out, err := exec.Command("git", "-C", repo, "pull", "--ff-only").CombinedOutput()
	if err != nil {
		fmt.Printf("update: pull failed: %s\n", out)
		return
	}
	fmt.Print(string(out))
	tmp := exe + ".new"
	cmd := exec.Command("go", "build", "-o", tmp, ".")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmp)
		fmt.Printf("update: build failed: %s\n", out)
		return
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		fmt.Println("update: couldn't replace binary:", err)
		return
	}
	fmt.Println("updated claude-go →", exe)
}

// --- Claude in Chrome (mod_0659.js startup wiring) ------------------------------

// chromeFlagState reads the --chrome/--no-chrome toggle (flags.go parks it
// in CLAUDE_GO_CHROME). nil = not passed.
func chromeFlagState() *bool {
	switch os.Getenv("CLAUDE_GO_CHROME") {
	case "1":
		v := true
		return &v
	case "0":
		v := false
		return &v
	}
	return nil
}

// claudeInChromeDefault reads le().claudeInChromeDefaultEnabled.
func claudeInChromeDefault() (set, enabled bool) {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return false, false
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return false, false
	}
	if v, ok := m["claudeInChromeDefaultEnabled"].(bool); ok {
		return true, v
	}
	return false, false
}

// chromeSessionSetup mirrors the `else if(Gt)` branch of the startup block
// in mod_0659.js: when Claude in Chrome is enabled for this session, wire
// the dynamic stdio MCP server (gme()) and append the browser-automation
// system prompt (W1e().systemPrompt). The entitlement gate (W()) runs only
// when an enable signal is present — matching real's lazy evaluation —
// and a denial on an explicit ask prints real's warning line.
func chromeSessionSetup(o *cliOpts, creds *auth.Credentials) (sysP string, cfgs map[string]mcp.ServerConfig) {
	chrome := chromeFlagState()
	defSet, defEnabled := claudeInChromeDefault()
	cfc := os.Getenv("CLAUDE_CODE_ENABLE_CFC")
	wanted := (chrome != nil && *chrome) || cfc == "true" || (defSet && defEnabled) ||
		(chrome == nil && cfc == "" && defSet) // let the setting decide
	if chrome != nil && !*chrome {
		return "", nil
	}
	if cfc == "false" {
		return "", nil
	}
	if !wanted {
		return "", nil
	}
	if o.mockMode {
		return "", nil // no OAuth → nothing to talk to
	}

	scopes := []string(nil)
	envTok := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" || os.Getenv("CLAUDE_GO_TOKEN") != ""
	if creds != nil {
		scopes = creds.Scopes
	}
	policyDenied := false
	deps := browsermcp.GateDeps{
		Provider: string(api.DetectProvider()),
		BaseURL:  os.Getenv("ANTHROPIC_BASE_URL"),
		OAuthToken: func() string {
			if creds == nil || creds.IsAPIKey {
				return ""
			}
			if creds.RefreshToken != "" {
				_ = creds.Refresh()
			}
			t, err := creds.Token()
			if err != nil {
				return ""
			}
			return t
		},
	}
	res := browsermcp.Gate(deps)
	policyDenied = res.Denied
	warn := func(s string) { fmt.Fprintln(os.Stderr, s) }
	if policyDenied && (chrome != nil && *chrome || cfc == "true") {
		// lp("allow_claude_browser_extension","Claude in Chrome","is") ?? fallback
		warn("Warning: Claude in Chrome is disabled by your organization")
	}
	in := browsermcp.EnabledInput{
		ChromeFlag:        chrome,
		SafeMode:          os.Getenv("CLAUDE_GO_SAFE_MODE") == "1" || os.Getenv("CLAUDE_GO_RESTRICTED") == "1",
		OAuthScopes:       scopes,
		EnvTokenSession:   envTok,
		PolicyDenied:      policyDenied,
		DefaultEnabledSet: defSet,
		DefaultEnabled:    defEnabled,
		Warn:              warn,
	}
	if !browsermcp.Enabled(in) {
		return "", nil
	}
	mcpCfg, _, prompt := browsermcp.SessionSetup(nil)
	if raw, ok := mcpCfg[browsermcp.ServerName].(map[string]any); ok {
		cmd, _ := raw["command"].(string)
		cfg := mcp.ServerConfig{Type: "stdio", Command: cmd,
			Args: []string{browsermcp.FlagClaudeInChromeMCP}}
		if env, ok := raw["env"].(map[string]any); ok && len(env) > 0 {
			cfg.Env = map[string]string{}
			for k, v := range env {
				cfg.Env[k], _ = v.(string)
			}
		}
		cfgs = map[string]mcp.ServerConfig{browsermcp.ServerName: cfg}
	}
	return prompt, cfgs
}
