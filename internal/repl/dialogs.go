package repl

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"claude-go/internal/agent"
	"claude-go/internal/hooks"
	"claude-go/internal/perms"
	"claude-go/internal/tools"
)

// permAskTimeout is the unattended auto-deny window (mod_1477's
// autoDenyWindow): after this the permission prompt resolves as "No" so a
// detached session can't wedge on an unanswered dialog.
const permAskTimeout = 5 * time.Minute

// briefMode is the ctrl+shift+b (app:toggleBrief) flag — when set, tool
// output previews collapse to a single line.
var briefMode atomic.Bool

func toggleBrief() {
	on := !briefMode.Load()
	briefMode.Store(on)
	state := "off"
	if on {
		state = "on — tool output collapses to 1 line"
	}
	compInline(fmt.Sprintf("\r\x1b[2K  %sbrief mode %s%s\r\n", dim, state, reset))
}

// askEx is the full permission prompt (mod_1477): an optional
// decision-reason line when a rule/hook/classifier forced the ask, a
// four-option menu where option 4 captures denial feedback, and an
// auto-deny after permAskTimeout unattended.
//
// Returns (allow, feedback). feedback is the text captured by option 4 —
// the caller delivers it to the model as the next user turn (see
// queuePermFeedback), which is the closest available channel to real CC's
// "deny with feedback" tool result.
func (p *promptUI) askEx(l *agent.Loop, ks *keySource, name, summary, reason, config string, input json.RawMessage) (bool, string) {
	if p.always == nil {
		p.always = map[string]bool{}
	}
	if p.always[name] {
		return true, ""
	}
	// PermissionRequest hook can auto-decide without prompting (real event)
	if l.Hooks != nil {
		pr := hooks.Run(l.Hooks, hooks.PermissionRequest, map[string]any{
			"session_id": l.Client.SessionID, "cwd": l.ToolCtx.CWD,
			"tool_name": name, "tool_input": json.RawMessage(input),
			"hook_event_name": "PermissionRequest", "permission_mode": l.PermMode,
		}, l.ToolCtx.CWD)
		switch pr.Permission {
		case "allow":
			return true, ""
		case "deny":
			return false, pr.Reason
		}
	}
	// take over stdin for the dialog (turn busy-loop forwards to us)
	dch := make(chan key, 16)
	ks.dialog.Store(&dch)
	defer ks.dialog.Store(nil)
	notifyAttention(l.ToolCtx.CWD)
	if l.Hooks != nil {
		hooks.Run(l.Hooks, hooks.Notification, map[string]any{
			"session_id": l.Client.SessionID, "cwd": l.ToolCtx.CWD,
			"message":           "Claude needs permission to use " + name,
			"notification_type": "permission_prompt", // matcher field
			"tool_name":         name,
			"hook_event_name":   "Notification",
			"permission_mode":   l.PermMode,
		}, l.ToolCtx.CWD)
	}
	// modal dialog owns the screen: un-pin the live zone so the box renders
	// and scrolls inline instead of fighting the footer
	compSuspend()
	defer compResume()
	// real dialog: titled bordered box, tool card with key:value rows (Edit
	// gets a colored diff preview), decision-reason line, live M:SS countdown
	fmt.Printf("\n%s╭─ %s%s%s\n", yellow, bold+name+reset, dim+" ─ permission"+reset, reset)
	printToolCard(name, input, summary)
	if reason != "" {
		fmt.Printf("  %s%s%s\n", yellow, reason, reset)
		if config != "" {
			fmt.Printf("  %s%s%s\n", dim, config, reset)
		}
	}
	deadline := time.Now().Add(permAskTimeout)
	// live countdown — real ticks `in M:SS`
	printOpts := func(rem time.Duration) {
		fmt.Printf("\r\x1b[K  %s⏵ auto-deny in %d:%02d — to avoid blocking an unattended session%s\n", dim, int(rem.Minutes()), int(rem.Seconds())%60, reset)
		fmt.Printf("  %s1.%s Yes\n  %s2.%s Yes, and don't ask again for %s in %s\n  %s3.%s No\n  %s4.%s No, and tell Claude what to do differently\n  %s⌘ enter confirm · escape cancel%s\n\x1b[6A\r",
			bold, reset, bold, reset, name, l.ToolCtx.CWD, bold, reset, bold, reset, dim, reset)
	}
	printOpts(permAskTimeout)
	timeout := time.NewTimer(permAskTimeout)
	tick := time.NewTicker(time.Second)
	defer timeout.Stop()
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			rem := time.Until(deadline)
			if rem > 0 {
				printOpts(rem)
			}
		case <-timeout.C:
			fmt.Print("\x1b[6B") // below the options block
			fmt.Printf("\n  %s⏵ auto-denied — unattended for %s%s\n", dim, permAskTimeout, reset)
			return false, ""
		case k, ok := <-dch:
			if !ok {
				return false, ""
			}
			fmt.Print("\x1b[6B\r") // clear the options block
			switch k.b {
			case '1', 'y', 'Y', '\r':
				fmt.Println("1. Yes")
				return true, ""
			case '2', 'a', 'A':
				fmt.Println("2. Yes, always")
				p.always[name] = true
				// real writes a permission rule to .claude/settings.local.json
				if rule := ruleSuggestion(name, summary); rule != "" {
					addLocalPermissionRule(l.ToolCtx.CWD, rule)
					fmt.Printf("  %s%s added to permission allowlist%s\n", dim, rule, reset)
				}
				return true, ""
			case '3', 'n', 'N', 0x1b, 3:
				fmt.Println("3. No")
				return false, ""
			case '4', 'd', 'D':
				fmt.Println("4. No, with feedback")
				return false, readFeedbackLine(ks, deadline)
			}
			fmt.Print("\x1b[6A\r") // unrecognized — back to the block
			printOpts(time.Until(deadline))
		}
	}
}

// readFeedbackLine captures the one-line "tell Claude what to do
// differently" text for option 4. The auto-deny deadline stays armed —
// walking away mid-capture denies with no feedback.
func readFeedbackLine(ks *keySource, deadline time.Time) string {
	fmt.Printf("  %stell Claude what to do differently:%s ", dim, reset)
	var buf []rune
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	for {
		select {
		case <-t.C:
			fmt.Printf("\n  %s⏵ auto-denied — unattended%s\n", dim, reset)
			return ""
		case k, ok := <-ks.ch:
			if !ok {
				return ""
			}
			switch {
			case k.b == '\r' || k.b == '\n':
				fmt.Println()
				return strings.TrimSpace(string(buf))
			case k.b == 0x1b && k.seq == "":
				fmt.Println()
				return ""
			case k.b == 3:
				fmt.Println()
				return ""
			case k.b == 0x7f || k.b == 8:
				if len(buf) > 0 {
					buf = buf[:len(buf)-1]
					os.Stdout.WriteString("\b \b")
				}
			case k.paste != "":
				buf = append(buf, []rune(k.paste)...)
				os.Stdout.WriteString(k.paste)
			case k.r != 0:
				buf = append(buf, k.r)
				os.Stdout.WriteString(string(k.r))
			case k.b >= 32 && k.b < 127:
				buf = append(buf, rune(k.b))
				os.Stdout.WriteString(string(k.b))
			}
		}
	}
}

// askReason builds the mod_1477 decision-reason line shown above the
// permission options when a rule forced the ask. Hooks can only block in
// this build (loop.go doesn't consult hooks.Result.Permission), so the
// rule case is the reachable one; callers pass "" for unforced asks.
func askReason(l *agent.Loop, name string, input json.RawMessage) (reason, config string) {
	r := matchedAskRule(l.PermRules["ask"], name, input, l.ToolCtx.CWD)
	if r == "" {
		return "", ""
	}
	subj := askSubject(name)
	if perms.Mode(l.PermMode) == perms.ModeAuto {
		return fmt.Sprintf("Ask rule `%s` overrides auto mode for this %s.", r, subj),
			"/permissions to let auto mode decide"
	}
	return fmt.Sprintf("Permission rule `%s` requires confirmation for this %s.", r, subj),
		"/permissions to update rules"
}

// matchedAskRule finds the first configured ask rule matching this call by
// re-checking each rule in isolation — in ModeDontAsk an unmatched call
// resolves to Deny, so an Ask verdict means this rule fired. This reuses
// the real matcher (subcommand splitting, prefix/glob rules) instead of
// duplicating it.
func matchedAskRule(rules []string, name string, input json.RawMessage, cwd string) string {
	for _, r := range rules {
		if perms.Check(perms.ModeDontAsk, &perms.Rules{Ask: []string{r}}, name, input, cwd) == perms.Ask {
			return r
		}
	}
	return ""
}

// askSubject is the noun in "requires confirmation for this X".
func askSubject(name string) string {
	switch name {
	case "Bash", "Monitor":
		return "command"
	case "Edit", "Write", "NotebookEdit":
		return "edit"
	}
	return "tool call"
}

// queuePermFeedback delivers deny-with-feedback text as the next user turn
// (the drain loop picks it up after this turn ends). Real CC embeds it in
// the denied tool result; the tool-result text is fixed in agent.run, so a
// queued user message is the faithful stand-in.
func queuePermFeedback(fb string) {
	select {
	case tools.SessionQueue <- fb:
		fmt.Printf("  %sfeedback queued — sent when this turn ends%s\n", dim, reset)
	default:
	}
}

// ---------- trust dialog inventory (mod_1849) ----------

// trustInventory enumerates what a folder's configuration will do without
// asking — the lines real CC lists under the trust question before the
// yes/no. Only folder-scoped sources count: user-level settings apply
// regardless of this trust decision.
func trustInventory(cwd string) []string {
	type settingsFile struct{ label, path string }
	files := []settingsFile{
		{".claude/settings.json", filepath.Join(cwd, ".claude", "settings.json")},
		{".claude/settings.local.json", filepath.Join(cwd, ".claude", "settings.local.json")},
	}
	hookCount := 0
	var hookSrcs, envKeys, envSrcs, allowRules, allowSrcs, addDirs, dirSrcs, helperNotes []string
	allowCount, dirCount := 0, 0
	for _, f := range files {
		b, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		if h, ok := m["hooks"].(map[string]any); ok {
			n := 0
			for _, ms := range h {
				arr, _ := ms.([]any)
				for _, it := range arr {
					mm, _ := it.(map[string]any)
					if hs, ok := mm["hooks"].([]any); ok {
						n += len(hs)
					}
				}
			}
			if n > 0 {
				hookCount += n
				hookSrcs = append(hookSrcs, f.label)
			}
		}
		if env, ok := m["env"].(map[string]any); ok && len(env) > 0 {
			for k := range env {
				envKeys = append(envKeys, k)
			}
			envSrcs = append(envSrcs, f.label)
		}
		if pm, ok := m["permissions"].(map[string]any); ok {
			if al, ok := pm["allow"].([]any); ok && len(al) > 0 {
				allowCount += len(al)
				for _, r := range al {
					if s, ok := r.(string); ok {
						allowRules = append(allowRules, s)
					}
				}
				allowSrcs = append(allowSrcs, f.label)
			}
			if ad, ok := pm["additionalDirectories"].([]any); ok && len(ad) > 0 {
				dirCount += len(ad)
				for _, r := range ad {
					if s, ok := r.(string); ok {
						addDirs = append(addDirs, s)
					}
				}
				dirSrcs = append(dirSrcs, f.label)
			}
		}
		for _, k := range []string{"apiKeyHelper", "awsAuthRefresh", "headersHelper"} {
			if _, ok := m[k]; ok {
				helperNotes = append(helperNotes, k+" in "+f.label)
			}
		}
	}

	// .mcp.json servers launch without asking — real collects them "in this
	// folder or above it".
	home, _ := os.UserHomeDir()
	mcpSeen := map[string]bool{}
	var mcpNames []string
	for dir := cwd; ; {
		if b, err := os.ReadFile(filepath.Join(dir, ".mcp.json")); err == nil {
			var m struct {
				MCPServers map[string]any `json:"mcpServers"`
			}
			if json.Unmarshal(b, &m) == nil {
				for n := range m.MCPServers {
					if !mcpSeen[n] {
						mcpSeen[n] = true
						mcpNames = append(mcpNames, n)
					}
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir || dir == home {
			break
		}
		dir = parent
	}
	sort.Strings(mcpNames)

	var lines []string
	if len(helperNotes) > 0 {
		lines = append(lines, "This folder runs credential/header helper commands: "+
			strings.Join(helperNotes, ", "))
	}
	if hookCount > 0 {
		lines = append(lines, fmt.Sprintf("This folder configures %d hook %s that run commands, declared in %s",
			hookCount, pluralize(hookCount, "command", "commands"), joinUnique(hookSrcs)))
	}
	if len(mcpNames) > 0 {
		lines = append(lines, fmt.Sprintf("Sessions started here launch, without asking, %d MCP %s from .mcp.json files in this folder or above it: %s",
			len(mcpNames), pluralize(len(mcpNames), "server", "servers"), trunc(strings.Join(mcpNames, ", "), 200)))
	}
	if allowCount > 0 {
		lines = append(lines, fmt.Sprintf("This folder pre-approves %d tool %s in %s: %s",
			allowCount, pluralize(allowCount, "permission", "permissions"), joinUnique(allowSrcs),
			trunc(strings.Join(uniqueSorted(allowRules), ", "), 200)))
	}
	if dirCount > 0 {
		lines = append(lines, fmt.Sprintf("This folder adds %d %s to the workspace in %s: %s",
			dirCount, pluralize(dirCount, "directory", "directories"), joinUnique(dirSrcs),
			trunc(strings.Join(uniqueSorted(addDirs), ", "), 200)))
	}
	if len(envKeys) > 0 {
		lines = append(lines, fmt.Sprintf("This folder sets environment variables for every session here (%s), declared in %s",
			trunc(strings.Join(uniqueSorted(envKeys), ", "), 120), joinUnique(envSrcs)))
	}
	if len(lines) > 0 {
		lines = append(lines, "These will apply without asking. Only proceed if you trust this configuration.")
	}
	return lines
}

// printTrustInventory renders the risk list under the trust question.
func printTrustInventory(cwd string) {
	for _, ln := range trustInventory(cwd) {
		fmt.Printf("  %s!%s %s\n", yellow, reset, ln)
	}
}

func pluralize(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func joinUnique(ss []string) string {
	return strings.Join(uniqueSorted(ss), ", ")
}

func uniqueSorted(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// ---------- /terminal-setup ----------

// handleSlashLocal handles the slash commands implemented in this file
// (commands.go is owned separately — these are intercepted before it).
// Returns true when the command was handled.
func handleSlashLocal(l *agent.Loop, line string, ed *editor, ks *keySource) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	switch strings.TrimPrefix(fields[0], "/") {
	case "terminal-setup":
		terminalSetup()
		return true
	}
	return false
}

// terminalSetup installs (or explains) the terminal keybindings real CC
// sets up: shift+enter sends a newline. macOS only; other platforms get an
// honest "unsupported".
func terminalSetup() {
	if runtime.GOOS != "darwin" {
		fmt.Println("  /terminal-setup currently supports macOS terminals only")
		return
	}
	home, _ := os.UserHomeDir()
	switch tp := os.Getenv("TERM_PROGRAM"); tp {
	case "vscode", "cursor", "windsurf":
		app := map[string]string{
			"vscode": "Code", "cursor": "Cursor", "windsurf": "Windsurf",
		}[tp]
		kb := filepath.Join(home, "Library", "Application Support", app, "User", "keybindings.json")
		installShiftEnterBinding(kb)
	case "iTerm.app":
		fmt.Println("  iTerm2: bind Shift+Enter to a newline manually —")
		fmt.Println("    Settings → Profiles → Keys → Key Mappings → +")
		fmt.Println("    Shortcut: ⇧↩   Action: Send Escape Sequence   Esc+: [13;2u")
	case "Apple_Terminal":
		fmt.Println("  Terminal.app can't bind Shift+Enter; to make Option act as Meta:")
		fmt.Println("    defaults write com.apple.Terminal UseOptionAsMetaKey -bool true")
		fmt.Println("  (or Settings → Profiles → Keyboard → “Use Option as Meta key”)")
	default:
		fmt.Printf("  unsupported terminal (TERM_PROGRAM=%q)\n", tp)
		fmt.Println("  supported: VS Code, Cursor, Windsurf (auto-install); iTerm2, Terminal.app (manual)")
	}
}

// installShiftEnterBinding appends the shift+enter sendSequence binding to
// a VS Code-family keybindings.json (an array; JSONC comments mean we defer
// to manual instructions rather than clobbering the file).
func installShiftEnterBinding(kb string) {
	var arr []any
	if b, err := os.ReadFile(kb); err == nil {
		if json.Unmarshal(b, &arr) != nil {
			fmt.Println("  couldn't parse", kb, "(comments?) — add manually:")
			fmt.Println(`    {"key":"shift+enter","command":"workbench.action.terminal.sendSequence","args":{"text":"\u001b\r"}}`)
			return
		}
	}
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok &&
			m["key"] == "shift+enter" &&
			m["command"] == "workbench.action.terminal.sendSequence" {
			fmt.Println("  shift+enter binding already present in", kb)
			return
		}
	}
	arr = append(arr, map[string]any{
		"key":     "shift+enter",
		"command": "workbench.action.terminal.sendSequence",
		"args":    map[string]any{"text": "\x1b\r"},
	})
	os.MkdirAll(filepath.Dir(kb), 0o755)
	b, _ := json.MarshalIndent(arr, "", "  ")
	if err := os.WriteFile(kb, b, 0o644); err != nil {
		fmt.Println("  write failed:", err)
		return
	}
	fmt.Println("  installed shift+enter → newline in", kb)
	fmt.Println("  reload the editor window to pick it up")
}

// printToolCard renders the tool input as real's card: key: value rows for
// scalars (capped), a colored diff for Edit, and a sentinel for oversized
// parameter sets.
func printToolCard(name string, input json.RawMessage, summary string) {
	var kv map[string]any
	if len(input) == 0 || json.Unmarshal(input, &kv) != nil {
		fmt.Printf("  %s(parameters are not an object — deny unless expected)%s\n", dim, reset)
		return
	}
	if name == "Edit" {
		printEditCard(kv)
		return
	}
	keys := 0
	for _, k := range sortedKeys(kv) {
		if keys >= 8 {
			fmt.Printf("  %s… (+%d more parameters)%s\n", dim, len(kv)-keys, reset)
			break
		}
		v := fmt.Sprint(kv[k])
		if len(v) > 200 {
			v = trunc(v, 200) + dim + " …" + reset
		}
		fmt.Printf("  %s%-16s%s %s\n", dim, k, reset, v)
		keys++
	}
	if len(kv) > 12 {
		fmt.Printf("  %s(%d parameters — too many to show — deny unless expected)%s\n", dim, len(kv), reset)
	}
}

// printEditCard shows old_string/new_string as red/green hunks (real shows a
// colored diff preview inside the permission box).
func printEditCard(kv map[string]any) {
	fmt.Printf("  %s%-16s%s %s\n", dim, "file_path", reset, kv["file_path"])
	old, _ := kv["old_string"].(string)
	newS, _ := kv["new_string"].(string)
	for _, ln := range strings.Split(strings.TrimRight(old, "\n"), "\n") {
		fmt.Printf("    %s-%s %s%s\n", red, reset, red, trunc(ln, 110)+reset)
	}
	for _, ln := range strings.Split(strings.TrimRight(newS, "\n"), "\n") {
		fmt.Printf("    %s+%s %s%s\n", green, reset, green, trunc(ln, 110)+reset)
	}
	if len(strings.Split(old, "\n")) > 8 || len(strings.Split(newS, "\n")) > 8 {
		fmt.Printf("    %s… (truncated diff)%s\n", dim, reset)
	}
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
