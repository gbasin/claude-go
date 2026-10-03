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

// permModeBanner is the colored banner real CC shows above the input when
// the permission mode isn't default ("⏵⏵ auto mode", "⏸ plan mode"…).
func permModeBanner(mode string) string {
	switch mode {
	case "", "default":
		return ""
	case "acceptEdits":
		return yellow + "⏵⏵ accept edits on" + reset
	case "plan":
		return yellow + "⏸ plan mode" + reset
	case "auto":
		return yellow + "⏵⏵ auto mode" + reset
	case "bypassPermissions":
		return red + "⏵⏵ bypass permissions on" + reset
	}
	return yellow + "⏵⏵ " + mode + " mode" + reset
}
