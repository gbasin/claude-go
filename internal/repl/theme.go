package repl

import (
	"claude-go/internal/config"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Themes: real CC ships dark/light/daltonian/auto + custom ~/.claude/themes/*.json
// and ANSI themes for dumb terminals. Our palette is minimal — remap the
// accent colors per theme.

type palette struct {
	Dim, Bold, Accent, Accent2, Warn, Err, Ok string
}

var themePalettes = map[string]palette{
	"dark":  {"\x1b[2m", "\x1b[1m", "\x1b[34m", "\x1b[36m", "\x1b[33m", "\x1b[31m", "\x1b[32m"},
	"light": {"\x1b[2m", "\x1b[1m", "\x1b[34m", "\x1b[34m", "\x1b[35m", "\x1b[31m", "\x1b[32m"},
	"daltonian": { // deuteranopia-safe: avoid red/green pairs
		"\x1b[2m", "\x1b[1m", "\x1b[34m", "\x1b[36m", "\x1b[35m", "\x1b[33m", "\x1b[34m"},
	"ansi":     {"\x1b[2m", "\x1b[1m", "\x1b[34m", "\x1b[36m", "\x1b[33m", "\x1b[31m", "\x1b[32m"},
	"none":     {"", "", "", "", "", "", ""},
	"no-color": {"", "", "", "", "", "", ""},
}

// applyTheme remaps the package color vars. Called once at REPL start.
func applyTheme(name string, cwd string) {
	if name == "" || name == "auto" {
		name = autoTheme()
	}
	p, ok := themePalettes[name]
	if !ok {
		// custom theme file: ~/.claude/themes/<name>.json with color overrides
		if cp := loadCustomTheme(name); cp != nil {
			p = *cp
		} else {
			p = themePalettes["dark"]
		}
	}
	dim, bold = p.Dim, p.Bold
	blue, cyan = p.Accent, p.Accent2
	yellow, red, green = p.Warn, p.Err, p.Ok
	_ = cwd
}

// theme vars mirror the repl.go package vars they remap

// autoTheme guesses dark vs light from the terminal env (COLORFGBG bg>7 = light).
func autoTheme() string {
	if c := os.Getenv("COLORFGBG"); c != "" {
		parts := strings.Split(c, ";")
		if len(parts) > 0 {
			bg := parts[len(parts)-1]
			if bg == "0" || bg == "7" || bg == "15" {
				if bg == "0" {
					return "dark"
				}
				return "light"
			}
			return "dark"
		}
	}
	return "dark"
}

// themeSetting reads theme from settings.
func themeSetting(cwd string) string {
	for _, p := range []string{
		filepath.Join(cwd, ".claude", "settings.json"),
		filepath.Join(config.ClaudeDir(), "settings.json"),
		filepath.Join(config.Home(), ".claude.json"),
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var st struct {
			Theme string `json:"theme"`
		}
		if json.Unmarshal(b, &st) == nil && st.Theme != "" {
			return st.Theme
		}
	}
	return ""
}

// loadCustomTheme reads ~/.claude/themes/<name>.json — {"dim":"\u001b[2m",...}
func loadCustomTheme(name string) *palette {
	b, err := os.ReadFile(filepath.Join(config.ClaudeDir(), "themes", name+".json"))
	if err != nil {
		return nil
	}
	var m map[string]string
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	p := themePalettes["dark"]
	for k, v := range m {
		switch strings.ToLower(k) {
		case "dim":
			p.Dim = v
		case "bold":
			p.Bold = v
		case "accent", "blue":
			p.Accent = v
		case "accent2", "cyan":
			p.Accent2 = v
		case "warn", "yellow":
			p.Warn = v
		case "error", "red":
			p.Err = v
		case "ok", "green":
			p.Ok = v
		}
	}
	return &p
}
