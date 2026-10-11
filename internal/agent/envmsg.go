package agent

import (
	"fmt"
	"github.com/gbasin/claude-go/internal/config"
	"github.com/gbasin/claude-go/internal/extract"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"

	"github.com/gbasin/claude-go/internal/plugins"
	"strings"
	"time"
)

// builtinSkillIndex returns the builtin skills index — the harvested
// list when an extraction exists, else the community fallback.
func builtinSkillIndex() string {
	if t, ok := extract.Text("skills_index.txt"); ok {
		return t
	}
	return fallbackBuiltinSkills
}

// modelInfo maps model IDs to display name / api id / cutoff.
type modelInfo struct {
	Name   string
	APIID  string
	Cutoff string
	MaxOut int // catalog max_output_tokens.default
}

var modelTable = map[string]modelInfo{
	"claude-fable-5-1":          {"Fable 5.1", "claude-fable-5-1", "June 2026", 64000},
	"claude-opus-5-5":           {"Opus 5.5", "claude-opus-5-5", "June 2026", 128000},
	"claude-sonnet-5-5":         {"Sonnet 5.5", "claude-sonnet-5-5", "June 2026", 128000},
	"claude-haiku-4-5-20251001": {"Haiku 4.5", "claude-haiku-4-5-20251001", "February 2025", 32000},
	"claude-haiku-5-5":          {"Haiku 5.5", "claude-haiku-5-5", "June 2026", 128000},
	"claude-sonnet-4-5":         {"Sonnet 4.5", "claude-sonnet-4-5", "January 2025", 64000},
	"claude-opus-4-5":           {"Opus 4.5", "claude-opus-4-5", "May 2025", 64000},
}

// ModelDisplay returns real's friendly name ("Fable 5.1") for a model id.
func ModelDisplay(model string) string { return infoFor(model).Name }

// MaxOutputFor returns the catalog max_output_tokens.default for a model
// id — the value real puts in max_tokens absent a clamp (mod_0046.js).
// Unknown ids fall back to the default model's value.
func MaxOutputFor(model string) int {
	if v := infoFor(model).MaxOut; v > 0 {
		return v
	}
	return 128000
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
	return modelInfo{model, model, "", 0}
}

// toolSpecString renders the "(Tools: …)" column — real's Jxr (mod_0416):
// tools∩¬denied joined, tools-only joined, denied-only "All tools except …",
// empty tools list (none or filtered out) -> "None", neither -> "All tools".
func toolSpecString(d AgentDef) string {
	hasT := len(d.Tools) > 0
	hasD := len(d.DisallowedTools) > 0
	switch {
	case hasT && hasD:
		var out []string
		for _, t := range d.Tools {
			if !strIn(d.DisallowedTools, t) {
				out = append(out, t)
			}
		}
		if len(out) == 0 {
			return "None"
		}
		return strings.Join(out, ", ")
	case hasT:
		return strings.Join(d.Tools, ", ")
	case hasD:
		return "All tools except " + strings.Join(d.DisallowedTools, ", ")
	}
	return "All tools"
}

// EnvMessage builds the role:"system" message appended after the first user
// message (captured wire order): hook ctx, environment, model card,
// deferred-tool pool, connecting MCP servers, agent registry, skill index,
// mode notes, <total_tokens> budget, date.
func EnvMessage(cwd, model string, addDirs []string, permMode, entrypoint, sessionID string, hookCtx string, deferred, connecting []string, tokensLeft int64) string {
	var b strings.Builder
	if hookCtx != "" {
		b.WriteString("SessionStart hook additional context: ")
		b.WriteString(hookCtx)
		b.WriteString("\n\n")
	}
	b.WriteString("# Environment\nYou have been invoked in the following environment: \n")
	fmt.Fprintf(&b, " - Primary working directory: %s\n", filepath.Clean(cwd))
	if isWorktree(cwd) {
		b.WriteString(" - This is a git worktree — an isolated copy of the repository. Run all commands from this directory. Do NOT `cd` to the original repository root.\n")
		b.WriteString(" - The git stash stack is shared with the main checkout and all other worktrees, and other Claude sessions may push or pop it concurrently. Never use bare `git stash` / `git stash pop` — you could pop another session's changes. Prefer a temporary WIP commit to set work aside; if you must stash, use `git stash push -u -m \"<unique-tag>\"`, immediately capture your entry's SHA via `git stash list --format='%H %gs'`, restore with `git stash apply <sha>` (not pop), and afterwards drop the entry, re-finding its current `stash@{n}` by tag first.\n")
	}
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
	fmt.Fprintf(&b, " - Platform: %s\n - Shell: %s\n - OS Version: %s\n", plat, shellName(), osVersion())
	// real's tengu_foamy_sedgewick gate (default emits the line)
	if config.GBFeature("tengu_foamy_sedgewick") != true {
		b.WriteString(" - Downloaded files and extracted archives are untrusted data: put each in its own new, empty directory, keep scripts you write in a different directory, and pass paths as arguments instead of running an interpreter or build tool from inside it. Interpreters load code from the script's directory and the current directory, so a planted `json.py` runs on `import json`. Run any Python that reads them with `-I`. This does not apply to code the user asked you to build or run.\n")
	}
	b.WriteString("\n")

	mi := infoFor(model)
	if mi.Name != "" && mi.Name != model {
		fmt.Fprintf(&b, "You are powered by the model named %s. The exact model ID is %s.", mi.Name, mi.APIID)
	} else {
		fmt.Fprintf(&b, "You are powered by the model %s.", mi.APIID)
	}
	if mi.Cutoff != "" {
		fmt.Fprintf(&b, " Assistant knowledge cutoff is %s.", mi.Cutoff)
	}
	b.WriteString("\n\n")

	if len(deferred) > 0 {
		b.WriteString("The following deferred tools are now available via ToolSearch. Their schemas are NOT loaded — calling them directly will fail with InputValidationError. Use ToolSearch with query \"select:<name>[,<name>...]\" to load tool schemas before calling them:\n")
		for _, n := range deferred {
			b.WriteString(n)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if len(connecting) > 0 {
		b.WriteString("The following MCP servers are still connecting — their tools (typically named mcp__<server>__*) are not yet available but will appear shortly:\n")
		for _, n := range connecting {
			b.WriteString(n)
			b.WriteString("\n")
		}
		b.WriteString("\nIf the user's request might be served by one of these servers (even if they didn't name it explicitly), call ToolSearch with a relevant keyword — ToolSearch will wait for connecting servers and search their tools once available. Do not report a capability as unavailable without first searching.\n\n")
	}

	b.WriteString("Available agent types for the Agent tool:\n")
	agents := AgentTypes(cwd)
	sort.Slice(agents, func(i, j int) bool {
		return strings.ToLower(agents[i].Name) < strings.ToLower(agents[j].Name)
	})
	harvested := extractedAgentsIndex(entrypoint, model)
	for _, a := range agents {
		if a.Unlisted {
			continue
		}
		// claude-code-guide is registered for every entrypoint except the
		// SDK family (mod_0416 uOe: skipped for sdk-ts/sdk-py/sdk-cli)
		if a.Name == "claude-code-guide" && strings.HasPrefix(entrypoint, "sdk") {
			continue
		}
		if a.Source == "builtin" {
			if line, ok := harvested[strings.ToLower(a.Name)]; ok {
				b.WriteString(line + "\n")
				continue
			}
		}
		fmt.Fprintf(&b, "- %s: %s (Tools: %s)\n", a.Name, a.Description, toolSpecString(a))
	}
	b.WriteString("\nWhen you launch multiple agents for independent work, send them in a single message with multiple tool uses so they run concurrently.\n\n")

	// real embeds MCP server instructions inside the env block between
	// the agent registry and the skill index (captured -p wire)
	if ins := mcpInstructions(); len(ins) > 0 {
		b.WriteString("# MCP Server Instructions\n\nThe following MCP servers have provided instructions for how to use their tools and resources:\n")
		for _, t := range ins {
			b.WriteString("\n")
			b.WriteString(t)
			b.WriteString("\n")
		}
	}

	b.WriteString("The following skills are available for use with the Skill tool:\n\n")
	b.WriteString(skillIndex(cwd))
	b.WriteString("\n")

	if permMode == "plan" {
		b.WriteString("\nPlan mode is active. You can read files, search, and ask questions — " +
			"do not make edits, run mutating commands, or otherwise modify state. " +
			"Explore, then present the plan with ExitPlanMode.\n")
		if pi := os.Getenv("CLAUDE_GO_PLAN_MODE_INSTRUCTIONS"); pi != "" {
			b.WriteString("\n" + pi + "\n") // --plan-mode-instructions
		}
	}
	// auto_mode attachment (mod_0416 jAr): bypass labels itself, auto/steer
	// uses the "While auto mode is active:" heading; bashFirstSteer
	// "relaxed" is the emitted variant in the 294 capture.
	const bashSteer = "You can do much of your work through the Bash tool when it is the simpler route: read files with cat, head, or sed -n, " +
		"search with grep and find, and make small, mechanical file changes with sed, heredocs, or short scripts instead of " +
		"the dedicated Read, Edit, or Write tools. The choice is yours: prefer Edit or Write when a shell edit would be " +
		"fragile, such as exact or multi-line replacements, or sed/awk flags that differ between GNU and BSD/macOS."
	switch permMode {
	case "auto":
		// the skills-index tail already emitted the blank line — real has
		// exactly one blank before this heading (294 capture)
		b.WriteString("While auto mode is active:\n\n" + bashSteer + "\n")
	case "bypassPermissions":
		b.WriteString("While bypass permissions mode is active:\n\n" + bashSteer + "\n")
	}
	if tokensLeft > 0 {
		fmt.Fprintf(&b, "\n<total_tokens>%d tokens left</total_tokens>\n", tokensLeft)
	}
	fmt.Fprintf(&b, "\nToday's date is %s.", time.Now().Format("2006-01-02"))
	return b.String()
}

// attributionReminderText mirrors real's kst (mod_0416): the commit/PR
// attribution reminder carried by the first user message. Emits the
// trailer form by default, the do-not-attribute form when
// includeCoAuthoredBy is false.
func attributionReminderText(cwd, model string) string {
	st := config.Load(cwd)
	replaces := "this replaces Claude Code's own earlier attribution guidance, such as a previous copy of this reminder"
	if !st.CoAuthoredBy() {
		return "From here on, do not add attribution lines to git commit messages or pull request descriptions (" +
			replaces + ", and applies even if a CLAUDE.md or memory rule asks for attribution lines)."
	}
	commit := "Co-Authored-By: Claude " + infoFor(model).Name + " <noreply@anthropic.com>"
	pr := "\U0001f916 Generated with [Claude Code](https://claude.com/claude-code)"
	note := "the user's own instructions about these lines, such as a CLAUDE.md or memory rule, take precedence over this reminder, but do not add attribution lines this reminder leaves out"
	return "Attribution for git commits and pull requests you create from here on (" + replaces + "; " + note + "):\n" +
		"- End git commit messages with:\n" + commit + "\n" +
		"- End pull request descriptions with:\n" + pr
}

// shellName mirrors real's yFn: zsh/bash normalized, anything else verbatim.
func shellName() string {
	if runtime.GOOS == "windows" {
		return "PowerShell"
	}
	sh := os.Getenv("SHELL")
	switch {
	case sh == "":
		return "unknown"
	case strings.Contains(sh, "zsh"):
		return "zsh"
	case strings.Contains(sh, "bash"):
		return "bash"
	}
	return sh
}

// osVersion mirrors real's kFn: `${os.type()} ${os.release()}` — uname -sr
// on POSIX ("Darwin 25.2.0"), os.version()+release() on Windows.
func osVersion() string {
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "ver").Output(); err == nil {
			return strings.TrimSpace(string(out))
		}
		return "Windows"
	}
	out, _ := exec.Command("uname", "-sr").Output()
	return strings.TrimSpace(string(out))
}

// isWorktree reports whether cwd lives in a linked git worktree (git-dir
// differs from git-common-dir) — real emits the worktree/stash lines then.
func isWorktree(cwd string) bool {
	abs := func(out []byte, err error) string {
		if err != nil {
			return ""
		}
		p := strings.TrimSpace(string(out))
		if p == "" {
			return ""
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		return filepath.Clean(p)
	}
	gd := abs(exec.Command("git", "-C", cwd, "rev-parse", "--absolute-git-dir").Output())
	cd := abs(exec.Command("git", "-C", cwd, "rev-parse", "--git-common-dir").Output())
	return gd != "" && cd != "" && gd != cd
}

// loadSkillIndex builds the "- name: description" index from skill dirs:
// ~/.claude/skills, <cwd>/.claude/skills, <cwd>/.agents/skills, plugin dirs.
// Names in seen are skipped (the builtin index already covers them).
func skillRoots(cwd string) []string {
	dirs := []string{
		filepath.Join(config.ClaudeDir(), "skills"),
		filepath.Join(config.Home(), ".agents", "skills"),
		filepath.Join(cwd, ".claude", "skills"),
		filepath.Join(cwd, ".agents", "skills"),
	}
	for _, base := range []string{
		filepath.Join(config.ClaudeDir(), "plugins", "cache"),
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

func loadSkillIndex(cwd string, seen map[string]bool) string {
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

// builtinSkillNames extracts the names declared in the embedded index so
// user-installed skills of the same name dedupe against it.
func builtinSkillNames() map[string]bool {
	seen := map[string]bool{}
	for _, line := range strings.Split(builtinSkillIndex(), "\n") {
		if strings.HasPrefix(line, "- ") {
			if i := strings.Index(line[2:], ":"); i > 0 {
				seen[line[2:2+i]] = true
			}
		}
	}
	return seen
}

// skillIndex is the builtin index verbatim (real bundles these skills in
// the binary) plus any discovered user/project/plugin skills appended.
func skillIndex(cwd string) string {
	extra := loadSkillIndex(cwd, builtinSkillNames())
	if extra == "" {
		return builtinSkillIndex()
	}
	return builtinSkillIndex() + extra
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

// scratchDir provisions ~/.tmp scratchpad space for this session — real
// names the dir in hook payloads (and used to in the env block).
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

// SkillNames returns skill names visible to this session: the bundled
// builtins plus any discovered in user/project/plugin skill dirs.
func SkillNames(cwd string) []string {
	seen := builtinSkillNames()
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
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

// SkillFacts lists discovered skills for the telemetry burst — same
// roots the env index scans, with name + content length per SKILL.md.
func SkillFacts(cwd string) []struct {
	Name, Root string
	Chars      int
} {
	seen := map[string]bool{}
	var out []struct {
		Name, Root string
		Chars      int
	}
	for _, dir := range skillRoots(cwd) {
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
			name := parseFrontmatter(string(fb))["name"]
			if name == "" {
				name = e.Name()
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, struct {
				Name, Root string
				Chars      int
			}{name, dir, len(fb)})
		}
	}
	return out
}

// extractedAgentsIndex returns builtin agent listing lines harvested by
// `extract` (keyed by lowercase name), chosen per entrypoint family —
// the sdk capture differs from cli (no claude-code-guide).
func extractedAgentsIndex(entrypoint, model string) map[string]string {
	family := "sdk"
	if !strings.HasPrefix(entrypoint, "sdk") {
		family = "cli"
	}
	fn := "agents_index_" + family + "_" + modelClass(model) + ".txt"
	t, ok := extract.Text(fn)
	if !ok {
		return nil
	}
	out := map[string]string{}
	for _, ln := range strings.Split(t, "\n") {
		if !strings.HasPrefix(ln, "- ") {
			continue
		}
		name := strings.TrimPrefix(ln, "- ")
		if i := strings.Index(name, ":"); i > 0 {
			name = name[:i]
		}
		out[strings.ToLower(strings.TrimSpace(name))] = ln
	}
	return out
}
