package main

// onboard.go — first-run onboarding, ~/.claude.json schema migration,
// and the minimal update-check marker read.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/term"
)

// maybeOnboard runs once when ~/.claude.json is missing or lacks
// hasCompletedOnboarding. Interactive runs get a theme pick; the
// per-directory trust prompt is handled separately by repl.TrustGate.
func maybeOnboard(interactive bool) {
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".claude.json")
	m, _ := readClaudeJSON(p)
	if m == nil {
		m = map[string]any{}
	}
	if done, _ := m["hasCompletedOnboarding"].(bool); done {
		return
	}
	if interactive && term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Println("\nWelcome to claude-go — a Go reimplementation of Claude Code.")
		fmt.Println("\nChoose a theme:")
		fmt.Println("  1. Dark")
		fmt.Println("  2. Light")
		fmt.Println("  3. Dark (high contrast)")
		fmt.Println("  4. Use terminal default")
		fmt.Print("\n  > ")
		var buf [8]byte
		n, _ := os.Stdin.Read(buf[:])
		theme := "system"
		for i := 0; i < n; i++ {
			switch buf[i] {
			case '1':
				theme = "dark"
			case '2':
				theme = "light"
			case '3':
				theme = "dark-daltonized"
			case '4':
				theme = "system"
			}
		}
		m["theme"] = theme
		fmt.Println()
	}
	m["hasCompletedOnboarding"] = true
	writeClaudeJSON(p, m)
}

// migrateClaudeJSON backfills missing schema fields in ~/.claude.json
// while preserving every existing field.
func migrateClaudeJSON() {
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".claude.json")
	m, err := readClaudeJSON(p)
	if err != nil || m == nil {
		return
	}
	changed := false
	if _, ok := m["version"]; !ok {
		m["version"] = 1
		changed = true
	}
	if changed {
		writeClaudeJSON(p, m)
	}
}

func readClaudeJSON(p string) (map[string]any, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil, fmt.Errorf("invalid json")
	}
	return m, nil
}

func writeClaudeJSON(p string, m map[string]any) {
	b, _ := json.MarshalIndent(m, "", "  ")
	os.WriteFile(p, b, 0o644)
}

// checkForUpdate reads the ~/.claude/version-check.json marker left by a
// previous run or external tooling and notes a newer build if flagged.
func checkForUpdate() {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".claude", "version-check.json"))
	if err != nil {
		return
	}
	var m struct {
		Version string `json:"version"`
		Latest  string `json:"latest"`
	}
	if json.Unmarshal(b, &m) != nil {
		return
	}
	v := m.Latest
	if v == "" {
		v = m.Version
	}
	if v != "" && v != version {
		fmt.Fprintf(os.Stderr, "a newer claude-go may exist (marker %s, running %s)\n", v, version)
	}
}
