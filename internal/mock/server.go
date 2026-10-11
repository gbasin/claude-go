// Package mock implements a functional stand-in for the Anthropic API.
// It speaks the real /v1/messages SSE protocol so claude-go can run
// end-to-end without network calls or credentials.
//
// Directives understood in the last user text:
//
//	calls:[{"name":"Bash","input":{"command":"ls"}}]  -> emit those tool_use blocks
//	tool:Read {"file_path":"/tmp/x"}                 -> single tool call
//	error:429                                        -> HTTP 429 (tests retry)
//	error:overloaded                                 -> SSE error event
//	think:<text>                                     -> emit a thinking block first
//	usage:<n>                                        -> report n input_tokens (drives auto-compact)
//	anything else                                    -> echo response as streamed text
package mock

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gbasin/claude-go/internal/api"
)

// Hit is one observed HTTP request — method, path?query, response status.
// The diff harness diffs real-vs-go request sequences, not just bodies.
type Hit struct {
	Method string
	Path   string
	Status int
}

type Server struct {
	seq atomic.Int64
	*http.Server
	Addr     string
	Requests atomic.Int64
	LastBody []byte
	// Bodies records every /v1/messages request body in order —
	// golden-diff inspection.
	Bodies [][]byte
	// Headers records the request headers alongside Bodies (golden
	// captures carry the full header set real sends).
	Headers []http.Header
	// Hits records every request (any path) in order.
	Hits []Hit
	mu   sync.Mutex
}

// Start launches the mock on 127.0.0.1:0 and returns it.
func Start() (*Server, error) { return StartOn("127.0.0.1:0") }

// StartOn launches the mock on a specific address (e.g. ":8080").
func StartOn(addr string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", s.handle)
	// claude-code-remote meta MCP endpoint — remote sessions mount it via
	// the injected claude-code-remote server; answers with real's verb set.
	mux.HandleFunc("/v2/ccr-sessions/-/meta/mcp", s.handleRemoteMCP)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"type":"error","error":{"type":"not_found_error","message":"unknown path"}}`, 404)
	})
	// record every request's method/path/status; CLAUDE_GO_MOCK_LOG=1 also
	// prints them to stderr (useful when driving real claude via mockd)
	s.Server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: 200}
		mux.ServeHTTP(sw, r)
		h := Hit{Method: r.Method, Path: r.URL.RequestURI(), Status: sw.status}
		s.mu.Lock()
		s.Hits = append(s.Hits, h)
		s.mu.Unlock()
		if os.Getenv("CLAUDE_GO_MOCK_LOG") != "" {
			fmt.Fprintf(os.Stderr, "mock: %s %s -> %d\n", h.Method, h.Path, h.Status)
		}
	})}
	go s.Serve(ln)
	s.Addr = "http://" + ln.Addr().String()
	return s, nil
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush forwards http.Flusher so SSE streaming still works through the
// status-recording wrapper.
func (w *statusWriter) Flush() {
	if fl, ok := w.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func writeErr(w http.ResponseWriter, code int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"type":"error","error":{"type":%q,"message":%q}}`, typ, msg)
}

type callSpec struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.Requests.Add(1)
	var req api.Request
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, 400, "invalid_request_error", err.Error())
		return
	}
	s.LastBody = raw
	s.mu.Lock()
	s.Bodies = append(s.Bodies, append([]byte{}, raw...))
	s.Headers = append(s.Headers, r.Header.Clone())
	n := len(s.Bodies)
	s.mu.Unlock()
	// CLAUDE_GO_MOCK_RECORD=<dir>: dump each request body — the golden
	// wire tests diff these against real captures.
	if dir := os.Getenv("CLAUDE_GO_MOCK_RECORD"); dir != "" {
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("%03d.json", n)), raw, 0o644)
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, 400, "invalid_request_error", err.Error())
		return
	}
	// safety-eval probe: the security-monitor call (sonnet, max≤128,
	// monitor prompt in sys[1]) — answer with a low severity so real
	// proceeds and we can capture the subagent/execution path.
	for _, sb := range req.System {
		if strings.Contains(sb.Text, "security monitor") && req.MaxTokens <= 128 {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"msg_eval","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"<severity>10</severity>"}],"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":5}}`, req.Model)
			return
		}
	}
	if msg := validateRequest(&req); msg != "" {
		writeErr(w, 400, "invalid_request_error", msg)
		return
	}

	lastText, hasToolResult := lastUserInfo(&req)

	// error directives
	if strings.HasPrefix(lastText, "error:") {
		kind := strings.TrimPrefix(lastText, "error:")
		if kind == "429" {
			w.Header().Set("retry-after", "0")
			writeErr(w, 429, "rate_limit_error", "mock rate limit")
			return
		}
		if strings.HasPrefix(lastText, "error:5") {
			writeErr(w, 503, "api_error", "mock "+kind)
			return
		}
	}

	var blocks []map[string]any // assembled response content
	stopReason := "end_turn"

	switch {
	case strings.HasPrefix(lastText, "calls:"):
		var calls []callSpec
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lastText, "calls:")), &calls); err != nil {
			writeErr(w, 400, "invalid_request_error", "bad calls directive: "+err.Error())
			return
		}
		for _, c := range calls {
			blocks = append(blocks, map[string]any{
				// ids must be unique across the whole session — two turns
				// with the same directive otherwise collide on wire
				"type": "tool_use", "id": fmt.Sprintf("toolu_mock_%d", s.seq.Add(1)),
				"name": c.Name, "input": c.Input,
			})
		}
		stopReason = "tool_use"
	case strings.HasPrefix(lastText, "tool:"):
		rest := strings.TrimPrefix(lastText, "tool:")
		name, js, _ := strings.Cut(rest, " ")
		var input json.RawMessage = json.RawMessage(`{}`)
		if js != "" {
			input = json.RawMessage(js)
		}
		blocks = append(blocks, map[string]any{
			"type": "tool_use", "id": fmt.Sprintf("toolu_mock_%d", s.seq.Add(1)), "name": name, "input": input,
		})
		stopReason = "tool_use"
	case hasToolResult:
		blocks = append(blocks, map[string]any{"type": "text",
			"text": "Tool results received:\n" + summarizeResults(&req)})
	case strings.HasPrefix(lastText, "think:"):
		think := strings.TrimPrefix(lastText, "think:")
		blocks = append(blocks,
			map[string]any{"type": "thinking", "thinking": think, "signature": "mock-sig"},
			map[string]any{"type": "text", "text": "Done thinking."})
	default:
		reply := "Mock response"
		if lastText != "" && lastText != "quota" {
			reply = "Mock response to: " + lastText
		}
		if lastText == "quota" {
			reply = "q"
		}
		blocks = append(blocks, map[string]any{"type": "text", "text": reply})
	}

	usage := map[string]int{"input_tokens": 1200, "output_tokens": 24,
		"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}
	// usage:<n> reports n input_tokens so tests can ramp context usage
	// across turns and trip the auto-compact threshold deterministically
	if strings.HasPrefix(lastText, "usage:") {
		if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(lastText, "usage:"))); err == nil {
			usage["input_tokens"] = n
		}
	}

	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_mock_1", "type": "message", "role": "assistant",
			"model": req.Model, "content": blocks, "stop_reason": stopReason,
			"usage": usage,
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)

	emit := func(event string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if fl != nil {
			fl.Flush()
		}
	}

	msgID := fmt.Sprintf("msg_mock_%d", time.Now().UnixNano()%1e6)
	emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": msgID, "type": "message", "role": "assistant", "model": req.Model,
		"content": []any{}, "stop_reason": nil, "usage": usage,
	}})

	for i, blk := range blocks {
		typ := blk["type"].(string)
		start := map[string]any{"type": typ}
		switch typ {
		case "text":
			start["text"] = ""
		case "thinking":
			start["thinking"] = ""
			start["signature"] = ""
		case "tool_use":
			start["id"] = blk["id"]
			start["name"] = blk["name"]
			start["input"] = map[string]any{}
		}
		emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": i, "content_block": start})
		switch typ {
		case "text":
			// stream word-by-word to exercise delta handling
			text := blk["text"].(string)
			for _, word := range strings.Fields(text + " ") {
				emit("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": i,
					"delta": map[string]any{"type": "text_delta", "text": word + " "}})
			}
		case "thinking":
			emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": i,
				"delta": map[string]any{"type": "thinking_delta", "thinking": blk["thinking"]}})
			emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": i,
				"delta": map[string]any{"type": "signature_delta", "signature": blk["signature"]}})
		case "tool_use":
			inJSON, _ := blk["input"].(json.RawMessage).MarshalJSON()
			if len(inJSON) == 0 {
				inJSON, _ = json.Marshal(blk["input"])
			}
			emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": i,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": string(inJSON)}})
		}
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}

	delta := map[string]any{"stop_reason": stopReason}
	// server-side dangerous_tool_use verdict: when the request carried the
	// dangerous_tool_use safeguard and the response holds tool_uses, the
	// verdict rides the final message_delta — the mock takes it from
	// CLAUDE_GO_MOCK_SGVERDICT (e2e) per tool_use id
	if v := os.Getenv("CLAUDE_GO_MOCK_SGVERDICT"); v != "" {
		uses := map[string]any{}
		for _, blk := range blocks {
			if blk["type"] == "tool_use" {
				uses[blk["id"].(string)] = map[string]any{
					"type": "evaluated", "outcome": v,
					"explanation": "mock verdict",
				}
			}
		}
		if len(uses) > 0 {
			delta["safeguard_results"] = []any{map[string]any{
				"type": "dangerous_tool_use",
				"status": map[string]any{
					"type": "available", "tool_uses": uses,
				},
			}}
		}
	}
	emit("message_delta", map[string]any{"type": "message_delta",
		"delta": delta, "usage": usage})
	emit("message_stop", map[string]any{"type": "message_stop"})
}

func lastUserInfo(req *api.Request) (text string, hasToolResult bool) {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != "user" {
			continue
		}
		var str string
		if json.Unmarshal(m.Content, &str) == nil {
			return str, false
		}
		var blocks []api.ContentBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			return "", false
		}
		var sb strings.Builder
		lastText := ""
		toolText := ""
		for _, b := range blocks {
			if b.Type == "tool_result" {
				hasToolResult = true
			}
			if b.Type == "text" {
				lastText = b.Text
				if strings.HasPrefix(b.Text, "tool:") || strings.HasPrefix(b.Text, "error:") {
					toolText = b.Text
				}
			}
		}
		// the LAST text block wins — except when it's an injected
		// reminder (subagent handback appends one after the real prompt).
		// Real user text appended into an earlier user message (interrupt
		// re-queue) must not lose to a stale directive block.
		if toolText != "" && strings.HasPrefix(lastText, "<system-reminder>") {
			lastText = toolText
		}
		sb.WriteString(lastText)
		return sb.String(), hasToolResult
	}
	return "", false
}

func summarizeResults(req *api.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != "user" {
			continue
		}
		var blocks []api.ContentBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			return ""
		}
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type != "tool_result" {
				continue
			}
			var inner []api.ContentBlock
			if err := json.Unmarshal(b.Content, &inner); err == nil {
				for _, c := range inner {
					sb.WriteString(c.Text)
				}
			} else {
				var s string
				if json.Unmarshal(b.Content, &s) == nil {
					sb.WriteString(s)
				}
			}
		}
		out := sb.String()
		if len(out) > 400 {
			out = out[:400] + "…"
		}
		return out
	}
	return ""
}

// handleRemoteMCP serves the claude-code-remote meta MCP endpoint — the
// streamable-HTTP handshake + the trigger/repo verb set real exposes in
// remote sessions (mod_0116.js Uwo/ATs).
func (s *Server) handleRemoteMCP(w http.ResponseWriter, r *http.Request) {
	var rpc struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if b, err := io.ReadAll(r.Body); err == nil {
		json.Unmarshal(b, &rpc)
	}
	if len(rpc.ID) == 0 || string(rpc.ID) == "null" {
		w.WriteHeader(202) // notification
		return
	}
	var result any
	switch rpc.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "Claude Code Remote", "version": "1"},
		}
	case "tools/list":
		var tools []map[string]any
		for _, v := range []string{
			"list_triggers", "create_trigger", "update_trigger",
			"delete_trigger", "fire_trigger", "send_later",
			"add_repo", "check_repo_access", "list_repos",
		} {
			tools = append(tools, map[string]any{
				"name":        v,
				"description": "remote trigger/repo verb",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			})
		}
		result = map[string]any{"tools": tools}
	case "tools/call":
		result = map[string]any{"content": []map[string]any{{
			"type": "text", "text": `{"ok":true}`}}}
	default:
		result = map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": json.RawMessage(rpc.ID), "result": result})
}
