package agent

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"claude-go/internal/api"
	"claude-go/internal/hooks"
)

//go:embed compact_prompt.txt
var compactPrompt string

var summaryRe = regexp.MustCompile(`(?s)<summary>(.*)</summary>`)

// Compact summarizes the conversation and replaces history with the
// continuation message, matching Claude Code's /compact behavior.
// Manual trigger (the /compact command).
func (l *Loop) Compact(ctx context.Context, transcriptPath string) error {
	return l.compactWithTrigger(ctx, transcriptPath, "manual", 0)
}

// PartialCompact compacts only messages before index keepLast — the
// --resume-session-at boundary form. Messages at/after keepLast are kept.
func (l *Loop) PartialCompact(ctx context.Context, keepLast int) error {
	return l.compactWithTrigger(ctx, l.Transcript, "manual", keepLast)
}

// compactAuto runs auto-trigger compaction, consuming a precomputed summary
// when one was cached for exactly this history (instant compact).
func (l *Loop) compactAuto(ctx context.Context) error {
	if h := l.runHook(hooks.PreCompact, map[string]any{"trigger": "auto"}); h.Block {
		return fmt.Errorf("compaction blocked by hook: %s", h.Reason)
	}
	if s := l.takePrecomputed(); s != "" {
		l.applySummary(s, l.Transcript, 0)
		l.runHook(hooks.PostCompact, map[string]any{"trigger": "auto"})
		return nil
	}
	return l.compactExec(ctx, l.Transcript, "auto", 0)
}

// compactWithTrigger fires PreCompact (blocking skips), summarizes, rewrites
// history, fires PostCompact. keepLast>0 preserves the tail of the history.
func (l *Loop) compactWithTrigger(ctx context.Context, transcriptPath, trigger string, keepLast int) error {
	if h := l.runHook(hooks.PreCompact, map[string]any{"trigger": trigger}); h.Block {
		return fmt.Errorf("compaction blocked by hook: %s", h.Reason)
	}
	return l.compactExec(ctx, transcriptPath, trigger, keepLast)
}

// compactExec summarizes the span and rewrites history; caller owns hooks.
func (l *Loop) compactExec(ctx context.Context, transcriptPath, trigger string, keepLast int) error {
	span := l.Messages
	if keepLast <= 0 || keepLast >= len(l.Messages) {
		keepLast = 0
	} else {
		span = l.Messages[:keepLast]
	}
	text, err := l.summarize(ctx, span)
	if err != nil {
		return err
	}
	l.applySummary(text, transcriptPath, keepLast)
	l.runHook(hooks.PostCompact, map[string]any{"trigger": trigger})
	return nil
}

// applySummary replaces the compacted span with the continuation message.
func (l *Loop) applySummary(text, transcriptPath string, keepLast int) {
	cont := "This session is being continued from a previous conversation that ran out of context. " +
		"The summary below covers the earlier portion of the conversation.\n\n" + text
	if transcriptPath != "" {
		cont += "\n\nIf you need specific details from before compaction (like exact code snippets, " +
			"error messages, or content you generated), read the full transcript at: " + transcriptPath
	}
	msgs := []api.Message{api.TextMessage("user", cont)}
	if keepLast > 0 && keepLast < len(l.Messages) {
		msgs = append(msgs, l.Messages[keepLast:]...)
	}
	l.Messages = msgs
	l.started = true
	// the compact_boundary transcript record is written via this callback
	// (wired to session.Store.LogCompactBoundary by the caller)
	// SessionStart refires with source:"compact" after compaction (real CC)
	l.SessionStartSource = "compact"
	l.runHook(hooks.SessionStart, map[string]any{"source": "compact"})
	if l.Events.OnCompact != nil {
		l.Events.OnCompact()
	}
}

// summarize asks the model to condense msgs into a <summary>.
func (l *Loop) summarize(ctx context.Context, msgs []api.Message) (string, error) {
	req := l.buildRequest()
	req.Tools = nil // summarization needs no tool surface
	req.Messages = append(append([]api.Message{}, msgs...),
		api.TextMessage("user", compactPrompt))
	resp, err := l.Client.StreamMessage(ctx, req, func(ev *api.StreamEvent) error {
		if ev.Type == "message_delta" && ev.Usage != nil {
			l.UsageTotal.OutputTokens += ev.Usage.OutputTokens
		}
		return nil
	})
	if err != nil {
		return "", err
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
	if text == "" {
		return "", fmt.Errorf("compaction produced no summary")
	}
	return text, nil
}

// precomputedSummary is a cached compaction result for a specific history.
type precomputedSummary struct {
	text  string
	count int // len(l.Messages) the summary was computed over
}

var (
	precompMu       sync.Mutex
	precompCache    = map[*Loop]*precomputedSummary{}
	precompInFlight = map[*Loop]bool{}
)

// maybePrecompute starts a background summary when usage is within 10% of the
// auto-compact threshold (precomputeCompactionEnabled).
func (l *Loop) maybePrecompute(u api.Usage) {
	if !l.precomputeOn() {
		return
	}
	total := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	threshold := l.contextWindow() - 20000 - 8000 - 4000
	if total < threshold*9/10 {
		return
	}
	precompMu.Lock()
	if precompInFlight[l] {
		precompMu.Unlock()
		return
	}
	if s := precompCache[l]; s != nil && s.count == len(l.Messages) {
		precompMu.Unlock()
		return
	}
	precompInFlight[l] = true
	msgs := append([]api.Message{}, l.Messages...)
	precompMu.Unlock()
	go func() {
		text, err := l.summarize(context.Background(), msgs)
		precompMu.Lock()
		defer precompMu.Unlock()
		delete(precompInFlight, l)
		if err == nil && text != "" {
			if precompCache == nil {
				precompCache = map[*Loop]*precomputedSummary{}
			}
			precompCache[l] = &precomputedSummary{text: text, count: len(msgs)}
		}
	}()
}

// takePrecomputed returns the cached summary if it covers current history.
func (l *Loop) takePrecomputed() string {
	precompMu.Lock()
	defer precompMu.Unlock()
	s := precompCache[l]
	if s == nil || s.count != len(l.Messages) {
		return ""
	}
	delete(precompCache, l)
	return s.text
}

// precomputeOn: loop flag + settings key precomputeCompactionEnabled
// (default true when the flag is set).
func (l *Loop) precomputeOn() bool {
	return l.Precompute && precomputeCompactionEnabled(l.ToolCtx.CWD)
}

// precomputeCompactionEnabled reads the setting from user + project
// settings.json files; absent means enabled.
func precomputeCompactionEnabled(cwd string) bool {
	enabled := true
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(cwd, ".claude", "settings.json"),
		filepath.Join(cwd, ".claude", "settings.local.json"),
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var s map[string]any
		if json.Unmarshal(b, &s) != nil {
			continue
		}
		if v, ok := s["precomputeCompactionEnabled"].(bool); ok {
			enabled = v // later sources win, project over user
		}
	}
	return enabled
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
	if transcriptPath != l.Transcript {
		l.Transcript = transcriptPath
	}
	return l.compactAuto(ctx)
}
