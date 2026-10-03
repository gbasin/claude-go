package repl

import (
	"encoding/json"
	"fmt"

	"claude-go/internal/agent"
	"claude-go/internal/api"
)

// showTranscript is ctrl+o's real behavior: dump the full conversation to
// scrollback — every message's text plus tool calls and results, no
// truncation — then wait for ctrl+o (or esc/q) to return to the prompt.
// The caller redraws the input box after we return.
func showTranscript(l *agent.Loop, ks *keySource) {
	fmt.Printf("\r\n%s── transcript ────────────────────────────────────────────%s\r\n", dim, reset)
	for _, m := range l.Messages {
		printTranscriptMessage(m)
	}
	fmt.Printf("%s── end transcript · ctrl+o to return ─────────────────────%s\r\n", dim, reset)
	for k := range ks.ch {
		if k.b == 15 || k.b == 3 || (k.b == 0x1b && k.seq == "") || k.b == 'q' {
			return
		}
	}
}

func printTranscriptMessage(m api.Message) {
	var blocks []api.ContentBlock
	if json.Unmarshal(m.Content, &blocks) != nil {
		var s string
		if json.Unmarshal(m.Content, &s) == nil && s != "" {
			printTranscriptText(m.Role, s)
		}
		return
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			printTranscriptText(m.Role, b.Text)
		case "thinking":
			printTranscriptDim("✻ thinking", b.Thinking)
		case "tool_use":
			fmt.Printf("  %s⏺%s %s%s%s\r\n", green, reset, bold+b.Name+reset, summarize(b.Name, b.Input), "")
		case "tool_result":
			printTranscriptResult(b)
		case "image":
			fmt.Printf("  %s⎿  [image]%s\r\n", dim, reset)
		}
	}
}

func printTranscriptText(role, text string) {
	if text == "" {
		return
	}
	switch role {
	case "user":
		fmt.Printf("%s>%s %s\r\n", cyan, reset, crlf(text))
	case "system":
		printTranscriptDim("system", text)
	default: // assistant
		fmt.Printf("%s⏺%s %s\r\n", reset, reset, crlf(text))
	}
}

func printTranscriptDim(label, text string) {
	if text == "" {
		return
	}
	fmt.Printf("  %s%s: %s%s\r\n", dim, label, crlf(text), reset)
}

func printTranscriptResult(b api.ContentBlock) {
	tag := "⎿"
	if b.IsError {
		tag = "⎿ error:"
	}
	var blocks []api.ContentBlock
	if json.Unmarshal(b.Content, &blocks) == nil && len(blocks) > 0 {
		for _, rb := range blocks {
			if rb.Type == "text" && rb.Text != "" {
				fmt.Printf("  %s%s%s %s\r\n", dim, tag, reset, crlf(rb.Text))
			} else if rb.Type == "image" {
				fmt.Printf("  %s%s [image]%s\r\n", dim, tag, reset)
			}
		}
		return
	}
	var s string
	if json.Unmarshal(b.Content, &s) == nil && s != "" {
		fmt.Printf("  %s%s%s %s\r\n", dim, tag, reset, crlf(s))
	}
}

// crlf makes embedded newlines print correctly while the terminal is in
// raw mode (no ONLCR translation).
func crlf(s string) string {
	out := make([]byte, 0, len(s)+8)
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, '\r')
		}
		out = append(out, s[i])
	}
	return string(out)
}
