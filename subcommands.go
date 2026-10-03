package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"claude-go/internal/api"
	"claude-go/internal/mcp"
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
		// claude-go mcp add <name> <command> [args...]
		if len(args) < 3 {
			fatal("usage: claude-go mcp add <name> <command> [args...]")
		}
		editClaudeJSON(func(m map[string]any) {
			servers, _ := m["mcpServers"].(map[string]any)
			if servers == nil {
				servers = map[string]any{}
			}
			servers[args[1]] = map[string]any{
				"type": "stdio", "command": args[2], "args": args[3:],
			}
			m["mcpServers"] = servers
		})
		fmt.Printf("added %s\n", args[1])
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
