package repl

import (
	"claude-go/internal/config"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

const (
	trustYellow = "\x1b[38;5;220m"
	trustCyan   = "\x1b[38;5;153m"
	trustGray   = "\x1b[38;5;246m"
	unbold      = "\x1b[22m"
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
	// real's workspace-trust dialog: yellow rule, bold "Accessing
	// workspace:" + path, the quick-safety-check paragraph, OSC8 security
	// link, and a two-option cursor list whose DEFAULT is "No, exit"
	fmt.Printf("\n%s%s%s\n", trustYellow, strings.Repeat("─", 78), reset)
	fmt.Printf("  %s%sAccessing workspace:%s\n\n", trustYellow, bold, unbold+reset)
	fmt.Printf("  %s%s%s\n\n", bold, cwd, unbold+reset)
	fmt.Print("  Quick safety check: Is this a project you created or one you trust? (Like your\r\n")
	fmt.Print("  own code, a well-known open source project, or work from your team). If not,\r\n")
	fmt.Print("  take a moment to review what's in this folder first.\r\n\r\n")
	fmt.Print("  Claude Code'll be able to read, edit, and execute files here.\r\n\r\n")
	printTrustInventory(cwd)
	fmt.Print("  \x1b]8;;https://code.claude.com/docs/en/security\x07")
	fmt.Printf("%sSecurity guide%s", trustGray, reset)
	fmt.Print("\x1b]8;;\x07\r\n\r\n")

	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		fmt.Print("  y/n: ")
		var buf [8]byte
		n, _ := os.Stdin.Read(buf[:])
		for i := 0; i < n; i++ {
			if buf[i] == 'y' || buf[i] == 'Y' {
				markTrusted(cwd)
				return true
			}
		}
		return false
	}
	defer term.Restore(fd, oldState)
	options := []string{"No, exit", "Yes, I trust this folder"}
	cursor := 0 // real's default is No
	draw := func() {
		for i, o := range options {
			if i == cursor {
				fmt.Printf("  %s\xe2\x9d\xaf%s %s%s%s\x1b[K\r\n", trustCyan, reset, trustCyan, o, reset)
			} else {
				fmt.Printf("    %s\x1b[K\r\n", o)
			}
		}
		fmt.Printf("  %sEnter to confirm · Esc to cancel%s\x1b[K\r\n", trustGray, reset)
	}
	draw()
	buf := make([]byte, 8)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return false
		}
		for i := 0; i < n; i++ {
			switch buf[i] {
			case '\r', '\n':
				if cursor == 1 {
					markTrusted(cwd)
					return true
				}
				return false
			case 0x1b:
				if i+2 < n && buf[i+1] == '[' {
					switch buf[i+2] {
					case 'A', 'B':
						cursor ^= 1
						fmt.Printf("\x1b[%dA", len(options)+1)
						draw()
					}
					i += 2
				}
			case 'y', 'Y':
				markTrusted(cwd)
				return true
			case 'n', 'N', 0x03:
				return false
			}
		}
	}
}

func projectsMap() (map[string]any, string) {
	return config.ReadClaudeJSON()
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
	config.WriteClaudeJSON(p, m)
}
