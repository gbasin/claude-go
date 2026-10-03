package repl

import (
	"os"
	"path/filepath"
	"strings"
)

type customCmd struct {
	Name        string
	Description string
	Body        string
	ArgHint     string
	AllowTools  string
}

var customCommands map[string]customCmd

// loadCustomCommands scans user + project + plugin command dirs.
func loadCustomCommands(cwd string) {
	customCommands = map[string]customCmd{}
	home, _ := os.UserHomeDir()
	var dirs []string
	for _, d := range []string{
		filepath.Join(cwd, ".claude", "commands"),
		filepath.Join(home, ".claude", "commands"),
	} {
		dirs = append(dirs, d)
	}
	// plugin commands
	for _, pat := range []string{
		filepath.Join(home, ".claude", "plugins", "synced", "*", "*", "commands"),
		filepath.Join(home, ".claude", "plugins", "local", "*", "commands"),
	} {
		matches, _ := filepath.Glob(pat)
		dirs = append(dirs, matches...)
	}
	for _, dir := range dirs {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			fm, body := splitMDFrontmatter(string(b))
			name := fm["name"]
			if name == "" {
				name = strings.TrimSuffix(e.Name(), ".md")
			}
			customCommands[name] = customCmd{
				Name:        name,
				Description: fm["description"],
				Body:        body,
				ArgHint:     fm["argument-hint"],
				AllowTools:  fm["allowed-tools"],
			}
		}
	}
}

func splitMDFrontmatter(s string) (map[string]string, string) {
	fm := map[string]string{}
	if !strings.HasPrefix(s, "---") {
		return fm, s
	}
	rest := s[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return fm, s
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok {
			fm[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return fm, strings.TrimSpace(rest[end+4:])
}

// expandCustomCommand substitutes $ARGUMENTS/$1..$9 and returns the prompt.
func expandCustomCommand(c customCmd, arg string) string {
	out := c.Body
	if !strings.Contains(out, "$ARGUMENTS") && arg != "" {
		out += "\n\n" + arg
	}
	out = strings.ReplaceAll(out, "$ARGUMENTS", arg)
	parts := strings.Fields(arg)
	for i, p := range parts {
		if i > 8 {
			break
		}
		out = strings.ReplaceAll(out, "$"+string(rune('1'+i)), p)
	}
	return out
}
