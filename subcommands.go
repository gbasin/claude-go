package main

import (
	"context"
	"bufio"
	"claude-go/internal/config"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"claude-go/internal/api"
	"claude-go/internal/auth"
	"claude-go/internal/bg"
	"claude-go/internal/bridge"
	"claude-go/internal/mcp"
	"claude-go/internal/netproxy"
	"claude-go/internal/plugins"
	"claude-go/internal/session"
	"claude-go/internal/tools"
)

// mcpSub implements `claude-go mcp list|add|remove`.
func mcpSub(args []string) {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list":
		cfgs := mcp.LoadConfigs(mustGetwd())
		pending := pendingMcpjson(mustGetwd())
		if len(cfgs) == 0 && len(pending) == 0 {
			fmt.Println("no MCP servers configured")
			return
		}
		for name, c := range cfgs {
			tgt := c.Command
			if c.URL != "" {
				tgt = c.URL
			}
			fmt.Printf("%s: %s %v (%s)\n", name, tgt, c.Args, c.Type)
		}
		for name := range pending {
			fmt.Printf("%s: ⏸ Pending approval\n", name)
		}
	case "add":
		// claude-go mcp add [--transport http|sse|stdio|ws] [--scope user|local|project]
		//   [-e KEY=VAL]... [--header "K: V"]... <name> <command-or-url> [args...]
		transport, scope := "stdio", "local"
		var env, headers, pos []string
		rest := args[1:]
		// flags may appear anywhere before `--` (commander semantics);
		// after `--` every token is part of the command line
		for len(rest) > 0 && rest[0] != "--" {
			switch rest[0] {
			case "--transport":
				transport = rest[1]
				rest = rest[2:]
			case "--scope":
				scope = rest[1]
				rest = rest[2:]
			case "-e", "--env":
				env = append(env, rest[1])
				rest = rest[2:]
			case "--header":
				headers = append(headers, rest[1])
				rest = rest[2:]
			default:
				pos = append(pos, rest[0])
				rest = rest[1:]
			}
		}
		if len(rest) > 0 && rest[0] == "--" {
			rest = rest[1:]
		}
		rest = append(pos, rest...)
		if len(rest) < 2 {
			fatal("usage: claude-go mcp add [--transport t] [--scope s] [-e K=V] [--header 'K: V'] <name> <command|url> [args...]")
		}
		name, tgt := rest[0], rest[1]
		cfg := map[string]any{}
		if transport == "stdio" {
			cfg = map[string]any{"type": "stdio", "command": tgt, "args": rest[2:]}
		} else {
			cfg = map[string]any{"type": transport, "url": tgt}
		}
		if len(env) > 0 {
			m := map[string]string{}
			for _, kv := range env {
				if i := strings.Index(kv, "="); i > 0 {
					m[kv[:i]] = kv[i+1:]
				}
			}
			cfg["env"] = m
		}
		if len(headers) > 0 {
			m := map[string]string{}
			for _, kv := range headers {
				if i := strings.Index(kv, ":"); i > 0 {
					m[strings.TrimSpace(kv[:i])] = strings.TrimSpace(kv[i+1:])
				}
			}
			cfg["headers"] = m
		}
		writeMCPServer(scope, name, cfg)
		fmt.Printf("added %s (%s) [%s scope]\n", name, transport, scope)
	case "add-json":
		if len(args) < 3 {
			fatal("usage: claude-go mcp add-json [--scope s] <name> <json>")
		}
		scope := "local"
		rest := args[1:]
		if rest[0] == "--scope" && len(rest) > 2 {
			scope, rest = rest[1], rest[2:]
		}
		var cfg map[string]any
		if err := json.Unmarshal([]byte(rest[1]), &cfg); err != nil {
			fatal("invalid server json: " + err.Error())
		}
		writeMCPServer(scope, rest[0], cfg)
		fmt.Printf("added %s [%s scope]\n", rest[0], scope)
	case "reset-project-choices":
		editLocalSettings(func(m map[string]any) {
			delete(m, "enabledMcpjsonServers")
			delete(m, "disabledMcpjsonServers")
		})
		fmt.Println("project .mcp.json approvals reset")
	case "get":
		if len(args) < 2 {
			fatal("usage: claude-go mcp get <name>")
		}
		cfgs := mcp.LoadConfigs(mustGetwd())
		if c, ok := cfgs[args[1]]; ok {
			b, _ := json.MarshalIndent(c, "", "  ")
			fmt.Println(string(b))
			return
		}
		if _, ok := pendingMcpjson(mustGetwd())[args[1]]; ok {
			fmt.Printf("%s: ⏸ Pending approval (project .mcp.json)\n", args[1])
			return
		}
		fatal("no such server: " + args[1])
	case "remove", "rm":
		if len(args) < 2 {
			fatal("usage: claude-go mcp remove <name>")
		}
		editClaudeJSON(func(m map[string]any) {
			if servers, ok := m["mcpServers"].(map[string]any); ok {
				delete(servers, args[1])
			}
		})
		fmt.Printf("removed %s\n", args[1])
	case "logout":
		if len(args) < 2 {
			fatal("usage: claude-go mcp logout <name>")
		}
		if err := mcp.DeleteServerAuth(args[1]); err != nil {
			fatal("logout failed: " + err.Error())
		}
		fmt.Printf("cleared OAuth credentials for %s\n", args[1])
	case "serve":
		// real's `claude mcp serve` — run as an MCP server over stdio,
		// exposing the builtin tool registry to MCP clients
		os.Exit(mcp.ServeStdio(mustGetwd()))
	case "login":
		if len(args) < 2 {
			fatal("usage: claude-go mcp login <name>")
		}
		cfgs := mcp.LoadConfigs(mustGetwd())
		c, ok := cfgs[args[1]]
		if !ok || c.URL == "" {
			fatal("no such remote server: " + args[1])
		}
		if err := mcp.RunOAuthFlow(ctxb(), args[1], c.URL); err != nil {
			fatal("login failed: " + err.Error())
		}
		fmt.Printf("logged in to %s\n", args[1])
	default:
		fatal("unknown mcp subcommand: " + args[0] + " (list|add|add-json|get|remove|login|logout|reset-project-choices)")
	}
}

// pendingMcpjson lists project .mcp.json servers that aren't approved —
// real shows them "⏸ Pending approval" and skips connecting.
func pendingMcpjson(cwd string) map[string]mcp.ServerConfig {
	out := map[string]mcp.ServerConfig{}
	b, err := os.ReadFile(filepath.Join(cwd, ".mcp.json"))
	if err != nil {
		return out
	}
	var top struct {
		Servers map[string]mcp.ServerConfig `json:"mcpServers"`
	}
	if json.Unmarshal(b, &top) != nil {
		return out
	}
	st := config.Load(cwd)
	for n := range top.Servers {
		if st.EnableAllProjectMcpServers {
			continue
		}
		if !hasStr(st.DisabledMcpjsonServers, n) && !hasStr(st.EnabledMcpjsonServers, n) {
			out[n] = top.Servers[n]
		}
	}
	return out
}

// writeMCPServer persists a server entry at the given scope:
// user → ~/.claude.json, local → ~/.claude.json (real's local scope),
// project → <cwd>/.mcp.json.
func writeMCPServer(scope, name string, cfg map[string]any) {
	if scope == "project" {
		p := filepath.Join(mustGetwd(), ".mcp.json")
		m := map[string]any{}
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &m)
		}
		servers, _ := m["mcpServers"].(map[string]any)
		if servers == nil {
			servers = map[string]any{}
		}
		servers[name] = cfg
		m["mcpServers"] = servers
		b, _ := json.MarshalIndent(m, "", "  ")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			fatal("write .mcp.json: " + err.Error())
		}
		return
	}
	// user + local both persist to ~/.claude.json in our model
	editClaudeJSON(func(m map[string]any) {
		servers, _ := m["mcpServers"].(map[string]any)
		if servers == nil {
			servers = map[string]any{}
		}
		servers[name] = cfg
		m["mcpServers"] = servers
	})
}

// editLocalSettings rewrites <cwd>/.claude/settings.local.json in place.
func editLocalSettings(edit func(map[string]any)) {
	p := filepath.Join(mustGetwd(), ".claude", "settings.local.json")
	m := map[string]any{}
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	edit(m)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		fatal("write settings.local.json: " + err.Error())
	}
}

func ctxb() context.Context { return context.Background() }

func doctorSub() {
	fmt.Println("claude-go", api.Version)
	fmt.Println("binary: self-contained Go, no runtime deps")
	cwd := mustGetwd()
	fmt.Println("cwd:", cwd)
	if out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output(); err == nil {
		fmt.Println("git root:", string(out[:len(out)-1]))
	}
	for _, p := range []string{
		filepath.Join(config.Home(), ".claude.json"),
		filepath.Join(config.ClaudeDir(), "settings.json"),
		filepath.Join(config.ClaudeDir(), "CLAUDE.md"),
	} {
		st := "missing"
		if _, err := os.Stat(p); err == nil {
			st = "ok"
		}
		fmt.Printf("  %-55s %s\n", p, st)
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		fmt.Println("auth: ANTHROPIC_API_KEY set")
	} else {
		fmt.Println("auth: keychain/credentials file")
	}
}

func configSub(args []string) {
	if len(args) < 1 {
		fatal("usage: claude-go config get|set|list [key] [value]")
	}
	switch args[0] {
	case "list":
		b, _ := json.MarshalIndent(config.UserSettings(), "", "  ")
		fmt.Println(string(b))
	case "get":
		if len(args) < 2 {
			fatal("usage: claude-go config get <key>")
		}
		if v, ok := config.UserSettings()[args[1]]; ok {
			out, _ := json.Marshal(v)
			fmt.Println(string(out))
		}
	case "set":
		if len(args) < 3 {
			fatal("usage: claude-go config set <key> <value>")
		}
		var v any
		if json.Unmarshal([]byte(args[2]), &v) == nil {
			config.WriteUserSetting(args[1], v)
		} else {
			config.WriteUserSetting(args[1], args[2])
		}
		fmt.Printf("set %s\n", args[1])
	default:
		fatal("unknown config subcommand: %s", args[0])
	}
}

func editClaudeJSON(edit func(map[string]any)) {
	m, p := config.ReadClaudeJSON()
	edit(m)
	config.WriteClaudeJSON(p, m)
}

func mustGetwd() string {
	d, _ := os.Getwd()
	return d
}

// authSub: claude auth login|status|logout
func authSub(args []string) {
	if len(args) == 0 {
		args = []string{"status"}
	}
	switch args[0] {
	case "login":
		authLoginSub()
	case "logout":
		if runtime.GOOS == "darwin" {
			exec.Command("security", "delete-generic-password",
				"-s", "Claude Code-credentials", "-a", os.Getenv("USER")).Run()
		}
		fmt.Println("logged out")
	case "status":
		creds, err := auth.Load()
		if err != nil {
			fmt.Println("not logged in")
			return
		}
		kind := "oauth"
		if creds.IsAPIKey {
			kind = "api_key"
		}
		fmt.Printf("logged in (%s)", kind)
		if creds.ExpiresAt > 0 {
			fmt.Printf(" · expires %s", time.UnixMilli(creds.ExpiresAt).Format("2006-01-02 15:04"))
		}
		fmt.Println()
	default:
		fatal("unknown auth subcommand: %s (login|status|logout)", args[0])
	}
}

// purgeSub: claude purge — delete all local project state.
func purgeSub(args []string) {
	dryRun := false
	all := false
	for _, a := range args[0:] {
		switch a {
		case "--dry-run":
			dryRun = true
		case "--all":
			all = true
		case "-y":
		}
	}
	cwd, _ := os.Getwd()
	targets := []string{filepath.Join(config.ClaudeDir(), "projects", session.Slug(cwd))}
	if all {
		targets = append(targets, filepath.Join(cwd, ".claude"))
	}
	for _, t := range targets {
		if dryRun {
			fmt.Println("would remove:", t)
			continue
		}
		if err := os.RemoveAll(t); err != nil {
			fmt.Fprintf(os.Stderr, "purge %s: %v\n", t, err)
		} else {
			fmt.Println("removed:", t)
		}
	}
}

// pluginSub: claude plugin marketplace add|list | install|list|remove
func pluginSub(args []string) {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "marketplace":
		if len(args) < 3 {
			fatal("usage: claude plugin marketplace add <repo|url|path> | list")
		}
		switch args[1] {
		case "add":
			name, err := plugins.AddMarketplace(args[2])
			if err != nil {
				fatal("marketplace add: %v", err)
			}
			fmt.Println("added marketplace", name)
		case "list":
			mkts := plugins.ListMarketplaces()
			if len(mkts) == 0 {
				fmt.Println("no marketplaces")
				return
			}
			for n, p := range mkts {
				fmt.Printf("%s: %s\n", n, p)
			}
		case "update":
			for n, p := range plugins.ListMarketplaces() {
				if strings.HasPrefix(p, plugins.SyncedBase()) || strings.Contains(p, "/marketplaces/") {
					out, err := exec.Command("git", "-C", p, "pull", "--ff-only").CombinedOutput()
					if err != nil {
						fmt.Fprintf(os.Stderr, "%s: pull failed: %s\n", n, out)
					} else {
						fmt.Println(n, "updated")
					}
				}
			}
		}
	case "install", "i":
		if len(args) < 2 {
			fatal("usage: claude plugin install <name[@marketplace]>")
		}
		name, mkt := args[1], ""
		if i := strings.Index(name, "@"); i > 0 {
			name, mkt = name[:i], name[i+1:]
		}
		if mkt == "" {
			mkts := plugins.ListMarketplaces()
			for n := range mkts {
				mkt = n
				break
			}
		}
		if err := plugins.Install(mkt, name); err != nil {
			fatal("install: %v", err)
		}
		fmt.Println("installed", name)
	case "list":
		inst := plugins.List()
		if len(inst) == 0 {
			fmt.Println("no plugins installed")
			return
		}
		for _, p := range inst {
			state := "enabled"
			if !p.Enabled {
				state = "disabled"
			}
			fmt.Printf("%s@%s  %s  %s\n", p.Name, p.Marketplace, p.Version, state)
		}
	case "remove", "rm", "uninstall":
		if len(args) < 2 {
			fatal("usage: claude plugin remove <name>")
		}
		if err := plugins.Remove(args[1]); err != nil {
			fatal("remove: %v", err)
		}
		fmt.Println("removed", args[1])
	case "details":
		if len(args) < 2 {
			fatal("usage: claude plugin details <marketplace>")
		}
		m, err := plugins.Marketplace(args[1])
		if err != nil {
			fatal("%v", err)
		}
		fmt.Println("marketplace:", m.Name)
		for _, p := range m.Plugins {
			fmt.Printf("  %s — %s\n", p.Name, p.Description)
		}
	default:
		fatal("unknown plugin subcommand: %s (list|install|remove|marketplace|details)", args[0])
	}
}

// agentsSub: claude agents — list agent types (built-in + file-loaded +
// --agents-cli store) and background tasks. Bg tasks live inside a running
// session's process, so a fresh invocation can only report none.
func agentsSub(args []string) {
	cwd := mustGetwd()
	fmt.Println("agent types:")
	for _, n := range []string{
		"general-purpose", "Explore", "Plan", "claude",
		"code-simplifier", "code-reviewer", "debugger", "statusline-setup",
	} {
		fmt.Printf("  %s (built-in)\n", n)
	}
	seen := map[string]bool{}
	for _, dir := range []string{
		filepath.Join(config.ClaudeDir(), "agents"),
		filepath.Join(cwd, ".claude", "agents"),
		filepath.Join(config.Home(), ".claude-go", "agents-cli"),
	} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".md")
			desc := ""
			if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
				fm := quickFrontmatter(string(b))
				if fm["name"] != "" {
					name = fm["name"]
				}
				desc = fm["description"]
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			fmt.Printf("  %s — %s (%s)\n", name, desc, dir)
		}
	}
	fmt.Println("\nbackground tasks: none (tasks live inside a running session)")
	fmt.Println("local bus agents: none (no messaging socket)")
	jobs := bg.List()
	fmt.Println("\nbackground sessions:")
	if pid := bg.DaemonPID(); pid != 0 {
		fmt.Printf("  (daemon running, pid %d)\n", pid)
	}
	if len(jobs) == 0 {
		fmt.Println("  none")
	}
	for _, j := range jobs {
		what := j.Prompt
		if what == "" {
			what = j.Name
		}
		if what == "" {
			what = "(idle)"
		}
		if len(what) > 60 {
			what = what[:60] + "…"
		}
		// real's fleet view marks attach state + version skew
		marks := ""
		if j.Attached > 0 {
			marks += "  attached"
		}
		if j.Version != "" && j.Version != version {
			marks += fmt.Sprintf("  ↑v%s→%s", j.Version, version)
		}
		fmt.Printf("  %s  %s  %s ago  %s%s\n", j.Short, j.CWD,
			time.Since(j.StartedAt).Round(time.Second), what, marks)
	}
}

// bgHostSub is the hidden bg-pty-host worker — detached by Spawn,
// never invoked by hand. Owns the session's pty + rendezvous socket.
func bgHostSub(args []string) {
	var o bg.HostOpts
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--session":
			i++
			o.SessionID = args[i]
		case "--cwd":
			i++
			o.CWD = args[i]
		case "--resume-from":
			i++
			o.ResumeFrom = args[i]
		case "--prompt":
			i++
			o.Prompt = args[i]
		case "--name":
			i++
			o.Name = args[i]
		}
	}
	if o.SessionID == "" {
		fmt.Fprintln(os.Stderr, "bg-host: --session required")
		os.Exit(2)
	}
	if o.CWD == "" {
		o.CWD = mustGetwd()
	}
	os.Exit(bg.RunHost(o))
}

// attachSub: claude attach <id> — live-attach this terminal to a running
// background session (real's attach; Ctrl+Z inside detaches).
func attachSub(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: claude attach <session-id|prefix|name>")
		os.Exit(2)
	}
	if err := bg.Attach(args[0], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// stopSub is `claude stop <id>` — ends a running background session. The
// transcript stays on disk (real's stop keeps transcript + worktree).
func stopSub(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: claude stop <session-id|prefix|name>")
		os.Exit(2)
	}
	j, err := bg.Lookup(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	short := args[0]
	if j != nil {
		short = j.Short
	}
	if err := bg.Stop(args[0]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("session %s stopped\n", short)
}

// daemonSub is `claude daemon` — the background-session supervisor:
// `run` foreground-sweeps respawning dead workers; `start` detaches one;
// `status`/`stop` manage it. Real's daemon is the service arm of the bg
// architecture — ours is a single-process supervisor.
func daemonSub(args []string) {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "run":
		sweep := 0
		if v := os.Getenv("CLAUDE_GO_BG_SWEEP_MS"); v != "" {
			sweep, _ = strconv.Atoi(v)
		}
		os.Exit(bg.RunDaemon(version, time.Duration(sweep)*time.Millisecond))
	case "start":
		if bg.DaemonAlive() {
			fmt.Println("daemon already running")
			return
		}
		if _, err := bg.StartDaemon(); err != nil {
			fmt.Fprintln(os.Stderr, "daemon start:", err)
			os.Exit(1)
		}
		fmt.Println("daemon started")
	case "stop":
		if err := bg.StopDaemon(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("daemon stopped")
	case "install":
		// real's daemon is an installable service — ours registers a
		// LaunchAgent (macOS) or user systemd unit (linux)
		if p, err := bg.InstallDaemon(); err != nil {
			fmt.Fprintln(os.Stderr, "daemon install:", err)
			os.Exit(1)
		} else {
			fmt.Printf("daemon service installed: %s\n", p)
		}
	case "uninstall":
		if err := bg.UninstallDaemon(); err != nil {
			fmt.Fprintln(os.Stderr, "daemon uninstall:", err)
			os.Exit(1)
		}
		fmt.Println("daemon service removed")
	case "status":
		if pid := bg.DaemonPID(); pid != 0 {
			jobs := bg.List()
			fmt.Printf("daemon running (pid %d) — %d background session(s)\n", pid, len(jobs))
		} else {
			fmt.Println("daemon not running — `claude daemon start`")
		}
	default:
		fmt.Fprintf(os.Stderr, "usage: claude daemon [run|start|status|stop|install|uninstall]\n")
		os.Exit(2)
	}
}

func quickFrontmatter(s string) map[string]string {
	out := map[string]string{}
	if !strings.HasPrefix(s, "---") {
		return out
	}
	rest := s[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return out
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(strings.Trim(v, " \t\"'"))
		}
	}
	return out
}

// importSub: claude import <file> — bring a transcript into this project's
// sessions dir. Accepts a .jsonl transcript (copied under a fresh session id)
// or a JSON array of {role,content} messages.
func importSub(args []string) {
	if len(args) < 1 {
		fatal("usage: claude-go import <transcript.jsonl|messages.json>")
	}
	src := args[0]
	b, err := os.ReadFile(src)
	if err != nil {
		fatal("import: %v", err)
	}
	cwd := mustGetwd()
	newID := session.NewID()

	// JSON array of messages?
	var msgs []api.Message
	if json.Unmarshal(b, &msgs) == nil && len(msgs) > 0 && msgs[0].Role != "" {
		st, err := session.Open(cwd, newID)
		if err != nil {
			fatal("import: %v", err)
		}
		for _, m := range msgs {
			st.LogMessage(m)
		}
		st.Close()
		fmt.Printf("imported %d messages → %s\n", len(msgs), st.Path)
		return
	}

	// JSONL transcript — validate then copy verbatim
	valid := 0
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var v map[string]any
		if json.Unmarshal([]byte(line), &v) != nil {
			fatal("import: line %d is not JSON — not a transcript?", valid+1)
		}
		valid++
	}
	if valid == 0 {
		fatal("import: empty or unrecognized file")
	}
	st, err := session.Open(cwd, newID)
	if err != nil {
		fatal("import: %v", err)
	}
	st.Close()
	// rewrite sessionId fields so the import doesn't collide with the source
	var out strings.Builder
	sc = bufio.NewScanner(strings.NewReader(string(b)))
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var v map[string]any
		if json.Unmarshal([]byte(line), &v) == nil {
			v["sessionId"] = newID
			lb, _ := json.Marshal(v)
			out.Write(lb)
			out.WriteByte('\n')
		}
	}
	if err := os.WriteFile(st.Path, []byte(out.String()), 0o644); err != nil {
		fatal("import: %v", err)
	}
	fmt.Printf("imported %d records → %s\n", valid, st.Path)
}

// setupTokenSub: claude setup-token — store a long-lived token the same
// place auth.Load looks: macOS Keychain or ~/.claude/.credentials.json.
func setupTokenSub() {
	fmt.Println("Set up a long-lived token for claude-go.")
	fmt.Println("  • API key: create one at https://console.anthropic.com/settings/keys")
	fmt.Println("  • OAuth token: paste an existing claude setup-token / CLAUDE_CODE_OAUTH_TOKEN value")
	fmt.Print("\nPaste token: ")
	var tok string
	fmt.Scanln(&tok)
	tok = strings.TrimSpace(tok)
	if tok == "" {
		fatal("no token entered")
	}
	creds := &auth.Credentials{
		AccessToken: tok,
		IsAPIKey:    strings.HasPrefix(tok, "sk-ant-"),
	}
	auth.SaveOwnCreds(creds)
	fmt.Println("stored in ~/.claude-go/.credentials.json")
}

// remoteControlSub: `claude remote-control` / `claude rc` — the Remote
// Control enable path (real: mod_0690.js enterRemoteControl). The gate and
// poll-loop skeleton live in internal/bridge; spawning local REPL sessions
// for incoming work is backend-gated and intentionally not faked.
func remoteControlSub(args []string) {
	creds, credErr := auth.Load()
	hasOAuth := credErr == nil && creds != nil && !creds.IsAPIKey && creds.AccessToken != ""
	settings := config.Load(mustGetwd())
	deps := &bridge.Deps{
		DisableRemoteControl: settings.DisableRemoteControl,
		HasOAuth:             hasOAuth,
		Version:              api.Version,
	}
	if hasOAuth {
		deps.Token = func() string {
			t, err := creds.Token()
			if err != nil {
				return ""
			}
			return t
		}
		deps.Refresh = func() bool { return creds.Refresh() == nil }
	}
	deps.OnSessionID = tools.SetBridgeSessionID
	if code := bridge.RunCLI(args, deps); code != 0 {
		os.Exit(code)
	}
}

// sandboxSub: claude-go sandbox trust — install the local MITM CA into the
// OS trust store so sandboxed commands (git/curl and friends that don't read
// SSL_CERT_FILE) verify the minted leaf certificates.
func sandboxSub(args []string) {
	if len(args) == 0 {
		fatal("usage: claude-go sandbox trust")
	}
	switch args[0] {
	case "trust":
		if _, err := netproxy.LoadOrCreateCA(netproxy.CADir()); err != nil {
			fatal("sandbox trust: %v", err)
		}
		msg, err := netproxy.InstallCA()
		if err != nil {
			fatal("sandbox trust: %v", err)
		}
		fmt.Println(msg)
	default:
		fatal("unknown sandbox subcommand: %s (trust)", args[0])
	}
}

func hasStr(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}
