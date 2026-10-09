package repl

import (
	"claude-go/internal/config"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type customCmd struct {
	mcp         func(args map[string]string) (string, error) // MCP prompt getter
	Name        string
	Description string
	Body        string
	ArgHint     string
	AllowTools  string
	Model       string
	Aliases     []string
	Hidden      bool
	Context     string // "fork" → run in a subagent
	Agent       string
	WhenToUse   string
	Source      string // "command"|"plugin:<name>"|mcp server name — hook payload
}

var customCommands map[string]customCmd

// hiddenCommands holds commands marked hidden: they still execute via
// handleSlash's default case but never appear in help or completion.
var hiddenCommands map[string]customCmd

// loadCustomCommands scans user + project + plugin command dirs.
func loadCustomCommands(cwd string) {
	customCommands = map[string]customCmd{}
	if os.Getenv("CLAUDE_CODE_SIMPLE") != "" {
		return // --bare: no command auto-discovery
	}
	hiddenCommands = map[string]customCmd{}
	var dirs []string
	for _, d := range []string{
		filepath.Join(cwd, ".claude", "commands"),
		filepath.Join(config.ClaudeDir(), "commands"),
	} {
		dirs = append(dirs, d)
	}
	// plugin commands
	for _, pat := range []string{
		filepath.Join(config.ClaudeDir(), "plugins", "cache", "*", "*", "*", "commands"),
		filepath.Join(config.ClaudeDir(), "plugins", "local", "*", "commands"),
	} {
		matches, _ := filepath.Glob(pat)
		dirs = append(dirs, matches...)
	}
	for _, dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		// recurse for subdirectory namespacing (dir:cmd like real CC)
		filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".md") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			fm, body := splitMDFrontmatter(string(b))
			name := fm["name"]
			if name == "" {
				rel, _ := filepath.Rel(dir, p)
				name = strings.TrimSuffix(filepath.ToSlash(rel), ".md")
				name = strings.ReplaceAll(name, "/", ":") // dir:cmd namespacing
			}
			var aliases []string
			if a := fm["aliases"]; a != "" {
				aliases = strings.Split(a, ",")
				for i := range aliases {
					aliases[i] = strings.TrimSpace(aliases[i])
				}
			}
			cmd := customCmd{
				Name:        name,
				Source:      dir, // command_source in the expansion hook
				Description: fm["description"],
				Body:        body,
				ArgHint:     fm["argument-hint"],
				AllowTools:  fm["allowed-tools"],
				Model:       fm["model"],
				Aliases:     aliases,
				Hidden:      fm["hidden"] == "true" || fm["isHidden"] == "true",
				Context:     fm["context"],
				Agent:       fm["agent"],
				WhenToUse:   fm["whenToUse"],
			}
			if cmd.Hidden {
				hiddenCommands[name] = cmd
				for _, a := range aliases {
					hiddenCommands[a] = cmd
				}
			} else {
				customCommands[name] = cmd
				for _, a := range aliases {
					customCommands[a] = cmd
				}
			}
			return nil
		})
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

// expandCustomCommand substitutes $ARGUMENTS/$1..$9, ${CLAUDE_*} vars, and
// !`cmd` shell interpolation, then returns the prompt.
func expandCustomCommand(c customCmd, arg string) string {
	out := c.Body
	// ${CLAUDE_*} substitutions like the real command engine
	cwd, _ := os.Getwd()
	vars := map[string]string{
		"${CLAUDE_PROJECT_DIR}":  cwd,
		"${CLAUDE_COMMAND_NAME}": "/" + c.Name,
	}
	for k, v := range vars {
		out = strings.ReplaceAll(out, k, v)
	}
	// !`cmd` interpolation: run the command, substitute stdout
	out = interpolateBang(out, cwd)
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

var bangRe = regexp.MustCompile("!`([^`]+)`")

// interpolateBang runs !`command` shell snippets inside command bodies.
func interpolateBang(s, cwd string) string {
	return bangRe.ReplaceAllStringFunc(s, func(m string) string {
		cmdStr := bangRe.FindStringSubmatch(m)[1]
		cmd := exec.Command("sh", "-c", cmdStr)
		cmd.Dir = cwd
		out, err := cmd.Output()
		if err != nil {
			return m // keep literal on failure
		}
		return strings.TrimSpace(string(out))
	})
}

// RegisterMCPPrompt exposes an MCP server prompt as a slash command —
// real surfaces them as /mcp__<server>__<prompt>. get returns the
// expanded prompt text (prompts/get's concatenated messages).
func RegisterMCPPrompt(fullname, desc string, get func(args map[string]string) (string, error)) {
	if customCommands == nil {
		customCommands = map[string]customCmd{} // lazy init for tests
	}
	customCommands[fullname] = customCmd{
		Name:        fullname,
		Body:        "", // dynamic — expanded by expandMCPPrompt
		Description: desc,
		mcp:         get,
	}
}
