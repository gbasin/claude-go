package agent

import (
	"context"
	_ "embed"
	"os"
	"regexp"
	"strings"

	"claude-go/internal/api"
)

//go:embed compact_prompt.txt
var compactPrompt string

var summaryRe = regexp.MustCompile(`(?s)<summary>(.*)</summary>`)

// Compact summarizes the conversation and replaces history with the
// continuation message, matching Claude Code's /compact behavior.
func (l *Loop) Compact(ctx context.Context, transcriptPath string) error {
	// ask the model to summarize using the existing context
	l.Messages = append(l.Messages, api.TextMessage("user", compactPrompt))
	resp, err := l.run(ctx, 0)
	if err != nil {
		return err
	}
	var text string
	for _, b := range resp.Content {
		if b.Type == "text" {
			text += b.Text
		}
	}
	if m := summaryRe.FindStringSubmatch(text); m != nil {
		text = strings.TrimSpace(m[1])
	}
	cont := "This session is being continued from a previous conversation that ran out of context. " +
		"The summary below covers the earlier portion of the conversation.\n\n" + text
	if transcriptPath != "" {
		cont += "\n\nIf you need specific details from before compaction (like exact code snippets, " +
			"error messages, or content you generated), read the full transcript at: " + transcriptPath
	}
	l.Messages = []api.Message{api.TextMessage("user", cont)}
	l.started = true
	if l.Events.OnCompact != nil {
		l.Events.OnCompact()
	}
	return nil
}

// ShouldAutoCompact: window minus max-output reserve (20000) minus buffers
// (8000+4000), matching real autoCompactWindow intent.
func (l *Loop) ShouldAutoCompact(u api.Usage) bool {
	if os.Getenv("DISABLE_COMPACT") != "" {
		return false
	}
	total := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	return total > l.contextWindow()-20000-8000-4000
}

func (l *Loop) contextWindow() int {
	// context-1m-2025-08-07 beta applies to fable/sonnet 5.x; else 200k
	if strings.Contains(l.Model, "fable") || strings.Contains(l.Model, "sonnet-5") ||
		strings.Contains(l.Model, "opus-5") {
		return 1_000_000
	}
	return 200_000
}

// AutoCompact runs compaction when usage crosses the threshold.
func (l *Loop) AutoCompact(ctx context.Context, u api.Usage, transcriptPath string) error {
	if !l.ShouldAutoCompact(u) {
		return nil
	}
	if l.Events.OnAutoCompact != nil {
		l.Events.OnAutoCompact(u.InputTokens)
	}
	return l.Compact(ctx, transcriptPath)
}
