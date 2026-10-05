package mock

// Strict request validation — the mock enforces the same shape rules the
// real API 400s on, so e2e catches wire bugs the old mock swallowed.
// Every rule here corresponds to a rejection observed live:
//   - >4 cache_control breakpoints total  ("A maximum of 4 blocks with
//     cache_control may be provided")
//   - ttl:"1h" after ttl:"5m" in wire order  ("a ttl='1h' cache_control
//     block must not come after a ttl='5m' cache_control block")
//   - tool_use without exactly one tool_result  ("each tool_use must
//     have a single result")
//   - system message not first-or-last  ("role 'system' must precede an
//     'assistant' message or end the array")
//   - thinking block missing thinking/signature  ("thinking.thinking:
//     Field required")
//   (consecutive user messages are NOT checked — real's post-compact
//    history is [summary-user, new-user] and the API accepts it)

import (
	"claude-go/internal/api"
	"encoding/json"
	"fmt"
)

// validateRequest returns an error string ("" when valid) describing the
// first wire-shape violation, matching real API error phrasing where
// known.
func validateRequest(req *api.Request) string {
	bp := 0
	saw5m := false
	checkCC := func(cc *api.CacheControl, where string) string {
		if cc == nil {
			return ""
		}
		bp++
		ttl := cc.TTL
		if ttl == "" {
			ttl = "5m"
		}
		if ttl == "1h" && saw5m {
			return fmt.Sprintf("%s: a ttl='1h' cache_control block must not come after a ttl='5m' cache_control block", where)
		}
		if ttl == "5m" {
			saw5m = true
		}
		return ""
	}
	for i, t := range req.Tools {
		// Tool.CacheControl lives inside Raw when marshaled — parse it out
		var raw struct {
			CacheControl *api.CacheControl `json:"cache_control"`
		}
		if b, err := json.Marshal(t); err == nil {
			json.Unmarshal(b, &raw)
		}
		if err := checkCC(raw.CacheControl, fmt.Sprintf("tools.%d", i)); err != "" {
			return err
		}
	}
	for _, s := range req.System {
		if err := checkCC(s.CacheControl, "system"); err != "" {
			return err
		}
	}
	prevToolIDs := map[string]bool{}
	usedToolIDs := map[string]bool{}
	for i, m := range req.Messages {
		var blocks []api.ContentBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			var s string
			if json.Unmarshal(m.Content, &s) == nil {
				blocks = nil // bare string content is legal
			} else {
				return fmt.Sprintf("messages.%d.content: could not parse", i)
			}
		}
		for j, b := range blocks {
			if err := checkCC(b.CacheControl, fmt.Sprintf("messages.%d.content.%d", i, j)); err != "" {
				return err
			}
			switch b.Type {
			case "thinking", "redacted_thinking":
				if b.Type == "thinking" && b.Thinking == "" {
					return fmt.Sprintf("messages.%d.content.%d.thinking.thinking: Field required", i, j)
				}
				if b.Signature == "" && b.Type == "thinking" {
					return fmt.Sprintf("messages.%d.content.%d.thinking.signature: Field required", i, j)
				}
			case "tool_use":
				if m.Role != "assistant" {
					return fmt.Sprintf("messages.%d: tool_use may only appear in assistant messages", i)
				}
				prevToolIDs[b.ID] = true
			case "tool_result":
				if m.Role != "user" {
					return fmt.Sprintf("messages.%d: tool_result may only appear in user messages", i)
				}
				if !prevToolIDs[b.ToolUseID] {
					return fmt.Sprintf("messages.%d.content.%d: unexpected tool_use_id %q found in tool_result block", i, j, b.ToolUseID)
				}
				if usedToolIDs[b.ToolUseID] {
					return fmt.Sprintf("each tool_use must have a single result. Found multiple `tool_result` blocks with id %q", b.ToolUseID)
				}
				usedToolIDs[b.ToolUseID] = true
			}
		}
		if m.Role == "system" && i < len(req.Messages)-1 && req.Messages[i+1].Role != "assistant" {
			// real: system role valid only before an assistant message
			// (index 0 head) or as the last element
			return fmt.Sprintf("messages.%d: role 'system' must precede an 'assistant' message or end the array", i)
		}
	}
	for id := range prevToolIDs {
		if !usedToolIDs[id] {
			return fmt.Sprintf("tool_use id %q found without a corresponding tool_result", id)
		}
	}
	if bp > 4 {
		return fmt.Sprintf("A maximum of 4 blocks with cache_control may be provided. Found %d", bp)
	}
	return ""
}
