// claude-go: a Go reimplementation of the Claude Code CLI.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gbasin/claude-go/internal/agent"
	"github.com/gbasin/claude-go/internal/api"
	"github.com/gbasin/claude-go/internal/auth"
	"github.com/gbasin/claude-go/internal/bg"
	"github.com/gbasin/claude-go/internal/bridge"
	"github.com/gbasin/claude-go/internal/browsermcp"
	"github.com/gbasin/claude-go/internal/config"
	"github.com/gbasin/claude-go/internal/hooks"
	"github.com/gbasin/claude-go/internal/mcp"
	"github.com/gbasin/claude-go/internal/mock"
	"github.com/gbasin/claude-go/internal/perms"
	"github.com/gbasin/claude-go/internal/plugins"
	"github.com/gbasin/claude-go/internal/repl"
	"github.com/gbasin/claude-go/internal/tools"

	"github.com/gbasin/claude-go/internal/session"
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
  --model <model>               model (default claude-opus-5-5)
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
  --allow-dangerously-skip-permissions  enable bypass mode availability
  --enable-auto-mode            start in auto permission mode
  --plan-mode-required          start in plan mode
  --permission-prompt-tool <t>  (accepted)
  --inherit-permission-mode <m> (accepted)
  --append-system-prompt <s>    append to the system prompt
  --append-system-prompt-file <f>  append system prompt from file
  --system-prompt <s>           extra system prompt text
  --system-prompt-file <f>      extra system prompt from file
  --system-prompt-snapshot <s>  (accepted)
  --append-subagent-system-prompt(-file)  append to every subagent system prompt
  --plan-mode-instructions <s>  custom plan-mode workflow body
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
  --resume-drops-turn <uuid>    with --resume-session-at: declare the
                                discarded tail belongs to that turn (refused otherwise)
  --fork-session                fork when resuming
  --no-session-persistence      don't write a transcript
  --rewind-files <uuid>         with -r: restore tracked files to the
                                state at that user message, then exit
  -w, --worktree [name]         create .claude/worktrees/<name> and cd into it
  --project <dir>               run in a different project directory
  --from-pr <ref>               (accepted)
  --tmux[=classic]              run the session in a detached tmux session for the worktree (requires -w)
  --ide, --desktop              IDE integration hints (bridge auto-connects via ~/.claude/ide/*.lock)
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
  --bg, --background            run this session as a background agent
                                (claude attach <id> rejoins, claude agents lists)
  --attach-serve <target>       (accepted)
  --init                        run Setup hooks (init trigger), then continue
  --maintenance                 run Setup hooks (maintenance trigger), then continue
  --init-only                   run Setup + SessionStart:startup hooks, then exit
  --autocompact <auto|tokens>   auto-compact window (auto, or 100k-1M)
  --advisor <model>             server-side advisor model (alias or full ID)
  --task-budget <tokens>        API-side token budget (output_config.task_budget)
  --proactivity <level>         (accepted)
  --prompt-suggestions [mode]   emit prompt_suggestion lines after each turn (print/stream-json)
  --prefill <text>              seed the input buffer without submitting
  --prefill-b64 <b64>           base64url --prefill (deep-link safe)
  --teammate-mode <mode>        (accepted)
  --messaging-socket-path <p>   peer-messaging inbox socket
  --team-name, --agent-id, --agent-name, --agent-color, --agent-type <v>
                                teammate identity fields (accepted)
  --parent-session-id <id>      (accepted)
  --remote-control-session-name-prefix <s>  (accepted)
  --deep-link-* <v>             deep link params (accepted)
  --handle-uri <uri>            (accepted)
  --correlation-id <id>         output_config correlation id
  --watch-artifact*, --ref, --on-branch, --routine <v>
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
  agents                        list agent types + background sessions
  attach <id>                   attach to a running background session (^Z detaches)
  daemon run|start|status|stop  background-session supervisor (respawns dead workers)
                                install|uninstall register as a service (launchd/systemd)
  stop <id>                     stop a running background session
  kill <id>                     kill a running background session
  logs <id>                     stream a background session's output
  rm <id>                       remove a stopped background session's state
  respawn <id>                  respawn a dead background worker
  auth login|status|logout      credential management
  config get|set|list           settings.json
  doctor                        diagnostics
  extract                       harvest real claude's prompts/schemas from a local install
  import <file>                 import a transcript into this project
  install, update               rebuild from source (git pull + go build)
  login                         OAuth login
  mcp list|add|add-json|add-from-claude-desktop|get|remove
      login|logout|reset-project-choices|serve  manage MCP servers / run claude-go as one
  plugin list|install|remove|marketplace
  purge [--dry-run] [--all]     delete local project state
  sandbox trust                 trust the sandbox MITM CA (macOS keychain)
  setup-token                   store a long-lived token
  remote-control, rc            Remote Control server (control-plane skeleton)
  bg-host                       (internal) background-session pty host
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
		case "extract":
			extractSub(args[1:])
			return
		case "config":
			configSub(args[1:])
			return
		case "update", "install":
			if os.Getenv("DISABLE_AUTOUPDATER") == "" {
				selfUpdate()
			}
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
		case "attach":
			attachSub(args[1:])
			return
		case "stop", "kill": // real: kill is an alias of stop
			stopSub(args[1:])
			return
		case "logs":
			logsSub(args[1:])
			return
		case "respawn":
			respawnSub(args[1:])
			return
		case "rm":
			rmSub(args[1:])
			return
		case "daemon":
			daemonSub(args[1:])
			return
		case "bg-host":
			bgHostSub(args[1:])
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
	// --handle-uri <uri>: real's deep-link entry — must be the last pair
	if uri := handleURIArgCheck(os.Args[1:]); uri != "" {
		dl, err := parseDeepLink(uri)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Deep link error:", err)
			os.Exit(1)
		}
		switch dl.action {
		case "open":
			// repo → resolve local clone else home; cwd overrides
			if dl.cwd != "" {
				if err := os.Chdir(dl.cwd); err != nil {
					fmt.Fprintln(os.Stderr, "Deep link error:", err)
					os.Exit(1)
				}
			}
			if dl.query != "" {
				o.prompt = []string{dl.query} // prefill the prompt
			}
		case "install-plugin":
			fmt.Printf("installing plugin %s", dl.plugin)
			if dl.marketplace != "" {
				fmt.Printf(" from %s", dl.marketplace)
			}
			fmt.Println()
			pluginSub([]string{"install", dl.plugin})
			return
		}
	}

	// --autocompact <auto|tokens>: real's argParser — 'auto' or 100k–1M
	if o.autoCompact != "" && o.autoCompact != "auto" {
		if n := agent.ParseWindow(o.autoCompact); n < 100_000 || n > 1_000_000 {
			fmt.Fprintf(os.Stderr,
				"Error: --autocompact must be 'auto', or between 100k and 1M (e.g. 500k, 200000)\n")
			os.Exit(1)
		}
	}

	// env wiring that must precede client construction
	if o.sdkURL != "" {
		os.Setenv("ANTHROPIC_BASE_URL", o.sdkURL)
	}
	// --betas flag + ANTHROPIC_BETAS env both append to the beta header
	// (mod_0137.js VN()); env entries get the afk-mode filter.
	if extra := o.betas; extra != "" {
		if eb := envBetas(); eb != "" {
			extra += "," + eb
		}
		applyBetaFlags(extra)
	} else if eb := envBetas(); eb != "" {
		applyBetaFlags(eb)
	}
	if o.hookEvents {
		os.Setenv("CLAUDE_GO_INCLUDE_HOOK_EVENTS", "1")
	}
	// CLAUDE_CODE_SAFE_MODE is the env equivalent of --safe-mode
	// (mod_0029.js Pr(): env || flag).
	if envOn("CLAUDE_CODE_SAFE_MODE") {
		os.Setenv("CLAUDE_GO_SAFE_MODE", "1")
	}
	if o.allowBypass {
		os.Setenv("CLAUDE_GO_ALLOW_BYPASS_PERMISSIONS", "1")
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
		if len(o.sessID) > 64 {
			fatal("Error: --session-id is too long")
		}
		sessionID = o.sessID
	}
	if o.bgMode {
		// real's --bg (v2.1.198+): start the session as a background agent
		// and return — prints the id + management commands
		opts := bg.SpawnOpts{
			SessionID: sessionID,
			CWD:       cwd,
			Prompt:    strings.Join(o.prompt, " "),
		}
		if o.resume != "" && o.resume != "-" {
			opts.ResumeFrom = o.resume // --bg -r <id> backgrounds an existing session
		}
		if _, err := bg.Spawn(opts); err != nil {
			fmt.Fprintln(os.Stderr, "--bg:", err)
			os.Exit(1)
		}
		// real stamps continued-in on the source transcript when its work
		// hands off to a different session id (TRe/Jns) — the picker then
		// hides the superseded file
		if o.resume != "" && o.resume != "-" {
			if strings.HasSuffix(o.resume, ".jsonl") {
				_ = session.AppendContinuedIn(o.resume,
					strings.TrimSuffix(filepath.Base(o.resume), ".jsonl"), sessionID)
			} else if src := session.ResolveArg(cwd, o.resume); src != "" && src != sessionID {
				_ = session.LogContinuedInTo(cwd, src, sessionID)
			}
		}
		fmt.Printf("background session %s started\n", sessionID)
		fmt.Printf("  attach:   claude attach %s\n", sid8(sessionID))
		fmt.Println("  monitor:  claude agents")
		os.Exit(0)
	}
	if o.tmux != "" {
		// real's --tmux requires --worktree and a tmux/iTerm2 backend
		// (classic = traditional tmux session for the worktree)
		if !o.worktreeSet {
			fmt.Fprintln(os.Stderr, "Error: --tmux requires --worktree")
			os.Exit(1)
		}
		if runtime.GOOS == "windows" {
			fmt.Fprintln(os.Stderr, "Error: --tmux is not supported on Windows")
			os.Exit(1)
		}
	}
	var worktreeState map[string]any
	if o.worktreeSet {
		var wc string
		wc, worktreeState = setupWorktree(cwd, o.worktreeName, sessionID)
		cwd = wc
		os.Chdir(cwd)
		if o.tmux != "" {
			setupTmuxSession(cwd, o.worktreeName, sessionID)
			if worktreeState != nil {
				if tn := os.Getenv("CLAUDE_CODE_TMUX_SESSION"); tn != "" {
					worktreeState["tmuxSessionName"] = tn
				}
			}
		}
	}

	migrateClaudeJSON()
	go checkForUpdate()
	maybeOnboard(!o.printMode)
	if o.initOnly {
		// real's --init-only: Setup hooks (init trigger) +
		// SessionStart:startup, then exit (mod_0659)
		hc := hooks.Load(cwd)
		hooks.Run(hc, hooks.Setup, map[string]any{
			"session_id": sessionID, "cwd": cwd,
			"trigger": "init", "hook_event_name": "Setup",
		}, cwd)
		hooks.Run(hc, hooks.SessionStart, map[string]any{
			"session_id": sessionID, "cwd": cwd,
			"source": "startup", "hook_event_name": "SessionStart",
		}, cwd)
		fmt.Println("claude-go initialized")
		return
	}
	// --init / --maintenance: Setup hooks with the named trigger, then
	// continue normally (mod_0659 — init continues to the REPL)
	if o.initHook != "" {
		hooks.Run(hooks.Load(cwd), hooks.Setup, map[string]any{
			"session_id": sessionID, "cwd": cwd,
			"trigger": o.initHook, "hook_event_name": "Setup",
		}, cwd)
	}
	for _, s := range o.stubs {
		fmt.Fprintf(os.Stderr, "note: %s accepted but not supported in claude-go\n", s)
	}
	if o.fromPRFlag {
		if o.fromPR != "" {
			// real resolves a session linked to the PR (number/URL); ours
			// scans local transcripts for the PR link
			if id := sessionForPR(cwd, o.fromPR); id != "" {
				o.resume = id
			} else {
				fmt.Fprintf(os.Stderr, "no session linked to PR %s\n", o.fromPR)
			}
		} else if id := repl.PickSession(cwd); id != "" {
			o.resume = id
		}
	}

	// Resolve the resume target BEFORE sessionID-dependent wiring (client,
	// store, checkpoints). Real continues the SAME transcript on -c/-r —
	// new turns append to the resumed session's file, parentUuid-chained
	// to the loaded leaf. Only --fork-session keeps the fresh id.
	resumeID := ""
	resumePath := "" // -r <file>.jsonl keeps the path for loading
	if o.cont || o.resume != "" {
		resumeID = o.resume
		if o.cont {
			if l2, err := session.Latest(cwd); err == nil {
				resumeID = l2
			}
		}
		if resumeID == "-" {
			if id2 := repl.PickSession(cwd); id2 != "" {
				resumeID = id2
			} else {
				return
			}
		}
		if strings.HasSuffix(resumeID, ".jsonl") {
			resumePath = resumeID
			resumeID = strings.TrimSuffix(filepath.Base(resumeID), ".jsonl")
		}
		// real resolves custom/ai titles on the session arg
		// (exact, then case-insensitive substring) before id lookup fails
		if resumeID != "" && resumePath == "" {
			if r := session.ResolveArg(cwd, resumeID); r != "" {
				resumeID = r
			}
		}
		if !o.forkSess && resumeID != "" {
			sessionID = resumeID
		}
		// real: "Can't open — this session is running in another terminal"
		// — a live bg worker owns the transcript; a second writer would
		// interleave records. --fork-session is the sanctioned bypass, and
		// a bg host's inner session resumes its own transcript (daemon
		// respawns mark the job live before the child starts).
		if resumeID != "" && !o.forkSess && os.Getenv("CLAUDE_CODE_SESSION_KIND") != "bg" {
			if j, _ := bg.Lookup(resumeID); j != nil && j.Alive() {
				fmt.Fprintf(os.Stderr,
					"Can't open — this session is running in another terminal (claude attach %s)\n", j.Short)
				os.Exit(1)
			}
		}
	}
	// real's flag wiring: these only make sense attached to a resume
	if o.resumeAt != "" && resumeID == "" {
		fmt.Fprintln(os.Stderr, "Error: --resume-session-at requires --resume")
		os.Exit(1)
	}
	if o.dropsTurn != "" && o.resumeAt == "" {
		fmt.Fprintln(os.Stderr, "Error: --resume-drops-turn requires --resume-session-at")
		os.Exit(1)
	}
	if o.rewindFiles != "" && resumeID == "" {
		fmt.Fprintln(os.Stderr, "Error: --rewind-files requires --resume")
		os.Exit(1)
	}
	if o.rewindFiles != "" && strings.Join(o.prompt, " ") != "" {
		fmt.Fprintln(os.Stderr, "Error: --rewind-files is a standalone operation and cannot be used with a prompt")
		os.Exit(1)
	}
	tools.Checkpoints.Init(sessionID) // file-history backups cover tool
	// writes in both print and interactive modes (real snapshots every
	// Write); was interactive-only — -p writes left no rewind trail

	if o.rewindFiles != "" {
		// standalone: restore tracked files to the state at that user
		// message, then exit — real never enters the session
		restored, removed, err := session.RewindFiles(cwd, resumeID, o.rewindFiles)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %s\n", err)
			os.Exit(1)
		}
		fmt.Printf("Files rewound to state at message %s (%d restored", o.rewindFiles, restored)
		if removed > 0 {
			fmt.Printf(", %d removed", removed)
		}
		fmt.Println(")")
		return
	}

	// --forward-home-settings without a cloud/env target is rejected
	// (real: "pass --cloud (a new session, or one to attach to) or
	// --environment")
	if os.Getenv("CLAUDE_GO_FORWARD_HOME_SETTINGS") != "" &&
		!o.cloud && !o.teleport && os.Getenv("CLAUDE_GO_ENVIRONMENT") == "" {
		fmt.Fprintln(os.Stderr, "Error: --forward-home-settings says whether this machine's settings go into a cloud session; pass --cloud (a new session, or one to attach to) or --environment")
		os.Exit(2)
	}

	// --plugin-url <url>: ad-hoc marketplace for the session (real
	// registers it like `plugin marketplace add`)
	if u := os.Getenv("CLAUDE_GO_PLUGIN_URL"); u != "" {
		if name, err := plugins.AddMarketplace(u); err != nil {
			fmt.Fprintf(os.Stderr, "plugin-url: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "  marketplace %s registered\n", name)
		}
	}

	// --attach-serve <session-id>: run this process as the session's
	// attach-serving worker (real's spawn entrypoint)
	if sid := os.Getenv("CLAUDE_GO_ATTACH_SERVE"); sid != "" {
		os.Exit(bg.RunHost(bg.HostOpts{SessionID: sid, CWD: cwd}))
	}

	agentCleanup := installCLIAgents(o.agentsJSON, cwd)
	if o.agentsJSON != "" {
		os.Setenv("CLAUDE_GO_AGENTS_JSON", o.agentsJSON)
	}
	defer agentCleanup() // note: os.Exit below skips defers; print mode calls it explicitly

	client, cleanup := buildClient(o.mockMode, sessionID)
	defer cleanup()
	pruneOldSessions(cleanupPeriod())

	// real's entrypoint is known at launch: interactive runs report
	// "cli", -p reports "sdk-cli". Set it before any aux goroutine reads
	// it (bootstrap query, code-sessions gate, beta list).
	if !o.printMode {
		client.Entrypoint = api.EntryCLI
	}

	// GB flag eval — real fires POST /api/eval/sdk-<key> near startup;
	// the feature map merges into cachedGrowthBookFeatures. Success/fail
	// selects real's flag-default header+beta set on later requests.
	if !o.mockMode {
		go func() {
			feats, err := client.EvalCheck(context.Background(), "")
			// any failed eval — transport error or empty features —
			// switches real to the flag-unavailable wire set (captured:
			// dangerous-tool-use + afk-mode + safeguards all drop out)
			client.MarkEvalFailed(err != nil || feats == nil || len(feats) == 0)
			api.CloseEvalDone()
		}()
		// real's startup burst around the first /v1/messages call: aux
		// GETs (settings/bootstrap/skills/plugins/mcp_servers), a haiku
		// quota probe, then the event_logging batch — all non-fatal.
		// Skipped in mock mode so e2e capture stays clean.
		auxModel := startupModel(cwd, o.model) // main thread: resolveModel sets wants1m
		auxDone = make(chan struct{})
		go func() {
			defer close(auxDone)
			bg := context.Background()
			client.StartupTraffic(bg, auxModel)
			client.QuotaProbe(bg, auxModel)
			// real's event batch lands after MCP results settle
			time.Sleep(1200 * time.Millisecond)
			client.LogSessionStart(bg, auxModel, !o.printMode, startupFacts(cwd, o))
		}()
	}

	// connect MCP servers in background (like CC's async init)
	mcp.SetSessionID(sessionID)
	chromeSysP, chromeMCP := chromeSessionSetup(o, lastCreds)
	mcpClients := startMCP(cwd, o.mcpConfig, o.strictMCP, chromeMCP, o.addDirs, sessionID)
	defer func() {
		for _, c := range mcpClients {
			c.Close()
		}
	}()
	// mcp_tool hooks dispatch onto the live client set (reuse the open
	// connection); unknown servers connect on demand for the hook call.
	hooks.MCPHookCaller = func(server, tool string, args json.RawMessage, cwd2 string) (string, error) {
		for _, c := range mcpClients {
			if c.Name == server {
				out, _, err := c.CallTool(context.Background(), tool, args)
				return out, err
			}
		}
		cfg, ok := mcp.LoadConfigs(cwd2)[server]
		if !ok {
			return "", fmt.Errorf("mcp server %q not configured", server)
		}
		c, err := mcp.Connect(server, cfg)
		if err != nil {
			return "", err
		}
		defer c.Close()
		out, _, err := c.CallTool(context.Background(), tool, args)
		return out, err
	}

	if o.bareMode {
		os.Setenv("CLAUDE_CODE_SIMPLE", "1")
	}
	var promptAtt string
	loop := agent.NewLoop(client, cwd)
	// --agent-id/--agent-name/--team-name must all be present or none
	// (real errors otherwise — mod_0659.js)
	aid, aname, tname := os.Getenv("CLAUDE_GO_AGENT_ID"), os.Getenv("CLAUDE_GO_AGENT_NAME"), os.Getenv("CLAUDE_GO_TEAM_NAME")
	if (aid != "") != (aname != "") || (aname != "") != (tname != "") {
		fmt.Fprintln(os.Stderr, "Error: --agent-id, --agent-name, and --team-name must all be provided together")
		os.Exit(1)
	}
	if aid != "" {
		loop.TeammateCtx = &agent.TeammateContext{
			AgentID: aid, AgentName: aname, TeamName: tname,
			Color: os.Getenv("CLAUDE_GO_AGENT_COLOR"),
		}
	}
	// --parent-session-id marks spawn lineage on the init record
	loop.ParentSessionID = os.Getenv("CLAUDE_GO_PARENT_SESSION_ID")
	// --system-prompt-snapshot on|off — on (default): the rendered sys
	// prompt is recorded on the first request and reused verbatim on every
	// request + resume, until compaction
	loop.SysPromptSnapshotOff = o.sysPromptSnapshotOff
	if resumeID != "" && !o.sysPromptSnapshotOff {
		if snap := session.InitSysPromptSnapshot(cwd, resumeID); snap != "" {
			loop.RestoreSysPromptSnapshot(snap)
		}
	}
	// --proactivity gates proactive suggestions (real: proactivityLevel)
	if v := os.Getenv("CLAUDE_GO_PROACTIVITY"); v != "" && v != "auto" {
		loop.ProactivityOff = v == "off" || v == "0"
	}
	// --project-config-root overrides where .claude/ resolves for this session
	if r := os.Getenv("CLAUDE_GO_PROJECT_CONFIG_ROOT"); r != "" {
		config.SetProjectConfigRoot(r)
	}

	// --prefill / --prefill-b64: assistant continuation appended after the
	// first user message (real's prefill / continuation)
	if pf := os.Getenv("CLAUDE_GO_PREFILL"); pf != "" {
		loop.Prefill = pf
	} else if pb := os.Getenv("CLAUDE_GO_PREFILL_B64"); pb != "" {
		if dec, err := base64.StdEncoding.DecodeString(pb); err == nil {
			loop.Prefill = string(dec)
		}
	}
	// --json-schema → output_config.format on the wire
	loop.OutputSchema = o.jsonSchema
	if m := os.Getenv("CLAUDE_GO_ADVISOR"); m != "" {
		loop.AdvisorModel = m // --advisor <model>
	}
	// elicitation/create: hook wiring needs the loop's hook config — wrap
	// whatever main's default Elicit resolved (HandleElicit form/answer-seam)
	for _, c := range mcpClients {
		base := c.Elicit
		serverName := c.Name
		c.Elicit = func(raw json.RawMessage) map[string]any {
			var params map[string]any
			json.Unmarshal(raw, &params)
			msg, _ := params["message"].(string)
			hooks.Run(loop.Hooks, hooks.Elicitation, map[string]any{
				"session_id": sessionID, "cwd": cwd,
				"hook_event_name": "Elicitation",
				"mcp_server_name": serverName, "message": msg,
			}, cwd)
			res := map[string]any{"action": "decline"}
			if base != nil {
				res = base(raw)
			}
			hooks.Run(loop.Hooks, hooks.ElicitationResult, map[string]any{
				"session_id": sessionID, "cwd": cwd,
				"hook_event_name": "ElicitationResult",
				"mcp_server_name": serverName, "action": res["action"],
				"content": res["content"],
			}, cwd)
			return res
		}
	}
	loop.Entrypoint = "sdk-cli"
	if o.model != "" {
		loop.Model = resolveModel(o.model)
	}
	loop.Wants1m = wants1m
	loop.MaxTurns = o.maxTurns
	if !o.maxTurnsSet {
		// CLAUDE_CODE_MAX_TURNS (real XEs(), mod_0029.js): flag wins; the env
		// is a positive-integer override — malformed values are a hard error.
		if t := strings.TrimSpace(os.Getenv("CLAUDE_CODE_MAX_TURNS")); t != "" {
			if n, err := strconv.Atoi(t); err == nil && n > 0 {
				loop.MaxTurns = n
			} else {
				fatal(`CLAUDE_CODE_MAX_TURNS must be a positive integer; got %q`, t)
			}
		}
	}
	loop.PermMode = o.permMode
	// CLAUDE_CODE_PLAN_MODE_REQUIRED is the env equivalent of
	// --plan-mode-required (mod_0124.js c7t() reads it as a M.bool).
	if envOn("CLAUDE_CODE_PLAN_MODE_REQUIRED") {
		loop.PermMode = "plan"
		o.permModeSet = true // suppresses the settings.defaultMode override below
	}
	loop.AppendSysP = o.appendSys
	loop.ToolCtx.Session = sessionID
	email, acct, _ := config.Account()
	dev := config.UserID()
	loop.User = agent.UserConfig{Email: email, AccountUUID: acct, DeviceID: dev}
	// real's first request waits on the GB client init — gate ours the
	// same bounded way so flag-gated wire surface is decided, not racy
	if !o.mockMode {
		loop.EvalWait = client.EvalAwait
	}
	loop.AddDirs = o.addDirs
	if o.sysPromptF != "" {
		// real fatales on a missing --system-prompt-file (mod_0706)
		b, err := os.ReadFile(o.sysPromptF)
		if err != nil {
			fatal("Error: System prompt file not found: %s", o.sysPromptF)
		}
		o.sysPrompt = string(b)
	}
	if o.sysPrompt != "" {
		loop.AppendSysP = o.sysPrompt
	}
	if chromeSysP != "" && !o.printMode {
		// real splices the chrome/browser block into the sys[3] session
		// block for interactive cli only — -p requests lack it
		// (captured: absent from real's -p sys[3] despite chrome tools
		// being registered deferred)
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
	// 294: stream-json output gets thinking display "updates" even on
	// the sdk-cli entrypoint (verified: -p --output-format stream-json)
	if loop.ThinkingDisplay == "" && o.outFormat == "stream-json" {
		loop.ThinkingDisplay = "updates"
	}
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
	if o.autoCompact != "" {
		loop.AutoCompactWindow = o.autoCompact // --autocompact <auto|tokens>
	}
	loop.CorrelationID = o.correlationID // --correlation-id (server-side telemetry)
	loop.TaskBudget = o.taskBudget       // --task-budget → output_config.task_budget
	loop.AdvisorModel = st.AdvisorModel
	if o.advisor != "" {
		loop.AdvisorModel = o.advisor // --advisor <model> (alias or full ID)
	}
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
	// --add-dir does the same from the command line
	for _, d := range o.addDirs {
		if !filepath.IsAbs(d) {
			d = filepath.Join(cwd, d)
		}
		d = filepath.Clean(d)
		tools.ExtraAllowedDirs = append(tools.ExtraAllowedDirs, d)
		perms.AdditionalDirs = append(perms.AdditionalDirs, d)
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
		// real's --tools grammar: "" disables all, "default"/"all" keeps
		// the full set; names select; a rule-shaped entry (Bash(git *))
		// withholds the whole tool with a warning, and a detached spec
		// ("Bash git*") warns that the arg was read as a rule.
		if o.toolsList == `""` {
			loop.AllowedTools = []string{}
		} else if o.toolsList == "default" || o.toolsList == "all" {
			// explicit default/all — nil (unrestricted) is the full set
		} else {
			var allowed []string
			for _, t := range strings.Split(o.toolsList, ",") {
				t = strings.TrimSpace(t)
				if t == "" {
					continue
				}
				if i := strings.Index(t, "("); i >= 0 && strings.HasSuffix(t, ")") {
					fmt.Fprintf(os.Stderr,
						"--tools entry %q names a rule, not a tool; the whole tool is withheld. "+
							"Write %q to say so.\n", t, t[:i])
					allowed = append(allowed, t[:i])
					continue
				}
				allowed = append(allowed, t)
			}
			loop.AllowedTools = allowed
		}
		loop.ToolCtx.AllowedTools = loop.AllowedTools
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
			var err error
			sb, err = os.ReadFile(o.settingsF)
			if err != nil {
				fatal("Error: Settings file not found: %s", o.settingsF)
			}
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
	// InstructionsLoaded fires once per loaded memory/CLAUDE.md file at
	// startup (real: load_reason + file_path per source)
	fireInstructionsLoaded(loop, cwd)
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
	// TeammateIdle hook fires when an admitted peer idle/exited notice
	// lands (real's per-teammate idle hook)
	tools.SetPeerIdleHook(func(from string) {
		loop.RunHookEvent(hooks.TeammateIdle, map[string]any{
			"teammate_name": from, "team_name": "",
		})
	})
	tools.RegisterSession(sessionID, cwd, "")
	// interactive cli only — real's -p wire shows no title/walk-away
	// calls (verified against captures)
	// session transcript — opened before OnTurnEnd so the ai-title write
	// resolves it
	var st2 *session.Store
	if !o.noPersist {
		// real sweeps transcripts past cleanupPeriodDays at session open
		session.SweepRetention(cwd)
		st2, _ = session.Open(cwd, sessionID)
	}
	if !o.mockMode && !o.printMode {
		var titled atomic.Bool
		loop.Events.OnTurnEnd = func(ask string, toolCalls int, tail string, elapsed time.Duration) {
			go func() {
				bg := context.Background()
				ep := string(loop.Client.Entrypoint)
				if !titled.Swap(true) && ask != "" {
					if t := client.TitleCall(bg, agent.AuxBillingLine(ep, ask, ask), ask); t != "" && st2 != nil {
						// real's saveAiGeneratedTitle writes the ai-title
						// record (mod_0409 fle)
						_ = st2.LogAITitle(t)
					}
				}
				state := fmt.Sprintf("Current state: working (for %dm)\nTool calls so far: %s\nUser's most recent ask: %q\n\nAssistant message tail (last 30 chars):\n%s",
					int(elapsed.Minutes()), auxToolCallCount(toolCalls), ask, last30(tail))
				_ = client.WalkAwayCall(bg, agent.AuxBillingLine(ep, state, state), state)
			}()
		}
	}
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
	// Real precedence (mod_0198.js): flag > ANTHROPIC_MODEL > settings.model.
	if st.Model != "" && o.model == "" && os.Getenv("ANTHROPIC_MODEL") == "" {
		loop.Model = resolveModel(st.Model)
		loop.Wants1m = wants1m
	}
	// ANTHROPIC_DEFAULT_MODEL applies when nothing else named a model —
	// flag, env pin, and settings all win over it (mv() in mod_0137.js).
	if o.model == "" && st.Model == "" && os.Getenv("ANTHROPIC_MODEL") == "" {
		if dm := defaultModelEnv(); dm != "" {
			loop.Model = resolveModel(dm)
			loop.Wants1m = wants1m
		}
	}

	if st2 != nil {
		defer st2.Close()
		// real's exit re-stamp (sJr / reAppendSessionMetadataAtExit) lands
		// the session-metadata rows ({type:"mode"},{type:"permission-mode"})
		// on the transcript tail — deferred so the final mode is written.
		// RunWithStore's SessionEnd path stamps too; StampMeta dedupes.
		defer func() { _ = st2.StampMeta("normal", loop.PermMode) }()
		loop.Transcript = st2.Path
		loop.OnSessionTitle = func(title string) {
			_ = st2.LogTitle(title)
		}
		if resumeID == "" || o.forkSess {
			st2.LogInitMeta(loop.Model, loop.ParentSessionID,
				os.Getenv("CLAUDE_GO_AGENT_TYPE"), loop.SysPromptSnapshot,
				o.permMode) // system/init — incl. perm mode for --inherit-permission-mode
			// resumed transcript already carries its own init record
		}
		// session-state stamps — real latches + flushes these at session
		// start (mod_0416 _2/mlr + planReAppendSessionMetadata):
		//   {type:"mode"}       — "coordinator"|"normal" (NOT the perm mode)
		//   {type:"permission-mode"} — the permission mode
		//   {type:"atis-latch"} — the atis latch; "" on standard sessions
		mode := "normal"
		if tools.IsCoordinatorMode() {
			mode = "coordinator"
		}
		_ = st2.StampMode(mode)
		// --inherit-permission-mode resolves BEFORE the materialize stamp —
		// the stamp itself writes last-wins permission-mode rows into the
		// resumed transcript, so reading after it would return our own write
		if os.Getenv("CLAUDE_GO_INHERIT_PERMISSION_MODE") != "" && !o.permModeSet && resumeID != "" {
			if m := session.InitMode(cwd, resumeID); m != "" {
				loop.PermMode = m
			}
		}
		_ = st2.StampPermMode(loop.PermMode)
		_ = st2.LogAtisLatch("")
		if tc := loop.TeammateCtx; tc != nil {
			// teammate sessions stamp their identity (saveAgentName/fAe,
			// saveAgentColor/XKt)
			_ = st2.LogAgentMeta("", tc.AgentName, "", "")
			_ = st2.LogAgentMeta("", "", tc.Color, "")
		}
		if o.agentName != "" {
			// --agent <type> → real's agent-setting latch (pnt)
			_ = st2.LogAgentMeta("", "", "", o.agentName)
		}
		// real's saveWorktreeState — stamped once when the session entered
		// the worktree (AM); resume dedupes an identical tail row, so a
		// same-state re-stamp would be a no-op anyway
		if worktreeState != nil {
			_ = st2.LogWorktreeState(worktreeState)
		}
		// real's file-history rows: a file-history-snapshot keyed to each
		// user message uuid (empty trackedFileBackups when nothing is
		// tracked yet) plus a file-history-delta per file first tracked
		// during that message's turn (pending first-tracks flush here).
		st2.OnUserPrompt = func(u string) {
			if os.Getenv("CLAUDE_CODE_DISABLE_FILE_CHECKPOINTING") != "" {
				return
			}
			_ = st2.LogFileHistorySnapshot(u, tools.Checkpoints.BackupMap(), false)
			for _, d := range tools.Checkpoints.TakePending() {
				_ = st2.LogFileHistoryDelta(u, u, d.Path, d.Wire())
			}
		}
		// hook system messages land as attachment rows
		// ({type:"attachment",attachment:{type:"hook_system_message",...}})
		loop.OnAttachment = func(att, extra map[string]any) {
			_ = st2.LogAttachmentMeta(att, extra)
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
		// pr-link: real writes the record when a gh PR URL lands in a
		// Bash result — the loop detects, we persist (mod_0409 q2s).
		// Bare: real's pr-link is a last-wins row, no uuid/parentUuid.
		loop.OnSessionRecord = func(rec map[string]any) { _ = st2.LogBare(rec) }
		// api-request* rows: real's request-replay recorder routes to the
		// transcript — only emitted under the elegant-meadow gate
		loop.OnRequestReplay = func(sh string, shape, request any, blobs []session.ReplayBlob) string {
			return st2.AppendReplayRecords(sh, shape, request, blobs)
		}
	}
	// querySource for api-request rows — real's repl_main_thread / sdk
	if o.printMode {
		loop.ReplaySource = "sdk"
	} else {
		loop.ReplaySource = "repl_main_thread"
	}

	if o.cont || o.resume != "" {
		loop.SessionStartSource = "resume"
		id := resumeID
		loadPath := resumePath
		if id != "" {
			var msgs []api.Message
			var leaf string
			var err error
			switch {
			case o.resumeAt != "" && loadPath != "":
				msgs, leaf, err = loadSessionAtPath(loadPath, o.resumeAt, o.dropsTurn)
			case o.resumeAt != "":
				msgs, leaf, err = loadSessionAt(cwd, id, o.resumeAt, o.dropsTurn)
			case loadPath != "":
				msgs, err = session.LoadPath(loadPath)
				leaf = session.LeafUUID(loadPath)
			default:
				msgs, err = session.Load(cwd, id)
				leaf = session.LeafUUID(filepath.Join(session.ProjectsDir(cwd), id+".jsonl"))
			}
			// --resume-drops-turn: drop the last N turns from the tail
			if raw := os.Getenv("CLAUDE_GO_RESUME_DROPS_TURN"); raw != "" {
				if n, nerr := strconv.Atoi(raw); nerr == nil && n > 0 && len(msgs) > 0 {
					// a "turn" = user message + following assistant/tool blocks —
					// walk back n user-boundaries
					dropFrom := len(msgs)
					for i := len(msgs) - 1; i >= 0 && n > 0; i-- {
						if msgs[i].Role == "user" {
							var blks []api.ContentBlock
							isTool := json.Unmarshal(msgs[i].Content, &blks) == nil &&
								len(blks) > 0 && blks[0].Type == "tool_result"
							if !isTool {
								dropFrom = i
								n--
							}
						}
					}
					msgs = msgs[:dropFrom]
				}
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "resume %s: %v\n", id, err)
			} else if len(msgs) > 0 {
				loop.Messages = msgs
				if os.Getenv("CLAUDE_GO_INHERIT_PERMISSION_MODE") != "" && !o.permModeSet {
					if m := session.InitMode(cwd, id); m != "" {
						loop.PermMode = m // --inherit-permission-mode
					}
				}
				// resumed history already carried the env composite on
				// the wire — real replays only the hook string
				loop.MarkEnvSent()
				if o.forkSess && st2 != nil {
					// real copies the session file to <newid>.jsonl with
					// per-record forkedFrom stamps — the fork keeps its
					// history so a later -r sees it (audit: high gap).
					src := loadPath
					if src == "" {
						src = filepath.Join(session.ProjectsDir(cwd), id+".jsonl")
					}
					if _, ferr := st2.ForkInto(src, id, leaf); ferr != nil {
						fmt.Fprintf(os.Stderr, "fork copy failed: %v\n", ferr)
						st2.LogRecord(map[string]any{
							"type": "system", "subtype": "fork",
							"forkedFrom": map[string]string{"sessionId": id},
						})
					} else {
						// real's TRe/Jns: the source transcript gets
						// {type:"continued-in",continuedInSessionId:<new>}
						// marking it superseded for the picker
						_ = session.StampContinuedIn(src, id, sessionID)
					}
					fmt.Fprintf(os.Stderr, "forked session %s → %s (%d messages)\n", id[:8], sessionID[:8], len(msgs))
				} else {
					if st2 != nil && leaf != "" {
						st2.SeedParent(leaf) // continue the branch —
						// the next record's parentUuid is the loaded leaf
					}
					fmt.Fprintf(os.Stderr, "resumed session %s (%d messages)\n", id, len(msgs))
					// awaySummaryEnabled (default on): recap when the last
					// message is 5+ minutes old — last assistant text
					// as the "where you left off" line
					if awaySummaryEnabled() {
						if fi, e := os.Stat(loadPathOr(cwd, id, loadPath)); e == nil &&
							time.Since(fi.ModTime()) > 5*time.Minute {
							if recap := lastAssistantLine(msgs); recap != "" {
								fmt.Fprintf(os.Stderr, "  %sSession recap:%s %s\n",
									"\x1b[38;5;246m", "\x1b[39m", recap)
							}
						}
					}
				}
			}
		}
	}

	if o.debug || o.verbose {
		// -d/--verbose enables all debug channels at once (per-channel
		// CLAUDE_GO_DEBUG_{KB,HTTP,REQ} still work individually)
		for _, ch := range []string{"", "_KB", "_HTTP", "_REQ"} {
			os.Setenv("CLAUDE_GO_DEBUG"+ch, "1")
		}
	}
	if o.debugFile != "" {
		if f, err := os.OpenFile(o.debugFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			os.Stderr = f
		}
	}
	// real's resume path: SessionStart hooks run in flight, and the
	// session waits up to CLAUDE_CODE_RESUME_SESSIONSTART_HOOKS_WAIT_MS
	// for them before continuing (default: run to completion inline)
	if o.cont || o.resume != "" {
		wait := 0
		if v := os.Getenv("CLAUDE_CODE_RESUME_SESSIONSTART_HOOKS_WAIT_MS"); v != "" {
			wait, _ = strconv.Atoi(v)
		}
		if wait > 0 {
			done := make(chan struct{})
			go func() { fireHook(loop, hooks.SessionStart); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Duration(wait) * time.Millisecond):
			}
		} else {
			fireHook(loop, hooks.SessionStart)
		}
	} else {
		fireHook(loop, hooks.SessionStart)
	}
	// session file materialized — real's reAppendSessionMetadata stamps
	// {type:"mode"} + {type:"permission-mode"} rows here (and on resume
	// adopt); PermMode is final by now (flag > inherit > settings)
	if st2 != nil {
		_ = st2.StampMeta("normal", loop.PermMode)
	}
	if o.printMode {
		tools.Interactive = false
		if o.inFormat == "stream-json" {
			runPrintStreamJSON(loop)
			return
		}
		// non-interactive: honor rules; unresolvable asks go to the
		// auto-mode security monitor (real's aux sonnet-5 eval)
		var lastAsk atomic.Bool
		loop.ToolCtx.PermissionAsk = func() bool { return lastAsk.Load() }
		loop.ToolCtx.Permission = func(name string, input json.RawMessage, _ string) bool {
			if name == "Bash" && tools.SandboxAutoAllow(cwd) {
				lastAsk.Store(false)
				return true // sandboxed bash auto-allowed per settings
			}
			d := perms.Check(perms.Mode(loop.PermMode), &perms.Rules{
				Allow: rules["allow"], Deny: rules["deny"], Ask: rules["ask"],
			}, name, input, loop.ToolCtx.CWD)
			lastAsk.Store(d == perms.Ask)
			return d == perms.Allow
		}
		if !o.mockMode {
			ep := string(loop.Client.Entrypoint)
			loop.SafetyEval = func(ctx context.Context, name string, input json.RawMessage) (int, string, error) {
				billing := agent.AuxBillingLine(ep, "The following is the user's CLAUDE.md configuration.", "")
				u, _ := user.Current()
				uname := os.Getenv("USER")
				if u != nil && uname == "" {
					uname = u.Username
				}
				sessCtx := fmt.Sprintf("- **User identity**: `%s`. The `$USER/...` pattern in the rules above resolves to `%s/...`. Branches whose first path segment is a different person's name (`<other-user>/...`) are NOT this user's personal branches.", uname, uname)
				tr := safetyTranscript(loop)
				md := agent.ClaudeMDContext(cwd)
				sev, cat, err := client.SafetyEvalCall(ctx, billing, sessCtx, md, tr)
				if err != nil || sev >= 50 {
					return sev, cat, err
				}
				// stage-2 confirmation eval on opus (captured: same shape,
				// fires whenever stage-1 engages)
				return client.SafetyEvalStage2(ctx, billing, sessCtx, md, tr)
			}
		}
		loop.MaxBudgetUSD = o.maxBudget
		code := runPrint(loop, strings.Join(o.prompt, " ")+promptAtt, o.outFormat, st2, o.jsonSchema, o.promptSuggestions)
		if o.maxBudget > 0 {
			fmt.Fprintf(os.Stderr, "note: --max-budget-usd %.2f enforced at turn end\n", o.maxBudget)
		}
		agentCleanup() // os.Exit skips the deferred call above
		// drain aux traffic briefly — real flushes its telemetry tail
		// before exiting print mode
		if auxDone != nil {
			select {
			case <-auxDone:
			case <-time.After(2 * time.Second):
			}
		}
		os.Exit(code)
	}
	tools.Interactive = true
	// entrypoint already set to cli before the aux goroutines (above)
	if !repl.TrustGate(cwd) {
		fmt.Println("not trusted — exiting")
		return
	}
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
	checkUpdateToast()
	repl.RunWithStore(loop, st2)
	if !loop.SessionEndFired {
		fireHook(loop, hooks.SessionEnd)
	}
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
		// real's stdin: model + agent_type + session_title
		payload["model"] = l.Model
		if t := os.Getenv("CLAUDE_GO_AGENT_TYPE"); t != "" {
			payload["agent_type"] = t
		}
		if t := session.ReadTitle(session.ProjectsDir(l.ToolCtx.CWD), l.Client.SessionID); t != "" {
			payload["session_title"] = t
		}
	case hooks.SessionEnd:
		payload["reason"] = l.SessionEndReason
	}
	res := hooks.Run(l.Hooks, ev, payload, l.ToolCtx.CWD)
	// SessionStart hook stdout/additionalContext injects into the session
	if ev == hooks.SessionStart {
		// real wires SessionStart context as a system message in the
		// messages array (merged into the env composite on request 1,
		// bare string after) — not into the system prompt
		if s := res.PlainText + res.AdditionalContext; s != "" {
			l.SessionHookCtx = strings.TrimSpace(s)
		}
		// watchPaths registers additional FileChanged watchers
		for _, p := range res.WatchPaths {
			l.WatchPaths = append(l.WatchPaths, p)
		}
		if res.SessionTitle != "" && l.OnSessionTitle != nil {
			l.OnSessionTitle(res.SessionTitle)
		}
		if res.InitialUserMessage != "" {
			l.InitialUserMessage = res.InitialUserMessage
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
func startMCP(cwd, configFile string, strict bool, dynamic map[string]mcp.ServerConfig, addDirs []string, sessionID string) (clients []*mcp.Client) {
	var cfgs map[string]mcp.ServerConfig
	if strict {
		// --strict-mcp-config: only the --mcp-config file's servers load
		cfgs = map[string]mcp.ServerConfig{}
	} else {
		cfgs = mcp.LoadConfigs(cwd)
	}
	if configFile != "" {
		b, err := os.ReadFile(configFile)
		if err != nil {
			fatal("Error: MCP config file not found: %s", configFile)
		}
		var top struct {
			McpServers map[string]mcp.ServerConfig `json:"mcpServers"`
		}
		if json.Unmarshal(b, &top) == nil {
			for n, c := range top.McpServers {
				cfgs[n] = c
			}
		}
	}
	if cfgs == nil {
		cfgs = map[string]mcp.ServerConfig{}
	}
	for name, c := range dynamic {
		if _, taken := cfgs[name]; !taken {
			cfgs[name] = c
		}
	}
	// claude-code-remote (mod_0116.js Uct): remote sessions get an
	// alwaysLoad http MCP server wired to the session's meta endpoint —
	// it carries the trigger/repo verbs (list_triggers, create_trigger,
	// update_trigger, delete_trigger, fire_trigger, send_later, add_repo,
	// check_repo_access, list_repos). The X-Session-UUID header binds the
	// calls to this session.
	if os.Getenv("CLAUDE_CODE_REMOTE") != "" {
		if _, taken := cfgs["claude-code-remote"]; !taken {
			base := os.Getenv("ANTHROPIC_BASE_URL")
			if base == "" {
				base = "https://api.anthropic.com"
			}
			// wss://sdk-url downgrades to https like real's jRe()
			base = strings.ReplaceAll(base, "wss://", "https://")
			base = strings.ReplaceAll(base, "ws://", "http://")
			cfgs["claude-code-remote"] = mcp.ServerConfig{
				Type: "http",
				URL:  strings.TrimRight(base, "/") + "/v2/ccr-sessions/-/meta/mcp",
				Headers: map[string]string{
					"X-MCP-Server-ID":   "bf7c680d-5fdc-5ef4-b4a0-abadb619bf0a",
					"X-Session-UUID":    sessionID,
					"anthropic-version": "2023-06-01",
				},
			}
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
	var wg sync.WaitGroup
	var mu sync.Mutex
	for name, cfg := range cfgs {
		if cfg.Command == "" && cfg.URL == "" {
			continue
		}
		// per-server enabled:false gates auto-connect (real's enable/
		// disable state — "enable before reconnect")
		if cfg.Enabled != nil && !*cfg.Enabled {
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
			// tools/list_changed: the client refreshes c.Tools; re-register
			// the new entries so they surface (real reloads in place)
			// prompts surface as /mcp__server__name slash commands (real
			// parity) — expand via prompts/get when invoked
			registerPrompts := func() {
				if ps, _, perr := c.ListPrompts(context.Background(), ""); perr == nil {
					for _, pr := range ps {
						pn := pr.Name
						tools.RegisterMCPPrompt(prefix, pn,
							func(ctx context.Context, args map[string]string) (string, error) {
								res, err := c.GetPrompt(ctx, pn, args)
								if err != nil {
									return "", err
								}
								var parts []string
								for _, m := range res.Messages {
									var pc struct {
										Type string `json:"type"`
										Text string `json:"text"`
									}
									if json.Unmarshal(m.Content, &pc) == nil && pc.Text != "" {
										parts = append(parts, pc.Text)
									}
								}
								return strings.Join(parts, "\n\n"), nil
							})
					}
				}
			}
			registerPrompts()
			c.OnPromptsChanged = registerPrompts // prompts/list_changed
			serverName := name
			c.Elicit = func(params json.RawMessage) map[string]any {
				return tools.HandleElicit(serverName, params)
			}
			c.SetNotificationHandler(func(method string, params json.RawMessage) {
				// notifications/message = server log lines — real shows
				// them as dim status notes above the prompt
				if method == "notifications/message" {
					var p struct {
						Level string `json:"level"`
						Data  string `json:"data"`
					}
					if json.Unmarshal(params, &p) == nil && p.Data != "" {
						fmt.Fprintf(os.Stderr, "  [mcp %s] %s: %s\n", name, p.Level, p.Data)
					}
				}
			})
			roots := append([]string{cwd}, addDirs...)
			c.Roots = func() []map[string]string {
				out := make([]map[string]string, 0, len(roots))
				for _, d := range roots {
					out = append(out, map[string]string{"uri": "file://" + d, "name": d})
				}
				return out
			}
			c.OnToolsChanged = func() {
				tools.UnregisterMCPServer(prefix)
				for _, t := range c.Tools {
					tools.RegisterMCP(prefix, c, t.Name, t.Description, t.InputSchema)
				}
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
	cl := api.NewClient(creds, sessionID, api.EntrySDKCLI)
	cl.ParentSessionID = os.Getenv("CLAUDE_GO_PARENT_SESSION_ID") // --parent-session-id
	return cl, func() {}
}

var lastCreds *auth.Credentials

// auxDone closes when startup aux traffic (GETs, quota probe, event
// batch) finishes — drained with a cap on exit so -p doesn't orphan
// real's telemetry tail.
var auxDone chan struct{}

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
	st, stErr := os.Stdin.Stat()
	if stErr == nil && st.Mode()&os.ModeCharDevice == 0 {
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

func runPrint(l *agent.Loop, prompt, format string, st *session.Store, schema string, promptSuggestions bool) int {
	var out strings.Builder
	sj := format == "stream-json"
	if sj {
		emitStreamJSONInit(l)
		emitInstructionSizeWarning(l)
		// real echoes the -p prompt as a user event after init
		if prompt != "" {
			ev, _ := json.Marshal(map[string]any{
				"type": "user", "uuid": session.NewID(),
				"parent_tool_use_id": nil,
				"message": map[string]any{
					"role": "user",
					"content": []map[string]any{{
						"type": "text", "text": prompt}}},
				"session_id": l.Client.SessionID,
			})
			fmt.Println(string(ev))
		}
	}
	if sj && os.Getenv("CLAUDE_GO_INCLUDE_HOOK_EVENTS") == "1" {
		l.OnHookEvent = func(rec map[string]any) {
			ev, _ := json.Marshal(rec)
			fmt.Println(string(ev))
		}
	}
	l.Events.OnTextDelta = func(t string) {
		switch format {
		case "text":
			fmt.Print(t)
		default:
			out.WriteString(t)
		}
	}
	if sj && flagPartial {
		// real's stream_event: the raw SSE frame verbatim + session_id /
		// parent_tool_use_id stamps (mod_0653 qVo)
		l.Events.OnStreamEvent = func(se *api.StreamEvent) {
			var raw json.RawMessage = se.Raw
			if len(raw) == 0 {
				raw, _ = json.Marshal(se)
			}
			ev, _ := json.Marshal(map[string]any{
				"type": "stream_event", "uuid": session.NewID(),
				"parent_tool_use_id": nil,
				"event":              json.RawMessage(raw),
				"session_id":         l.Client.SessionID,
			})
			fmt.Println(string(ev))
		}
	}
	turns := 0
	// stream-json assistant events (real 294): one per content block, each
	// carrying the FULL API message (id/model/usage/stop_reason); tool_use
	// ids are the API-assigned ones, never fabricated.
	if sj {
		l.Events.OnAssistantMessage = func(resp *api.Response) {
			for range resp.Content {
				ev, _ := json.Marshal(map[string]any{
					"type": "assistant", "uuid": session.NewID(),
					"parent_tool_use_id": nil,
					"message":            resp,
					"session_id":         l.Client.SessionID,
				})
				fmt.Println(string(ev))
			}
		}
	}
	l.OnToolUseSummary = func(summary string, ids []string) {
		if sj {
			ev, _ := json.Marshal(map[string]any{
				"type": "tool_use_summary", "summary": summary,
				"preceding_tool_use_ids": ids,
				"uuid":                   session.NewID(),
				"session_id":             l.Client.SessionID,
			})
			fmt.Println(string(ev))
		}
		if st != nil {
			st.LogRecord(map[string]any{
				"type": "tool_use_summary", "summary": summary,
				"preceding_tool_use_ids": ids,
			})
		}
	}
	l.ToolCtx.OnTaskEvent = func(rec map[string]any) {
		if sj {
			rec["type"] = "system"
			rec["uuid"] = session.NewID()
			rec["session_id"] = l.Client.SessionID
			ev, _ := json.Marshal(rec)
			fmt.Println(string(ev))
		}
	}
	// real's system/file_attachments_missing — a bridge-shape event
	// ({message_uuid, sent_count, missing:[{file_uuid,reason}]}); our
	// attachments are path markers so the missing entries carry the path
	l.Events.OnFileMissing = func(path string) {
		if sj {
			ev, _ := json.Marshal(map[string]any{
				"type": "system", "subtype": "file_attachments_missing",
				"sent_count": 1,
				"missing": []map[string]any{{
					"file_uuid": path, "reason": "file missing",
				}},
				"uuid": session.NewID(), "session_id": l.Client.SessionID,
			})
			fmt.Println(string(ev))
		}
	}
	l.Events.OnVCSChange = func(kind, branch string) {
		if sj {
			ev := map[string]any{
				"type": "system", "subtype": "vcs_state_changed",
				"kind": kind, "cwd": l.ToolCtx.CWD,
				"uuid": session.NewID(), "session_id": l.Client.SessionID,
			}
			if branch != "" {
				ev["branch"] = branch
			}
			b, _ := json.Marshal(ev)
			fmt.Println(string(b))
		}
	}
	l.Events.OnDevIntent = func(kind string) {
		if sj {
			ev, _ := json.Marshal(map[string]any{
				"type": "system", "subtype": "dev_intent",
				"kind": kind,
				"uuid": session.NewID(), "session_id": l.Client.SessionID,
			})
			fmt.Println(string(ev))
		}
	}
	l.Events.OnPerTurnEffort = func(active bool) {
		if sj {
			ev, _ := json.Marshal(map[string]any{
				"type": "system", "subtype": "per_turn_effort_changed",
				"per_turn_effort_active": active,
				"uuid":                   session.NewID(), "session_id": l.Client.SessionID,
			})
			fmt.Println(string(ev))
		}
	}
	l.Events.OnRateLimit = func(status string, info map[string]any) {
		if sj {
			info["status"] = status
			ev, _ := json.Marshal(map[string]any{
				"type": "rate_limit_event", "rate_limit_info": info,
				"uuid": session.NewID(), "session_id": l.Client.SessionID,
			})
			fmt.Println(string(ev))
		}
	}
	l.Events.OnToolProgress = func(name, id string, sec int, hb bool) {
		if sj {
			ev, _ := json.Marshal(map[string]any{
				"type": "tool_progress", "tool_use_id": id, "tool_name": name,
				"parent_tool_use_id":   nil,
				"elapsed_time_seconds": sec,
				"uuid":                 session.NewID(), "session_id": l.Client.SessionID,
				"heartbeat": hb,
			})
			fmt.Println(string(ev))
		}
	}
	l.Events.OnPermissionCheck = func(tuID, status string) {
		if sj {
			ev, _ := json.Marshal(map[string]any{
				"type": "system", "subtype": "permission_check_status",
				"tool_use_id": tuID, "status": status,
				"uuid": session.NewID(), "session_id": l.Client.SessionID,
			})
			fmt.Println(string(ev))
		}
	}
	l.Events.OnThinkingTokens = func(n int) {
		if sj {
			ev, _ := json.Marshal(map[string]any{
				"type": "system", "subtype": "thinking_tokens",
				"thinking_tokens": n,
				"uuid":            session.NewID(), "session_id": l.Client.SessionID,
			})
			fmt.Println(string(ev))
		}
	}
	l.Events.OnModelFallback = func(from, to string) {
		if sj {
			ev, _ := json.Marshal(map[string]any{
				"type": "system", "subtype": "model_fallback",
				"original_model": from, "fallback_model": to,
				"uuid": session.NewID(), "session_id": l.Client.SessionID,
			})
			fmt.Println(string(ev))
		}
	}
	// real emits system/compact_boundary on the stream when the loop
	// compacts mid-turn (mod_1695 boundary render)
	if sj {
		l.Events.OnCompact = func() {
			_, trig := l.CompactMeta()
			ev, _ := json.Marshal(map[string]any{
				"type": "system", "subtype": "compact_boundary",
				"uuid": session.NewID(), "session_id": l.Client.SessionID,
				"compact_metadata": map[string]any{
					"trigger": trig, "pre_tokens": l.LastInputTokens(),
				},
			})
			fmt.Println(string(ev))
		}
	}
	l.Events.OnToolResult = func(name, toolUseID string, tr tools.Result) {
		if sj {
			rec := map[string]any{
				"type": "user", "uuid": session.NewID(),
				"parent_tool_use_id": nil,
				"message": map[string]any{"role": "user", "content": []map[string]any{{
					"type": "tool_result", "tool_use_id": toolUseID,
					"content": tr.Content, "is_error": tr.IsError}}},
				"session_id": l.Client.SessionID,
			}
			// real's user event carries the tool's structured Output at
			// tool_use_result (mod_0069) — absent when the tool has none
			if tr.Structured != nil {
				rec["tool_use_result"] = tr.Structured
			}
			ev, _ := json.Marshal(rec)
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
		// cost-state: the ledger save that runs on graceful shutdown —
		// for -p the completed run is that boundary
		if rec := l.CostState(); rec != nil {
			_ = st.LogRawRecord(rec)
		}
	}
	if format == "text" {
		fmt.Println()
	}
	if err != nil {
		if sj {
			ev, _ := json.Marshal(map[string]any{
				"type": "result", "subtype": "error_during_execution",
				"is_error": true, "errors": []string{err.Error()},
				"session_id":      l.Client.SessionID,
				"duration_ms":     time.Since(start).Milliseconds(),
				"duration_api_ms": l.APIDuration().Milliseconds(),
				"num_turns":       turns, "uuid": session.NewID(),
			})
			fmt.Println(string(ev))
		} else {
			fmt.Fprintln(os.Stderr, "Error:", err)
		}
		return 1
	}
	if sj && promptSuggestions {
		// real's prompt_suggestion lane: aux haiku call predicts the next
		// user prompt; emitted as a line BEFORE the result frame
		tail := "User's most recent ask: " + prompt + "\n\nAssistant response:\n" + out.String()
		if s := l.Client.SuggestionCall(ctxb(), agent.AuxBillingLine(string(l.Client.Entrypoint), tail, tail), tail); strings.TrimSpace(s) != "" {
			ev, _ := json.Marshal(map[string]any{
				"type": "prompt_suggestion", "suggestion": strings.TrimSpace(s),
				"uuid": session.NewID(), "session_id": l.Client.SessionID,
			})
			fmt.Println(string(ev))
		}
	}
	switch format {
	case "json", "stream-json":
		if schema != "" {
			// real wraps the result in structured output when a schema is given
			var v any
			if json.Unmarshal([]byte(out.String()), &v) != nil {
				raw := out.String()
				out.Reset()
				b2, _ := json.Marshal(map[string]any{"result": raw})
				out.Write(b2)
			}
		}
		res := map[string]any{
			"type":            "result",
			"subtype":         "success",
			"is_error":        false,
			"result":          out.String(),
			"session_id":      l.Client.SessionID,
			"duration_ms":     time.Since(start).Milliseconds(),
			"duration_api_ms": l.APIDuration().Milliseconds(),
			"num_turns":       turns,
			// real's result enrichments (captured stream-json): request-
			// start + first-token latencies, terminal accounting, per-
			// session subagent stats, null-origin human turns
			"time_to_request_ms":     l.TimeToRequest().Milliseconds(),
			"ttft_ms":                l.TTFT().Milliseconds(),
			"ttft_stream_ms":         l.TTFT().Milliseconds(),
			"first_content_frame_ms": l.TTFT().Milliseconds(),
			"queued_turn_count":      0,
			"result_index":           0,
			"safety_stops":           0,
			"terminal_reason":        "completed",
			"api_error_status":       nil,
			"origin":                 nil,
			"stop_reason":            resp.StopReason,
			"total_cost_usd":         l.CostUSD(),
			"permission_denials":     []string{},
			"subagent_stats":         agent.SubagentStats(l.Client.SessionID),
			// fast mode is off in the sdk-cli lane — real's reason string
			"fast_mode_state":           map[bool]string{true: "on", false: "off"}[l.FastMode],
			"fast_mode_disabled_reason": fastModeDisabledReason(l),
			"uuid":                      session.NewID(),
			"usage":                     fullUsage(resp.Usage),
			"modelUsage": map[string]any{l.Model: map[string]any{
				"inputTokens":              resp.Usage.InputTokens,
				"outputTokens":             resp.Usage.OutputTokens,
				"cacheReadInputTokens":     resp.Usage.CacheReadInputTokens,
				"cacheCreationInputTokens": resp.Usage.CacheCreationInputTokens,
				"costUSD":                  l.CostUSD(),
				"webSearchRequests":        0,
				"contextWindow":            l.ContextWindowSize(),
				"maxOutputTokens":          agent.MaxOutputFor(l.Model),
				"thinkingTokens":           0,
				"canonicalModel":           l.Model,
				"provider":                 "firstParty",
				"costBasis":                "list",
			}},
		}
		if schema != "" {
			var v any
			if json.Unmarshal([]byte(out.String()), &v) == nil {
				res["structured_output"] = v
			}
		}
		b, _ := json.Marshal(res) // single line — real emits JSONL
		fmt.Println(string(b))
	}
	return 0
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// sid8 clips a session id for display names without panicking on a
// short --session-id (real rejects non-UUIDs; we degrade to the id).
func sid8(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
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
		var askErr error
		func() {
			defer func() {
				if r := recover(); r != nil {
					// a recovered fault surfaces as real's ui_client_fault
					// event (mod_1744 dQe) before the error result
					ev, _ := json.Marshal(map[string]any{
						"type": "system", "subtype": "ui_client_fault",
						"error":      fmt.Sprint(r),
						"component":  "turn",
						"session_id": l.Client.SessionID, "uuid": session.NewID(),
					})
					fmt.Println(string(ev))
					askErr = fmt.Errorf("internal fault: %v", r)
				}
			}()
			_, askErr = l.Ask(context.Background(), text)
		}()
		if err := askErr; err != nil {
			ev, _ := json.Marshal(map[string]any{
				"type": "result", "subtype": "error_during_execution",
				"is_error": true, "errors": []string{err.Error()},
				"session_id":      l.Client.SessionID,
				"duration_ms":     time.Since(start).Milliseconds(),
				"duration_api_ms": l.APIDuration().Milliseconds(),
				"num_turns":       turns, "uuid": session.NewID(),
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
func emitInstructionSizeWarning(l *agent.Loop) {
	total, fileCount, largest := agent.InstructionSizeStats(l.ToolCtx.CWD)
	if total <= 120000 {
		return
	}
	// real gates the emit on tengu_jolly_aurora (mod_1760 II)
	if v, _ := config.GBFeature("tengu_jolly_aurora").(bool); !v &&
		os.Getenv("CLAUDE_CODE_INSTRUCTION_SIZE_WARNING") == "" {
		return
	}
	ev := map[string]any{
		"type": "system", "subtype": "instruction_size_warning",
		"total_chars": total, "total_limit_chars": 120000,
		"file_count": fileCount,
		"uuid":       session.NewID(), "session_id": l.Client.SessionID,
	}
	if fileCount > 1 && largest > 120000 {
		ev["largest_chars"] = largest
	}
	b, _ := json.Marshal(ev)
	fmt.Println(string(b))
}

func emitStreamJSONInit(l *agent.Loop) {
	toolNames := tools.Names()
	// mcp_servers as [{name,status}] objects (real's shape)
	var servers []map[string]string
	for _, t := range toolNames {
		if strings.HasPrefix(t, "mcp__") {
			parts := strings.SplitN(t, "__", 3)
			if len(parts) > 1 {
				servers = append(servers, map[string]string{
					"name": parts[1], "status": "connected"})
			}
		}
	}
	seen := map[string]bool{}
	var uniq []map[string]string
	for _, s := range servers {
		if !seen[s["name"]] {
			seen[s["name"]] = true
			uniq = append(uniq, s)
		}
	}
	if uniq == nil {
		uniq = []map[string]string{}
	}
	initFields := map[string]any{
		"type": "system", "subtype": "init",
		"session_id": l.Client.SessionID, "model": l.Model, "cwd": l.CWD,
		"tools": toolNames, "mcp_servers": uniq, "permissionMode": l.PermMode,
		"slash_commands": repl.SlashCommandNames(),
		"apiKeySource":   apiKeySource(),
		// real ships the live beta list, not []
		"betas":               betaListForInit(l),
		"claude_code_version": version,
		"output_style":        "default",
		"agents":              agentTypesForInit(),
		"skills":              skillNamesForInit(),
		"plugins":             pluginInitRecords(),
		"fast_mode_state":     l.FastMode,
		"uuid":                session.NewID(),
	}
	if mps := memoryPathsForInit(l); len(mps) > 0 {
		initFields["memory_paths"] = mps
	}
	if errs := mcpServerErrorRows(); len(errs) > 0 {
		initFields["mcp_server_errors"] = errs
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
	// real fires the auth_success Notification hook after login
	if hc := hooks.Load(mustGetwd()); hc != nil {
		hooks.Run(hc, hooks.Notification, map[string]any{
			"session_id": "", "cwd": mustGetwd(),
			"message":           "Login successful",
			"notification_type": "auth_success",
			"hook_event_name":   "Notification",
		}, mustGetwd())
	}
	fmt.Println("logged in")
}

// resolveModel maps short aliases + env overrides to concrete model IDs,
// matching real resolution: sonnet/opus/haiku/default + ANTHROPIC_DEFAULT_*.
func resolveModel(m string) string {
	switch m {
	case "default":
		// ANTHROPIC_DEFAULT_MODEL redefines what "default" resolves to
		// (mod_0137.js mv(): env default unless sentinel/unavailable).
		if v := defaultModelEnv(); v != "" {
			return resolveModel(v)
		}
		return ""
	case "":
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
		return "claude-haiku-5-5"
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

// defaultModelEnv is real's dLe() (mod_0137.js): ANTHROPIC_DEFAULT_MODEL,
// trimmed; "default"/"inherit"/empty are sentinels meaning "no override".
// Values that fail real's model registry check are passed through here —
// the API rejects an unknown id the same way --model does.
func defaultModelEnv() string {
	v := strings.TrimSpace(os.Getenv("ANTHROPIC_DEFAULT_MODEL"))
	if strings.EqualFold(v, "default") || strings.EqualFold(v, "inherit") {
		return ""
	}
	return v
}

// wants1m is set when the configured model used the [1m] suffix — the
// request then carries the context-1m beta (client.go).
var wants1m bool

// startupModel resolves the model the aux bootstrap call reports before
// loop.Model is finalized — same effective precedence as the flag/
// settings/env chain below (flag wins; settings.model otherwise;
// ANTHROPIC_MODEL only when neither is set; then the default).
func startupModel(cwd, flagModel string) string {
	m := resolveModel(flagModel)
	if m == "" {
		m = resolveModel(config.Load(cwd).Model)
	}
	if m == "" {
		m = resolveModel(os.Getenv("ANTHROPIC_MODEL"))
	}
	if m == "" {
		m = resolveModel(defaultModelEnv())
	}
	if m == "" {
		m = "claude-opus-5-5"
	}
	return m
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
// checkUpdateToast prints "✔ Update installed · Restart to update" once
// after a successful selfUpdate (real's post-install toast).
func checkUpdateToast() {
	marker := filepath.Join(config.ClaudeDir(), ".update-installed")
	if b, err := os.ReadFile(marker); err == nil {
		os.Remove(marker)
		fmt.Printf("  \x1b[38;5;107m✔%s\x1b[39m Update installed · Restart to update\n", "")
		_ = b
	}
}

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
		fmt.Println("update: binary isn't inside a claude-go source checkout — rebuild with `go build -o claude-go .` in a clone of github.com/gbasin/claude-go")
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
	// next launch toasts "✔ Update installed · Restart to update"
	marker := filepath.Join(config.ClaudeDir(), ".update-installed")
	os.WriteFile(marker, []byte(api.Version), 0o644)
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

// auxToolCallCount renders real's "Tool calls so far" line.
func auxToolCallCount(n int) string {
	if n == 0 {
		return "none"
	}
	return fmt.Sprintf("%d", n)
}

func last30(s string) string {
	r := []rune(s)
	if len(r) > 30 {
		return string(r[len(r)-30:])
	}
	return s
}

// startupFacts feeds the telemetry event plane real session state —
// installed plugins, discovered skills, MCP results — so the startup
// burst mirrors real's lifecycle markers in volume and shape.
func startupFacts(cwd string, o *cliOpts) api.SessionFacts {
	f := api.SessionFacts{SessionSrc: "startup"}
	for _, p := range plugins.List() {
		mkt := p.Marketplace
		scope := "local"
		if strings.Contains(mkt, "claude-code-plugins") {
			scope = "official"
		}
		f.Plugins = append(f.Plugins, api.PluginFact{Name: p.Name, Marketplace: mkt, Scope: scope})
	}
	for _, s := range agent.SkillFacts(cwd) {
		f.Skills = append(f.Skills, api.SkillFact{
			Name: s.Name, Source: "userSettings", LoadedFrom: "skills", ContentChars: s.Chars})
	}
	for _, st := range tools.MCPServerStatus() {
		if st.Err != nil {
			f.MCPFail = append(f.MCPFail, st.Name)
		} else {
			f.MCPOk = append(f.MCPOk, st.Name)
		}
	}
	if o.cont {
		f.CLIFlags = append(f.CLIFlags, "continue")
	}
	if o.resume != "" {
		f.CLIFlags = append(f.CLIFlags, "resume")
		f.SessionSrc = "resume"
	}
	if o.printMode {
		f.CLIFlags = append(f.CLIFlags, "print")
	}
	return f
}

// safetyTranscript builds real's compact {"role":"summary"} JSON lines
// the auto-mode eval sees — user prompts verbatim, tool calls as
// {"Name":"<summary>"}, then the pending call itself.
func safetyTranscript(l *agent.Loop) []string {
	var lines []string
	for _, m := range l.Messages {
		var blocks []api.ContentBlock
		if json.Unmarshal(m.Content, &blocks) != nil || len(blocks) == 0 {
			var s string
			if json.Unmarshal(m.Content, &s) == nil && s != "" {
				if m.Role == "user" && !strings.HasPrefix(s, "SessionStart hook") {
					b, _ := json.Marshal(map[string]string{"user": s})
					lines = append(lines, string(b))
				}
			}
			continue
		}
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		for _, blk := range blocks {
			switch blk.Type {
			case "text":
				// ctx reminders and hook context aren't transcript
				// content — real serializes prompt/reply text only
				if strings.HasPrefix(blk.Text, "<system-reminder>") ||
					strings.HasPrefix(blk.Text, "SessionStart hook") {
					continue
				}
				key := "user"
				if m.Role == "assistant" {
					key = "assistant"
				}
				b, _ := json.Marshal(map[string]string{key: blk.Text})
				lines = append(lines, string(b))
			case "tool_use":
				b, _ := json.Marshal(map[string]string{blk.Name: safetyInputSummary(blk.Input)})
				lines = append(lines, string(b))
			}
		}
	}
	// the proposed call is already the last assistant tool_use in
	// l.Messages — no extra line needed
	return lines
}

// safetyInputSummary renders a tool input the way real's eval transcript
// does — "(general): say hi" for an Agent call, the command for Bash.
func safetyInputSummary(input json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return string(input)
	}
	if st, _ := m["subagent_type"].(string); st != "" {
		p, _ := m["prompt"].(string)
		return "(" + st + "): " + p
	}
	if cmd, _ := m["command"].(string); cmd != "" {
		return cmd
	}
	b, _ := json.Marshal(m)
	s := string(b)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// setupTmuxSession creates real's --tmux worktree session — a detached tmux
// session bound to the worktree with CLAUDE_CODE_TMUX_SESSION advertised to
// the UI ("tmux session: <name>"); --tmux=classic forces traditional tmux.
func setupTmuxSession(wtPath, wtName, sessionID string) {
	name := "claude-" + sid8(sessionID)
	if wtName != "" {
		name = "claude-" + wtName
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		fmt.Fprintf(os.Stderr, "Error: tmux is not installed.\nInstall tmux or use --worktree alone.\n")
		return
	}
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", name,
		"-c", wtPath).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Failed to create tmux session: %s\n", strings.TrimSpace(string(out)))
		return
	}
	os.Setenv("CLAUDE_CODE_TMUX_SESSION", name)
	fmt.Fprintf(os.Stderr, "Created tmux session: %s\n", name)
}

// sessionForPR finds a local session transcript whose records link to the
// given PR number/URL — mirrors real's --from-pr resolution.
func sessionForPR(cwd, ref string) string {
	// same slug rule as the session store — EvalSymlinks + /→- (the old
	// loose replacer also ate dots and missed sessions under dotted dirs)
	dir := filepath.Join(config.ClaudeDir(), "projects", session.Slug(cwd))
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	want := strings.TrimPrefix(ref, "#")
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, ln := range strings.Split(string(b), "\n") {
			var rec struct {
				PrNumber int    `json:"prNumber"`
				PrURL    string `json:"prUrl"`
			}
			if json.Unmarshal([]byte(ln), &rec) != nil {
				continue
			}
			if rec.PrURL == want || strings.HasSuffix(rec.PrURL, "/pull/"+want) {
				return strings.TrimSuffix(e.Name(), ".jsonl")
			}
			if n, err := strconv.Atoi(want); err == nil && rec.PrNumber == n && n != 0 {
				return strings.TrimSuffix(e.Name(), ".jsonl")
			}
		}
	}
	return ""
}

func awaySummaryEnabled() bool {
	v, ok := config.UserSetting("awaySummaryEnabled").(bool)
	return !ok || v
}

// lastAssistantLine pulls the last assistant text for the away recap.
func lastAssistantLine(msgs []api.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(msgs[i].Content, &blocks) == nil {
			for _, bl := range blocks {
				if bl.Type == "text" && strings.TrimSpace(bl.Text) != "" {
					line := strings.TrimSpace(strings.Split(bl.Text, "\n")[0])
					if len(line) > 100 {
						line = line[:100] + "…"
					}
					return line
				}
			}
		}
	}
	return ""
}

func loadPathOr(cwd, id, p string) string {
	if p != "" {
		return p
	}
	slug := session.Slug(cwd)
	return filepath.Join(config.ClaudeDir(), "projects", slug, id+".jsonl")
}

// cleanupPeriodDays — how long to keep transcripts (default 30, min 1).
func cleanupPeriod() int {
	if f, ok := config.UserSetting("cleanupPeriodDays").(float64); ok && f >= 1 {
		return int(f)
	}
	return 30
}

// pruneOldSessions deletes transcript files older than the period —
// real's startup cleanup (cleanupPeriodDays).
func pruneOldSessions(days int) {
	if days <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	root := filepath.Join(config.ClaudeDir(), "projects")
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return nil
		}
		if fi, err := d.Info(); err == nil && fi.ModTime().Before(cutoff) {
			os.Remove(p)
		}
		return nil
	})
}

// apiKeySource resolves where auth came from for the init event.
func apiKeySource() string {
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return "ANTHROPIC_API_KEY"
	}
	if os.Getenv("CLAUDE_CODE_API_KEY") != "" {
		return "CLAUDE_CODE_API_KEY"
	}
	// real's source ordering: env keys, then apiKeyHelper, then oauth
	if _, ok := config.UserSetting("apiKeyHelper").(string); ok {
		return "apiKeyHelper"
	}
	if c, err := auth.Load(); err == nil && c != nil {
		return "oauth"
	}
	return "none"
}

// fullUsage expands api.Usage to real's stream-json usage object.
func fullUsage(u api.Usage) map[string]any {
	// real's full usage block (captured): token details + service tier +
	// iterations + speed + geo — the extras ride the same response usage
	return map[string]any{
		"input_tokens":                u.InputTokens,
		"output_tokens":               u.OutputTokens,
		"output_tokens_details":       map[string]any{"thinking_tokens": 0},
		"cache_creation_input_tokens": u.CacheCreationInputTokens,
		"cache_read_input_tokens":     u.CacheReadInputTokens,
		"server_tool_use": map[string]any{
			"web_search_requests": 0, "web_fetch_requests": 0,
		},
		"service_tier": "standard",
		"cache_creation": map[string]any{
			"ephemeral_1h_input_tokens": 0, "ephemeral_5m_input_tokens": u.CacheCreationInputTokens,
		},
		"inference_geo":   "",
		"iterations":      []any{},
		"speed":           "standard",
		"fallback_credit": nil,
	}
}

// fastModeDisabledReason mirrors real's Z1()/l8() result for the sdk-cli
// lane — fast mode requires interactive opt-in; off otherwise.
func fastModeDisabledReason(l *agent.Loop) string {
	if l.FastMode {
		return ""
	}
	return "sdk_opt_in_required"
}

// fireInstructionsLoaded emits InstructionsLoaded for each CLAUDE.md /
// memory file that actually feeds the session (real fires per file with
// load_reason=session_start).
func fireInstructionsLoaded(l *agent.Loop, cwd string) {
	if len(l.Hooks["InstructionsLoaded"]) == 0 {
		return
	}
	var files []string
	for _, p := range []string{
		filepath.Join(config.ClaudeDir(), "CLAUDE.md"),
		filepath.Join(cwd, "CLAUDE.md"),
		filepath.Join(cwd, "CLAUDE.local.md"),
		filepath.Join(cwd, ".claude", "CLAUDE.md"),
		filepath.Join(config.ClaudeDir(), "projects",
			session.Slug(cwd), "memory", "MEMORY.md"),
	} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			files = append(files, p)
		}
	}
	for _, f := range files {
		hooks.Run(l.Hooks, hooks.InstructionsLoaded, map[string]any{
			"session_id": l.Client.SessionID, "cwd": cwd,
			"hook_event_name": "InstructionsLoaded", "permission_mode": l.PermMode,
			"transcript_path": l.Transcript,
			"load_reason":     "session_start",
			"file_path":       f,
		}, cwd)
	}
}

// betaListForInit returns the live beta header list for the init event —
// real ships the same names the request carries.
func betaListForInit(l *agent.Loop) []string {
	if c := l.Client; c != nil {
		return c.BetaNames()
	}
	return []string{}
}

// agentTypesForInit lists the registered subagent type names — real
// reports the builtin set plus any .claude/agents/*.md discovered types.
func agentTypesForInit() []string {
	// real's builtin order is fixed; discovered types append after
	out := []string{"general-purpose", "Explore", "Plan"}
	var extra []string
	g, _ := filepath.Glob(filepath.Join(config.ClaudeDir(), "agents", "*.md"))
	for _, p := range g {
		extra = append(extra, strings.TrimSuffix(filepath.Base(p), ".md"))
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// skillNamesForInit lists invocable skill names — real reports the skill
// surface, not the slash-command set.
func skillNamesForInit() []string {
	var out []string
	for _, d := range []string{
		filepath.Join(config.ClaudeDir(), "skills"),
	} {
		ents, _ := os.ReadDir(d)
		for _, e := range ents {
			if e.IsDir() {
				out = append(out, e.Name())
			}
		}
	}
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}

// pluginInitRecords emits real's plugins array — {name,path} objects for
// each installed plugin (not bare names).
func pluginInitRecords() []map[string]any {
	var out []map[string]any
	for _, d := range plugins.EnabledPluginDirs() {
		out = append(out, map[string]any{
			"name": filepath.Base(d), "path": d,
		})
	}
	if out == nil {
		out = []map[string]any{}
	}
	return out
}

// memoryPathsForInit lists the CLAUDE.md/rules paths that actually
// loaded — real's memory_paths init field.
func memoryPathsForInit(l *agent.Loop) []string {
	var out []string
	for _, p := range agent.MemoryPaths(l.ToolCtx.CWD) {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// mcpServerErrorRows lists MCP servers that failed to connect — real's
// mcp_server_errors init field.
func mcpServerErrorRows() []map[string]any {
	var out []map[string]any
	for _, s := range tools.MCPServerStatus() {
		if s.Err != nil {
			out = append(out, map[string]any{"name": s.Name, "error": s.Err.Error()})
		}
	}
	return out
}
