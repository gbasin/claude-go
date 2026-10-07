package main

// flags.go — CLI flag surface, matching Claude Code 2.1.288's parser shape:
// value flags (take next arg or --flag=v), optional-value flags (take a value
// only when the next arg isn't a flag), and boolean flags.

import (
	"bufio"
	"claude-go/internal/config"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"claude-go/internal/api"
)

type cliOpts struct {
	printMode    bool
	mockMode     bool
	mockPort     int
	model        string
	outFormat    string
	maxTurns     int
	permMode     string
	permModeSet  bool
	appendSys    string
	addDirs      []string
	cont         bool
	resume       string
	prompt       []string
	allowTools   string
	denyTools    string
	sysPrompt    string
	sysPromptF   string
	sessID       string
	forkSess     bool
	bareMode     bool
	agentsJSON   string
	toolsList    string
	workload     string
	fileArgs     []string
	thinkingMode string
	thinkingDisp string
	thinkingTok  int
	maxBudget    float64
	debugFile    string
	debug        bool
	verbose      bool
	mcpConfig    string
	strictMCP    bool
	settingsF    string
	fallback     string
	inFormat     string
	jsonSchema   string
	agentName    string
	// new surface
	effort       string
	sessionName  string
	resumeAt     string
	dropsTurn    string
	rewindFiles  string
	autoCompact  string
	advisor      string
	worktreeSet  bool
	worktreeName string
	tmux         string // "" | "auto" | "classic"
	projectDir   string
	sdkURL       string
	betas        string
	initOnly     bool
	bgMode       bool
	noPersist    bool
	partialMsgs  bool
	replayUser   bool
	hookEvents   bool
	execPrompt   string
	teleport     bool
	teleportArg  string
	stubs        []string // feature flags accepted but not wired
	fromPR       string   // --from-pr <ref> — session linked to a PR
	fromPRFlag   bool     // --from-pr passed at all (bare → picker)
}

// stubEnv records a parse-only flag as an env var so the value is at least
// observable to the process tree, and remembers it for the "not supported"
// notice list.
func (o *cliOpts) stubEnv(flag, envName, val string, note bool) {
	if val != "" {
		os.Setenv(envName, val)
	}
	if note {
		o.stubs = append(o.stubs, flag)
	}
}

func stubName(flag string) string {
	// --messaging-socket-path -> CLAUDE_GO_MESSAGING_SOCKET_PATH
	s := strings.TrimLeft(flag, "-")
	s = strings.ReplaceAll(s, "-", "_")
	return "CLAUDE_GO_" + strings.ToUpper(s)
}

func parseCLI(args []string) *cliOpts {
	o := &cliOpts{outFormat: "text", permMode: "default"}

	valueFlags := map[string]func(string){
		"--model":           func(v string) { o.model = v },
		"--agent":           func(v string) { o.agentName = v },
		"--effort":          func(v string) { o.effort = v },
		"--permission-mode": func(v string) { o.permMode = v; o.permModeSet = true },
		"--output-format":   func(v string) { o.outFormat = v },
		"--input-format":    func(v string) { o.inFormat = v },
		"--max-turns":       func(v string) { fmt.Sscanf(v, "%d", &o.maxTurns) },
		"--max-thinking-tokens": func(v string) {
			if n, err := strconv.Atoi(v); err == nil {
				o.thinkingTok = n
			}
		},
		"--max-budget-usd":       func(v string) { o.maxBudget, _ = strconv.ParseFloat(v, 64) },
		"--append-system-prompt": func(v string) { o.appendSys = v },
		"--append-system-prompt-file": func(v string) {
			if b, err := os.ReadFile(v); err == nil {
				if o.appendSys != "" {
					o.appendSys += "\n"
				}
				o.appendSys += string(b)
			} else {
				fatal("--append-system-prompt-file: %v", err)
			}
		},
		"--system-prompt":      func(v string) { o.sysPrompt = v },
		"--system-prompt-file": func(v string) { o.sysPromptF = v },
		"--json-schema":        func(v string) { o.jsonSchema = v },
		"--fallback-model":     func(v string) { o.fallback = v },
		"--agents":             func(v string) { o.agentsJSON = v },
		"--name":               func(v string) { o.sessionName = v },
		"-n":                   func(v string) { o.sessionName = v },
		"--allowedTools":       func(v string) { o.allowTools = v },
		"--allowed-tools":      func(v string) { o.allowTools = v },
		"--disallowedTools":    func(v string) { o.denyTools = v },
		"--disallowed-tools":   func(v string) { o.denyTools = v },
		"--tools":              func(v string) { o.toolsList = v },
		"--add-dir":            func(v string) { o.addDirs = append(o.addDirs, v) },
		"--mcp-config":         func(v string) { o.mcpConfig = v },
		"--betas":              func(v string) { o.betas = v },
		"--file":               func(v string) { o.fileArgs = append(o.fileArgs, v) },
		"--sdk-url":            func(v string) { o.sdkURL = v },
		"--exec":               func(v string) { o.execPrompt = v },
		"-m":                   func(v string) { o.execPrompt = v },
		"--thinking":           func(v string) { o.thinkingMode = v },
		"--thinking-display":   func(v string) { o.thinkingDisp = v },
		"--resume-session-at":  func(v string) { o.resumeAt = v },
		"--resume-drops-turn":  func(v string) { o.dropsTurn = v },
		"--rewind-files":       func(v string) { o.rewindFiles = v },
		"--autocompact":        func(v string) { o.autoCompact = v },
		"--advisor":            func(v string) { o.advisor = v },
		"--workload":           func(v string) { o.workload = v; os.Setenv("CLAUDE_GO_WORKLOAD", v) },
		"--session-id":         func(v string) { o.sessID = v },
		"--settings":           func(v string) { o.settingsF = v },
		"--debug-file":         func(v string) { o.debugFile = v },
		"--mock-port":          func(v string) { fmt.Sscanf(v, "%d", &o.mockPort) },
	}
	// parse-only value flags -> env vars
	for _, f := range []string{
		"--prefill", "--prefill-b64", "--handle-uri",
		"--managed-settings", "--setting-sources", "--client-data-url",
		"--watch-artifact", "--watch-artifact-no-autoreact",
		"--team-name", "--agent-id", "--agent-name", "--agent-color",
		"--parent-session-id", "--agent-type", "--routine",
		"--inherit-permission-mode", "--proactivity",
		"--system-prompt-snapshot", "--append-subagent-system-prompt",
		"--append-subagent-system-prompt-file", "--plan-mode-instructions",
		"--permission-prompt-tool", "--permission-prompts",
		"--channels", "--dangerously-load-development-channels",
		"--plugin-dir", "--plugin-dir-no-mcp", "--plugin-url",
		"--remote-control-session-name-prefix",
		"--task-budget", "--teammate-mode", "--messaging-socket-path",
		"--environment", "--pool", "--ref", "--on-branch",
		"--correlation-id", "--forward-home-settings", "--project-config-root",
		"--attach-serve",
	} {
		flag := f
		valueFlags[flag] = func(v string) { o.stubEnv(flag, stubName(flag), v, false) }
	}
	// prefill: still parse-only, but keep decoded value around
	valueFlags["--prefill"] = func(v string) { os.Setenv("CLAUDE_GO_PREFILL", v) }
	valueFlags["--prefill-b64"] = func(v string) { os.Setenv("CLAUDE_GO_PREFILL_B64", v) }
	// attach-serve and feature flags the user will notice missing
	for _, f := range []string{"--attach-serve", "--teammate-mode", "--messaging-socket-path"} {
		flag := f
		valueFlags[flag] = func(v string) { o.stubEnv(flag, stubName(flag), v, true) }
	}

	boolFlags := map[string]func(){
		"-p":                                   func() { o.printMode = true },
		"--print":                              func() { o.printMode = true },
		"--mock": func() {
			o.mockMode = true
			// env is the propagation contract for spawned children (bg
			// workers, daemon respawns read CLAUDE_GO_MOCK to add --mock);
			// a flag-only --mock silently spawned an unmocked worker that
			// died on credentials
			os.Setenv("CLAUDE_GO_MOCK", "1")
		},
		"-d":                                   func() { o.debug = true },
		"--debug":                              func() { o.debug = true },
		"-d2e":                                 func() { o.debug = true },
		"--debug-to-stderr":                    func() { o.debug = true },
		"--verbose":                            func() { o.verbose = true },
		"--bare":                               func() { o.bareMode = true },
		"--init-only":                          func() { o.initOnly = true },
		"--include-hook-events":                func() { o.hookEvents = true },
		"--include-partial-messages":           func() { o.partialMsgs = true },
		"--replay-user-messages":               func() { o.replayUser = true },
		"--dangerously-skip-permissions":       func() { o.permMode = "bypassPermissions" },
		"--allow-dangerously-skip-permissions": func() { o.permMode = "bypassPermissions" },
		"-c":                                   func() { o.cont = true },
		"--bg":                                  func() { o.bgMode = true },
		"--background":                          func() { o.bgMode = true },
		"--continue":                           func() { o.cont = true },
		"--fork-session":                       func() { o.forkSess = true },
		"--strict-mcp-config":                  func() { o.strictMCP = true },
		"--enable-auto-mode":                   func() { o.permMode = "auto" },
		"--plan-mode-required":                 func() { o.permMode = "plan" },
		"--no-session-persistence":             func() { o.noPersist = true },
		"--ide":                                func() { o.stubEnv("--ide", "CLAUDE_GO_IDE", "1", true) },
		"--desktop":                            func() { o.stubEnv("--desktop", "CLAUDE_GO_DESKTOP", "1", true) },
		"--chrome":                             func() { os.Setenv("CLAUDE_GO_CHROME", "1") },
		"--no-chrome":                          func() { os.Setenv("CLAUDE_GO_CHROME", "0") },
		"--tmux":                               func() { o.tmux = "auto" },
		"--tmux=classic":                       func() { o.tmux = "classic" },
		"--init":                               func() { o.stubs = append(o.stubs, "--init") },
	}
	for _, f := range []string{
		"--safe-mode", "--maintenance", "--forward-subagent-text",
		"--session-mirror", "--await-claim", "--await-initialize",
		"--enable-auth-status", "--restricted",
		"--exclude-dynamic-system-prompt-sections", "--deep-link-origin",
		"--reply-on-resume", "--disable-slash-commands",
		"--brief", "--ax-screen-reader", "--hard-fail",
	} {
		flag := f
		boolFlags[flag] = func() { os.Setenv(stubName(flag), "1") }
	}

	// optional-value flags: consume next arg only when it isn't a flag
	optFlags := map[string]func(string){
		"-r": func(v string) {
			o.resume = v
			if v == "" {
				o.resume = "-"
			}
		},
		"--resume": func(v string) {
			o.resume = v
			if v == "" {
				o.resume = "-"
			}
		},
		"--from-pr":        func(v string) { o.fromPRFlag = true; o.fromPR = v },
		"-w":               func(v string) { o.worktreeSet = true; o.worktreeName = v },
		"--worktree":       func(v string) { o.worktreeSet = true; o.worktreeName = v },
		"--project":        func(v string) { o.projectDir = v },
		"--teleport":       func(v string) { o.teleport = true; o.teleportArg = v },
		"--cloud":          func(v string) { o.stubs = append(o.stubs, "--cloud") },
		"--remote":         func(v string) { o.stubs = append(o.stubs, "--remote") },
		"--remote-control": func(v string) { o.stubs = append(o.stubs, "--remote-control") },
		"--rc":             func(v string) { o.stubs = append(o.stubs, "--remote-control") },
		"--prompt-suggestions": func(v string) {
			o.stubEnv("--prompt-suggestions", "CLAUDE_GO_PROMPT_SUGGESTIONS", v, false)
		},
	}

	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			o.prompt = append(o.prompt, args[i+1:]...)
			break
		}
		name, inline, hasInline := a, "", false
		if strings.HasPrefix(a, "--") {
			if eq := strings.IndexByte(a, '='); eq >= 0 {
				name, inline, hasInline = a[:eq], a[eq+1:], true
			}
		}
		next := func() string {
			i++
			if i >= len(args) {
				fatal("flag %s requires a value", name)
			}
			return args[i]
		}
		switch {
		case boolFlags[name] != nil:
			if hasInline {
				fatal("flag %s does not take a value", name)
			}
			boolFlags[name]()
		case valueFlags[name] != nil:
			v := inline
			if !hasInline {
				v = next()
			}
			valueFlags[name](v)
		case optFlags[name] != nil:
			if hasInline {
				optFlags[name](inline)
			} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				optFlags[name](next())
			} else {
				optFlags[name]("")
			}
		case name == "-v" || name == "-V" || name == "--version":
			fmt.Println(version + " (Claude Code)")
			os.Exit(0)
		case name == "-h" || name == "--help":
			usage()
			os.Exit(0)
		case strings.HasPrefix(name, "--deep-link-") ||
			strings.HasPrefix(name, "--watch-artifact-"):
			// deep-link params are value flags
			v := inline
			if !hasInline {
				v = next()
			}
			os.Setenv(stubName(name), v)
		case strings.HasPrefix(a, "-"):
			fatal("unknown flag: %s", a)
		default:
			o.prompt = append(o.prompt, a)
		}
	}
	if o.execPrompt != "" {
		o.printMode = true
		o.prompt = append(o.prompt, o.execPrompt)
	}
	return o
}

// --- --betas ----------------------------------------------------------------

// baseBetaFlags mirrors api's unexported betaFlags const. The client builds
// the anthropic-beta header from it and then applies ANTHROPIC_CUSTOM_HEADERS
// on top — we ride that override to append --betas without touching
// internal/api. Keep in sync with internal/api/client.go.
const baseBetaFlags = "claude-code-20250219," +
	"oauth-2025-04-20," +
	"context-1m-2025-08-07," +
	"interleaved-thinking-2025-05-14," +
	"thinking-token-count-2026-05-13," +
	"context-management-2025-06-27," +
	"prompt-caching-scope-2026-01-05," +
	"mid-conversation-system-2026-04-07," +
	"per-turn-control-2026-07-01," +
	"mid-conversation-tool-changes-2026-07-01," +
	"advisor-tool-2026-03-01," +
	"effort-2025-11-24," +
	"dangerous-tool-use-2026-09-03," +
	"afk-mode-2026-01-31," +
	"extended-cache-ttl-2025-04-11," +
	"thinking-display-updates-2026-08-18" // EntryCLI flag; harmless on sdk-cli

func applyBetaFlags(extra string) {
	full := baseBetaFlags + "," + extra
	ch := os.Getenv("ANTHROPIC_CUSTOM_HEADERS")
	if ch != "" {
		ch += "\n"
	}
	os.Setenv("ANTHROPIC_CUSTOM_HEADERS", ch+"anthropic-beta: "+full)
}

// --- --agents -------------------------------------------------------------

type cliAgentDef struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Prompt      string   `json:"prompt"`
	Tools       []string `json:"-"`
	ToolsRaw    any      `json:"tools"`
}

func parseAgentDefs(js string) []cliAgentDef {
	// accept {"name": {def}} object or [{name,...}] array
	var arr []cliAgentDef
	if err := json.Unmarshal([]byte(js), &arr); err == nil {
		if len(arr) == 0 {
			fatal("--agents: empty agent list")
		}
		return finishAgentDefs(arr)
	}
	var obj map[string]struct {
		Description string `json:"description"`
		Prompt      string `json:"prompt"`
		Tools       any    `json:"tools"`
	}
	if err := json.Unmarshal([]byte(js), &obj); err != nil {
		fatal("invalid --agents JSON: %v", err)
	}
	for name, d := range obj {
		def := cliAgentDef{Name: name, Description: d.Description, Prompt: d.Prompt, ToolsRaw: d.Tools}
		arr = append(arr, def)
	}
	return finishAgentDefs(arr)
}

func finishAgentDefs(arr []cliAgentDef) []cliAgentDef {
	for i := range arr {
		if arr[i].Name == "" {
			fatal("--agents: entry %d missing \"name\"", i)
		}
		switch t := arr[i].ToolsRaw.(type) {
		case string:
			arr[i].Tools = []string{t}
		case []any:
			for _, v := range t {
				arr[i].Tools = append(arr[i].Tools, fmt.Sprint(v))
			}
		}
	}
	return arr
}

func renderAgentMD(d cliAgentDef) string {
	tools := strings.Join(d.Tools, ", ")
	if tools == "" {
		tools = "*"
	}
	return fmt.Sprintf("---\nname: %s\ndescription: %s\ntools: %s\n---\n%s\n",
		d.Name, d.Description, tools, d.Prompt)
}

// installCLIAgents materializes --agents JSON as agent definition files.
// Canonical copies live in ~/.claude-go/agents-cli/<name>.md; transient
// symlinks are placed in <cwd>/.claude/agents/ so internal/agent's
// file-based loader (envmsg listing, Agent tool, --agent) sees them —
// it only reads directories, and internal/ is off-limits. The symlinks
// are removed by the returned cleanup func.
func installCLIAgents(js, cwd string) func() {
	if js == "" {
		return func() {}
	}
	defs := parseAgentDefs(js)
	home, _ := os.UserHomeDir()
	storeDir := filepath.Join(home, ".claude-go", "agents-cli")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		fatal("--agents: %v", err)
	}
	projDir := filepath.Join(cwd, ".claude", "agents")
	// clear stale links from crashed runs before adding ours
	if ents, err := os.ReadDir(projDir); err == nil {
		for _, e := range ents {
			p := filepath.Join(projDir, e.Name())
			if dst, err := os.Readlink(p); err == nil && strings.HasPrefix(dst, storeDir) {
				os.Remove(p)
			}
		}
	}
	var links []string
	for _, d := range defs {
		target := filepath.Join(storeDir, d.Name+".md")
		if err := os.WriteFile(target, []byte(renderAgentMD(d)), 0o644); err != nil {
			fatal("--agents: %v", err)
		}
		link := filepath.Join(projDir, d.Name+".md")
		if fi, err := os.Lstat(link); err == nil && fi.Mode()&os.ModeSymlink == 0 {
			fmt.Fprintf(os.Stderr, "note: --agents %q conflicts with existing %s; project file wins\n", d.Name, link)
			continue
		}
		os.MkdirAll(projDir, 0o755)
		os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			fmt.Fprintf(os.Stderr, "note: --agents %q link failed: %v\n", d.Name, err)
			continue
		}
		links = append(links, link)
	}
	return func() {
		for _, l := range links {
			os.Remove(l)
		}
		// remove project agents dir if we left it empty
		if ents, err := os.ReadDir(projDir); err == nil && len(ents) == 0 {
			os.Remove(projDir)
		}
	}
}

// --- --worktree / --project ------------------------------------------------

// setupWorktree creates a git worktree at <cwd>/.claude/worktrees/<name>
// on a new branch worktree-<name> and returns the directory to cd into.
func setupWorktree(cwd, name, sessionID string) string {
	if name == "" {
		name = "claude-" + sessionID[:8]
	}
	root := filepath.Join(cwd, ".claude", "worktrees")
	wt := filepath.Join(root, name)
	if fi, err := os.Stat(wt); err == nil && fi.IsDir() {
		fmt.Fprintf(os.Stderr, "reusing existing worktree %s\n", wt)
		return wt
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "worktree: %v (staying in %s)\n", err, cwd)
		return cwd
	}
	branch := "worktree-" + name
	out, err := exec.Command("git", "-C", cwd, "worktree", "add", wt, "-b", branch).CombinedOutput()
	if err != nil {
		// branch may already exist — try attaching to it directly
		out2, err2 := exec.Command("git", "-C", cwd, "worktree", "add", wt, branch).CombinedOutput()
		if err2 != nil {
			fmt.Fprintf(os.Stderr, "worktree failed: %s%s (staying in %s)\n", out, out2, cwd)
			return cwd
		}
	}
	fmt.Fprintf(os.Stderr, "worktree %s at %s\n", branch, wt)
	return wt
}

// --- --resume-session-at ---------------------------------------------------

// resumeAtRec is the record view --resume-session-at loads (shared by the
// loader and the drops-turn guard).
type resumeAtRec struct {
	Type            string `json:"type"`
	UUID            string `json:"uuid"`
	ParentUUID      string `json:"parentUuid"`
	IsSidechain     bool   `json:"isSidechain"`
	IsMeta          bool   `json:"isMeta"`
	IsCompactSummary bool  `json:"isCompactSummary"`
	Message         struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// loadSessionAt rebuilds the message chain ending at a specific message
// uuid — session.Load always starts from the leaf; --resume-session-at
// resumes mid-chain.
func loadSessionAt(cwd, sessionID, atUUID, dropsTurn string) ([]api.Message, string, error) {
	return loadSessionAtPath(filepath.Join(config.ClaudeDir(), "projects",
		strings.ReplaceAll(filepath.Clean(cwd), "/", "-"), sessionID+".jsonl"), atUUID, dropsTurn)
}

// loadSessionAtPath is loadSessionAt on an arbitrary transcript file —
// needed when -r takes a path and --resume-session-at targets a uuid
// inside it. Also returns the leaf uuid — the parentUuid anchor a
// same-transcript resume continues from. dropsTurn is real's
// --resume-drops-turn <turn-uuid>: a safety declaration that every record
// discarded by resuming at atUUID belongs to that turn — refused when the
// tail contains anything else.
func loadSessionAtPath(p, atUUID, dropsTurn string) ([]api.Message, string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	type rec = resumeAtRec
	var records []rec
	byUUID := map[string]*rec{}
	var target *rec
	targetIdx := -1
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var r rec
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		records = append(records, r)
		if r.UUID != "" {
			cp := records[len(records)-1] // copy: appends may reallocate
			byUUID[r.UUID] = &cp
			if r.UUID == atUUID || strings.HasPrefix(r.UUID, atUUID) {
				target = byUUID[r.UUID]
				targetIdx = len(records) - 1
			}
		}
	}
	if target == nil {
		return nil, "", fmt.Errorf("message %s not found in %s", atUUID, p)
	}
	if dropsTurn != "" {
		if err := dropGuard(records[targetIdx+1:], dropsTurn); err != nil {
			return nil, "", fmt.Errorf("resuming at %s would discard entries not attributable to turn %s: %s",
				atUUID, dropsTurn, err)
		}
	}
	var chain []rec
	seen := map[string]bool{}
	for cur := target; cur != nil; {
		if seen[cur.UUID] {
			break // cyclic parentUuid chain (corrupt transcript) — stop
		}
		seen[cur.UUID] = true
		chain = append(chain, *cur)
		cur = byUUID[cur.ParentUUID]
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	var msgs []api.Message
	for _, r := range chain {
		if r.IsSidechain || r.IsMeta || r.Message.Role == "" {
			continue
		}
		msgs = append(msgs, api.Message{Role: r.Message.Role, Content: r.Message.Content})
	}
	return msgs, target.UUID, nil
}

// dropGuard is real's --resume-drops-turn check (vg in the decompile):
// the caller declares that everything discarded by the resume-at — the
// transcript tail after the anchor — belongs to one turn, turnID. The
// first discarded entry must BE that turn's prompt; anything else
// (another user prompt, a compaction summary, externally-sourced input)
// refuses. Meta records, sidechains, tool results, and furniture pass.
func dropGuard(tail []resumeAtRec, turnID string) error {
	if !uuidShape(turnID) {
		return fmt.Errorf("declared turn id is not a UUID: %s", turnID)
	}
	i := 0
	for i < len(tail) && tail[i].Type != "user" {
		i++ // skip leading non-user furniture (system/progress/attachment)
	}
	if i == len(tail) {
		return nil
	}
	first := tail[i]
	if first.UUID != turnID {
		return fmt.Errorf("range does not start with the declared turn prompt")
	}
	if first.IsMeta || promptless(first.Message.Content) {
		return fmt.Errorf("declared turn id names a non-prompt user entry")
	}
	for _, r := range tail[i+1:] {
		if r.Type != "user" {
			continue // assistant/progress/system/attachment all pass
		}
		if r.IsMeta || r.IsSidechain || promptless(r.Message.Content) {
			continue // tool results, meta entries, sidechain chatter
		}
		if r.IsCompactSummary {
			return fmt.Errorf("range contains a compaction summary")
		}
		return fmt.Errorf("range contains a user turn not attributable to the declared turn")
	}
	return nil
}

// promptless reports whether a user-message content carries no real text
// prompt — tool_result payloads ride the user role but aren't a turn start.
func promptless(content json.RawMessage) bool {
	return !strings.Contains(string(content), `"type":"text"`)
}

func uuidShape(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		want := i == 8 || i == 13 || i == 18 || i == 23
		if (c == '-') != want {
			return false
		}
		if c != '-' && !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
