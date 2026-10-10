package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// ElicitRequest is an MCP server's elicitation/create awaiting an answer.
// The REPL drains ElicitCh between key reads; the responder writes Result.
type ElicitRequest struct {
	Server  string          // MCP server name
	Message string          `json:"message"`
	Schema  json.RawMessage `json:"requestedSchema"`
	Result  chan ElicitAnswer
}

// ElicitAnswer is the client response: action accept|decline|cancel +
// content map for accepts (real's elicitation/create result).
type ElicitAnswer struct {
	Action  string         `json:"action"`
	Content map[string]any `json:"content,omitempty"`
}

// RegisterElicitHandler installs the prompt-side dialog handler.
var elicitMu sync.Mutex
var elicitFn func(*ElicitRequest) ElicitAnswer

func SetElicitHandler(f func(*ElicitRequest) ElicitAnswer) {
	elicitMu.Lock()
	elicitFn = f
	elicitMu.Unlock()
}

// HandleElicit routes an elicitation request to the registered handler
// or declines when there's no UI.
func HandleElicit(server string, params json.RawMessage) map[string]any {
	var p struct {
		Message string          `json:"message"`
		Schema  json.RawMessage `json:"requestedSchema"`
	}
	_ = json.Unmarshal(params, &p)
	// CLAUDE_GO_ELICIT_ANSWER {action, content} is the explicit answer seam
	// (tests/SDK/non-interactive) — it wins over the form when set.
	if raw := os.Getenv("CLAUDE_GO_ELICIT_ANSWER"); raw != "" {
		var a struct {
			Action  string         `json:"action"`
			Content map[string]any `json:"content"`
		}
		if json.Unmarshal([]byte(raw), &a) == nil && a.Action != "" {
			out := map[string]any{"action": a.Action}
			if len(a.Content) > 0 {
				out["content"] = a.Content
			}
			return out
		}
	}
	req := &ElicitRequest{Server: server, Message: p.Message, Schema: p.Schema,
		Result: make(chan ElicitAnswer, 1)}
	elicitMu.Lock()
	f := elicitFn
	elicitMu.Unlock()
	if f == nil {
		// no form UI (print mode / bg worker): surface the question, decline.
		fmt.Fprintf(os.Stderr, "  %s asks: %s\n", server, p.Message)
		return map[string]any{"action": "decline"}
	}
	ans := f(req)
	out := map[string]any{"action": ans.Action}
	if len(ans.Content) > 0 {
		out["content"] = ans.Content
	}
	return out
}
