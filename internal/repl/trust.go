package repl

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/term"
)

// TrustGate asks "do you trust this folder?" once per directory,
// recorded in ~/.claude.json -> projects[cwd].hasTrustDialogAccepted.
func TrustGate(cwd string) bool {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return true
	}
	if accepted(cwd) {
		return true
	}
	fmt.Printf("\n%sDo you trust the files in this folder?%s\n", bold, reset)
	fmt.Printf("  %s%s%s\n", dim, cwd, reset)
	printTrustInventory(cwd)
	fmt.Print("\n  1. Yes, proceed\n  2. No, exit\n  ")
	var buf [8]byte
	n, _ := os.Stdin.Read(buf[:])
	for i := 0; i < n; i++ {
		switch buf[i] {
		case '1', 'y', '\r', '\n':
			markTrusted(cwd)
			return true
		case '2', 'n':
			return false
		}
	}
	return true
}

func projectsMap() (map[string]any, string) {
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".claude.json")
	var m map[string]any
	if b, err := os.ReadFile(p); err == nil {
		json.Unmarshal(b, &m)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, p
}

func accepted(cwd string) bool {
	m, _ := projectsMap()
	if projs, ok := m["projects"].(map[string]any); ok {
		if pr, ok := projs[cwd].(map[string]any); ok {
			v, _ := pr["hasTrustDialogAccepted"].(bool)
			return v
		}
	}
	return false
}

func markTrusted(cwd string) {
	m, p := projectsMap()
	projs, _ := m["projects"].(map[string]any)
	if projs == nil {
		projs = map[string]any{}
	}
	pr, _ := projs[cwd].(map[string]any)
	if pr == nil {
		pr = map[string]any{}
	}
	pr["hasTrustDialogAccepted"] = true
	projs[cwd] = pr
	m["projects"] = projs
	b, _ := json.MarshalIndent(m, "", "  ")
	os.WriteFile(p, b, 0o644)
}
