package repl

import (
	"fmt"
	"sync/atomic"

	"claude-go/internal/agent"
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
	if ed != nil && ed.vim {
		mode := ed.mode
		if mode == "" {
			mode = "insert"
		}
		hints = append(hints, "vim:"+mode)
	}
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

// permModeBanner renders real's footer mode pill + cycle hint:
// "⏵⏵ <mode> on (shift+tab to cycle) · ← for agents" — yellow-220 pill,
// gray-246 hint. Empty only in a bare `? for shortcuts` default mode.
func permModeBanner(mode string) string {
	yel := "\x1b[38;5;220m"
	gray := "\x1b[38;5;246m"
	hint := gray + " (shift+tab to cycle) · ← for agents" + reset
	switch mode {
	case "", "default":
		return gray + "? for shortcuts" + reset
	case "acceptEdits":
		return yel + "⏵⏵ accept edits on" + reset + hint
	case "plan":
		return yel + "⏸ plan mode" + reset + hint
	case "auto":
		return yel + "⏵⏵ auto mode on" + reset + hint
	case "bypassPermissions":
		return yel + "⏵⏵ bypass permissions on" + reset + hint
	}
	return yel + "⏵⏵ " + mode + " mode" + reset + hint
}
