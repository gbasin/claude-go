package repl

// OSC 52 clipboard emit — audits/tui.md §72. Real's copy-on-select
// stack (Scroll ctx selection:copy on ctrl+shift+c / cmd+c) writes the
// selection to the clipboard via OSC 52 with terminal-multiplexer
// fallbacks.
//
// A bare OSC 52 only reaches the outer terminal's clipboard. Inside
// tmux the sequence must be wrapped in the DCS passthrough
// (\x1bPtmux;<escaped>\x1b\\) or tmux eats it; GNU screen wants the
// same DCS wrap without the "tmux;" prefix.
//
// HOOK (editor-agent): term.go's writeOSC52 (term.go:~3209) emits the
// bare form today — its body should become
//   os.Stdout.WriteString(osc52Sequence(s))
// to pick up the multiplexer fallback.

import (
	"encoding/base64"
	"os"
	"strings"
)

// osc52Sequence returns the clipboard-write bytes for s, choosing the
// passthrough envelope the terminal stack needs.
func osc52Sequence(s string) string {
	payload := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(s)) + "\x07"
	if os.Getenv("TMUX") != "" {
		// tmux passthrough: every ESC inside the payload is doubled so
		// tmux forwards the inner sequence verbatim.
		return "\x1bPtmux;" + strings.ReplaceAll(payload, "\x1b", "\x1b\x1b") + "\x1b\\"
	}
	if t := os.Getenv("TERM"); strings.HasPrefix(t, "screen") {
		// GNU screen / tmux-foreign TERM: plain DCS wrap.
		return "\x1bP" + payload + "\x1b\\"
	}
	return payload
}
