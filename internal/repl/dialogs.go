package repl

import (
	"encoding/json"
	"fmt"
	"golang.org/x/term"
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

// focusMode is /focus — real's "focus view: just your prompt, summary, and
// response". When set, tool-use lines and tool results don't render.
var focusMode atomic.Bool

// antTrace is /ant-trace — per-request usage trace lines after each API
// response.
var antTrace atomic.Bool

// askEx is the full permission prompt (mod_1477): an optional
// decision-reason line when a rule/hook/classifier forced the ask, a
// four-option menu where option 4 captures denial feedback, and an
// auto-deny after permAskTimeout unattended.
//
// Returns (allow, feedback). feedback is the text captured by option 4 —
// the caller delivers it to the model as the next user turn (see
// queuePermFeedback), which is the closest available channel to real CC's
// "deny with feedback" tool result.
// dprintf/dprintln translate \n → \r\n — the editor holds raw mode for
// the whole REPL, so bare \n never carriage-returns and dialog lines
// land mid-row (verified: permission dialogs rendered at stale columns).
// Mid-turn (a stream owns the scroll bottom) they route through the
// tracked stream writer instead — a raw print would land at the
// hardware cursor but leave compStreamCol stale, so the next tracked
// write repositions mid-text and overwrites it.
func dprintf(format string, a ...any) {
	s := fmt.Sprintf(format, a...)
	compMu.Lock()
	streaming := compActive.Load() && compStreamOn && compSusp == 0 && compZoneH > 0
	compMu.Unlock()
	if streaming {
		streamWrite(s) // tracked — keeps the stream position honest
		return
	}
	fmt.Print(strings.ReplaceAll(s, "\n", "\r\n"))
}
func dprintln(a ...any) {
	dprintf("%s", fmt.Sprintln(a...))
}

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
	// real 2.1.291's dialog (captured bytes):
	//   cyan-153 ─ rule · bold "<Tool> command" · auto-mode tip (non-auto
	//   only) · gray-246 description · gray-239 ╌ frame around the content ·
	//   "Do you want to proceed?" · cursor list numbered 1..N ·
	//   "Esc to cancel · Tab to amend"
	cyan153 := "\x1b[38;5;153m"
	gray246 := "\x1b[38;5;246m"
	gray239 := "\x1b[38;5;239m"
	width, _, _ := term.GetSize(int(os.Stdout.Fd()))
	if width <= 0 {
		width = 120
	}
	rule := cyan153 + strings.Repeat("─", width)
	dot := gray239 + strings.Repeat("╌", width)

	// titles + questions are per-tool (captured):
	//   Bash  → "Bash command" / "Do you want to proceed?"
	//   Write → "Create file"  / "Do you want to create <file>?"
	//   Edit/MultiEdit → "Edit file" / "Do you want to edit <file>?"
	title := name
	question := "Do you want to proceed?"
	fname := permFilePath(input)
	switch name {
	case "Bash":
		title = "Bash command"
	case "Write":
		title = "Create file"
		question = "Do you want to create " + fname + "?"
	case "Edit", "MultiEdit":
		title = "Edit file"
		question = "Do you want to edit " + fname + "?"
	}
	desc := permDesc(name, input, summary)
	if desc == "" && isFileTool(name) {
		desc = permFilePath(input) // real shows the path as the gray line
	}
	content := permContent(name, input)

	// the tip rides with the switch-to-auto option (captured: it shows on
	// the Bash dialog, not the Write one)
	tip := ""
	if l.PermMode != "auto" && !isFileTool(name) {
		tip = `Tip: auto mode handles these prompts for you — choose "switch to auto mode" below`
	}
	// options are per-tool-class (captured):
	//   Bash:        Yes / always-allow-<grant> / switch-to-auto / No
	//   file tools:  Yes / switch-to-acceptEdits / No
	alwaysSpec := ruleSuggestion(name, summary)
	alwaysLabel := alwaysSpec
	if dir := outsideDir(name, input, l.ToolCtx.CWD); dir != "" {
		alwaysLabel = "access to " + dir
	}
	type opt struct {
		label  string
		action string
	}
	var opts []opt
	opts = append(opts, opt{label: "Yes", action: "yes"})
	if isFileTool(name) && l.PermMode != "acceptEdits" {
		opts = append(opts, opt{
			label:  "Yes, and switch to accept edits (auto-approve file edits and common file \n      commands) for this session (shift+tab)",
			action: "acceptedits"})
	} else if !isFileTool(name) {
		if alwaysSpec != "" {
			opts = append(opts, opt{label: fmt.Sprintf("Yes, and always allow %s from this project", alwaysLabel), action: "always"})
		}
		if l.PermMode != "auto" {
			opts = append(opts, opt{label: "Yes, and switch to auto mode" + gray246 + " · auto mode handles these prompts for you" + "\x1b[39m", action: "automode"})
		}
	}
	opts = append(opts, opt{label: "No", action: "no"})

	sel := 0
	renderRows := 0
	draw := func() {
		var sb strings.Builder
		if renderRows > 0 {
			fmt.Fprintf(&sb, "\x1b[%dA", renderRows)
		}
		renderRows = 0
		put := func(s string) {
			sb.WriteString("\r\x1b[K" + s + "\r\n")
			renderRows++
		}
		put(rule)
		put(" " + bold + title + unbold + "\x1b[39m")
		if tip != "" {
			put(" " + tip)
		}
		if desc != "" && desc != content {
			put(" " + gray246 + desc + reset)
		}
		put(dot)
		for _, cl := range permContentLines(name, input, content) {
			put(cl)
		}
		put(dot)
		put(" " + question)
		for i, o := range opts {
			if i == sel {
				put(fmt.Sprintf(" %s❯%s %s%d. %s%s%s", cyan153, "\x1b[39m", gray246, i+1, cyan153, o.label, "\x1b[39m"))
			} else {
				put(fmt.Sprintf("   %s%d. %s%s", gray246, i+1, "\x1b[39m", o.label))
			}
		}
		put(" " + gray246 + "Esc to cancel · Tab to amend" + reset)
		fmt.Print(sb.String())
	}
	draw()
	deadline := time.Now().Add(permAskTimeout)
	timeout := time.NewTimer(permAskTimeout)
	defer timeout.Stop()
	deny := func() (bool, string) {
		hooks.Run(l.Hooks, hooks.PermissionDenied, map[string]any{
			"session_id": l.Client.SessionID, "cwd": l.ToolCtx.CWD,
			"tool_name": name, "tool_input": json.RawMessage(input),
			"hook_event_name": "PermissionDenied", "permission_mode": l.PermMode,
		}, l.ToolCtx.CWD)
		return false, ""
	}
	for {
		select {
		case <-timeout.C:
			dprintf("\n  %s⏵ auto-denied — unattended for %s%s\n", dim, permAskTimeout, reset)
			return deny()
		case k, ok := <-dch:
			if !ok {
				return false, ""
			}
			switch {
			case k.seq == "[A":
				if sel > 0 {
					sel--
					draw()
				}
				continue
			case k.seq == "[B":
				if sel < len(opts)-1 {
					sel++
					draw()
				}
				continue
			case k.b == '\t':
				// Tab to amend — edit the command inline, Enter re-approves
				if name == "Bash" {
					if amended := amendCommand(dch, content, deadline); amended != "" {
						input, _ = json.Marshal(map[string]string{"command": amended})
						content = amended
						draw()
					}
				}
				continue
			case k.b == 0x1b || k.b == 3:
				dprintln("  Esc — denied")
				return deny()
			case k.b >= '1' && k.b <= '9' && int(k.b-'1') < len(opts):
				sel = int(k.b - '1')
				fallthrough
			case k.b == '\r':
				switch opts[sel].action {
				case "yes":
					dprintln("  ✔ " + title + " approved")
					return true, ""
				case "no":
					dprintln("  ✗ No")
					return deny()
				case "automode":
					l.PermMode = "auto"
					dprintln("  switched to auto mode — " + title + " approved")
					return true, ""
				case "acceptedits":
					l.PermMode = "acceptEdits"
					dprintln("  switched to accept edits — " + title + " approved")
					return true, ""
				case "always":
					p.always[name] = true
					if alwaysSpec != "" {
						addLocalPermissionRule(l.ToolCtx.CWD, alwaysSpec)
						dprintf("  %s%s added to permission allowlist%s\n", dim, alwaysSpec, reset)
					}
					return true, ""
				}
			}
		}
	}
}

// permDesc returns the dialog's gray description line — the model's
// description field for Bash, else a derived summary.
func permDesc(name string, input json.RawMessage, summary string) string {
	var m map[string]any
	json.Unmarshal(input, &m)
	if d, _ := m["description"].(string); d != "" {
		return d
	}
	if summary != "" {
		return summary
	}
	return ""
}

// permContent renders the framed content line: command for Bash, file
// path for file tools, compact JSON otherwise.
func permContent(name string, input json.RawMessage) string {
	var m map[string]any
	json.Unmarshal(input, &m)
	s := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				return fmt.Sprint(v)
			}
		}
		return ""
	}
	switch name {
	case "Bash":
		return s("command")
	case "Read", "Write", "Edit", "MultiEdit":
		return s("file_path")
	case "Glob", "LS":
		return s("path", "pattern")
	case "Grep":
		return s("pattern", "path")
	}
	b, _ := json.Marshal(m)
	if len(b) > 200 {
		b = append(b[:200], []byte("…")...)
	}
	return string(b)
}

// amendCommand lets the user edit the Bash command inline (real's
// "Tab to amend"); Enter returns the edited text, Esc cancels.
func amendCommand(dch chan key, cur string, deadline time.Time) string {
	buf := []rune(cur)
	pos := len(buf)
	gray := "\x1b[38;5;246m"
	dprintf("  %samend command (enter applies, esc cancels):%s\n", gray, reset)
	for {
		dprintf("  > %s\r\x1b[K", string(buf))
		if pos < len(buf) {
			dprintf("\x1b[%dD", len(buf)-pos)
		}
		select {
		case <-time.After(time.Until(deadline)):
			return ""
		case k, ok := <-dch:
			if !ok {
				return ""
			}
			switch {
			case k.b == '\r':
				dprintln()
				return string(buf)
			case k.b == 0x1b || k.b == 3:
				dprintln()
				return ""
			case k.seq == "[D":
				if pos > 0 {
					pos--
				}
			case k.seq == "[C":
				if pos < len(buf) {
					pos++
				}
			case k.b == 0x7f || k.b == 8:
				if pos > 0 {
					buf = append(buf[:pos-1], buf[pos:]...)
					pos--
				}
			case k.r != 0:
				buf = append(buf[:pos], append([]rune{k.r}, buf[pos:]...)...)
				pos++
			}
		}
	}
}

// readFeedbackLine captures the one-line "tell Claude what to do
// differently" text for option 4. Keys must come from the dialog channel
// (dch) — reading ks.ch races the turn busy-loop, which is already
// forwarding every key into dch (verified live: feedback text starved).
// The auto-deny deadline stays armed — walking away mid-capture denies
// with no feedback.
func readFeedbackLine(dch chan key, deadline time.Time) string {
	dprintf("  %stell Claude what to do differently:%s ", dim, reset)
	var buf []rune
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	for {
		select {
		case <-t.C:
			dprintf("\n  %s⏵ auto-denied — unattended%s\n", dim, reset)
			return ""
		case k, ok := <-dch:
			if !ok {
				return ""
			}
			switch {
			case k.b == '\r' || k.b == '\n':
				dprintln()
				return strings.TrimSpace(string(buf))
			case k.b == 0x1b && k.seq == "":
				dprintln()
				return ""
			case k.b == 3:
				dprintln()
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
		dprintf("  %sfeedback queued — sent when this turn ends%s\n", dim, reset)
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
		dprintf("  %s!%s %s\n", yellow, reset, ln)
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
		dprintln("  /terminal-setup currently supports macOS terminals only")
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
		dprintln("  iTerm2: bind Shift+Enter to a newline manually —")
		dprintln("    Settings → Profiles → Keys → Key Mappings → +")
		dprintln("    Shortcut: ⇧↩   Action: Send Escape Sequence   Esc+: [13;2u")
	case "Apple_Terminal":
		dprintln("  Terminal.app can't bind Shift+Enter; to make Option act as Meta:")
		dprintln("    defaults write com.apple.Terminal UseOptionAsMetaKey -bool true")
		dprintln("  (or Settings → Profiles → Keyboard → “Use Option as Meta key”)")
	default:
		dprintf("  unsupported terminal (TERM_PROGRAM=%q)\n", tp)
		dprintln("  supported: VS Code, Cursor, Windsurf (auto-install); iTerm2, Terminal.app (manual)")
	}
}

// installShiftEnterBinding appends the shift+enter sendSequence binding to
// a VS Code-family keybindings.json (an array; JSONC comments mean we defer
// to manual instructions rather than clobbering the file).
func installShiftEnterBinding(kb string) {
	var arr []any
	if b, err := os.ReadFile(kb); err == nil {
		if json.Unmarshal(b, &arr) != nil {
			dprintln("  couldn't parse", kb, "(comments?) — add manually:")
			dprintln(`    {"key":"shift+enter","command":"workbench.action.terminal.sendSequence","args":{"text":"\u001b\r"}}`)
			return
		}
	}
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok &&
			m["key"] == "shift+enter" &&
			m["command"] == "workbench.action.terminal.sendSequence" {
			dprintln("  shift+enter binding already present in", kb)
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
		dprintln("  write failed:", err)
		return
	}
	dprintln("  installed shift+enter → newline in", kb)
	dprintln("  reload the editor window to pick it up")
}

// printToolCard renders the tool input as real's card: key: value rows for
// scalars (capped), a colored diff for Edit, and a sentinel for oversized
// parameter sets.
func printToolCard(name string, input json.RawMessage, summary string) {
	var kv map[string]any
	if len(input) == 0 || json.Unmarshal(input, &kv) != nil {
		dprintf("  %s(parameters are not an object — deny unless expected)%s\n", dim, reset)
		return
	}
	if name == "Edit" {
		printEditCard(kv)
		return
	}
	keys := 0
	for _, k := range sortedKeys(kv) {
		if keys >= 8 {
			dprintf("  %s… (+%d more parameters)%s\n", dim, len(kv)-keys, reset)
			break
		}
		v := fmt.Sprint(kv[k])
		if len(v) > 200 {
			v = trunc(v, 200) + dim + " …" + reset
		}
		dprintf("  %s%-16s%s %s\n", dim, k, reset, v)
		keys++
	}
	if len(kv) > 12 {
		dprintf("  %s(%d parameters — too many to show — deny unless expected)%s\n", dim, len(kv), reset)
	}
}

// printEditCard shows old_string/new_string as red/green hunks (real shows a
// colored diff preview inside the permission box).
func printEditCard(kv map[string]any) {
	dprintf("  %s%-16s%s %s\n", dim, "file_path", reset, kv["file_path"])
	old, _ := kv["old_string"].(string)
	newS, _ := kv["new_string"].(string)
	for _, ln := range strings.Split(strings.TrimRight(old, "\n"), "\n") {
		dprintf("    %s-%s %s%s\n", red, reset, red, trunc(ln, 110)+reset)
	}
	for _, ln := range strings.Split(strings.TrimRight(newS, "\n"), "\n") {
		dprintf("    %s+%s %s%s\n", green, reset, green, trunc(ln, 110)+reset)
	}
	if len(strings.Split(old, "\n")) > 8 || len(strings.Split(newS, "\n")) > 8 {
		dprintf("    %s… (truncated diff)%s\n", dim, reset)
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

// outsideDir returns the directory a Bash command targets outside the
// workspace (for real's "always allow access to <dir>" option), "" when
// nothing outside is referenced.
func outsideDir(name string, input json.RawMessage, cwd string) string {
	if name != "Bash" {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	cmd, _ := m["command"].(string)
	for _, tok := range strings.Fields(cmd) {
		if strings.HasPrefix(tok, "/") && !strings.HasPrefix(tok, cwd+"/") && !strings.HasPrefix(tok, cwd) {
			// the first outside-path arg's parent dir
			if strings.HasSuffix(tok, "/") {
				return strings.TrimSuffix(tok, "/")
			}
			return filepath.Dir(tok)
		}
	}
	return ""
}

// permFilePath extracts the file_path input field.
func permFilePath(input json.RawMessage) string {
	var m map[string]any
	json.Unmarshal(input, &m)
	s, _ := m["file_path"].(string)
	return s
}

// permContentLines returns the framed content rows — Write shows the new
// file's content with line numbers; Edit a diff preview; others the
// single summary line.
func permContentLines(name string, input json.RawMessage, single string) []string {
	var m map[string]any
	json.Unmarshal(input, &m)
	switch name {
	case "Write":
		if body, _ := m["content"].(string); body != "" {
			var out []string
			for i, ln := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
				if i >= 10 {
					out = append(out, " "+dim+"…")
					break
				}
				out = append(out, fmt.Sprintf("%s%2d%s %s", dim, i+1, reset, ln))
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	return []string{" " + single + reset}
}

// isFileTool — the dialog class that offers "switch to accept edits".
func isFileTool(name string) bool {
	switch name {
	case "Write", "Edit", "MultiEdit", "NotebookEdit":
		return true
	}
	return false
}
