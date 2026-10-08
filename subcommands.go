package main

import (
	"bufio"
	"claude-go/internal/config"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
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

	"golang.org/x/term"
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
		// real's mcp list probes each server and shows connected status
		if len(cfgs) > 0 {
			fmt.Println("Checking MCP server health…")
			fmt.Println()
		}
		type row struct {
			name, tgt, typ, status string
			disabled               bool
		}
		var rows []row
		var wg2 sync.WaitGroup
		var mu2 sync.Mutex

		for name, c := range cfgs {
			tgt := c.Command
			if c.URL != "" {
				tgt = c.URL
			}
			r := row{name: name, tgt: tgt, typ: c.Type}
			if c.Enabled != nil && !*c.Enabled {
				r.disabled = true
				r.status = "disabled"
				rows = append(rows, r)
				continue
			}
			rows = append(rows, r)
			wg2.Add(1)
			go func(name string, c mcp.ServerConfig, i int) {
				defer wg2.Done()
				cl, err := mcp.Connect(name, c)
				mu2.Lock()
				if err != nil {
					rows[i].status = "✗ Failed to connect"
				} else {
					rows[i].status = fmt.Sprintf("✓ Connected (%d tools)", len(cl.Tools))
					cl.Close()
				}
				mu2.Unlock()
			}(name, c, len(rows)-1)
		}
		wg2.Wait()
		sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
		for _, r := range rows {
			fmt.Printf("%s: %s (%s) — %s\n", r.name, r.tgt, r.typ, r.status)
		}
		for name := range pending {
			fmt.Printf("%s: ⏸ Pending approval (run `claude` to approve)\n", name)
		}
	case "add":
		// claude-go mcp add [-s|--scope user|local|project] [-t|--transport stdio|sse|http|ws]
		//   [-e KEY=VAL]... [-H|--header "K: V"]... [--client-id id]
		//   [--client-secret] [--callback-port N] <name> <command-or-url> [args...]
		transport, scope := "stdio", "local"
		var env, headers, pos []string
		var clientID, clientSecret string
		callbackPort := 0
		wantSecret := false
		rest := args[1:]
		// flags may appear anywhere before `--` (commander semantics);
		// after `--` every token is part of the command line
		for i := 0; i < len(rest) && rest[i] != "--"; i++ {
			a := rest[i]
			val := func() string {
				if i+1 >= len(rest) {
					fatal("missing value for " + a)
				}
				i++
				return rest[i]
			}
			switch {
			case a == "-t" || a == "--transport":
				transport = val()
			case a == "-s" || a == "--scope":
				scope = val()
			case strings.HasPrefix(a, "--transport="):
				transport = a[len("--transport="):]
			case strings.HasPrefix(a, "--scope="):
				scope = a[len("--scope="):]
			case a == "-e" || a == "--env":
				env = append(env, val())
			case a == "-H" || a == "--header":
				headers = append(headers, val())
			case a == "--client-id":
				clientID = val()
			case a == "--client-secret":
				wantSecret = true
			case a == "--callback-port":
				p, err := strconv.Atoi(val())
				if err != nil || p <= 0 || p > 65535 {
					fatal("--callback-port must be a port number")
				}
				callbackPort = p
			case strings.HasPrefix(a, "-") && len(pos) == 0:
				fatal("unknown flag: " + a)
			default:
				// positionals — and once they start, flag-shaped tokens are
				// the command's own args (mcp add srv mybin --serve)
				pos = append(pos, a)
			}
		}
		// keep only the tokens after `--`
		var tail []string
		for i, a := range rest {
			if a == "--" {
				tail = rest[i+1:]
				break
			}
		}
		rest = append(pos, tail...)
		rest = append(pos, rest...)
		// real validates both enumerations before touching config
		switch scope {
		case "local", "user", "project":
		default:
			fatal("invalid scope %q — expected local, user, or project", scope)
		}
		if transport == "streamable-http" {
			transport = "http" // real's alias
		}
		switch transport {
		case "stdio", "sse", "http", "ws":
		default:
			fatal("invalid transport %q — expected stdio, sse, http, or ws", transport)
		}
		if wantSecret {
			clientSecret = os.Getenv("MCP_CLIENT_SECRET")
			if clientSecret == "" {
				clientSecret = promptSecret("OAuth client secret: ")
			}
		}
		if len(rest) < 2 {
			fatal("usage: claude-go mcp add [-s scope] [-t transport] [-e K=V] [-H 'K: V'] [--client-id id] [--client-secret] [--callback-port N] <name> <command|url> [args...]")
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
		if clientID != "" || callbackPort > 0 {
			o := map[string]any{}
			if clientID != "" {
				o["clientId"] = clientID
			}
			if callbackPort > 0 {
				o["callbackPort"] = callbackPort
			}
			cfg["oauth"] = o
		}
		writeMCPServer(scope, name, cfg)
		// the secret goes to the credential store's mcpOAuthClientConfig
		// keyed Hl(name,cfg) — never plaintext into the config file
		if clientSecret != "" {
			var sc mcp.ServerConfig
			if b, e := json.Marshal(cfg); e == nil {
				_ = json.Unmarshal(b, &sc)
			}
			if err := mcp.SaveClientSecret(name, sc, clientSecret); err != nil {
				fmt.Fprintf(os.Stderr, "warning: client secret not stored: %v\n", err)
			}
		}
		fmt.Printf("added %s (%s) [%s scope]\n", name, transport, scope)
	case "add-json":
		// real: add-json [-s|--scope s] <name> '<json>' — JSON is validated,
		// including the transport type enum (stdio|sse|http|ws|sdk|…)
		scope := "local"
		rest := args[1:]
		for len(rest) > 2 {
			switch {
			case rest[0] == "-s" || rest[0] == "--scope":
				scope, rest = rest[1], rest[2:]
			case strings.HasPrefix(rest[0], "--scope="):
				scope, rest = rest[0][len("--scope="):], rest[1:]
			default:
				goto parsed
			}
		}
	parsed:
		switch scope {
		case "local", "user", "project":
		default:
			fatal("invalid scope %q — expected local, user, or project", scope)
		}
		if len(rest) < 2 {
			fatal("usage: claude-go mcp add-json [-s scope] <name> '<json>'")
		}
		var cfg map[string]any
		if err := json.Unmarshal([]byte(rest[1]), &cfg); err != nil {
			fatal("invalid server json: " + err.Error())
		}
		if ty, _ := cfg["type"].(string); ty != "" {
			switch ty {
			case "stdio", "sse", "http", "ws", "sdk", "claudeai-proxy", "streamable-http":
				if ty == "streamable-http" {
					cfg["type"] = "http"
				}
			default:
				fatal("invalid server type %q — expected stdio, sse, http, or ws", ty)
			}
		}
		if cfg["command"] == nil && cfg["url"] == nil && cfg["type"] != "sdk" {
			fatal("invalid server json: needs command (stdio) or url (sse/http/ws)")
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
		// real: remove [-s|--scope s] <name> — without a scope it removes
		// from whichever scope holds the entry; with one, a scoped miss is
		// an error naming the scope.
		scope := ""
		rest := args[1:]
		for len(rest) > 1 {
			switch {
			case rest[0] == "-s" || rest[0] == "--scope":
				scope, rest = rest[1], rest[2:]
			case strings.HasPrefix(rest[0], "--scope="):
				scope, rest = rest[0][len("--scope="):], rest[1:]
			default:
				goto rmParsed
			}
		}
	rmParsed:
		if len(rest) < 1 {
			fatal("usage: claude-go mcp remove [-s scope] <name>")
		}
		name := rest[0]
		if scope != "" {
			if !removeMCPServer(scope, name) {
				fatal("No MCP server named %s in %s scope", name, scope)
			}
			fmt.Printf("removed %s\n", name)
			return
		}
		for _, sc := range []string{"local", "user", "project"} {
			if removeMCPServer(sc, name) {
				fmt.Printf("removed %s (%s scope)\n", name, sc)
				return
			}
		}
		fatal("No MCP server named %s", name)
	case "logout":
		if len(args) < 2 {
			fatal("usage: claude-go mcp logout <name>")
		}
		cfg := mcp.LoadConfigs(mustGetwd())[args[1]]
		// real's mcp_oauth_revoke: RFC 7009-revoke the held tokens at the
		// server's revocation_endpoint, then drop the local record
		if err := mcp.RevokeServerAuth(ctxb(), args[1], cfg); err != nil {
			fatal("logout failed: " + err.Error())
		}
		fmt.Printf("cleared OAuth credentials for %s\n", args[1])
	case "login":
		if len(args) < 2 {
			fatal("usage: claude-go mcp login <name>")
		}
		cfgs := mcp.LoadConfigs(mustGetwd())
		c, ok := cfgs[args[1]]
		if !ok || c.URL == "" {
			fatal("no such remote server: " + args[1])
		}
		if err := mcp.RunOAuthFlow(ctxb(), args[1], c); err != nil {
			fatal("login failed: " + err.Error())
		}
		fmt.Printf("logged in to %s\n", args[1])
	case "serve":
		// real's `claude mcp serve` — expose the tool surface as an MCP
		// server over stdio
		var o mcp.ServeOpts
		for _, a := range args[1:] {
			switch a {
			case "-d", "--debug":
				o.Debug = true
			case "--verbose":
				o.Verbose = true
			}
		}
		os.Exit(mcp.Serve(ctxb(), os.Stdin, os.Stdout, os.Stderr, o))
	case "add-from-claude-desktop":
		// real imports the Claude Desktop config (macOS / WSL) — the file
		// maps mcpServers just like .mcp.json
		scope := "local"
		rest := args[1:]
		for len(rest) > 0 {
			switch {
			case rest[0] == "-s" || rest[0] == "--scope":
				if len(rest) < 2 {
					fatal("missing value for --scope")
				}
				scope, rest = rest[1], rest[2:]
			case strings.HasPrefix(rest[0], "--scope="):
				scope, rest = rest[0][len("--scope="):], rest[1:]
			default:
				rest = rest[1:]
			}
		}
		switch scope {
		case "local", "user", "project":
		default:
			fatal("invalid scope %q — expected local, user, or project", scope)
		}
		importClaudeDesktop(scope)
	default:
		fatal("unknown mcp subcommand: " + args[0] + " (list|add|add-json|add-from-claude-desktop|get|remove|login|logout|reset-project-choices|serve)")
	}
}

// claudeDesktopConfigPath locates Claude Desktop's config — real's
// darwin path (WSL is handled there too; we do macOS + linux-native).
func claudeDesktopConfigPath() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
	default:
		return filepath.Join(home, ".config", "Claude", "claude_desktop_config.json")
	}
}

// importClaudeDesktop merges Claude Desktop's mcpServers into the given
// scope. Existing entries keep precedence (real skips same-name servers).
func importClaudeDesktop(scope string) {
	b, err := os.ReadFile(claudeDesktopConfigPath())
	if err != nil {
		fatal("no Claude Desktop config found (Mac and WSL only)")
	}
	var top struct {
		Servers map[string]map[string]any `json:"mcpServers"`
	}
	if json.Unmarshal(b, &top) != nil || len(top.Servers) == 0 {
		fatal("no MCP servers found in Claude Desktop config")
	}
	added := 0
	for name, cfg := range top.Servers {
		if mcpServerExists(scope, name) {
			fmt.Printf("skipped %s (already configured)\n", name)
			continue
		}
		if cfg["type"] == nil {
			cfg["type"] = "stdio"
		}
		writeMCPServer(scope, name, cfg)
		fmt.Printf("added %s\n", name)
		added++
	}
	fmt.Printf("imported %d server(s) [%s scope]\n", added, scope)
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

// writeMCPServer persists a server entry at the given scope — real's
// model: local → ~/.claude.json projects["<cwd>"].mcpServers (the default,
// visible only in this project), user → top-level mcpServers (every
// project), project → <cwd>/.mcp.json (checked in, needs approval).
func writeMCPServer(scope, name string, cfg map[string]any) {
	switch scope {
	case "project":
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
	case "user":
		editClaudeJSON(func(m map[string]any) {
			servers, _ := m["mcpServers"].(map[string]any)
			if servers == nil {
				servers = map[string]any{}
			}
			servers[name] = cfg
			m["mcpServers"] = servers
		})
	default: // local
		editClaudeJSON(func(m map[string]any) {
			projects, _ := m["projects"].(map[string]any)
			if projects == nil {
				projects = map[string]any{}
			}
			cwd := mustGetwd()
			proj, _ := projects[cwd].(map[string]any)
			if proj == nil {
				proj = map[string]any{}
			}
			servers, _ := proj["mcpServers"].(map[string]any)
			if servers == nil {
				servers = map[string]any{}
			}
			servers[name] = cfg
			proj["mcpServers"] = servers
			projects[cwd] = proj
			m["projects"] = projects
		})
	}
}

// mcpServerExists reports whether name is configured in the given scope.
func mcpServerExists(scope, name string) bool {
	switch scope {
	case "project":
		if b, err := os.ReadFile(filepath.Join(mustGetwd(), ".mcp.json")); err == nil {
			var top struct {
				Servers map[string]any `json:"mcpServers"`
			}
			if json.Unmarshal(b, &top) == nil {
				_, ok := top.Servers[name]
				return ok
			}
		}
		return false
	case "user":
		m, _ := config.ReadClaudeJSON()
		servers, _ := m["mcpServers"].(map[string]any)
		_, ok := servers[name]
		return ok
	default: // local
		m, _ := config.ReadClaudeJSON()
		projects, _ := m["projects"].(map[string]any)
		proj, _ := projects[mustGetwd()].(map[string]any)
		servers, _ := proj["mcpServers"].(map[string]any)
		_, ok := servers[name]
		return ok
	}
}

// removeMCPServer deletes name from exactly one scope; false when absent.
func removeMCPServer(scope, name string) bool {
	if !mcpServerExists(scope, name) {
		return false
	}
	switch scope {
	case "project":
		p := filepath.Join(mustGetwd(), ".mcp.json")
		m := map[string]any{}
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &m)
		}
		if servers, _ := m["mcpServers"].(map[string]any); servers != nil {
			delete(servers, name)
		}
		b, _ := json.MarshalIndent(m, "", "  ")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			fatal("write .mcp.json: " + err.Error())
		}
	case "user":
		editClaudeJSON(func(m map[string]any) {
			if servers, _ := m["mcpServers"].(map[string]any); servers != nil {
				delete(servers, name)
			}
		})
	default: // local
		editClaudeJSON(func(m map[string]any) {
			projects, _ := m["projects"].(map[string]any)
			proj, _ := projects[mustGetwd()].(map[string]any)
			if servers, _ := proj["mcpServers"].(map[string]any); servers != nil {
				delete(servers, name)
				proj["mcpServers"] = servers
			}
		})
	}
	return true
}

// promptSecret reads a secret without echo (mcp add --client-secret).
// Non-TTY callers must use MCP_CLIENT_SECRET.
func promptSecret(prompt string) string {
	fmt.Fprint(os.Stderr, prompt)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	var line string
	if _, err := fmt.Fscanln(os.Stdin, &line); err != nil {
		fatal("no client secret — set MCP_CLIENT_SECRET")
	}
	return strings.TrimSpace(line)
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
		// real 294 accepts --marketplace <mkt> alongside name@mkt
		var rest []string
		mkt := ""
		for i := 1; i < len(args); i++ {
			if args[i] == "--marketplace" && i+1 < len(args) {
				mkt = args[i+1]
				i++
			} else {
				rest = append(rest, args[i])
			}
		}
		name := ""
		if len(rest) > 0 {
			name = rest[0]
		}
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
	case "enable", "disable":
		// real writes the settings enabledPlugins gate ("name@mkt": bool);
		// the installed-file flag is not the toggle store
		if len(args) < 2 {
			fatal("usage: claude plugin " + args[0] + " <name[@marketplace]>")
		}
		key := args[1]
		if !strings.Contains(key, "@") {
			for _, p := range plugins.List() {
				if p.Name == key {
					key = p.Name + "@" + p.Marketplace
					break
				}
			}
		}
		enable := args[0] == "enable"
		editClaudeSettings(func(m map[string]any) {
			en, _ := m["enabledPlugins"].(map[string]any)
			if en == nil {
				en = map[string]any{}
			}
			en[key] = enable
			m["enabledPlugins"] = en
		})
		fmt.Printf("%s %s\n", args[0]+"d", key)
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
		fatal("unknown plugin subcommand: %s (list|install|remove|marketplace|details|enable|disable)", args[0])
	}
}

// editClaudeSettings mutates ~/.claude/settings.json in place.
func editClaudeSettings(mut func(map[string]any)) {
	p := filepath.Join(config.ClaudeDir(), "settings.json")
	m := map[string]any{}
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	mut(m)
	if b, err := json.MarshalIndent(m, "", "  "); err == nil {
		if err := os.WriteFile(p, b, 0o644); err != nil {
			fatal("write settings.json: " + err.Error())
		}
	}
}

// agentsSub: claude agents — list agent types (built-in + file-loaded +
// --agents-cli store) and background tasks. Bg tasks live inside a running
// session's process, so a fresh invocation can only report none.
func agentsSub(args []string) {
	cwd := mustGetwd()
	fmt.Println("agent types:")
	for _, n := range []string{
		"general-purpose", "statusline-setup", "claude", "Explore", "Plan",
		"claude-code-guide",
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

// logsSub is `claude logs <id>` — prints the session's recent terminal
// output (the pty tail the host replays to attachers).
func logsSub(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: claude logs <session-id|prefix|name>")
		os.Exit(2)
	}
	if err := bg.Logs(args[0], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// respawnSub is `claude respawn <id|--all>` — on-demand worker respawn,
// same as the daemon's dead-worker sweep.
func respawnSub(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: claude respawn <session-id|prefix|name>|--all")
		os.Exit(2)
	}
	var err error
	if args[0] == "--all" {
		err = bg.RespawnAll()
	} else {
		err = bg.Respawn(args[0])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("respawned")
}

// rmSub is `claude rm <id>` — delete a background session's job record.
func rmSub(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: claude rm <session-id|prefix|name>")
		os.Exit(2)
	}
	if err := bg.RemoveJob(args[0]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("removed")
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
