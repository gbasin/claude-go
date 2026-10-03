package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// `claude remote-control` — the standalone bridge server entry (mod_0690.js
// enterRemoteControl + mod_1269.js bridgeMain). Flags mirror real's usage
// block; the ones that need infrastructure we don't have (spawn modes that
// exec fresh `claude` children per work item) are accepted but only the
// control-plane loop runs — sessions show as accepted work, not spawned
// REPLs, unless a Spawner is wired in.

// Options is the parsed remote-control CLI surface.
type Options struct {
	Name           string // --name
	NamePrefix     string // --remote-control-session-name-prefix (env fallback)
	Continue       bool   // -c / --continue
	SessionID      string // --session-id
	PermissionMode string // --permission-mode
	Spawn          string // --spawn same-dir|worktree|session (def same-dir)
	Capacity       int    // --capacity (def 32 = real's Ar)
	Chrome         *bool  // --[no-]chrome
	CreateInDir    *bool  // --[no-]create-session-in-dir (def on)
	Verbose        bool
	Debug          string
	DebugFile      string
}

// parseOptions mirrors mod_1269.js arg parsing; returns error text in
// real's "Unknown argument" shape.
func parseOptions(args []string) (*Options, error) {
	o := &Options{Spawn: "same-dir", Capacity: 32}
	boolFlag := func(dst **bool, v bool) { *dst = &v }
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("Unknown argument: %s\nRun 'claude remote-control --help' for usage.", a)
			}
			i++
			return args[i], nil
		}
		switch {
		case a == "--name":
			v, err := next()
			if err != nil {
				return nil, err
			}
			o.Name = v
		case a == "--remote-control-session-name-prefix":
			v, err := next()
			if err != nil {
				return nil, err
			}
			o.NamePrefix = v
		case a == "-c" || a == "--continue":
			o.Continue = true
		case a == "--session-id":
			v, err := next()
			if err != nil {
				return nil, err
			}
			o.SessionID = v
		case a == "--permission-mode":
			v, err := next()
			if err != nil {
				return nil, err
			}
			o.PermissionMode = v
		case a == "--spawn":
			v, err := next()
			if err != nil {
				return nil, err
			}
			switch v {
			case "same-dir", "worktree", "session":
				o.Spawn = v
			default:
				return nil, fmt.Errorf("Unknown argument: --spawn %s\nRun 'claude remote-control --help' for usage.", v)
			}
		case strings.HasPrefix(a, "--spawn="):
			v := a[len("--spawn="):]
			switch v {
			case "same-dir", "worktree", "session":
				o.Spawn = v
			default:
				return nil, fmt.Errorf("Unknown argument: %s\nRun 'claude remote-control --help' for usage.", a)
			}
		case a == "--capacity":
			v, err := next()
			if err != nil {
				return nil, err
			}
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 1 {
				return nil, fmt.Errorf("Unknown argument: --capacity %s\nRun 'claude remote-control --help' for usage.", v)
			}
			o.Capacity = n
		case strings.HasPrefix(a, "--capacity="):
			var n int
			if _, err := fmt.Sscanf(a[len("--capacity="):], "%d", &n); err != nil || n < 1 {
				return nil, fmt.Errorf("Unknown argument: %s\nRun 'claude remote-control --help' for usage.", a)
			}
			o.Capacity = n
		case a == "--chrome":
			boolFlag(&o.Chrome, true)
		case a == "--no-chrome":
			boolFlag(&o.Chrome, false)
		case a == "--create-session-in-dir":
			boolFlag(&o.CreateInDir, true)
		case a == "--no-create-session-in-dir":
			boolFlag(&o.CreateInDir, false)
		case a == "-v" || a == "--verbose":
			o.Verbose = true
		case a == "-d" || a == "--debug":
			o.Debug = "1"
		case strings.HasPrefix(a, "--debug="):
			o.Debug = a[len("--debug="):]
		case a == "--debug-file":
			v, err := next()
			if err != nil {
				return nil, err
			}
			o.DebugFile = v
		case a == "-h" || a == "--help":
			return nil, errHelp
		default:
			return nil, fmt.Errorf("Unknown argument: %s\nRun 'claude remote-control --help' for usage.", a)
		}
	}
	if o.NamePrefix == "" {
		o.NamePrefix = os.Getenv("CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX")
	}
	// real: --session-id can't combine with spawn flags or --continue.
	if o.SessionID != "" && o.Continue {
		return nil, fmt.Errorf("--session-id cannot be used with --continue.")
	}
	return o, nil
}

var errHelp = fmt.Errorf("help")

// Root-option carry-over (mod_0690.js E/k/D): real refuses to start when
// root-level flags that wouldn't apply to the sessions Remote Control
// spawns appear before the verb. Whitelist = the carried-over set.
var rootOptionCarryOver = map[string]func(v string) bool{
	"--verbose":                            func(string) bool { return true },
	"-v":                                   func(string) bool { return true },
	"--debug":                              func(string) bool { return true },
	"-d":                                   func(string) bool { return true },
	"--debug-file":                         func(string) bool { return true },
	"--ax-screen-reader":                   func(string) bool { return true },
	"--workload":                           func(string) bool { return true },
	"--session-id":                         func(string) bool { return true },
	"--name":                               func(string) bool { return true },
	"-n":                                   func(string) bool { return true },
	"--remote-control-session-name-prefix": func(string) bool { return true },
	"--plugin-dir":                         func(string) bool { return true },
	"--plugin-dir-no-mcp":                  func(string) bool { return true },
	"--plugin-url":                         func(string) bool { return true },
	"--add-dir":                            func(string) bool { return true },
	"--ide":                                func(string) bool { return true },
	"--chrome":                             func(v string) bool { return v == "true" }, // chrome only when enabled
	"--model":                              func(string) bool { return true },
	"--effort":                             func(string) bool { return true },
	"--fallback-model":                     func(string) bool { return true },
	"--betas":                              func(string) bool { return true },
	"--thinking":                           func(string) bool { return true },
	"--thinking-display":                   func(string) bool { return true },
	"--max-thinking-tokens":                func(string) bool { return true },
	"--autocompact":                        func(string) bool { return true },
	"--allowed-tools":                      func(string) bool { return true },
	"--allowedTools":                       func(string) bool { return true },
	"--dangerously-skip-permissions":       func(string) bool { return true },
	"--allow-dangerously-skip-permissions": func(string) bool { return true },
	"--enable-auto-mode":                   func(string) bool { return true },
	"--bare":                               func(string) bool { return true },
}

// valueTakingRootFlags are root flags whose next token is a value — needed
// so a value that happens to spell "remote-control" isn't read as the verb.
var valueTakingRootFlags = map[string]bool{
	"--name": true, "-n": true, "--session-id": true, "--model": true,
	"--effort": true, "--fallback-model": true, "--betas": true,
	"--thinking": true, "--max-thinking-tokens": true, "--autocompact": true,
	"--allowed-tools": true, "--allowedTools": true, "--workload": true,
	"--plugin-dir": true, "--plugin-dir-no-mcp": true, "--plugin-url": true,
	"--add-dir": true, "--debug-file": true, "--debug": true,
	"--remote-control-session-name-prefix": true, "--permission-mode": true,
	"--thinking-display": true, "--mcp-config": true, "--settings": true,
	"--setting-sources": true, "--managed-settings": true, "--channels": true,
	"--project": true, "--resume": true, "-r": true, "--fork-session": false,
	"--from-pr": true, "--append-system-prompt": true, "--system-prompt": true,
	"-p": true, "--print": false,
}

// RefusedRootOptions mirrors k(): of the flag spellings appearing before
// the remote-control verb, return those not carried over.
func RefusedRootOptions(rootArgs []string) []string {
	var refused []string
	for _, a := range rootArgs {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		spelling := a
		val := ""
		if i := strings.Index(a, "="); i >= 0 {
			spelling, val = a[:i], a[i+1:]
		}
		if allow, ok := rootOptionCarryOver[spelling]; ok && allow(val) {
			continue
		}
		refused = append(refused, spelling)
	}
	return refused
}

// RootOptionsRefusedMessage mirrors D().
func RootOptionsRefusedMessage(flags []string) string {
	quoted := make([]string, len(flags))
	for i, f := range flags {
		quoted[i] = "`" + f + "`"
	}
	it, are := "it", "is"
	if len(flags) > 1 {
		it, are = "them", "are"
	}
	perm := ""
	for _, f := range flags {
		if f == "--permission-mode" {
			perm = " Set the permission mode with `claude remote-control --permission-mode <mode>`."
		}
	}
	return fmt.Sprintf("Error: %s before `remote-control` %s not carried over to the sessions "+
		"Remote Control starts, so Remote Control refuses to start rather than drop %s — "+
		"remove %s, and give Remote Control's own options after the verb "+
		"(see `claude remote-control --help`).%s",
		strings.Join(quoted, ", "), are, it, it, perm)
}

// FindVerb locates the remote-control verb in args ("" when absent),
// skipping flag values.
func FindVerb(args []string) (idx int) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "remote-control" || a == "rc" {
			return i
		}
		if strings.HasPrefix(a, "-") && !strings.Contains(a, "=") && valueTakingRootFlags[a] {
			i++ // skip the value
		}
	}
	return -1
}

const helpText = `
Remote Control - Control local sessions from claude.ai/code or the Claude mobile app

USAGE
  claude remote-control [options]
OPTIONS
  --name <name>                    Name for the session (shown in claude.ai/code)
  --remote-control-session-name-prefix <prefix>
                                   Prefix for auto-generated session names
                                   (default: hostname; env:
                                   CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX)
  -c, --continue                   Reattach to the session ` + "`claude remote-control`" + `
                                   last recorded for this directory (or one of its
                                   git worktrees) instead of creating a new one.
                                   Exits with an error if nothing was recorded
                                   here within roughly the last 4 hours
  --session-id <id>                Reattach to a specific session by ID (cannot be
                                   used with spawn flags or --continue)
  --permission-mode <mode>         Permission mode for spawned sessions
  --[no-]chrome                    Claude in Chrome for spawned sessions
                                   (default: off; a session started from a
                                   project follows this machine's /chrome
                                   setting)
  -d, --debug[=<filter>]           Enable debug mode for this process, with
                                   optional category filtering (e.g.
                                   --debug=api,hooks)
  --debug-file <path>              Write debug logs to file
  -v, --verbose                    Enable verbose output
  -h, --help                       Show this help
  --spawn <mode>                   Spawn mode: same-dir, worktree, session
                                   (default: same-dir)
  --capacity <N>                   Max concurrent sessions in worktree or
                                   same-dir mode (default: 32)
  --[no-]create-session-in-dir     Pre-create a session in the current
                                   directory; in worktree mode this session
                                   stays in cwd while on-demand sessions get
                                   isolated worktrees (default: on)

DESCRIPTION
  Remote Control allows you to control sessions on your local device from
  claude.ai/code (https://claude.ai/code) or the Claude mobile app. Run
  this command in the directory you want to work in, then connect from
  your phone or a browser.

  Remote Control runs as a persistent server that accepts multiple concurrent
  sessions in the current directory. One session is pre-created on start so
  you have somewhere to type immediately. Use --spawn=worktree to isolate
  each on-demand session in its own git worktree, or --spawn=session for
  the classic single-session mode (exits when that session ends). Press 'w'
  during runtime to toggle between same-dir and worktree.

NOTES
  - You must be logged in with a Claude account that has a subscription
  - In a folder you haven't trusted yet you'll be asked whether to trust it
    (with no terminal to ask on, run ` + "`claude`" + ` there once first)
  - Worktree mode requires a git repository or WorktreeCreate/WorktreeRemove hooks
`

// Deps wires the command to claude-go subsystems — injected so tests can
// stub every boundary.
type Deps struct {
	// DisableRemoteControl is settings.disableRemoteControl===true from
	// ANY settings tier (real reads all of them, mod_1259.js Xe()).
	DisableRemoteControl bool
	// HasOAuth reports a usable claude.ai OAuth token exists (NOT an API
	// key — cloud sessions refuse those).
	HasOAuth bool
	// Token resolves a fresh OAuth bearer; Refresh forces a refresh and
	// reports whether it worked.
	Token   func() string
	Refresh func() bool
	// BaseURL is the API base (real's pn().BASE_API_URL ≈ ANTHROPIC_BASE_URL).
	BaseURL string
	// Version is the CLI version for the min_version floor + UA.
	Version string
	// NewBridge builds the server. Nil => DefaultFactory.
	NewBridge func(api *Client, env *RegisterParams, o *Options) *Bridge
	// OnSessionID is called once a bridge session id exists — the caller
	// stamps sessions/<pid>.json bridgeSessionId with it (mod_2173 gate).
	OnSessionID func(id string)
	// Now is injectable for tests.
	Now func() time.Time
}

// RunCLI is `claude remote-control [options]` — returns the process exit
// code; user-facing text goes to stdout/stderr like real.
func RunCLI(args []string, d *Deps) int {
	o, err := parseOptions(args)
	if err == errHelp {
		fmt.Print(helpText)
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	if d == nil {
		d = &Deps{}
	}
	// Gate order (mod_0690.js O = h() -> b() -> C()):
	//   h: disableRemoteControl, stored OAuth
	//   b: diagnosis (approximated), min_version floor, allow_remote_control
	if err := CheckDisabledLocally(d.DisableRemoteControl); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	if err := CheckOAuth(d.HasOAuth); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	cfg := LoadREPLConfig()
	if v := CheckMinVersion(cfg, d.Version); v != "" {
		fmt.Fprintln(os.Stderr, v)
		return 1
	}
	if err := CheckOrgPolicy(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	if o.SessionID != "" {
		if err := checkID(o.SessionID, "sessionId"); err != nil {
			fmt.Fprintln(os.Stderr, "session id contains unsupported characters — check the --session-id value")
			return 1
		}
	}
	if o.Continue {
		// Real looks up the per-directory pointer written by the last
		// remote-control run (and its git worktrees). We don't persist one,
		// so report real's no-pointer error verbatim rather than fake it.
		fmt.Fprintln(os.Stderr,
			"Error: No recent session found in this directory or its worktrees. "+
				"Run `claude remote-control` to start a new one.")
		return 1
	}

	base := d.BaseURL
	if base == "" {
		base = os.Getenv("ANTHROPIC_BASE_URL")
	}
	if base == "" {
		base = "https://api.anthropic.com"
	}
	base = strings.TrimRight(base, "/")

	api := &Client{
		BaseURL:       base,
		RunnerVersion: d.Version,
		GetAccessToken: func() string {
			if d.Token == nil {
				return ""
			}
			return d.Token()
		},
		OnAuth401: func(old string) bool {
			if d.Refresh == nil {
				return false
			}
			return d.Refresh()
		},
	}

	cwd, _ := os.Getwd()
	host, _ := os.Hostname()
	env := &RegisterParams{
		MachineName: host,
		Directory:   cwd,
		Branch:      gitBranch(cwd),
		GitRepoURL:  gitRemoteURL(cwd),
		MaxSessions: o.Capacity,
		Metadata:    map[string]any{"worker_type": "cli"},
	}
	mk := d.NewBridge
	var br *Bridge
	if mk != nil {
		br = mk(api, env, o)
	} else {
		br = &Bridge{
			API:             api,
			Env:             env,
			Pacing:          DefaultPacing(),
			ResumeSessionID: o.SessionID,
		}
	}
	if br.Log == nil {
		br.Log = func(s string) { fmt.Fprintln(os.Stderr, s) }
	}
	if br.LogError == nil {
		br.LogError = func(s string) { fmt.Fprintln(os.Stderr, "Error: "+s) }
	}
	verbose := o.Verbose || o.Debug != ""
	if verbose {
		br.OnDebug = func(s string) { fmt.Fprintln(os.Stderr, s) }
		api.OnDebug = br.OnDebug
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if o.Spawn != "session" {
		fmt.Fprintf(os.Stderr, "Spawn mode: %s\n", o.Spawn)
		fmt.Fprintf(os.Stderr, "Max concurrent sessions: %d\n", o.Capacity)
	}
	// Session spawning (fresh `claude` children per work item) is the part
	// real delegates to spawn handlers; claude-go's loop accepts + acks +
	// heartbeats work and reports sessions, but no local REPL is spawned —
	// the ws/SSE data plane frames are backend-gated (see transport.go).
	if br.Spawner == nil {
		br.Spawner = SpawnerFunc(func(ctx context.Context, w WorkItem, secret *WorkSecret) (*WorkSession, error) {
			// The honest path: accept the work so heartbeats keep it leased
			// and stamp the pid file so /teleport's gate sees a bridge
			// session (mod_2173). The session's own data plane (worker
			// SSE + /worker/* writes) is intentionally stubbed.
			var wd WorkData
			_ = json.Unmarshal(w.Data, &wd)
			if d.OnSessionID != nil && wd.ID != "" {
				d.OnSessionID(codeSessionID(wd.ID))
			}
			return &WorkSession{SessionID: wd.ID, WorkID: w.ID,
				AccessToken: secret.SessionIngressToken}, nil
		})
	}
	err = br.Run(ctx)
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	return 0
}

func gitBranch(cwd string) string {
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitRemoteURL(cwd string) string {
	out, err := exec.Command("git", "-C", cwd, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
