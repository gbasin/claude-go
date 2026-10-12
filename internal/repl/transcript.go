package repl

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gbasin/claude-go/internal/agent"
	"github.com/gbasin/claude-go/internal/api"
	"github.com/gbasin/claude-go/internal/session"
	"github.com/gbasin/claude-go/internal/tools"
)

// showTranscript is real's ctrl+o — a scrollable overlay listing the whole
// conversation; ↑↓ scroll, pgup/pgdn (and ctrl+d/u half-page) page,
// esc/q/ctrl+o close. ctrl+e toggles thinking-block detail (real's
// transcript:toggleShowAll).
func showTranscript(l *agent.Loop, ks *keySource) {
	lines := transcriptLines(l, true)
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
				lines = transcriptLines(l, showAll)
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
			lines = transcriptLines(l, showAll)
			if top+viewH > len(lines) {
				top = max(0, len(lines)-viewH)
			}
			draw()
		}
	}
}

// transcriptLines flattens messages into styled display rows. showAll=false
// hides thinking blocks (real's default transcript detail toggle). When
// showAll is on, Agent/Task calls also replay the spawned agent's inner
// transcript — real's VerboseAgentTranscript (mod_1522 mmt renders every
// progress message under the agent row in transcript mode).
func transcriptLines(l *agent.Loop, showAll bool) []string {
	var out []string
	var agents *agentCursor
	for _, m := range l.Messages {
		out = append(out, transcriptMessageLines(m, showAll, func(b api.ContentBlock) []string {
			if agents == nil {
				agents = newAgentCursor(l)
			}
			return agents.splice(b, showAll)
		})...)
	}
	return out
}

// agentCursor pairs Agent/Task tool_use calls with spawned-agent records
// in call order, then reads the sidechain transcript file
// (<sid>/subagents/<agentID>.jsonl — session.OpenSubagent's layout).
type agentCursor struct {
	dir      string               // <transcript-dir>/<sid>/subagents
	recs     []*tools.AgentRecord // in-process agents, started-order
	claimed  map[string]bool      // agentIDs already spliced
	fallback []string             // *.jsonl paths by mtime (resume case)
}

func newAgentCursor(l *agent.Loop) *agentCursor {
	c := &agentCursor{claimed: map[string]bool{}}
	if l.Transcript == "" {
		return c
	}
	sid := strings.TrimSuffix(filepath.Base(l.Transcript), ".jsonl")
	c.dir = filepath.Join(filepath.Dir(l.Transcript), sid, "subagents")
	c.recs = tools.ListInProcessAgents()
	ents, err := os.ReadDir(c.dir)
	if err == nil {
		type fe struct {
			path string
			mt   int64
		}
		var files []fe
		for _, e := range ents {
			if !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			p := filepath.Join(c.dir, e.Name())
			var mt int64
			if fi, err := e.Info(); err == nil {
				mt = fi.ModTime().UnixNano()
			}
			files = append(files, fe{p, mt})
		}
		sort.Slice(files, func(i, j int) bool { return files[i].mt < files[j].mt })
		for _, f := range files {
			c.fallback = append(c.fallback, f.path)
		}
	}
	return c
}

// splice resolves the subagent transcript for one Agent/Task tool_use and
// returns its rendered rows, indented one level under the ⏺ Agent row.
func (c *agentCursor) splice(b api.ContentBlock, showAll bool) []string {
	if c.dir == "" {
		return nil
	}
	var path string
	// prefer a record whose name/type matches the call; else positional
	// pairing (call order ≈ start order)
	var in struct {
		Description string `json:"description"`
		Subagent    string `json:"subagent_type"`
	}
	json.Unmarshal(b.Input, &in)
	pick := func(a *tools.AgentRecord) {
		c.claimed[a.AgentID] = true
		path = filepath.Join(c.dir, a.AgentID+".jsonl")
		for i, f := range c.fallback {
			if f == path {
				c.fallback = append(c.fallback[:i], c.fallback[i+1:]...)
				break
			}
		}
	}
	for _, a := range c.recs {
		if !c.claimed[a.AgentID] &&
			(a.Name == in.Description || (in.Subagent != "" && a.Type == in.Subagent)) {
			pick(a)
			break
		}
	}
	if path == "" {
		for _, a := range c.recs {
			if !c.claimed[a.AgentID] {
				pick(a)
				break
			}
		}
	}
	if path == "" && len(c.fallback) > 0 {
		path, c.fallback = c.fallback[0], c.fallback[1:]
	}
	if path == "" {
		return nil
	}
	msgs, err := session.LoadPath(path)
	if err != nil || len(msgs) == 0 {
		return nil
	}
	var out []string
	for _, sm := range msgs {
		for _, ln := range transcriptMessageLines(sm, showAll, nil) {
			out = append(out, "  "+ln)
		}
	}
	return out
}

func transcriptMessageLines(m api.Message, showAll bool, agentRows func(api.ContentBlock) []string) []string {
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
			// expanded view replays the spawned agent's inner transcript
			// under its row (real's VerboseAgentTranscript)
			if showAll && agentRows != nil && (b.Name == "Agent" || b.Name == "Task") {
				out = append(out, agentRows(b)...)
			}
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
