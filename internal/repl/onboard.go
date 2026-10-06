package repl

import (
	"claude-go/internal/api"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"claude-go/internal/config"
	"golang.org/x/term"
)

// onboardThemes is real's first-run list (mod: theme picker order).
var onboardThemes = []struct {
	key, label string
}{
	{"", "Auto (match terminal)"},
	{"dark", "Dark mode"},
	{"light", "Light mode"},
	{"dark-daltonized", "Dark mode (colorblind-friendly)"},
	{"light-daltonized", "Light mode (colorblind-friendly)"},
	{"dark-ansi", "Dark mode (ANSI colors only)"},
	{"light-ansi", "Light mode (ANSI colors only)"},
}

// runThemePicker renders real's interactive theme select — the same dialog
// shown at first run and via /theme. Returns the picked key ("" = auto).
func runThemePicker(ks *keySource) string {
	if ks == nil {
		st, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			return ""
		}
		var err2 error
		ks, err2 = startKeys()
		if err2 != nil {
			term.Restore(int(os.Stdin.Fd()), st)
			return ""
		}
		defer ks.stop()
	}
	dch := make(chan key, 16)
	ks.dialog.Store(&dch)
	defer ks.dialog.Store(nil)
	cur := themeSetting("")
	sel := 0
	for i, t := range onboardThemes {
		if t.key == cur {
			sel = i
		}
	}
	cyan := "\x1b[38;5;153m"
	pink := "\x1b[38;5;174m"
	gray := "\x1b[38;5;246m"
	rows := len(onboardThemes) + 14
	draw := func() {
		var sb strings.Builder
		fmt.Fprintf(&sb, "\x1b[%dA\r", rows)
		sb.WriteString("\x1b[K" + pink + "Welcome to Claude Code" + reset + " " + gray + "v" + api.Version + reset + "\r\n")
		sb.WriteString("\x1b[K\r\n")
		sb.WriteString("\x1b[K  Let's get started.\r\n\r\n")
		sb.WriteString("\x1b[K  Choose the text style that looks best with your terminal\r\n")
		sb.WriteString("\x1b[K  " + gray + "To change this later, run /theme" + reset + "\r\n\r\n")
		for i, t := range onboardThemes {
			mark := "  "
			if i == sel {
				mark = cyan + "❯ ✔ " + reset
			}
			sb.WriteString("\x1b[K  " + mark + t.label + "\r\n")
		}
		// syntax preview box — real shows a small code diff in Monokai
		sb.WriteString("\x1b[K  " + gray + strings.Repeat("╌", 76) + reset + "\r\n")
		sb.WriteString("\x1b[K  " + dim + "1 function greet(){" + reset + "\r\n")
		sb.WriteString("\x1b[K  " + dim + "2 " + "\x1b[38;5;160m" + "-  console.log(\"Hello, World!\");" + reset + "\r\n")
		sb.WriteString("\x1b[K  " + dim + "2 " + "\x1b[38;5;34m" + "+  console.log(\"Hello, Claude!\");" + reset + "\r\n")
		sb.WriteString("\x1b[K  " + dim + "3 }" + reset + "\r\n")
		sb.WriteString("\x1b[K  " + gray + strings.Repeat("╌", 76) + reset + "\r\n")
		sb.WriteString("\x1b[K  " + gray + "Syntax theme: Monokai Extended (ctrl+t to disable)" + reset + "\r\n")
		fmt.Print(sb.String())
	}
	// initial paint
	var first strings.Builder
	first.WriteString(pink + "Welcome to Claude Code" + reset + " " + gray + "v" + api.Version + reset + "\r\n\r\n")
	first.WriteString("  Let's get started.\r\n\r\n")
	first.WriteString("  Choose the text style that looks best with your terminal\r\n")
	first.WriteString("  " + gray + "To change this later, run /theme" + reset + "\r\n\r\n")
	for range onboardThemes {
		first.WriteString("\r\n")
	}
	first.WriteString("  " + gray + strings.Repeat("╌", 76) + reset + "\r\n")
	first.WriteString("\r\n\r\n\r\n\r\n")
	first.WriteString("  " + gray + strings.Repeat("╌", 76) + reset + "\r\n")
	first.WriteString("  " + gray + "Syntax theme: Monokai Extended (ctrl+t to disable)" + reset + "\r\n")
	fmt.Print(first.String())
	draw()
	syntaxOn := true
	for k := range dch {
		switch {
		case k.seq == "[A":
			if sel > 0 {
				sel--
				draw()
			}
		case k.seq == "[B":
			if sel < len(onboardThemes)-1 {
				sel++
				draw()
			}
		case k.b == 0x1b || k.b == 3:
			return ""
		case k.b == '\r':
			pick := onboardThemes[sel].key
			if pick == "" {
				pick = "dark"
			}
			return pick
		case k.b == 't' && k.mod == modCtrl:
			syntaxOn = !syntaxOn
			draw()
		}
		_ = syntaxOn
	}
	return ""
}

// maybeOnboard runs the first-run flow: theme picker → trust gate → done.
// Real shows it only when hasCompletedOnboarding is unset.
func MaybeOnboard(cwd string) {
	m, path := config.ReadClaudeJSON()
	if done, _ := m["hasCompletedOnboarding"].(bool); done {
		return
	}
	pick := runThemePicker(nil)
	if pick != "" {
		applyTheme(pick, cwd)
		persistTheme(cwd, pick)
	}
	m["hasCompletedOnboarding"] = true
	if b, err := json.MarshalIndent(m, "", "  "); err == nil {
		os.WriteFile(path, b, 0o644)
	}
	time.Sleep(50 * time.Millisecond)
}
