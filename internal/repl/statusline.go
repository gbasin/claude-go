package repl

import (
	"fmt"
	"sync/atomic"

	"github.com/gbasin/claude-go/internal/agent"
)

// queuedPending counts messages queued while a turn runs (typed input plus
// SessionQueue injections) — surfaced in the footer as "N queued".
var queuedPending atomic.Int64

// footerHints builds the contextual hint badges under the status bar, in
// real CC's spirit: running background tasks, queued input, the vim-mode
// badge, and brief mode.
func footerHints(l *agent.Loop, ed *editor) []string {
	var hints []string
	if n := runningTaskCount(l); n > 0 {
		hints = append(hints, fmt.Sprintf("%d %s · ↓ to view", n, pluralize(n, "task", "tasks")))
	}
	if n := int(queuedPending.Load()); n > 0 {
		hints = append(hints, fmt.Sprintf("%d queued", n))
	}
	// vim badge renders at draw time (editor knows live mode) — see
	// editor.footerText in term.go
	if briefMode.Load() {
		hints = append(hints, "brief")
	}
	return hints
}

// runningTaskCount counts background tasks whose Done channel hasn't fired.
func runningTaskCount(l *agent.Loop) int {
	if l.ToolCtx == nil || l.ToolCtx.BgTasks == nil {
		return 0
	}
	running := 0
	for _, t := range l.ToolCtx.BgTasks.List() {
		select {
		case <-t.Done:
		default:
			running++
		}
	}
	return running
}

// permModeBanner renders real's footer mode pill:
// "⏵⏵ <mode> on · ← for agents" — no "(shift+tab to cycle)" suffix in
// 296 (verified absent in the carved source); the hint only appears in
// the onboarding explainer. Default mode has NO pill — real renders just
// "? for shortcuts · ← for agents" (default's symbol is "").
// modePillColor maps each permission mode to real's palette token —
// auto = warning yellow (captured 38;5;220), bypass = error red,
// plan = planMode teal, acceptEdits = autoAccept violet.
func modePillColor(mode string) string {
	switch mode {
	case "bypassPermissions":
		return token("error")
	case "plan":
		return token("planMode")
	case "acceptEdits":
		return token("autoAccept")
	case "auto":
		return "\x1b[38;5;220m" // real's auto pill (captured)
	default:
		return token("warning")
	}
}

func permModeBanner(mode string) string {
	gray := "\x1b[38;5;246m"
	pill := modePillColor(mode)
	hint := gray + " · ← for agents" + reset
	switch mode {
	case "", "default":
		return gray + "? for shortcuts" + reset + hint
	case "acceptEdits":
		return pill + "⏵⏵ accept edits on" + reset + hint
	case "plan":
		return pill + "⏸ plan mode on" + reset + hint
	case "auto":
		return pill + "⏵⏵ auto mode on" + reset + hint
	case "bypassPermissions":
		return pill + "⏵⏵ bypass permissions on" + reset + hint
	}
	return pill + "⏵⏵ " + mode + " mode on" + reset + hint
}
