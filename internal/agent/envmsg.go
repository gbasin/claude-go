package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
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

// builtinAgents are the default agent types with their tool access.
var builtinAgents = []agentDef{
	{"general-purpose", "General-purpose agent for researching complex questions, searching for code, and executing multi-step tasks. When you are searching for a keyword or file and are not confident that you will find the right match in the first few tries use this agent to perform the search for you.", "*"},
	{"Explore", "Read-only search agent for broad fan-out searches — when answering means sweeping many files, directories, or naming conventions and you only need the conclusion, not the file dumps. It reads excerpts rather than whole files, so it locates code; it doesn't review or audit it. Specify search breadth: \"medium\" for moderate exploration, \"very thorough\" for multiple locations and naming conventions.", "All tools except Agent, Artifact, ArtifactComments, ArtifactData, ArtifactCheck, ExitPlanMode, Edit, Write, NotebookEdit"},
	{"Plan", "Software architect agent for designing implementation plans. Use this when you need to plan the implementation strategy for a task. Returns step-by-step plans", "All tools except Agent, Artifact, ArtifactComments, ArtifactData, ArtifactCheck, ExitPlanMode, Edit, Write, NotebookEdit"},
	{"claude", "Catch-all for any task that doesn't fit a more specific agent. FleetView's default when no agent name is typed.", "*"},
	{"code-simplifier", "code-simplifier: Simplifies and refines code for clarity, consistency, and maintainability while preserving all functionality. Focuses on recently modified code unless instructed otherwise.", "All tools"},
	{"code-reviewer", "Expert code review specialist. Proactively reviews code for quality, security, and maintainability. Use immediately after writing or modifying code.", "Task, Bash, Glob, Grep, LS, ExitPlanMode, Read, Edit, MultiEdit, Write, NotebookRead, NotebookEdit, WebFetch, TodoWrite, WebSearch"},
	{"debugger", "Debugging specialist for errors, test failures, and unexpected behavior. Use proactively when encountering any issues.", "All tools"},
	{"statusline-setup", "Use this agent to configure the user's Claude Code status line setting.", "Read, Edit"},
}

// EnvMessage builds the role:"system" message appended after the first user
// message: environment, model card, agent registry, skill index, mode notes.
func EnvMessage(cwd, model string, addDirs []string, permMode string) string {
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
	fmt.Fprintf(&b, " - Platform: %s\n - Shell: %s\n - OS Version: %s\n\n", plat, filepath.Base(shell), osVersion())

	mi := infoFor(model)
	fmt.Fprintf(&b, "You are powered by the model named %s. The exact model ID is %s.", mi.Name, mi.APIID)
	if mi.Cutoff != "" {
		fmt.Fprintf(&b, " Assistant knowledge cutoff is %s.", mi.Cutoff)
	}
	b.WriteString("\n\n")

	b.WriteString("Available agent types for the Agent tool:\n")
	agents := append(append([]agentDef{}, builtinAgents...), loadCustomAgents(cwd)...)
	sort.Slice(agents, func(i, j int) bool {
		return strings.ToLower(agents[i].name) < strings.ToLower(agents[j].name)
	})
	for _, a := range agents {
		fmt.Fprintf(&b, "- %s: %s (Tools: %s)\n", a.name, a.desc, a.toolSpec)
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
	return b.String()
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

type agentDef struct{ name, desc, toolSpec string }

// loadCustomAgents scans agent definition dirs: ~/.claude/agents/*.md and
// <cwd>/.claude/agents/*.md — files have YAML frontmatter (name, description,
// tools) then a markdown system prompt.
func loadCustomAgents(cwd string) []agentDef {
	home, _ := os.UserHomeDir()
	var out []agentDef
	for _, dir := range []string{
		filepath.Join(home, ".claude", "agents"),
		filepath.Join(cwd, ".claude", "agents"),
	} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			fm := parseFrontmatter(string(b))
			name := fm["name"]
			if name == "" {
				name = strings.TrimSuffix(e.Name(), ".md")
			}
			spec := fm["tools"]
			if spec == "" {
				spec = "All tools"
			}
			out = append(out, agentDef{name, fm["description"], spec})
		}
	}
	return out
}

// loadSkillIndex builds the "- name: description" index from skill dirs:
// ~/.claude/skills, <cwd>/.claude/skills, <cwd>/.agents/skills, plugin dirs.
func loadSkillIndex(cwd string) string {
	home, _ := os.UserHomeDir()
	seen := map[string]bool{}
	var b strings.Builder
	for _, dir := range []string{
		filepath.Join(home, ".claude", "skills"),
		filepath.Join(home, ".agents", "skills"),
		filepath.Join(cwd, ".claude", "skills"),
		filepath.Join(cwd, ".agents", "skills"),
	} {
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
