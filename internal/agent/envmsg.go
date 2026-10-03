package agent

import (
	"claude-go/internal/config"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"

	"claude-go/internal/plugins"
	"strings"
	"time"
)

// modelInfo maps model IDs to display name / api id / cutoff.
type modelInfo struct {
	Name   string
	APIID  string
	Cutoff string
}

var modelTable = map[string]modelInfo{
	"claude-fable-5-1":          {"Fable 5.1", "claude-fable-5-1[1m]", "June 2026"},
	"claude-opus-5-5":           {"Opus 5.5", "claude-opus-5-5", "March 2026"},
	"claude-sonnet-5-5":         {"Sonnet 5.5", "claude-sonnet-5-5", "March 2026"},
	"claude-haiku-4-5-20251001": {"Haiku 4.5", "claude-haiku-4-5-20251001", "February 2026"},
	"claude-sonnet-4-5":         {"Sonnet 4.5", "claude-sonnet-4-5", "January 2025"},
	"claude-opus-4-5":           {"Opus 4.5", "claude-opus-4-5", "January 2025"},
}

func infoFor(model string) modelInfo {
	if mi, ok := modelTable[model]; ok {
		return mi
	}
	for k, mi := range modelTable {
		if strings.HasPrefix(model, k) {
			return mi
		}
	}
	return modelInfo{model, model, ""}
}

// toolSpecString renders the "(Tools: …)" column in the agent-type listing.
func toolSpecString(d AgentDef) string {
	if len(d.Tools) == 0 && len(d.DisallowedTools) == 0 {
		return "All tools"
	}
	if len(d.DisallowedTools) > 0 && (len(d.Tools) == 0 || strIn(d.Tools, "*")) {
		return "All tools except " + strings.Join(d.DisallowedTools, ", ")
	}
	if len(d.Tools) == 0 {
		return "All tools"
	}
	return strings.Join(d.Tools, ", ")
}

// EnvMessage builds the role:"system" message appended after the first user
// message: environment, model card, agent registry, skill index, mode notes.
func EnvMessage(cwd, model string, addDirs []string, permMode string, sessionID string) string {
	var b strings.Builder
	b.WriteString("# Environment\nYou have been invoked in the following environment: \n")
	fmt.Fprintf(&b, " - Primary working directory: %s\n", filepath.Clean(cwd))
	isGit := exec.Command("git", "-C", cwd, "rev-parse", "--is-inside-work-tree").Run() == nil
	fmt.Fprintf(&b, " - Is a git repository: %t\n", isGit)
	if len(addDirs) > 0 {
		b.WriteString(" - Additional working directories:\n")
		for _, d := range addDirs {
			fmt.Fprintf(&b, "  - %s\n", d)
		}
	}
	plat := "darwin"
	if runtime.GOOS != "darwin" {
		plat = runtime.GOOS
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	fmt.Fprintf(&b, " - Platform: %s\n - Shell: %s\n - OS Version: %s\n", plat, filepath.Base(shell), osVersion())
	if scratch := scratchDir(cwd, sessionID); scratch != "" {
		fmt.Fprintf(&b, " - Scratchpad directory: %s — always use it for temporary files (intermediate results, scripts, outputs that don't belong in the project) instead of `/tmp` or other system temp directories; it is session-specific, isolated from the project, and can generally be used without permission prompts. Only use `/tmp` if the user explicitly asks.\n", scratch)
	}
	b.WriteString("\n")

	mi := infoFor(model)
	fmt.Fprintf(&b, "You are powered by the model named %s. The exact model ID is %s.", mi.Name, mi.APIID)
	if mi.Cutoff != "" {
		fmt.Fprintf(&b, " Assistant knowledge cutoff is %s.", mi.Cutoff)
	}
	b.WriteString("\n\n")

	b.WriteString("Available agent types for the Agent tool:\n")
	agents := AgentTypes(cwd)
	sort.Slice(agents, func(i, j int) bool {
		return strings.ToLower(agents[i].Name) < strings.ToLower(agents[j].Name)
	})
	for _, a := range agents {
		fmt.Fprintf(&b, "- %s: %s (Tools: %s)\n", a.Name, a.Description, toolSpecString(a))
	}
	b.WriteString("\nWhen you launch multiple agents for independent work, send them in a single message with multiple tool uses so they run concurrently.\n\n")

	if skills := loadSkillIndex(cwd); skills != "" {
		b.WriteString("The following skills are available for use with the Skill tool:\n\n")
		b.WriteString(skills)
		b.WriteString("\n")
	}

	if permMode == "auto" {
		b.WriteString("\nWhile auto mode is active:\n\n" +
			"Do your work through the Bash tool wherever it can accomplish the job: read files with cat, head, or sed -n, " +
			"search with grep and find, and make file changes with sed, heredocs, or short scripts, rather than using " +
			"the dedicated Read, Edit, or Write tools. Fall back to a dedicated tool only when Bash genuinely cannot do the job.\n")
	}
	fmt.Fprintf(&b, "\nToday's date is %s.\n", time.Now().Format("2006-01-02"))
	if ar := attributionReminder(cwd); ar != "" {
		b.WriteString("\n" + ar + "\n")
	}
	return b.String()
}

// attributionReminder mirrors real CC's commit/PR attribution reminder:
// "End git commit messages with: <trailer>" or the do-not-attribute form
// when includeCoAuthoredBy is false.
func attributionReminder(cwd string) string {
	st := config.Load(cwd)
	replaces := "this replaces Claude Code's own earlier attribution guidance, such as a previous copy of this reminder"
	if !st.CoAuthoredBy() {
		return "From here on, do not add attribution lines to git commit messages or pull request descriptions (" +
			replaces + ", and applies even if a CLAUDE.md or memory rule asks for attribution lines)."
	}
	trailer := "Co-Authored-By: Claude <noreply@anthropic.com>"
	note := "the user's own instructions about these lines, such as a CLAUDE.md or memory rule, take precedence over this reminder, but do not add attribution lines this reminder leaves out"
	return "Attribution for git commits and pull requests you create from here on (" + replaces + "; " + note + "):\n" +
		"- End git commit messages with:\n" + trailer
}

func osVersion() string {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("sw_vers", "-productName").Output()
		ver, err2 := exec.Command("sw_vers", "-productVersion").Output()
		if err == nil && err2 == nil {
			return strings.TrimSpace(string(out)) + " " + strings.TrimSpace(string(ver))
		}
		out2, err3 := exec.Command("uname", "-rs").Output()
		if err3 == nil {
			return strings.TrimSpace(string(out2))
		}
	}
	out, _ := exec.Command("uname", "-sr").Output()
	return strings.TrimSpace(string(out))
}

// loadSkillIndex builds the "- name: description" index from skill dirs:
// ~/.claude/skills, <cwd>/.claude/skills, <cwd>/.agents/skills, plugin dirs.
func skillRoots(cwd string) []string {
	dirs := []string{
		filepath.Join(config.ClaudeDir(), "skills"),
		filepath.Join(config.Home(), ".agents", "skills"),
		filepath.Join(cwd, ".claude", "skills"),
		filepath.Join(cwd, ".agents", "skills"),
	}
	for _, base := range []string{
		filepath.Join(config.ClaudeDir(), "plugins", "synced"),
		filepath.Join(config.ClaudeDir(), "plugins", "local"),
	} {
		machines, _ := filepath.Glob(filepath.Join(base, "*", "*", "skills"))
		dirs = append(dirs, machines...)
	}
	for _, pd := range plugins.EnabledPluginDirs() {
		if st, err := os.Stat(filepath.Join(pd, "skills")); err == nil && st.IsDir() {
			dirs = append(dirs, filepath.Join(pd, "skills"))
		}
	}
	return dirs
}

func loadSkillIndex(cwd string) string {
	seen := map[string]bool{}
	var b strings.Builder
	dirs := skillRoots(cwd)
	for _, dir := range dirs {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			f := filepath.Join(dir, e.Name(), "SKILL.md")
			fb, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			fm := parseFrontmatter(string(fb))
			name := fm["name"]
			if name == "" {
				name = e.Name()
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			fmt.Fprintf(&b, "- %s: %s\n", name, fm["description"])
		}
	}
	return b.String()
}

// parseFrontmatter extracts simple "key: value" pairs from --- yaml ---.
func parseFrontmatter(s string) map[string]string {
	out := map[string]string{}
	if !strings.HasPrefix(s, "---") {
		return out
	}
	rest := s[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return out
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(strings.Trim(v, " \t\"'"))
	}
	return out
}

var scratchOnce = map[string]string{}

func scratchDir(cwd, sessionID string) string {
	if d, ok := scratchOnce[sessionID]; ok {
		return d
	}
	d := filepath.Join(os.TempDir(), fmt.Sprintf("claude-%d", os.Getuid()),
		projectSlug(cwd), sessionID, "scratchpad")
	if err := os.MkdirAll(d, 0o755); err != nil {
		return ""
	}
	scratchOnce[sessionID] = d
	return d
}

// SkillNames returns skill names visible to this session (user/project/plugin).
func SkillNames(cwd string) []string {
	var out []string
	seen := map[string]bool{}
	for _, base := range skillRoots(cwd) {
		ents, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, ent := range ents {
			if !ent.IsDir() || seen[ent.Name()] {
				continue
			}
			if _, err := os.Stat(filepath.Join(base, ent.Name(), "SKILL.md")); err == nil {
				seen[ent.Name()] = true
				out = append(out, ent.Name())
			}
		}
	}
	sort.Strings(out)
	return out
}
