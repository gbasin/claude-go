package repl

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/gbasin/claude-go/internal/agent"
	"github.com/gbasin/claude-go/internal/api"
)

// showTranscript is real's ctrl+o — a scrollable overlay listing the whole
// conversation; ↑↓ scroll, pgup/pgdn (and ctrl+d/u half-page) page,
// esc/q/ctrl+o close. ctrl+e toggles thinking-block detail (real's
// transcript:toggleShowAll).
func showTranscript(l *agent.Loop, ks *keySource) {
	lines := transcriptLines(l.Messages, true)
	_, rows, _ := compGetSize()
	viewH := rows - 6
	if viewH < 4 {
		viewH = 4
	}
	showAll := true
	top := max(0, len(lines)-viewH) // start at the latest
	dch := make(chan key, 16)
	ks.dialog.Store(&dch)
	defer ks.dialog.Store(nil)
	compSuspend()
	defer compResume()
	draw := func() {
		var sb strings.Builder
		sb.WriteString("\x1b[H\x1b[2J") // home + clear — overlay owns the screen
		sb.WriteString(fmt.Sprintf("%s── transcript · %d messages · ↑↓ scroll · esc close%s\r\n",
			dim, len(l.Messages), reset))
		end := top + viewH
		if end > len(lines) {
			end = len(lines)
		}
		for _, ln := range lines[top:end] {
			sb.WriteString(ln + "\r\n")
		}
		fmt.Print(sb.String())
	}
	draw()
	for k := range dch {
		// user remaps in the Transcript context take precedence —
		// real's dialogs resolve through the same keymap as the editor
		// Transcript/Global remaps: actions this view understands run
		// first; unrecognized actions fall through to the hardcoded keys
		if act := dialogAction("Transcript", k); act != "" &&
			(strings.HasPrefix(act, "scroll:") || strings.HasPrefix(act, "transcript:")) {
			switch act {
			case "transcript:exit":
				fmt.Print("\x1b[H\x1b[2J")
				return
			case "transcript:toggleShowAll":
				showAll = !showAll
				lines = transcriptLines(l.Messages, showAll)
				if top+viewH > len(lines) {
					top = max(0, len(lines)-viewH)
				}
				draw()
				continue
			case "scroll:lineUp":
				if top > 0 {
					top--
				}
				draw()
				continue
			case "scroll:lineDown":
				if top+viewH < len(lines) {
					top++
				}
				draw()
				continue
			case "scroll:halfPageUp":
				top -= viewH / 2
				if top < 0 {
					top = 0
				}
				draw()
				continue
			case "scroll:halfPageDown":
				top += viewH / 2
				if top+viewH > len(lines) {
					top = max(0, len(lines)-viewH)
				}
				draw()
				continue
			case "scroll:top":
				top = 0
				draw()
				continue
			case "scroll:fullPageUp":
				top -= viewH
				if top < 0 {
					top = 0
				}
				draw()
				continue
			case "scroll:fullPageDown":
				top += viewH
				if top+viewH > len(lines) {
					top = max(0, len(lines)-viewH)
				}
				draw()
				continue
			case "scroll:bottom":
				top = max(0, len(lines)-viewH)
				draw()
				continue
			}
		}
		switch {
		case k.b == 15 || k.b == 3 || k.b == 'q' || (k.b == 0x1b && k.seq == ""):
			fmt.Print("\x1b[H\x1b[2J")
			return
		case k.seq == "[A" || k.b == 'k' || k.b == 0x10: // up / ctrl+p
			if top > 0 {
				top--
				draw()
			}
		case k.seq == "[B" || k.b == 'j' || k.b == 0x0e: // down / ctrl+n
			if top+viewH < len(lines) {
				top++
				draw()
			}
		case k.seq == "[5~" || k.b == 'b' || k.b == 2: // pgup / ctrl+b
			top -= viewH
			if top < 0 {
				top = 0
			}
			draw()
		case k.seq == "[6~" || k.b == 'f' || k.b == 6 || k.b == ' ': // pgdn / ctrl+f / space (real)
			top += viewH
			if top+viewH > len(lines) {
				top = max(0, len(lines)-viewH)
			}
			draw()
		case k.b == 4: // ctrl+d half-page
			top += viewH / 2
			if top+viewH > len(lines) {
				top = max(0, len(lines)-viewH)
			}
			draw()
		case k.b == 21: // ctrl+u half-page
			top -= viewH / 2
			if top < 0 {
				top = 0
			}
			draw()
		case k.b == 'g' || k.seq == "[H" || k.seq == "[1~" || k.seq == "[7~": // g / home (real)
			top = 0
			draw()
		case k.b == 'G' || k.seq == "[F" || k.seq == "[4~" || k.seq == "[8~": // G / end (real)
			top = max(0, len(lines)-viewH)
			draw()
		case k.seq == "[<wheel-up":
			step := wheelScrollStep()
			top -= step
			if top < 0 {
				top = 0
			}
			draw()
		case k.seq == "[<wheel-down":
			top += wheelScrollStep()
			if top+viewH > len(lines) {
				top = max(0, len(lines)-viewH)
			}
			draw()
		case k.b == 5: // ctrl+e — transcript:toggleShowAll
			showAll = !showAll
			lines = transcriptLines(l.Messages, showAll)
			if top+viewH > len(lines) {
				top = max(0, len(lines)-viewH)
			}
			draw()
		}
	}
}

// transcriptLines flattens messages into styled display rows. showAll=false
// hides thinking blocks (real's default transcript detail toggle).
func transcriptLines(msgs []api.Message, showAll bool) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, transcriptMessageLines(m, showAll)...)
	}
	return out
}

func transcriptMessageLines(m api.Message, showAll bool) []string {
	var out []string
	var blocks []api.ContentBlock
	if json.Unmarshal(m.Content, &blocks) != nil {
		var s string
		if json.Unmarshal(m.Content, &s) == nil && s != "" {
			out = append(out, transcriptTextLines(m.Role, s)...)
		}
		return out
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, transcriptTextLines(m.Role, b.Text)...)
		case "thinking":
			if showAll && b.Thinking != "" {
				out = append(out, "  "+dim+"✻ thinking:"+reset)
				for _, ln := range strings.Split(b.Thinking, "\n") {
					out = append(out, "  "+dim+ln+reset)
				}
			}
		case "tool_use":
			out = append(out, fmt.Sprintf("  %s⏺%s %s%s", green, reset, bold+b.Name+reset, summarize(b.Name, b.Input)))
		case "tool_result":
			out = append(out, transcriptResultLines(b)...)
		case "image":
			out = append(out, "  "+dim+"⎿  [image]"+reset)
		}
	}
	return out
}

func transcriptTextLines(role, text string) []string {
	if text == "" {
		return nil
	}
	var prefix string
	switch role {
	case "user":
		prefix = cyan + ">" + reset + " "
	case "system":
		prefix = "  " + dim + "system: "
	default:
		prefix = "⏺ "
	}
	var out []string
	for i, ln := range strings.Split(text, "\n") {
		if i == 0 {
			out = append(out, prefix+ln)
		} else {
			out = append(out, "  "+ln)
		}
	}
	return out
}

func transcriptResultLines(b api.ContentBlock) []string {
	tag := "⎿"
	if b.IsError {
		tag = "⎿ error:"
	}
	var out []string
	var blocks []api.ContentBlock
	if json.Unmarshal(b.Content, &blocks) == nil && len(blocks) > 0 {
		for _, rb := range blocks {
			if rb.Type == "text" && rb.Text != "" {
				for i, ln := range strings.Split(rb.Text, "\n") {
					if i == 0 {
						out = append(out, "  "+dim+tag+reset+" "+ln)
					} else {
						out = append(out, "    "+dim+ln+reset)
					}
				}
			} else if rb.Type == "image" {
				out = append(out, "  "+dim+tag+" [image]"+reset)
			}
		}
		return out
	}
	var s string
	if json.Unmarshal(b.Content, &s) == nil && s != "" {
		for i, ln := range strings.Split(s, "\n") {
			if i == 0 {
				out = append(out, "  "+dim+tag+reset+" "+ln)
			} else {
				out = append(out, "    "+dim+ln+reset)
			}
		}
	}
	return out
}

// wheelScrollStep is CLAUDE_CODE_SCROLL_SPEED (a row multiplier, real
// clamps at 20); absent/invalid means 1.
func wheelScrollStep() int {
	v, err := strconv.ParseFloat(os.Getenv("CLAUDE_CODE_SCROLL_SPEED"), 64)
	if err != nil || v <= 0 {
		return 1
	}
	if v > 20 {
		v = 20
	}
	return int(v)
}
