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

	"github.com/gbasin/claude-go/internal/agent"
	"github.com/gbasin/claude-go/internal/config"
	"github.com/gbasin/claude-go/internal/hooks"
	"github.com/gbasin/claude-go/internal/perms"
	"github.com/gbasin/claude-go/internal/tools"
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
	// PermissionRequest hook can auto-decide without prompting (real event)
	if l.Hooks != nil {
		pr := hooks.Run(l.Hooks, hooks.PermissionRequest, map[string]any{
			"session_id": l.Client.SessionID, "cwd": l.ToolCtx.CWD,
			"tool_name": name, "tool_input": json.RawMessage(input),
			"hook_event_name": "PermissionRequest", "permission_mode": l.PermMode,
		}, l.ToolCtx.CWD)
		switch pr.Permission {
		case "allow":
			// real persists the hook's updatedPermissions rules
			// (hookSpecificOutput.updatedPermissions) into local settings
			if len(pr.UpdatedPermissions) > 0 {
				var add []string
				if json.Unmarshal(pr.UpdatedPermissions, &add) == nil {
					for _, rule := range add {
						addLocalPermissionRule(l.ToolCtx.CWD, rule)
					}
				}
			}
			return true, ""
		case "deny":
			return false, pr.Reason
		}
	}
	// --permission-prompt-tool: an MCP tool answers the prompt headlessly —
	// real delegates {tool_name, tool_input} and honors {behavior}
	if tn := os.Getenv("CLAUDE_GO_PERMISSION_PROMPT_TOOL"); tn != "" {
		in, _ := json.Marshal(map[string]any{
			"tool_name": name, "tool_input": json.RawMessage(input)})
		r := tools.Dispatch(l.ToolCtx, tn, in)
		var ans struct {
			Behavior string `json:"behavior"`
			Message  string `json:"message"`
		}
		if err := json.Unmarshal([]byte(r.Content), &ans); err == nil {
			switch ans.Behavior {
			case "allow":
				return true, ""
			case "deny":
				return false, ans.Message
			}
		}
		// malformed/no-answer → fall through to the interactive dialog
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
	// real 2.1.296's dialog (mod_1596 $tt/Cnt + mod_1531 consentRows):
	//   cyan-153 ─ rule · bold "<Tool> command" · auto-mode tip (subtitle,
	//   only when the enable-auto-mode row is offered) · gray-246
	//   description · gray-239 ╌ frame around the content · decision-reason
	//   row · "Do you want to proceed?" · cursor list numbered 1..N ·
	//   "Esc to cancel · Tab to next field" — tab moves between the input
	//   rows (nextField), it does not amend the tool call.
	cyan153 := "\x1b[38;5;153m"
	gray246 := "\x1b[38;5;246m"
	gray239 := "\x1b[38;5;239m"
	width, _, _ := term.GetSize(int(os.Stdout.Fd()))
	if width <= 0 {
		width = 120
	}
	rule := cyan153 + strings.Repeat("─", width)
	if name == "ExitPlanMode" {
		rule = token("planMode") + strings.Repeat("─", width) // plan-colored card (real)
	}
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
		question = "Do you want to make this edit to " + bold + fname + unbold + "?"
	case "ExitPlanMode":
		title = "Exit plan mode?" // real's plan-colored approval dialog
		// real's plan-exit card footer (mod_1596 Got): dim status line
		question = dim + "Claude has written up a plan and is ready to execute. Would you like to proceed?" + reset
	}
	desc := permDesc(name, input, summary)
	if desc == "" && isFileTool(name) {
		desc = permFilePath(input) // real shows the path as the gray line
	}
	content := permContent(name, input)
	if name == "ExitPlanMode" {
		// the plan-exit card has no description/content frame (real shows
		// the plan above the card or the bare "wants to exit" line)
		desc, content = "", ""
	}

	// the enable-auto-mode row — and the tip subtitle riding it — are
	// offered only when auto mode can take over: not already auto, not
	// managed-disabled, and no ask rule/hook forced this prompt (real's
	// `wo = canOfferAutoMode && !decisionReason` gate in $tt).
	offerAuto := !isFileTool(name) && name != "ExitPlanMode" &&
		l.PermMode != "auto" && reason == "" && autoModeAvailable(l)
	tip := ""
	if offerAuto {
		tip = `Tip: auto mode handles these prompts for you — choose "switch to auto mode" below`
	}
	// the suggested rule/prefix and the outside-dir target feed both the
	// option rows and the "always" submit path
	alwaysSpec := ruleSuggestion(name, summary)
	outside := outsideDir(name, input, l.ToolCtx.CWD)
	opts, inputs := p.permOpts(l, name, input, summary, offerAuto)

	sel := 0
	renderRows := 0
	deadline := time.Now().Add(permAskTimeout)
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
		if name == "ExitPlanMode" {
			put(" Claude wants to exit plan mode")
		}
		if tip != "" {
			put(" " + tip)
		}
		if desc != "" && desc != content {
			put(" " + gray246 + desc + reset)
		}
		if name != "ExitPlanMode" {
			put(dot)
			for _, cl := range permContentLines(name, input, content) {
				put(cl)
			}
			put(dot)
		}
		if reason != "" {
			// real's decision-reason row (Kw) lands above the question —
			// e.g. "Permission rule `X` requires confirmation for this
			// command." + dim "/permissions to update rules"
			ln := " " + reason
			if config != "" {
				ln += " " + dim + config + reset
			}
			put(ln)
		}
		put(" " + question)
		if rem := time.Until(deadline); rem > 0 {
			m, s := int(rem.Minutes()), int(rem.Seconds())%60
			put(" " + token("warning") + fmt.Sprintf(
				"⚠ Claude Code will automatically deny this request in %d:%02d, to avoid blocking progress on an unattended session",
				m, s) + reset)
		}
		for i, o := range opts {
			row := o.label
			if o.input != "" {
				if len(inputs[i]) > 0 {
					row += bold + string(inputs[i]) + unbold
				} else {
					row += gray246 + o.placeholder + "\x1b[39m"
				}
				row += o.suffix
			}
			if i == sel {
				put(fmt.Sprintf(" %s❯%s %s%d. %s%s%s", cyan153, "\x1b[39m", gray246, i+1, cyan153, row, "\x1b[39m"))
			} else {
				put(fmt.Sprintf("   %s%d. %s%s", gray246, i+1, "\x1b[39m", row))
			}
		}
		put(" " + gray246 + "Esc to cancel · Tab to next field" + reset)
		fmt.Print(sb.String())
	}
	draw()
	timeout := time.NewTimer(permAskTimeout)
	defer timeout.Stop()
	tick := time.NewTicker(time.Second) // repaints the ⚠ countdown in place
	defer tick.Stop()
	deny := func() (bool, string, bool) {
		res := hooks.Run(l.Hooks, hooks.PermissionDenied, map[string]any{
			"session_id": l.Client.SessionID, "cwd": l.ToolCtx.CWD,
			"tool_name": name, "tool_input": json.RawMessage(input),
			"hook_event_name": "PermissionDenied", "permission_mode": l.PermMode,
		}, l.ToolCtx.CWD)
		if res.Retry {
			// real: a retry:true output re-prompts instead of denying
			draw()
			timeout.Reset(permAskTimeout)
			return false, "", false
		}
		return false, "", true
	}
	denyWith := func(fb string) (bool, string, bool) {
		if res := hooks.Run(l.Hooks, hooks.PermissionDenied, map[string]any{
			"session_id": l.Client.SessionID, "cwd": l.ToolCtx.CWD,
			"tool_name": name, "tool_input": json.RawMessage(input),
			"hook_event_name": "PermissionDenied", "permission_mode": l.PermMode,
		}, l.ToolCtx.CWD); res.Retry {
			draw()
			return false, fb, false // not done — stay in the prompt loop
		}
		return false, fb, true
	}
	for {
		select {
		case <-tick.C:
			draw()
		case <-timeout.C:
			dprintf("\n  %s⏵ auto-denied — unattended for %s%s\n", dim, permAskTimeout, reset)
			if a, fb, done := deny(); done {
				return a, fb
			}
		case k, ok := <-dch:
			if !ok {
				return false, ""
			}
			submit := func() (bool, string, bool) {
				// returns (allow, feedback, done) — done=false keeps the loop
				fb := ""
				if opts[sel].input == "feedback" {
					fb = strings.TrimSpace(string(inputs[sel]))
				}
				switch opts[sel].action {
				case "yes":
					dprintln("  ✔ " + title + " approved")
					return true, fb, true
				case "no":
					dprintln("  ✗ No")
					if name == "ExitPlanMode" && fb != "" {
						// PlanApprove's bool signature drops the feedback —
						// queue it as the next user turn like the deny path
						queuePermFeedback(fb)
					}
					if fb != "" {
						return denyWith(fb)
					}
					a, s, done := deny()
					return a, s, done
				case "automode":
					l.PermMode = "auto"
					dprintln("  switched to auto mode — " + title + " approved")
					return true, fb, true
				case "acceptedits":
					l.PermMode = "acceptEdits"
					dprintln("  switched to accept edits — " + title + " approved")
					return true, fb, true
				case "setmode":
					// ExitPlanMode lane rows (jot): the approval carries the
					// session mode — stash it so execExitPlan doesn't flat-
					// reset to "default" after PlanApprove returns
					l.PermMode = opts[sel].mode
					tools.NotePlanExitMode(l.Client.SessionID, opts[sel].mode)
					dprintln("  switched to " + opts[sel].mode + " — plan approved")
					return true, fb, true
				case "always":
					if outside != "" {
						// real's "always allow access to <dir>" row mints an
						// addDirectories session update, not a tool rule
						l.AddDirs = append(l.AddDirs, outside)
						dprintf("  %s%s added to the workspace for this session%s\n", dim, outside, reset)
						return true, fb, true
					}
					spec := alwaysSpec
					if opts[sel].input == "prefix" {
						// the edited prefix becomes the persisted rule —
						// empty submits as a plain allow (real's
						// allowEmptySubmitToCancel)
						pfx := strings.TrimSpace(string(inputs[sel]))
						if pfx == "" {
							return true, "", true
						}
						if pfx == "*" {
							spec = name // real's "any <tool> command" rule
						} else {
							spec = name + "(" + pfx + ")"
							if !strings.HasSuffix(pfx, ":*") && !strings.HasSuffix(pfx, "*") {
								spec = name + "(" + pfx + ":*)"
							}
						}
					}
					if spec != "" {
						addLocalPermissionRule(l.ToolCtx.CWD, spec)
						// honor the rule for the rest of this session too —
						// the allow list was read once at startup
						if l.PermRules == nil {
							l.PermRules = map[string][]string{}
						}
						l.PermRules["allow"] = append(l.PermRules["allow"], spec)
						dprintf("  %s%s added to permission allowlist%s\n", dim, spec, reset)
					}
					return true, fb, true
				}
				return false, "", true
			}
			// keybinding dispatch: the Confirmation context's resolved
			// action wins — user remaps apply (sessionKB carries the
			// defaults). Digits/input-row typing fall through below.
			switch dialogAction("Confirmation", k) {
			case "confirm:yes":
				if a, fb, done := submit(); done {
					return a, fb
				}
				continue
			case "confirm:toggle":
				// real's space=toggle — on editable input rows a space is
				// text, not a toggle; only submit on non-input rows
				if opts[sel].input == "" {
					if a, fb, done := submit(); done {
						return a, fb
					}
					continue
				}
			case "confirm:no":
				dprintln("  Esc — denied")
				if a, fb, done := deny(); done {
					return a, fb
				}
				continue
			case "confirm:previous":
				if sel > 0 {
					sel--
					draw()
				}
				continue
			case "confirm:next":
				if sel < len(opts)-1 {
					sel++
					draw()
				}
				continue
			case "confirm:nextField":
				if sel < len(opts)-1 {
					sel++
				} else {
					sel = 0
				}
				draw()
				continue
			case "confirm:cycleMode":
				if sel > 0 {
					sel--
				} else {
					sel = len(opts) - 1
				}
				draw()
				continue
			case "":
				// not bound — fall through to digits/input typing
			default:
				continue // bound to an action with no dialog meaning
			}
			_ = submit
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
				// real: tab = nextField — moves between input rows
				if sel < len(opts)-1 {
					sel++
				} else {
					sel = 0
				}
				draw()
				continue
			case k.seq == "[Z": // shift+tab — prevField
				if sel > 0 {
					sel--
				} else {
					sel = len(opts) - 1
				}
				draw()
				continue
			case k.b == 0x1b || k.b == 3:
				dprintln("  Esc — denied")
				if a, fb, done := deny(); done {
					return a, fb
				}
			case k.b >= '1' && k.b <= '9' && int(k.b-'1') < len(opts):
				sel = int(k.b - '1')
				fallthrough
			case k.b == '\r':
				if a, fb, done := submit(); done {
					return a, fb
				}
			case (k.b == 0x7f || k.b == 8) && len(inputs[sel]) > 0:
				inputs[sel] = inputs[sel][:len(inputs[sel])-1]
				draw()
				continue
			case k.r != 0 && k.r >= 32 && k.mod&(modAlt|modCtrl|modCmd) == 0 && opts[sel].input != "":
				// typing goes into the selected row's input field —
				// real's rows are editable inputs
				inputs[sel] = append(inputs[sel], k.r)
				draw()
				continue
			case k.b >= 32 && k.b < 127 && k.mod == 0 && opts[sel].input != "":
				inputs[sel] = append(inputs[sel], rune(k.b))
				draw()
				continue
			}
		}
	}
}

// permOpt is one numbered row in the permission dialog. input marks an
// editable row ("feedback" free text / "prefix" rule content); mode is the
// target permission mode for "setmode" rows (ExitPlanMode lanes).
type permOpt struct {
	label       string
	action      string
	mode        string
	input       string // "feedback" | "prefix" — row carries an editable field
	placeholder string
	suffix      string // static text after the field (e.g. " in <dir>")
}

// permOpts builds the dialog's option rows (mod_1596 qtt/Cnt + jot) and the
// per-row editable-field seeds. Real's rows are input fields:
//
//	Yes (input row, "and tell Claude what to do next"),
//	"Yes, and don't ask again for <rule> in <dir>" (editable command
//	prefix, placeholder "command prefix (e.g., npm run *)"),
//	"Yes, and switch to auto mode" (conditional),
//	No (input row, "and tell Claude what to do differently").
//
// file tools: Yes / switch-to-acceptEdits (shift+tab) / No.
// ExitPlanMode (jot): optional clear-context row, the keep-context mode
// lanes, and the deny-with-feedback input row.
func (p *promptUI) permOpts(l *agent.Loop, name string, input json.RawMessage, summary string, offerAuto bool) ([]permOpt, [][]rune) {
	var opts []permOpt
	alwaysSpec := ruleSuggestion(name, summary)
	// display form of the suggested rule — strip the Tool(...) wrapper
	ruleDisp := alwaysSpec
	if i := strings.IndexByte(alwaysSpec, '('); i >= 0 && strings.HasSuffix(alwaysSpec, ")") {
		ruleDisp = alwaysSpec[i+1 : len(alwaysSpec)-1]
	}
	// the project dir shown in "in <dir>" — ~-abbreviated like real's Fn()
	dirDisp := l.ToolCtx.CWD
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(dirDisp, home) {
		dirDisp = "~" + dirDisp[len(home):]
	}
	outside := outsideDir(name, input, l.ToolCtx.CWD)

	if name == "ExitPlanMode" {
		// jot: the clear-context row rides the
		// settings.showClearContextOnPlanAccept flag and carries the
		// context-usage suffix; below it the keep-context lanes pick
		// bypass > resume-auto > accept-edits, then the default lane.
		if sc, _ := userSetting("showClearContextOnPlanAccept").(bool); sc {
			used := planUsedSuffix(l)
			switch {
			case bypassModeAvailable(l):
				opts = append(opts, permOpt{label: "Yes, clear context" + used + " and bypass permissions",
					action: "setmode", mode: "bypassPermissions"})
			case autoModeAvailable(l):
				opts = append(opts, permOpt{label: "Yes, clear context" + used + " and use auto mode",
					action: "setmode", mode: "auto"})
			default:
				opts = append(opts, permOpt{label: "Yes, clear context" + used + " and auto-accept edits",
					action: "setmode", mode: "acceptEdits"})
			}
		}
		switch {
		case bypassModeAvailable(l):
			// Vwe("bypassPermissions") — the default-variant label
			opts = append(opts, permOpt{label: "Yes, and switch to " + bold + "BYPASS PERMISSIONS (no further prompts)" + unbold + " for this session",
				action: "setmode", mode: "bypassPermissions"})
		case autoModeAvailable(l):
			// Dln("exit-plan-resume")
			opts = append(opts, permOpt{label: "Yes, and use auto mode",
				action: "setmode", mode: "auto"})
		default:
			// Vwe("acceptEdits", labelVariant "plan-keep-context")
			opts = append(opts, permOpt{label: "Yes, auto-accept edits",
				action: "setmode", mode: "acceptEdits"})
		}
		// Vwe("default", labelVariant "plan-keep-context")
		opts = append(opts, permOpt{label: "Yes, manually approve edits", action: "yes"})
		opts = append(opts, permOpt{label: "No, keep planning", action: "no",
			input: "feedback", placeholder: "Tell Claude what to change"})
	} else {
		opts = append(opts, permOpt{label: "Yes", action: "yes", input: "feedback",
			placeholder: "and tell Claude what to do next"})
		if isFileTool(name) && l.PermMode != "acceptEdits" {
			// Cnt: the session row is the accept-edits mode switch; the
			// "(shift+tab)" suffix is the resolved confirm:cycleMode binding
			opts = append(opts, permOpt{
				label:  "Yes, and switch to " + bold + "accept edits (auto-approve file edits and common file commands)" + unbold + " for this session " + bold + "(shift+tab)" + unbold,
				action: "acceptedits"})
		} else if !isFileTool(name) {
			if outside != "" {
				opts = append(opts, permOpt{
					label:  "Yes, and always allow access to " + bold + outside + unbold + " from this project",
					action: "always"})
			} else if alwaysSpec != "" {
				// real: editable-prefix input row — the seed is the
				// suggested prefix; editing it rewrites the persisted rule
				opts = append(opts, permOpt{
					label:  "Yes, and don’t ask again for ",
					action: "always",
					input:  "prefix",
					// real's input placeholder
					placeholder: "command prefix (e.g., npm run *)",
					suffix:      " in " + bold + dirDisp + unbold})
			}
			if offerAuto {
				// P0r.workflow + Wtt description
				opts = append(opts, permOpt{label: "Yes, and switch to auto mode" + "\x1b[38;5;246m" + " · auto mode handles these prompts for you" + "\x1b[39m", action: "automode"})
			}
		}
		opts = append(opts, permOpt{label: "No", action: "no", input: "feedback",
			placeholder: "and tell Claude what to do differently"})
	}

	// per-option input text (feedback / edited prefix seed)
	inputs := make([][]rune, len(opts))
	for i := range opts {
		if opts[i].input == "prefix" {
			inputs[i] = []rune(ruleDisp)
		}
	}
	return opts, inputs
}

// autoModeAvailable reports whether the session may switch into auto mode —
// managed settings can disable the lane (permissions.disableAutoMode).
func autoModeAvailable(l *agent.Loop) bool {
	if st := config.Load(l.ToolCtx.CWD); st != nil && st.Permissions != nil &&
		st.Permissions.DisableAutoMode == "disable" {
		return false
	}
	return true
}

// bypassModeAvailable reports whether the bypass lane can be offered —
// real's isBypassPermissionsModeAvailable: the --dangerously-skip-permissions
// family opted the session in and managed settings haven't disabled it.
func bypassModeAvailable(l *agent.Loop) bool {
	if os.Getenv("CLAUDE_GO_ALLOW_BYPASS_PERMISSIONS") == "" {
		return false
	}
	if st := config.Load(l.ToolCtx.CWD); st != nil && st.Permissions != nil &&
		st.Permissions.DisableBypassPermissionsMode == "disable" {
		return false
	}
	return true
}

// planUsedSuffix renders jot's " (N% used)" context-usage suffix on the
// clear-context row; empty when no usage has been reported yet.
func planUsedSuffix(l *agent.Loop) string {
	total := l.UsageTotal.InputTokens + l.UsageTotal.CacheReadInputTokens +
		l.UsageTotal.CacheCreationInputTokens
	if total <= 0 {
		return ""
	}
	if w := l.ContextWindowSize(); w > 0 {
		return fmt.Sprintf(" (%d%% used)", total*100/w)
	}
	return ""
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
		b = []byte(truncRunes(string(b), 200))
	}
	return string(b)
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
	// real's guardrail rows (mod_1531): unrenderable params get a warn
	// line instead of content — deny unless the shape is expected
	guard := func(s string) []string {
		return []string{token("warning") + s + reset}
	}
	if err := json.Unmarshal(input, &m); err != nil {
		return guard("(parameters are not an object — deny unless expected)")
	}
	if len(m) > 10 {
		return guard(fmt.Sprintf("(%d parameters — too many to show — deny unless expected)", len(m)))
	}
	// real's per-value sentinel (fu/Fu in mod_1531): a parameter value that
	// can't be rendered inside the display budgets — >200k rendered chars,
	// >10k elements, ≥64 levels deep — degrades to this guard line
	if unrenderableParam(m) {
		return guard("(value too large or too deeply nested to render — deny unless expected)")
	}
	switch name {
	case "Edit", "MultiEdit":
		// captured: " 1 -old" fg-167/bg-52, " 1 +new" fg-77/bg-22,
		// white-231 text, padded to fill (bg spans the row)
		oldS, _ := m["old_string"].(string)
		newS, _ := m["new_string"].(string)
		if oldS != "" || newS != "" {
			pad := func(s string) string {
				return s + strings.Repeat(" ", max(0, 78-visWidth(s)))
			}
			var out []string
			if oldS != "" {
				for _, ln := range strings.Split(strings.TrimRight(oldS, "\n"), "\n") {
					out = append(out, "\x1b[38;5;167m\x1b[48;5;52m"+pad(" 1 -"+ln)+"\x1b[49m\x1b[39m")
				}
			}
			if newS != "" {
				for _, ln := range strings.Split(strings.TrimRight(newS, "\n"), "\n") {
					out = append(out, "\x1b[38;5;77m\x1b[48;5;22m"+pad(" 1 +"+ln)+"\x1b[49m\x1b[39m")
				}
			}
			if len(out) > 0 {
				return out
			}
		}
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

// render budgets — real's Fu() bounds (mod_1531: Ke, As, Ns)
const (
	permValueMaxChars = 200_000 // rendered-characters units budget
	permValueMaxElems = 10_000  // element display budget
	permValueMaxDepth = 64      // nesting depth bound
)

// unrenderableParam reports whether any parameter value exceeds the display
// budgets — the permission card degrades it to the deny-unless-expected
// sentinel rather than dumping the value.
func unrenderableParam(m map[string]any) bool {
	var chars, elems int
	var bad bool
	var walk func(v any, depth int)
	walk = func(v any, depth int) {
		if bad {
			return
		}
		if depth >= permValueMaxDepth {
			bad = true
			return
		}
		switch t := v.(type) {
		case string:
			chars += len(t)
		case map[string]any:
			elems += len(t)
			for k, val := range t {
				chars += len(k)
				walk(val, depth+1)
			}
		case []any:
			elems += len(t)
			for _, val := range t {
				walk(val, depth+1)
			}
		}
		if chars > permValueMaxChars || elems > permValueMaxElems {
			bad = true
		}
	}
	for _, v := range m {
		chars, elems = 0, 0
		walk(v, 0)
		if bad {
			return true
		}
	}
	return false
}

// isFileTool — the dialog class that offers "switch to accept edits".
func isFileTool(name string) bool {
	switch name {
	case "Write", "Edit", "MultiEdit", "NotebookEdit":
		return true
	}
	return false
}
