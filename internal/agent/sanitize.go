package agent

// sanitize.go: 400-class sanitization + retry (mod_0278 C4n, mod_0386
// Pne/XT/J0/vMe/b4n/qFr). On API 400s matching a known class the request is
// stripped of the offending content and retried once per class, max 3
// sanitation attempts; prompt_too_long drops oldest non-system messages
// progressively, up to 5 attempts.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"claude-go/internal/api"
)

const (
	maxSanitizeAttempts = 3
	maxPromptDropTries  = 5
)

// classify400 maps an API error to a sanitization class ("" = not actionable).
func classify400(err error) string {
	if err == nil {
		return ""
	}
	var ae *api.ErrAPI
	if errors.As(err, &ae) {
		if h := ae.Sanitizable(); h != "" {
			return string(h)
		}
	}
	if !errors.As(err, &ae) && !strings.Contains(err.Error(), "400") &&
		!strings.Contains(err.Error(), "413") {
		return ""
	}
	m := strings.ToLower(err.Error())
	switch {
	case strings.Contains(m, "prompt is too long"),
		strings.Contains(m, "input is too long"),
		strings.Contains(m, "request_too_large"),
		strings.Contains(m, "request too large"),
		strings.Contains(m, "prompt_too_long"),
		strings.Contains(err.Error(), "413"):
		return "prompt_too_long"
	case strings.Contains(m, "advisor_tool_result"):
		return "advisor_tool_result"
	case thinking400(m):
		return "thinking"
	case strings.Contains(m, "encrypt"),
		strings.Contains(m, "could not decrypt"),
		strings.Contains(m, "redacted"):
		return "encrypted"
	case strings.Contains(m, "unexpected value"),
		strings.Contains(m, "clear_at"):
		return "unexpected_value"
	}
	return ""
}

// thinking400: thinking-config rejections (signature/type). Mirrors KFr/YFr.
func thinking400(m string) bool {
	if strings.Contains(m, "signature in thinking block") ||
		strings.Contains(m, "invalid data in redacted_thinking block") {
		return true
	}
	if strings.Contains(m, "thinking.signature") && strings.Contains(m, "field required") {
		return true
	}
	if strings.Contains(m, "thinking") && strings.Contains(m, "not supported") {
		return true
	}
	if strings.Contains(m, "thinking block") || strings.Contains(m, "redacted_thinking") {
		return strings.Contains(m, "cannot be modified") || strings.Contains(m, "invalid signature")
	}
	return false
}

// applySanitize strips l.Messages / request config for the given class.
func (l *Loop) applySanitize(class string) {
	switch class {
	case "thinking":
		l.noThinking = true
		l.Messages = stripBlockType(l.Messages,
			func(b map[string]any) bool {
				t, _ := b["type"].(string)
				return t == "thinking" || t == "redacted_thinking"
			}, "[Thinking removed]")
	case "advisor_tool_result":
		l.Messages = stripBlockType(l.Messages,
			func(b map[string]any) bool {
				t, _ := b["type"].(string)
				return strings.Contains(t, "advisor")
			}, "[Advisor response]")
	case "encrypted":
		l.Messages = stripBlockType(l.Messages,
			func(b map[string]any) bool {
				t, _ := b["type"].(string)
				if t == "redacted_thinking" || t == "redacted_result" ||
					strings.Contains(t, "redacted") {
					return true
				}
				_, hasEnc := b["encrypted_content"]
				return hasEnc
			}, "[Redacted content removed]")
	case "unexpected_value", "clear_at":
		l.Messages = stripClearAt(l.Messages)
	case "conversation_binding", "mid_conversation":
		// drop system-role messages bound to a prior turn/env
		l.Messages = stripBlockType(l.Messages, func(b map[string]any) bool {
			return false // handled below at message level
		}, "")
		var kept []api.Message
		for i, m := range l.Messages {
			if m.Role == "system" && i < len(l.Messages)-1 {
				continue
			}
			kept = append(kept, m)
		}
		l.Messages = kept
	case "safeguards":
		l.dropSafeguards = true
	case "output_config":
		l.dropOutputConfig = true
	case "invalid_beta_flag":
		l.dropBetas = true
	case "media_budget", "image_block", "document_block":
		// strip image/document blocks from oldest messages
		l.Messages = stripBlockType(l.Messages, func(b map[string]any) bool {
			t, _ := b["type"].(string)
			return t == "image" || t == "document"
		}, "[Media removed]")
	case "max_tokens_context_overflow":
		l.maxTokensClamp = true
	case "effort_unsupported":
		l.Effort = ""
	case "thinking_type_enabled", "thinking_type_adaptive":
		// swap thinking type
		l.ThinkingMode = "enabled"
	case "prompt_too_long":
		// handled by dropOldestForRetry in the caller
	}
}

// stripBlockType removes content blocks matching drop from every message;
// messages left empty get a placeholder text block. Content stored as a plain
// string is untouched.
func stripBlockType(msgs []api.Message, drop func(map[string]any) bool, placeholder string) []api.Message {
	out := make([]api.Message, len(msgs))
	for i, m := range msgs {
		out[i] = m
		blocks, ok := blocksOf(m)
		if !ok {
			continue
		}
		kept := blocks[:0]
		for _, b := range blocks {
			if !drop(b) {
				kept = append(kept, b)
			}
		}
		if len(kept) == 0 {
			kept = append(kept, map[string]any{"type": "text", "text": placeholder})
		}
		if raw, err := json.Marshal(kept); err == nil {
			out[i].Content = raw
		}
	}
	return out
}

// stripClearAt removes clear_at and per-message output_config fields — the
// fields behind "Unexpected value" 400s.
func stripClearAt(msgs []api.Message) []api.Message {
	out := make([]api.Message, len(msgs))
	for i, m := range msgs {
		out[i] = m
		out[i].OutputConfig = nil
		blocks, ok := blocksOf(m)
		if !ok {
			continue
		}
		for _, b := range blocks {
			delete(b, "clear_at")
			delete(b, "output_config")
		}
		if raw, err := json.Marshal(blocks); err == nil {
			out[i].Content = raw
		}
	}
	return out
}

// blocksOf decodes a message's array-form content into generic maps so
// unknown fields (citations, encrypted_content, clear_at) survive re-marshal.
func blocksOf(m api.Message) ([]map[string]any, bool) {
	var blocks []map[string]any
	if err := json.Unmarshal(m.Content, &blocks); err != nil || blocks == nil {
		return nil, false
	}
	return blocks, true
}

// dropOldestForRetry removes n oldest messages, skipping turn-scoped system
// messages unless they lead the slice, and never dropping the newest message.
func (l *Loop) dropOldestForRetry(n int) bool {
	dropped := false
	for n > 0 && len(l.Messages) > 1 {
		idx := -1
		for i := 0; i < len(l.Messages)-1; i++ {
			if l.Messages[i].Role != "system" || i == 0 {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		l.Messages = append(l.Messages[:idx], l.Messages[idx+1:]...)
		dropped = true
		n--
	}
	if dropped {
		l.Messages = repairToolPairing(l.Messages)
	}
	return dropped
}

// repairToolPairing removes tool_result blocks whose tool_use no longer
// exists in history (dangling after a drop); emptied messages get a marker.
func repairToolPairing(msgs []api.Message) []api.Message {
	seen := map[string]bool{}
	for i := range msgs {
		m := msgs[i]
		blocks, ok := blocksOf(m)
		if !ok {
			continue
		}
		if m.Role == "assistant" {
			for _, b := range blocks {
				if t, _ := b["type"].(string); t == "tool_use" {
					if id, _ := b["id"].(string); id != "" {
						seen[id] = true
					}
				}
			}
			continue
		}
		kept := blocks[:0]
		for _, b := range blocks {
			if t, _ := b["type"].(string); t == "tool_result" {
				if id, _ := b["tool_use_id"].(string); !seen[id] {
					continue
				}
			}
			kept = append(kept, b)
		}
		if len(kept) != len(blocks) {
			if len(kept) == 0 {
				kept = append(kept, map[string]any{"type": "text", "text": "[Tool result removed]"})
			}
			if raw, err := json.Marshal(kept); err == nil {
				msgs[i].Content = raw
			}
		}
	}
	return msgs
}

// noteRateLimit records the slow-lane UX note once per loop and emits it.
// TODO(api-agent): when api.Client exposes RateLimit (unified rate-limit
// headers with a "continue at lower priority" claim), surface its details
// here instead of the generic note.
func (l *Loop) noteRateLimit(err error) {
	if err == nil || l.RateLimitNote != "" {
		return
	}
	s := strings.ToLower(err.Error())
	if !strings.Contains(s, "429") && !strings.Contains(s, "rate_limit") &&
		!strings.Contains(s, "overloaded") {
		return
	}
	l.RateLimitNote = "rate limited — continuing at lower priority; retrying"
	if l.Events.OnTextDelta != nil {
		l.Events.OnTextDelta("\n  (" + l.RateLimitNote + ")\n")
	}
}

// streamWithSanitizeRetry runs the model call with 400-class sanitization and
// prompt_too_long history dropping, layered over the model fallback chain.
func (l *Loop) streamWithSanitizeRetry(ctx context.Context) (*api.Response, error) {
	sanitized := map[string]bool{}
	attempts := 0
	dropTries := 0
	for {
		resp, err := l.tryModels(ctx)
		if err == nil {
			return resp, nil
		}
		l.noteRateLimit(err)
		class := classify400(err)
		if class == "prompt_too_long" {
			if dropTries >= maxPromptDropTries || !l.dropOldestForRetry(dropTries+1) {
				return nil, err
			}
			dropTries++
			continue
		}
		if class != "" && !sanitized[class] && attempts < maxSanitizeAttempts {
			l.applySanitize(class)
			sanitized[class] = true
			attempts++
			continue
		}
		return nil, err
	}
}
