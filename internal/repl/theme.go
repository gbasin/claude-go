package repl

import (
	"claude-go/internal/config"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Themes: real CC ships dark/light/daltonian/auto + custom ~/.claude/themes/*.json
// and ANSI themes for dumb terminals. Our palette is minimal — remap the
// accent colors per theme.

// activeTokens is the resolved token palette (real's 72-token theme
// object — generated in palettes_gen.go).
var activeTokens = themeTokenPalettes["dark"]

// token fetches a semantic color from the active palette.
func token(k string) string { return activeTokens.get(k) }

type palette struct {
	Dim, Bold, Accent, Accent2, Warn, Err, Ok string
}

// applyTheme remaps the package color vars. Called once at REPL start.
func applyTheme(name string, cwd string) {
	// NO_COLOR disables color entirely unless FORCE_COLOR is set (real CC honors both)
	if os.Getenv("NO_COLOR") != "" && os.Getenv("FORCE_COLOR") == "" {
		name = "none"
	}
	if fc := os.Getenv("FORCE_COLOR"); fc != "" && fc != "0" && fc != "false" {
		// FORCE_COLOR forces color on (level 1-3 all enable color)
		if name == "" || name == "none" || name == "no-color" {
			name = "dark"
		}
	}
	if name == "" || name == "auto" {
		name = autoTheme()
	}
	if tp, ok := themeTokenPalettes[name]; ok {
		activeTokens = tp
	} else {
		activeTokens = themeTokenPalettes["dark"]
	}
	// custom theme file: ~/.claude/themes/<name>.json overrides tokens
	applyCustomTokens(name)
	dim = token("subtle")
	if dim == "" {
		dim = "\x1b[2m"
	}
	bold = "\x1b[1m"
	blue = token("claudeBlue_FOR_SYSTEM_SPINNER")
	cyan = token("claude")
	yellow = token("warning")
	red = token("error")
	green = token("success")
	_ = cwd
}

// theme vars mirror the repl.go package vars they remap

// autoTheme guesses dark vs light from the terminal env — real's
// COLORFGBG rule (mod_0521 s()): last component ≤6 or ==8 → dark,
// 7 and 9-15 → light, non-integer → undecided (dark).
func autoTheme() string {
	if c := os.Getenv("COLORFGBG"); c != "" {
		parts := strings.Split(c, ";")
		if len(parts) > 0 {
			if n, err := strconv.Atoi(parts[len(parts)-1]); err == nil && n >= 0 && n <= 15 {
				if n <= 6 || n == 8 {
					return "dark"
				}
				return "light"
			}
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

// applyCustomTokens overlays ~/.claude/themes/<name>.json onto activeTokens.
func applyCustomTokens(name string) {
	b, err := os.ReadFile(filepath.Join(config.ClaudeDir(), "themes", name+".json"))
	if err != nil {
		return
	}
	var m map[string]string
	if json.Unmarshal(b, &m) != nil {
		return
	}
	for k, v := range m {
		activeTokens[k] = v
	}
}

// loadCustomTheme reads ~/.claude/themes/<name>.json — {"dim":"\u001b[2m",...}
