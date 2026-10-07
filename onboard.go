package main

// onboard.go — first-run onboarding, ~/.claude.json schema migration,
// and the minimal update-check marker read.

import (
	"claude-go/internal/config"
	"claude-go/internal/hooks"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"
)

// maybeOnboard runs once when ~/.claude.json is missing or lacks
// hasCompletedOnboarding. Interactive runs get a theme pick; the
// per-directory trust prompt is handled separately by repl.TrustGate.
func maybeOnboard(interactive bool) {
	p := filepath.Join(config.Home(), ".claude.json")
	m, _ := readClaudeJSON(p)
	if m == nil {
		m = map[string]any{}
	}
	if done, _ := m["hasCompletedOnboarding"].(bool); done {
		return
	}
	if interactive && term.IsTerminal(int(os.Stdin.Fd())) {
		m["theme"] = onboardThemePicker()
		fmt.Println()
	}
	m["hasCompletedOnboarding"] = true
	writeClaudeJSON(p, m)
	// Setup hooks fire once on first run (real: trigger=startup; and
	// trigger=maintenance after updates — we don't ship an updater)
	if cwd, err := os.Getwd(); err == nil {
		hooks.Run(hooks.Load(cwd), hooks.Setup, map[string]any{
			"cwd":             cwd,
			"hook_event_name": "Setup",
			"trigger":         "startup",
		}, cwd)
	}
}

// onboardThemePicker renders real's first-run theme selector:
// a cursor list over the six styles + auto, with a syntax-highlight
// preview box that re-renders as the cursor moves.
func onboardThemePicker() string {
	const (
		pink   = "\x1b[38;5;174m"
		gray   = "\x1b[38;5;246m"
		cyan   = "\x1b[38;5;153m"
		green  = "\x1b[38;5;114m"
		dim239 = "\x1b[38;5;239m"
		bold   = "\x1b[1m"
		reset  = "\x1b[39m"
		unbold = "\x1b[22m"
	)
	options := []struct{ label, key string }{
		{"Auto (match terminal)", "system"},
		{"Dark mode", "dark"},
		{"Light mode", "light"},
		{"Dark mode (colorblind-friendly)", "dark-daltonized"},
		{"Light mode (colorblind-friendly)", "light-daltonized"},
		{"Dark mode (ANSI colors only)", "dark-ansi"},
		{"Light mode (ANSI colors only)", "light-ansi"},
	}
	fmt.Printf("%sWelcome to Claude Code %sv%s%s\n\n", pink, gray, version, reset)
	fmt.Println("Let's get started.")
	fmt.Println()
	fmt.Printf("%sChoose the text style that looks best with your terminal%s\n", bold, unbold)
	fmt.Printf("%sTo change this later, run /theme%s\n\n", gray, reset)

	// raw mode for arrow-key nav
	fd := int(os.Stdin.Fd())
	oldState, _ := term.MakeRaw(fd)
	defer term.Restore(fd, oldState)
	hideCursor := "\x1b[?25l"
	showCursor := "\x1b[?25h"
	fmt.Print(hideCursor)
	defer fmt.Print(showCursor)

	cursor := 1       // real's default lands on Dark mode
	previewLines := 7 // dashes + 4 code lines + dashes + theme line
	draw := func() {
		for i, o := range options {
			if i == cursor {
				fmt.Printf("%s\xe2\x9d\xaf%s %s\xe2\x9c\x94%s %s%s%s\x1b[K\r\n",
					cyan, reset, green, reset, cyan, o.label, reset)
			} else {
				fmt.Printf("    %s\x1b[K\r\n", o.label)
			}
		}
		// real's live preview — a diff sample under a dashed rule,
		// re-rendered for the hovered style
		dash := strings.Repeat("\xe2\x95\x8c", 80)
		fmt.Printf("%s%s%s\x1b[K\r\n", dim239, dash, reset)
		fmt.Printf("\x1b[2m 1%s \x1b[22m\x1b[38;5;81mfunction\x1b[38;5;231m \x1b[38;5;148mgreet\x1b[38;5;231m() {\x1b[39m\x1b[K\r\n", reset)
		fmt.Printf("\x1b[38;5;167m\x1b[48;5;52m 2 -\x1b[38;5;231m  console.log(\"Hello, World!\");%s\x1b[49m\x1b[39m\x1b[K\r\n",
			strings.Repeat(" ", 39))
		fmt.Printf("\x1b[38;5;77m\x1b[48;5;22m 2 +\x1b[38;5;231m  console.log(\"Hello, Claude!\");%s\x1b[39m\x1b[49m\x1b[K\r\n",
			strings.Repeat(" ", 39))
		fmt.Printf("\x1b[2m 3%s \x1b[22m}\x1b[39m\x1b[K\r\n", reset)
		fmt.Printf("%s%s%s\x1b[K\r\n", dim239, dash, reset)
		fmt.Printf("%s Syntax theme: Monokai Extended (ctrl+t to disable)%s\x1b[K\r\n", gray, reset)
	}
	erase := func() {
		for i := 0; i < len(options)+previewLines; i++ {
			fmt.Print("\x1b[A")
		}
	}
	draw()
	buf := make([]byte, 8)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			break
		}
		for i := 0; i < n; i++ {
			switch buf[i] {
			case '\r', '\n':
				return options[cursor].key
			case 0x1b: // arrow / escape seq
				if i+2 < n && buf[i+1] == '[' {
					switch buf[i+2] {
					case 'A':
						if cursor > 0 {
							cursor--
							erase()
							draw()
						}
					case 'B':
						if cursor < len(options)-1 {
							cursor++
							erase()
							draw()
						}
					}
					i += 2
				}
			case 'j':
				if cursor < len(options)-1 {
					cursor++
					erase()
					draw()
				}
			case 'k':
				if cursor > 0 {
					cursor--
					erase()
					draw()
				}
			case 'q', 0x03:
				return "system"
			}
		}
	}
	return options[cursor].key
}

// migrateClaudeJSON backfills missing schema fields in ~/.claude.json
// while preserving every existing field.
func migrateClaudeJSON() {
	p := filepath.Join(config.Home(), ".claude.json")
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
	b, err := os.ReadFile(filepath.Join(config.ClaudeDir(), "version-check.json"))
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
