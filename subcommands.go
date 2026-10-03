package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"claude-go/internal/api"
	"claude-go/internal/auth"
	"claude-go/internal/mcp"
	"claude-go/internal/session"
)

// mcpSub implements `claude-go mcp list|add|remove`.
func mcpSub(args []string) {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list":
		cfgs := mcp.LoadConfigs(mustGetwd())
		if len(cfgs) == 0 {
			fmt.Println("no MCP servers configured")
			return
		}
		for name, c := range cfgs {
			fmt.Printf("%s: %s %v\n", name, c.Command, c.Args)
		}
	case "add":
		// claude-go mcp add [--transport http|sse] <name> <command-or-url> [args...]
		transport := "stdio"
		rest := args[1:]
		for len(rest) > 0 && strings.HasPrefix(rest[0], "--") {
			if rest[0] == "--transport" && len(rest) > 1 {
				transport = rest[1]
				rest = rest[2:]
			} else {
				rest = rest[1:]
			}
		}
		if len(rest) < 2 {
			fatal("usage: claude-go mcp add [--transport http|sse] <name> <command|url> [args...]")
		}
		editClaudeJSON(func(m map[string]any) {
			servers, _ := m["mcpServers"].(map[string]any)
			if servers == nil {
				servers = map[string]any{}
			}
			if transport == "stdio" {
				servers[rest[0]] = map[string]any{
					"type": "stdio", "command": rest[1], "args": rest[2:],
				}
			} else {
				servers[rest[0]] = map[string]any{"type": transport, "url": rest[1]}
			}
			m["mcpServers"] = servers
		})
		fmt.Printf("added %s (%s)\n", rest[0], transport)
	case "get":
		if len(args) < 2 {
			fatal("usage: claude-go mcp get <name>")
		}
		cfgs := mcp.LoadConfigs(mustGetwd())
		c, ok := cfgs[args[1]]
		if !ok {
			fatal("no such server: %s", args[1])
		}
		b, _ := json.MarshalIndent(c, "", "  ")
		fmt.Println(string(b))
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
	default:
		fatal("unknown mcp subcommand: %s (list|add|remove)", args[0])
	}
}

func doctorSub() {
	fmt.Println("claude-go", api.Version)
	fmt.Println("binary: self-contained Go, no runtime deps")
	cwd := mustGetwd()
	fmt.Println("cwd:", cwd)
	if out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output(); err == nil {
		fmt.Println("git root:", string(out[:len(out)-1]))
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(home, ".claude", "CLAUDE.md"),
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
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".claude", "settings.json")
	read := func() map[string]any {
		var m map[string]any
		b, _ := os.ReadFile(p)
		json.Unmarshal(b, &m)
		if m == nil {
			m = map[string]any{}
		}
		return m
	}
	switch args[0] {
	case "list":
		b, _ := json.MarshalIndent(read(), "", "  ")
		fmt.Println(string(b))
	case "get":
		if len(args) < 2 {
			fatal("usage: claude-go config get <key>")
		}
		if v, ok := read()[args[1]]; ok {
			out, _ := json.Marshal(v)
			fmt.Println(string(out))
		}
	case "set":
		if len(args) < 3 {
			fatal("usage: claude-go config set <key> <value>")
		}
		m := read()
		var v any
		if json.Unmarshal([]byte(args[2]), &v) == nil {
			m[args[1]] = v
		} else {
			m[args[1]] = args[2]
		}
		out, _ := json.MarshalIndent(m, "", "  ")
		os.WriteFile(p, out, 0o644)
		fmt.Printf("set %s\n", args[1])
	default:
		fatal("unknown config subcommand: %s", args[0])
	}
}

func editClaudeJSON(edit func(map[string]any)) {
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".claude.json")
	var m map[string]any
	b, _ := os.ReadFile(p)
	json.Unmarshal(b, &m)
	if m == nil {
		m = map[string]any{}
	}
	edit(m)
	out, _ := json.MarshalIndent(m, "", "  ")
	os.WriteFile(p, out, 0o644)
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
	home, _ := os.UserHomeDir()
	targets := []string{filepath.Join(home, ".claude", "projects", session.Slug(cwd))}
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
