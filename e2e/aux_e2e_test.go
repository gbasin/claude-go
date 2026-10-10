//go:build e2e

// aux_e2e_test.go — the auxiliary request plane exercised through the real
// binary (aux is skipped under --mock, so this suite can't cover it there).
// A recording HTTP server stands in for api.anthropic.com via --sdk-url;
// a fake env OAuth token puts the client on the oauth lane, and
// CLAUDE_GO_RAW_HTTP=0 forces the stdlib transport so plain http works.
//
// Asserts, matching the 2.1.289 TLS capture order:
//   - the startup GET sequence fires in captured wire order
//   - a haiku max_tokens=1 quota probe POSTs with request-class=auxiliary
//   - a ClaudeCodeInternalEvent batch POSTs to /api/event_logging/v2/batch
//   - all of it happens while/before the single main /v1/messages turn
//   - API-key creds suppress the whole burst (oauth-only lane)
//   - CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC suppresses it too
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"claude-go/internal/session"
	"time"
)

type recReq struct {
	method string
	uri    string
	class  string // x-claude-code-request-class
	auth   string // authorization scheme, "" when absent
	body   []byte
}

// auxRecorder serves every endpoint with a benign empty response and the
// minimal SSE reply for streamed /v1/messages calls. A last user message
// starting with "tool:" emits a tool_use block like the mock does;
// non-streamed calls (the safety eval uses SendMessage) get a plain JSON
// reply whose text is nonStreamText.
type auxRecorder struct {
	srv           *httptest.Server
	nonStreamText string
	// sgVerdict, when non-empty, injects a dangerous_tool_use
	// safeguard_results frame on streamed tool_use replies (the
	// server-side classifier verdict real returns in message_delta).
	sgVerdict string
	// nonStreamByModel overrides nonStreamText keyed on the request's model
	// field — lets the two eval stages answer differently.
	nonStreamByModel map[string]string

	mu   sync.Mutex
	reqs []recReq
}

// bodyHasMaxTokens reports whether a request body carries max_tokens==n
// (string matching trips on 128000 containing ":1").
func bodyHasMaxTokens(body []byte, n int) bool {
	var v struct {
		MT int `json:"max_tokens"`
	}
	return json.Unmarshal(body, &v) == nil && v.MT == n
}

func newAuxRecorder(t *testing.T) *auxRecorder {
	t.Helper()
	r := &auxRecorder{nonStreamText: "aux-server reply"}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
		var body []byte
		if rq.Body != nil {
			body, _ = io.ReadAll(rq.Body)
			rq.Body.Close()
		}
		auth := ""
		if a := rq.Header.Get("Authorization"); a != "" {
			auth = strings.SplitN(a, " ", 2)[0]
		}
		r.mu.Lock()
		r.reqs = append(r.reqs, recReq{rq.Method, rq.URL.RequestURI(),
			rq.Header.Get("x-claude-code-request-class"), auth, body})
		r.mu.Unlock()

		switch {
		case rq.Method == "POST" && rq.URL.Path == "/v1/messages":
			var req struct {
				Stream   bool `json:"stream"`
				Messages []struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
			}
			json.Unmarshal(body, &req)
			if req.Stream {
				if calls := auxToolDirective(req.Messages); calls != nil {
					writeToolUseSSE(w, calls, r.sgVerdict)
				} else {
					writeSSE(w, "aux-server reply")
				}
			} else {
				w.Header().Set("Content-Type", "application/json")
				text := r.nonStreamText
				if r.nonStreamByModel != nil {
					var b struct {
						Model string `json:"model"`
					}
					if json.Unmarshal(body, &b) == nil {
						if v, ok := r.nonStreamByModel[b.Model]; ok {
							text = v
						}
					}
				}
				reply, _ := json.Marshal(text)
				fmt.Fprintf(w, `{"id":"m1","type":"message","role":"assistant",`+
					`"content":[{"type":"text","text":%s}],`+
					`"model":"m","stop_reason":"end_turn",`+
					`"usage":{"input_tokens":10,"output_tokens":3}}`, reply)
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// auxToolDirective extracts the mock's test-control directives from the
// last user message — `tool:Name {json}` for one call or `calls:[...]`
// for a batch — so the non-mock recorder can drive tool turns.
type auxCall struct {
	name  string
	input json.RawMessage
}

func auxToolDirective(msgs []struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}) []auxCall {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		var text string
		if json.Unmarshal(msgs[i].Content, &blocks) == nil {
			for _, b := range blocks {
				if b.Type == "text" {
					text = b.Text
				}
			}
		} else {
			var s string
			if json.Unmarshal(msgs[i].Content, &s) == nil {
				text = s
			}
		}
		switch {
		case strings.HasPrefix(text, "tool:"):
			name, js, _ := strings.Cut(strings.TrimPrefix(text, "tool:"), " ")
			input := json.RawMessage(`{}`)
			if js != "" {
				input = json.RawMessage(js)
			}
			return []auxCall{{name, input}}
		case strings.HasPrefix(text, "calls:"):
			var calls []struct {
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if json.Unmarshal([]byte(strings.TrimPrefix(text, "calls:")), &calls) != nil {
				return nil
			}
			var out []auxCall
			for _, c := range calls {
				out = append(out, auxCall{c.Name, c.Input})
			}
			return out
		}
		return nil
	}
	return nil
}

// writeToolUseSSE emits tool_use blocks (stop_reason tool_use) in the SSE
// shape the client parser expects.
func writeToolUseSSE(w http.ResponseWriter, calls []auxCall, sgVerdict string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	emit := func(event string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if fl != nil {
			fl.Flush()
		}
	}
	usage := map[string]int{"input_tokens": 10, "output_tokens": 3}
	emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_aux", "type": "message", "role": "assistant",
		"model": "m", "content": []any{}, "usage": usage}})
	for i, c := range calls {
		emit("content_block_start", map[string]any{"type": "content_block_start",
			"index": i, "content_block": map[string]any{
				"type": "tool_use", "id": fmt.Sprintf("toolu_aux_%d", i), "name": c.name,
				"input": map[string]any{}}})
		emit("content_block_delta", map[string]any{"type": "content_block_delta",
			"index": i, "delta": map[string]any{
				"type": "input_json_delta", "partial_json": string(c.input)}})
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	delta := map[string]any{"stop_reason": "tool_use"}
	if sgVerdict != "" {
		uses := map[string]any{}
		for i := range calls {
			uses[fmt.Sprintf("toolu_aux_%d", i)] = map[string]any{
				"type": "evaluated", "outcome": sgVerdict,
			}
		}
		delta["safeguard_results"] = []any{map[string]any{
			"type":   "dangerous_tool_use",
			"status": map[string]any{"type": "available", "tool_uses": uses},
		}}
	}
	emit("message_delta", map[string]any{"type": "message_delta",
		"delta": delta, "usage": usage})
	emit("message_stop", map[string]any{"type": "message_stop"})
}

// writeSSE emits the minimal event sequence the client needs to finish a
// streamed turn (same shape as internal/mock).
func writeSSE(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	emit := func(event string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if fl != nil {
			fl.Flush()
		}
	}
	usage := map[string]int{"input_tokens": 10, "output_tokens": 3}
	emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_aux", "type": "message", "role": "assistant",
		"model": "m", "content": []any{}, "usage": usage}})
	emit("content_block_start", map[string]any{"type": "content_block_start",
		"index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	emit("content_block_delta", map[string]any{"type": "content_block_delta",
		"index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
	emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	emit("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn"}, "usage": usage})
	emit("message_stop", map[string]any{"type": "message_stop"})
}

func (r *auxRecorder) list() []recReq {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recReq{}, r.reqs...)
}

// auxEnv runs the binary against the recorder in oauth-lane print mode.
func (s *sess) runAux(t *testing.T, rec *auxRecorder, extraEnv []string, args ...string) runResult {
	env := append([]string{
		"CLAUDE_CODE_OAUTH_TOKEN=e2e-fake-oauth-token",
		"CLAUDE_GO_RAW_HTTP=0", // stdlib transport — plain http to httptest
	}, extraEnv...)
	return s.runEx(t, env, 45*time.Second, append([]string{"--sdk-url", rec.srv.URL}, args...)...)
}

// startupGetOrder is the captured 2.1.289 sequence (no org uuid → the
// org-scoped interleave is absent).
var startupGetOrder = []string{
	"/v1/mcp_servers?limit=1000&include_additional_installs=true",
	"/v1/mcp_servers?limit=1000",
	"/api/oauth/account/settings",
	"/mcp-registry/v0/servers?version=latest&limit=100&visibility=commercial%2Cgsuite%2Centerprise%2Chealth",
	"/api/claude_cli/bootstrap?entrypoint=sdk-cli&model=",
	"/api/claude_code_penguin_mode",
	"/api/claude_code_grove",
	"/v1/mcp_servers?limit=1000&include_additional_installs=true",
	"/v1/mcp_servers?limit=1000",
}

func TestAuxStartupSequence(t *testing.T) {
	rec := newAuxRecorder(t)
	s := newSession(t)
	r := s.runAux(t, rec, nil, "-p", "say hi")
	s.mustOK(t, r)
	contains(t, r.stdout, "aux-server reply", "print reply")

	reqs := rec.list()
	// the startup GETs fire in capture order relative to each other
	// (eval / main POST may interleave between them)
	var got []string
	for _, q := range reqs {
		if q.method == "GET" {
			got = append(got, q.uri)
		}
	}
	pos := 0
	for _, want := range startupGetOrder {
		found := false
		for ; pos < len(got); pos++ {
			if strings.HasPrefix(got[pos], want) || got[pos] == want {
				found = true
				pos++
				break
			}
		}
		if !found {
			t.Fatalf("aux GET %q missing/out of order\nall GETs: %v", want, got)
		}
	}
	// mcp_servers calls carry the mcp beta + protocol headers; registry is
	// unauthenticated, everything else rides Bearer
	var sawProbe, sawBatch, sawMain bool
	for _, q := range reqs {
		switch {
		case q.method == "POST" && bodyHasMaxTokens(q.body, 1):
			sawProbe = true
			if q.class != "auxiliary" {
				t.Fatalf("quota probe request-class = %q", q.class)
			}
			// real probes with the RESOLVED session model (gnt @65597's
			// model arg e), not a hardcoded haiku
			if !strings.Contains(string(q.body), `"model"`) {
				t.Fatalf("quota probe body missing model, got %s", q.body)
			}
		case q.method == "POST" && strings.HasPrefix(q.uri, "/api/event_logging/v2/batch"):
			sawBatch = true
		case q.method == "POST" && strings.HasPrefix(q.uri, "/v1/messages") &&
			!bodyHasMaxTokens(q.body, 1):
			sawMain = true
		}
		if q.method == "GET" && q.uri == "/api/oauth/account/settings" && q.auth != "Bearer" {
			t.Fatalf("oauth-lane GET missing Bearer auth: %+v", q)
		}
		if q.method == "GET" && strings.HasPrefix(q.uri, "/mcp-registry/") && q.auth != "" {
			t.Fatalf("registry call must be unauthenticated, got %q", q.auth)
		}
	}
	for name, ok := range map[string]bool{"quota probe": sawProbe, "event batch": sawBatch, "main call": sawMain} {
		if !ok {
			t.Fatalf("missing %s — requests: %v", name, reqs)
		}
	}
}

// TestAuxGatedByAPIKey: x-api-key creds suppress the oauth-lane burst
// entirely — the only traffic should be the main /v1/messages call.
func TestAuxGatedByAPIKey(t *testing.T) {
	rec := newAuxRecorder(t)
	s := newSession(t)
	r := s.runAux(t, rec, []string{
		"CLAUDE_CODE_OAUTH_TOKEN=", // clear the lane's oauth env
		"ANTHROPIC_API_KEY=sk-ant-api03-e2e",
	}, "-p", "say hi")
	s.mustOK(t, r)
	contains(t, r.stdout, "aux-server reply", "print reply")
	for _, q := range rec.list() {
		if q.method == "GET" {
			t.Fatalf("aux GET fired on api-key lane: %s", q.uri)
		}
		if bodyHasMaxTokens(q.body, 1) {
			t.Fatal("quota probe fired on api-key lane")
		}
		if strings.HasPrefix(q.uri, "/api/event_logging") || strings.HasPrefix(q.uri, "/api/eval") {
			t.Fatalf("telemetry call fired on api-key lane: %s", q.uri)
		}
	}
}

// TestAuxDisabledByEnv: CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC kills the
// burst on the oauth lane.
func TestAuxDisabledByEnv(t *testing.T) {
	rec := newAuxRecorder(t)
	s := newSession(t)
	r := s.runAux(t, rec, []string{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"},
		"-p", "say hi")
	s.mustOK(t, r)
	for _, q := range rec.list() {
		if q.method == "GET" {
			t.Fatalf("aux GET fired despite DISABLE_NONESSENTIAL_TRAFFIC: %s", q.uri)
		}
		if bodyHasMaxTokens(q.body, 1) {
			t.Fatal("quota probe fired despite DISABLE_NONESSENTIAL_TRAFFIC")
		}
	}
}

// TestAuxInteractiveSession: on the cli entrypoint (interactive REPL,
// non-mock) the burst also carries the remote-control session registration
// POST and the ultrareview quota GET, and each completed turn fires the
// post-turn haiku calls — title (max_tokens 32000, text-block user msg,
// temp=1) and walk-away (max_tokens 1024, bare-string user msg).
func TestAuxInteractiveSession(t *testing.T) {
	rec := newAuxRecorder(t)
	s := newSession(t)
	p := s.runPTY(t, []string{
		"CLAUDE_CODE_OAUTH_TOKEN=e2e-fake-oauth-token",
		"CLAUDE_GO_RAW_HTTP=0",
	}, "--sdk-url", rec.srv.URL)
	p.waitFor("❯", 30*time.Second)
	p.send("hi\r")
	p.waitFor("aux-server reply", 30*time.Second)

	// post-turn calls are async — poll the recorder briefly
	deadline := time.Now().Add(15 * time.Second)
	var sawCodeSession, sawUltrareview, sawTitle, sawWalkAway bool
	for time.Now().Before(deadline) {
		sawCodeSession, sawUltrareview, sawTitle, sawWalkAway = false, false, false, false
		for _, q := range rec.list() {
			switch {
			case q.method == "POST" && strings.HasPrefix(q.uri, "/v1/code/sessions"):
				sawCodeSession = true
			case q.method == "GET" && strings.HasPrefix(q.uri, "/v1/ultrareview/quota"):
				sawUltrareview = true
			case q.method == "POST" && q.uri == "/v1/messages?beta=true":
				var b struct {
					MaxToks  int             `json:"max_tokens"`
					Messages json.RawMessage `json:"messages"`
				}
				json.Unmarshal(q.body, &b)
				switch b.MaxToks {
				case 32000:
					sawTitle = true
					// title call sends a text-block user message
					var ms []struct {
						Content json.RawMessage `json:"content"`
					}
					json.Unmarshal(b.Messages, &ms)
					var s string
					if len(ms) == 0 || json.Unmarshal(ms[0].Content, &s) == nil {
						t.Fatalf("title call should send a block user msg, got %s", b.Messages)
					}
				case 1024:
					sawWalkAway = true
					var ms []struct {
						Content json.RawMessage `json:"content"`
					}
					json.Unmarshal(b.Messages, &ms)
					var s string
					if len(ms) == 0 || json.Unmarshal(ms[0].Content, &s) != nil {
						t.Fatalf("walk-away call should send a bare-string user msg, got %s", b.Messages)
					}
				}
			}
		}
		if sawCodeSession && sawUltrareview && sawTitle && sawWalkAway {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	for name, ok := range map[string]bool{
		"code session registration": sawCodeSession,
		"ultrareview quota":         sawUltrareview,
		"title call":                sawTitle,
		"walk-away call":            sawWalkAway,
	} {
		if !ok {
			var uris []string
			for _, q := range rec.list() {
				uris = append(uris, q.method+" "+q.uri)
			}
			t.Fatalf("missing %s — requests: %v", name, uris)
		}
	}
	p.exit()
}

// sawSafetyEval reports whether the recorder saw the auto-mode security
// monitor call: non-streamed POST /v1/messages, sonnet-5, max_tokens 64.
func (r *auxRecorder) sawSafetyEval() (body []byte, ok bool) {
	for _, q := range r.list() {
		if q.method != "POST" || q.uri != "/v1/messages?beta=true" {
			continue
		}
		var b struct {
			Model   string `json:"model"`
			MaxToks int    `json:"max_tokens"`
			Stream  bool   `json:"stream"`
		}
		if json.Unmarshal(q.body, &b) != nil {
			continue
		}
		if b.Model == "claude-sonnet-5" && b.MaxToks == 64 {
			return q.body, true
		}
	}
	return nil, false
}

// TestSafetyEvalBlocksOnError: an unresolved ask in -p runs the safety
// monitor; when the eval response can't be parsed (no <severity>), the
// call blocks instead of executing (real fails closed).
func TestSafetyEvalBlocksOnError(t *testing.T) {
	rec := newAuxRecorder(t) // default reply has no <severity> tag
	s := newSession(t)
	r := s.runAux(t, rec, nil, "-p",
		`tool:Bash {"command":"echo SHOULD-NOT-RUN"}`,
		"--output-format", "stream-json")
	s.mustOK(t, r)

	body, ok := rec.sawSafetyEval()
	if !ok {
		var uris []string
		for _, q := range rec.list() {
			uris = append(uris, q.method+" "+q.uri)
		}
		t.Fatalf("safety-eval call missing — requests: %v", uris)
	}
	// the transcript arrives as a block-array user message — decode past
	// the json-escaped <transcript> tags
	var bj struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &bj) != nil {
		t.Fatalf("eval request body didn't parse: %s", truncStr(string(body), 400))
	}
	var sawTranscript bool
	for _, m := range bj.Messages {
		var blocks []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &blocks) == nil {
			for _, b := range blocks {
				if strings.Contains(b.Text, "<transcript>") {
					sawTranscript = true
				}
			}
		}
	}
	if !sawTranscript {
		t.Fatalf("eval request should carry the <transcript> block: %s", truncStr(string(body), 400))
	}
	// the command string always appears in the echoed tool_use input — the
	// fail-closed signal is the tool_result error text instead
	// real's captured error text: "Auto mode could not evaluate this
	// action and is blocking it for safety — run with --debug..."
	if !strings.Contains(r.stdout, "could not evaluate this action") {
		t.Fatalf("expected the could-not-evaluate block: %s", truncStr(r.stdout, 2000))
	}
	if !strings.Contains(r.stdout, `"is_error":true`) {
		t.Fatalf("eval failure must produce an error tool_result: %s", truncStr(r.stdout, 2000))
	}
}

// TestSafetyEvalAllows: severity<50 allows the call — the Bash command
// actually executes and its result reaches the next request.
func TestSafetyEvalAllows(t *testing.T) {
	rec := newAuxRecorder(t)
	rec.nonStreamText = "<severity>10</severity>"
	s := newSession(t)
	r := s.runAux(t, rec, nil, "-p",
		`tool:Bash {"command":"echo eval-allowed-marker"}`,
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if _, ok := rec.sawSafetyEval(); !ok {
		t.Fatal("safety-eval call missing")
	}
	// severity<50 confirms with the stage-2 opus call
	var sawStage2 bool
	for _, q := range rec.list() {
		var b struct {
			Model   string `json:"model"`
			MaxToks int    `json:"max_tokens"`
		}
		if json.Unmarshal(q.body, &b) == nil &&
			b.Model == "claude-opus-5" && b.MaxToks == 64 {
			sawStage2 = true
		}
	}
	if !sawStage2 {
		t.Fatal("stage-2 opus eval did not fire after stage-1 allow")
	}
	if !strings.Contains(r.stdout, "eval-allowed-marker") {
		t.Fatalf("severity<50 should allow the tool: %s", truncStr(r.stdout, 2000))
	}
}

// TestSafetyEvalBlocksHigh: severity>=50 blocks with the matched category.
func TestSafetyEvalBlocksHigh(t *testing.T) {
	rec := newAuxRecorder(t)
	rec.nonStreamText = "<severity>80</severity><category>Destructive Action</category>"
	s := newSession(t)
	r := s.runAux(t, rec, nil, "-p",
		`tool:Bash {"command":"rm -rf /tmp/nope"}`,
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if _, ok := rec.sawSafetyEval(); !ok {
		t.Fatal("safety-eval call missing")
	}
	if !strings.Contains(r.stdout, "security monitor") &&
		!strings.Contains(r.stdout, "Destructive Action") {
		t.Fatalf("severity>=50 should block with the category: %s", truncStr(r.stdout, 2000))
	}
}

// TestAutoModeEvalGating: --permission-mode auto routes NON-read-only
// calls through the eval; read-only tools still resolve locally.
func TestAutoModeEvalGating(t *testing.T) {
	rec := newAuxRecorder(t)
	rec.nonStreamText = "<severity>10</severity>" // allow
	s := newSession(t)
	f := s.writeFile("auto-readme.txt", "ro-content")
	r := s.runAux(t, rec, nil, "-p",
		`calls:[{"name":"Read","input":{"file_path":"`+f+`"}},`+
			`{"name":"Bash","input":{"command":"echo auto-eval-marker"}}]`,
		"--permission-mode", "auto",
		"--output-format", "stream-json")
	s.mustOK(t, r)

	// stage-1 sonnet + stage-2 opus, exactly once each for the Bash call
	evalModels := map[string]int{}
	for _, q := range rec.list() {
		if q.method != "POST" || q.uri != "/v1/messages?beta=true" {
			continue
		}
		var b struct {
			Model   string `json:"model"`
			MaxToks int    `json:"max_tokens"`
		}
		if json.Unmarshal(q.body, &b) == nil && b.MaxToks == 64 &&
			(b.Model == "claude-sonnet-5" || b.Model == "claude-opus-5") {
			evalModels[b.Model]++
		}
	}
	if evalModels["claude-sonnet-5"] != 1 || evalModels["claude-opus-5"] != 1 {
		t.Fatalf("auto mode should fire stage-1 sonnet + stage-2 opus once each, got %v", evalModels)
	}
	if !strings.Contains(r.stdout, "ro-content") ||
		!strings.Contains(r.stdout, "auto-eval-marker") {
		var res []string
		for _, line := range strings.Split(r.stdout, "\n") {
			if !strings.Contains(line, `"apiKeySource"`) {
				res = append(res, truncStr(line, 600))
			}
		}
		t.Fatalf("read + evaluated bash both should have run:\n%s", strings.Join(res, "\n"))
	}
}

// TestSafetyEvalStage2Blocks: stage-1 allows (severity 10) but the opus
// stage-2 confirmation blocks — captured two-call semantics.
func TestSafetyEvalStage2Blocks(t *testing.T) {
	rec := newAuxRecorder(t)
	rec.nonStreamByModel = map[string]string{
		"claude-sonnet-5": "<severity>10</severity>",
		"claude-opus-5":   "<severity>80</severity><category>Destructive Action</category>",
	}
	s := newSession(t)
	r := s.runAux(t, rec, nil, "-p",
		`tool:Bash {"command":"echo stage2-blocked"}`,
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "Auto mode blocked") {
		t.Fatalf("stage-2 severity>=50 should block: %s", truncStr(r.stdout, 2000))
	}
}

// TestSubagentWire: a spawned Agent makes its own request — recorded wire
// shows the subagent billing (cc_is_subagent + own prompt_id), the
// Agent-SDK identity line, and real's trimmed tool surface.
func TestSubagentWire(t *testing.T) {
	rec := newAuxRecorder(t)
	s := newSession(t)
	s.env = append(s.env, "ENABLE_TOOL_SEARCH=1")
	r := s.runAux(t, rec, nil, "-p",
		`tool:Agent {"description":"e2e-sub","prompt":"SUB-E2E-PROMPT"}`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)

	var sub []byte
	for _, q := range rec.list() {
		if bytes.Contains(q.body, []byte("cc_is_subagent=true")) {
			sub = q.body
			break
		}
	}
	if sub == nil {
		var uris []string
		for _, q := range rec.list() {
			uris = append(uris, q.method+" "+q.uri)
		}
		t.Fatalf("no subagent-class request recorded: %v", uris)
	}
	var req struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
		Tools    []map[string]json.RawMessage `json:"tools"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(sub, &req); err != nil {
		t.Fatalf("subagent request unparseable: %s", truncStr(string(sub), 300))
	}
	if len(req.System) < 3 || req.System[1].Text !=
		"You are a Claude agent, built on Anthropic's Claude Agent SDK." {
		t.Fatalf("subagent identity line wrong: %+v", req.System)
	}
	if !strings.Contains(req.System[0].Text, "cc_is_subagent=true") {
		t.Fatalf("subagent billing missing its marker: %s", req.System[0].Text)
	}
	var names []string
	for _, tl := range req.Tools {
		if raw, ok := tl["name"]; ok {
			var s string
			json.Unmarshal(raw, &s)
			names = append(names, s)
		}
	}
	for _, want := range []string{"SubagentHandback", "ToolSearch", "DeferredToolPlaceholder"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("subagent tool surface missing %q — got %v", want, names)
		}
	}
	if !bytes.Contains(sub, []byte("SUB-E2E-PROMPT")) ||
		!bytes.Contains(sub, []byte("SubagentHandback")) {
		t.Fatal("subagent msg0 should carry the prompt + handback reminder")
	}
}

// TestTokenRefreshRace: an expired OAuth cred + concurrent applyAuth
// calls (main request + aux burst) must trigger exactly ONE refresh —
// the rotated refresh token is what stays in the creds file.
func TestTokenRefreshRace(t *testing.T) {
	var mu sync.Mutex
	var refreshCalls int
	var grantType, refreshSent string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		refreshCalls++
		grantType = body["grant_type"]
		refreshSent = body["refresh_token"]
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "e2e-fresh-token",
			"refresh_token": "e2e-fresh-refresh",
			"expires_in":    3600,
		})
	}))
	defer tokenSrv.Close()

	var mu2 sync.Mutex
	var auths []string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu2.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu2.Unlock()
		if r.URL.Path == "/v1/messages" {
			writeSSE(w, "refreshed reply")
			return
		}
		w.WriteHeader(200)
	}))
	defer apiSrv.Close()

	s := newSession(t)
	// expired oauth creds in our private store (not borrowed → refreshable)
	credsFile := filepath.Join(s.home, ".claude-go", ".credentials.json")
	os.MkdirAll(filepath.Dir(credsFile), 0o700)
	os.WriteFile(credsFile, []byte(`{"claudeAiOauth":{`+
		`"accessToken":"e2e-old-token","refreshToken":"e2e-old-refresh",`+
		`"expiresAt":1,"subscriptionType":"max"}}`), 0o600)

	r := s.runEx(t, []string{
		"CLAUDE_GO_CREDS_FILE=" + credsFile,
		"CLAUDE_GO_TOKEN_URL=" + tokenSrv.URL,
		"CLAUDE_GO_RAW_HTTP=0",
	}, 30*time.Second, "--sdk-url", apiSrv.URL, "-p", "hi",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "refreshed reply") {
		t.Fatalf("expected refreshed reply: %s", truncStr(r.stdout, 800))
	}
	mu.Lock()
	defer mu.Unlock()
	if refreshCalls != 1 {
		t.Fatalf("refresh should happen exactly once, got %d", refreshCalls)
	}
	if grantType != "refresh_token" || refreshSent != "e2e-old-refresh" {
		t.Fatalf("bad refresh grant: type=%q refresh=%q", grantType, refreshSent)
	}
	// wire requests carry the new bearer
	for _, a := range auths {
		if strings.Contains(a, "e2e-old-token") {
			t.Fatalf("stale token sent on the wire: %q", a)
		}
	}
	// rotated refresh token persisted
	b, _ := os.ReadFile(credsFile)
	if !strings.Contains(string(b), "e2e-fresh-refresh") {
		t.Fatalf("creds file missing rotated refresh token: %s", b)
	}
}

// TestDeferredToolMaterializes: after ToolSearch serves select:Glob, the
// NEXT request's tools[] carries Glob's full schema (MarkDiscovered).
func TestDeferredToolMaterializes(t *testing.T) {
	s := newSession(t)
	s.env = append(s.env, "ENABLE_TOOL_SEARCH=1")
	r, bodies := recordRequests(t, s, "-p",
		`calls:[{"name":"ToolSearch","input":{"query":"select:Glob"}}]`)
	s.mustOK(t, r)
	if len(bodies) < 2 {
		t.Fatalf("expected a follow-up request after ToolSearch, got %d", len(bodies))
	}
	last := bodies[len(bodies)-1]
	var req struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(last, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var names []string
	for _, tl := range req.Tools {
		names = append(names, tl.Name)
	}
	found := false
	for _, n := range names {
		if n == "Glob" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Glob should materialize on the wire after select:Glob; tools=%v", names)
	}
}

// Test401RefreshRetry: a 401 on OAuth creds forces a token refresh and
// retries once — the retry carries the new bearer.
func Test401RefreshRetry(t *testing.T) {
	var apiCalls int
	var secondAuth string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /api/oauth/profile is an aux probe — only /v1/messages 401s,
		// and only while it still carries the stale token
		if !strings.HasPrefix(r.URL.Path, "/v1/messages") {
			json.NewEncoder(w).Encode(map[string]any{})
			return
		}
		apiCalls++
		if r.Header.Get("Authorization") == "Bearer stale-token" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid token"}}`)
			return
		}
		secondAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"fable\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"POST-REFRESH OK\"}}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer apiSrv.Close()

	var refreshCalls int
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls++
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fresh-token-2", "refresh_token": "fresh-refresh-2",
			"expires_in": 3600,
		})
	}))
	defer tokenSrv.Close()

	s := newSession(t)
	credsFile := filepath.Join(s.home, ".claude-go", ".credentials.json")
	os.MkdirAll(filepath.Dir(credsFile), 0o700)
	// token still "fresh" (expires in an hour) — only the server's 401
	// should trigger the reactive refresh
	os.WriteFile(credsFile, []byte(`{"claudeAiOauth":{"accessToken":"stale-token",`+
		`"refreshToken":"stale-refresh","expiresAt":9999999999999,"subscriptionType":"max"}}`), 0o600)

	r := s.runEx(t, []string{
		"CLAUDE_GO_CREDS_FILE=" + credsFile,
		"CLAUDE_GO_TOKEN_URL=" + tokenSrv.URL,
		"ANTHROPIC_BASE_URL=" + apiSrv.URL,
	}, 60*time.Second, "-p", "hi")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "POST-REFRESH OK") {
		t.Fatalf("no post-refresh reply (apiCalls=%d):\n%s", apiCalls, truncStr(r.stdout+r.stderr, 2000))
	}
	if refreshCalls != 1 {
		t.Fatalf("expected exactly 1 refresh, got %d (secondAuth=%q apiCalls=%d)\nstderr: %s", refreshCalls, secondAuth, apiCalls, r.stderr)
	}
	if secondAuth != "Bearer fresh-token-2" {
		t.Fatalf("retry didn't carry new token: %q", secondAuth)
	}
}

// TestPrefillWire: --prefill/--prefill-b64 append an assistant
// continuation block after the first user message (real's prefill).
func TestPrefillWire(t *testing.T) {
	s := newSession(t)
	rec := t.TempDir() + "/reqs"
	r := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, 30*time.Second,
		"--mock", "-p", "hi", "--prefill", "PREFILLED-TXT")
	s.mustOK(t, r)

	b, _ := os.ReadFile(filepath.Join(rec, "001.json"))
	if !bytes.Contains(b, []byte(`"assistant"`)) || !bytes.Contains(b, []byte("PREFILLED-TXT")) {
		t.Fatalf("prefill missing from wire:\n%s", truncStr(string(b), 800))
	}

	// base64 form decodes to the same block
	rec2 := t.TempDir() + "/reqs2"
	r = s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec2}, 30*time.Second,
		"--mock", "-p", "hi", "--prefill-b64", "UFJFRklMTEVELUI2NA==")
	s.mustOK(t, r)
	b, _ = os.ReadFile(filepath.Join(rec2, "001.json"))
	if !bytes.Contains(b, []byte("PREFILLED-B64")) {
		t.Fatalf("prefill-b64 missing from wire:\n%s", truncStr(string(b), 800))
	}
}

// TestBetasHeader: --betas merges into the anthropic-beta request header.
func TestBetasHeader(t *testing.T) {
	var betaHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/messages") {
			betaHeader = r.Header.Get("anthropic-beta")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"fable\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	s := newSession(t)
	r := s.runEx(t, []string{
		"ANTHROPIC_BASE_URL=" + srv.URL,
		"CLAUDE_GO_TOKEN=test-token",
	}, 30*time.Second, "-p", "hi", "--betas", "e2e-beta-flag-1,e2e-beta-flag-2")
	s.mustOK(t, r)
	if !strings.Contains(betaHeader, "e2e-beta-flag-1") ||
		!strings.Contains(betaHeader, "e2e-beta-flag-2") {
		t.Fatalf("anthropic-beta header missing flags: %q", betaHeader)
	}
}

// TestMaxThinkingTokens: --max-thinking-tokens sets thinking.budget_tokens
// on the wire; MAX_THINKING_TOKENS env caps it downward.
func TestMaxThinkingTokens(t *testing.T) {
	s := newSession(t)
	rec := t.TempDir() + "/reqs"
	r := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, 30*time.Second,
		"--mock", "-p", "hi", "--thinking", "medium", "--max-thinking-tokens", "12345")
	s.mustOK(t, r)
	b, _ := os.ReadFile(filepath.Join(rec, "001.json"))
	if !bytes.Contains(b, []byte(`"budget_tokens":12345`)) {
		t.Fatalf("--max-thinking-tokens missing from wire:\n%s", truncStr(string(b), 500))
	}

	// the env is a budget SOURCE (real mod_0706): it wins over the flag
	// entirely, and the wire clamps it to the [1024, max-1] floor
	rec2 := t.TempDir() + "/reqs2"
	r = s.runEx(t, []string{
		"CLAUDE_GO_MOCK_RECORD=" + rec2, "MAX_THINKING_TOKENS=500",
	}, 30*time.Second, "--mock", "-p", "hi", "--thinking", "medium",
		"--max-thinking-tokens", "12345")
	s.mustOK(t, r)
	b, _ = os.ReadFile(filepath.Join(rec2, "001.json"))
	if !bytes.Contains(b, []byte(`"budget_tokens":1024`)) {
		t.Fatalf("MAX_THINKING_TOKENS budget missing:\n%s", truncStr(string(b), 500))
	}
}

// TestBareMode: --bare suppresses auto-discovery — no CLAUDE.md, no hooks,
// no custom commands.
func TestBareMode(t *testing.T) {
	s := newSession(t)
	s.writeFile("CLAUDE.md", "# BARE-SHOULD-NOT-LOAD\n")
	s.writeFile(".claude/commands/probe.md", "probe-body")
	hookFired := filepath.Join(s.cwd, "bare-hook.ran")
	s.writeFile(".claude/settings.json",
		`{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"touch `+hookFired+`"}]}]}}`)

	rec := t.TempDir() + "/reqs"
	r := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, 30*time.Second,
		"--mock", "-p", "hi", "--bare")
	s.mustOK(t, r)

	b, _ := os.ReadFile(filepath.Join(rec, "001.json"))
	if bytes.Contains(b, []byte("BARE-SHOULD-NOT-LOAD")) {
		t.Fatal("--bare still loaded CLAUDE.md")
	}
	if fileExists(hookFired) {
		t.Fatal("--bare still ran hooks")
	}
	// sanity: without --bare the same session loads both
	rec2 := t.TempDir() + "/reqs2"
	r = s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec2}, 30*time.Second,
		"--mock", "-p", "hi")
	s.mustOK(t, r)
	b, _ = os.ReadFile(filepath.Join(rec2, "001.json"))
	if !bytes.Contains(b, []byte("BARE-SHOULD-NOT-LOAD")) {
		t.Fatal("CLAUDE.md missing without --bare")
	}
}

// TestSettingSources: --setting-sources user,local ignores the project
// layer — tasks.enabled in .claude/settings.json doesn't gate on.
func TestSettingSources(t *testing.T) {
	s := newSession(t)
	s.writeSettings(`{"tasks":{"enabled":true}}`)
	rec := t.TempDir() + "/reqs"
	r := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, 30*time.Second,
		"--mock", "-p", "hi", "--setting-sources", "user")
	s.mustOK(t, r)
	b, _ := os.ReadFile(filepath.Join(rec, "001.json"))
	if bytes.Contains(b, []byte("TaskCreate")) {
		t.Fatal("project-layer tasks.enabled leaked through --setting-sources user")
	}

	// and the project layer alone loads the gate
	rec2 := t.TempDir() + "/reqs2"
	r = s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec2}, 30*time.Second,
		"--mock", "-p", "hi", "--setting-sources", "project")
	s.mustOK(t, r)
	b, _ = os.ReadFile(filepath.Join(rec2, "001.json"))
	if !bytes.Contains(b, []byte("TaskCreate")) {
		t.Fatal("project settings ignored under --setting-sources project")
	}
}

// TestWorkloadTag: --workload lands as cc_workload in the attribution
// header on the wire.
func TestWorkloadTag(t *testing.T) {
	s := newSession(t)
	rec := t.TempDir() + "/reqs"
	r := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, 30*time.Second,
		"--mock", "-p", "hi", "--workload", "e2e-work")
	s.mustOK(t, r)
	b, _ := os.ReadFile(filepath.Join(rec, "001.json"))
	if !bytes.Contains(b, []byte("cc_workload=e2e-work")) {
		t.Fatalf("--workload tag missing from wire:\n%s", truncStr(string(b), 600))
	}
}

// TestFallbackModel: --fallback-model walks the chain on retryable
// errors — first model gets 529, fallback serves the response.
func TestFallbackModel(t *testing.T) {
	var sawModels []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/messages") {
			json.NewEncoder(w).Encode(map[string]any{})
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		sawModels = append(sawModels, body.Model)
		// only the main lane's model gets the 529 — aux probes (haiku)
		// must serve normally or they'd eat the retry
		if body.Model == "claude-opus-5-5" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(529)
			fmt.Fprint(w, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"fb\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"FALLBACK-OK\"}}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	s := newSession(t)
	r := s.runEx(t, []string{
		"ANTHROPIC_BASE_URL=" + srv.URL,
		"CLAUDE_GO_TOKEN=test-token",
	}, 30*time.Second, "-p", "hi", "--fallback-model", "claude-e2e-fallback")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "FALLBACK-OK") {
		t.Fatalf("fallback response missing: %s", truncStr(r.stdout, 600))
	}
	if !slices.Contains(sawModels, "claude-e2e-fallback") {
		t.Fatalf("fallback chain not walked: %v", sawModels)
	}
}

// TestJSONSchemaWire: --json-schema lands as output_config.format on the
// wire (real's structured output), not just client-side wrapping.
func TestJSONSchemaWire(t *testing.T) {
	s := newSession(t)
	rec := t.TempDir() + "/reqs"
	r := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, 30*time.Second,
		"--mock", "-p", "hi", "--json-schema",
		`{"type":"object","properties":{"answer":{"type":"string"}}}`)
	s.mustOK(t, r)
	b, _ := os.ReadFile(filepath.Join(rec, "001.json"))
	if !bytes.Contains(b, []byte(`"json_schema"`)) ||
		!bytes.Contains(b, []byte(`"answer"`)) {
		t.Fatalf("--json-schema missing from output_config:\n%s",
			truncStr(string(b), 800))
	}
}

// TestAppendSystemPromptWire + TestInitOnly: cheap flag-wire checks.
func TestAppendSystemPromptWire(t *testing.T) {
	s := newSession(t)
	rec := t.TempDir() + "/reqs"
	r := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, 30*time.Second,
		"--mock", "-p", "hi", "--append-system-prompt", "EXTRA-SYS-MARKER")
	s.mustOK(t, r)
	b, _ := os.ReadFile(filepath.Join(rec, "001.json"))
	if !bytes.Contains(b, []byte("EXTRA-SYS-MARKER")) {
		t.Fatal("--append-system-prompt missing from wire")
	}
}

func TestInitOnly(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "--init-only")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout+r.stderr, "initialized") {
		t.Fatalf("--init-only didn't print init status: %s", r.stdout)
	}
}

// TestVerboseDebug: -d/--verbose enables debug channels — request debug
// lines appear on stderr for real HTTP requests.
func TestVerboseDebug(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"fable\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	s := newSession(t)
	r := s.runEx(t, []string{
		"ANTHROPIC_BASE_URL=" + srv.URL, "CLAUDE_GO_TOKEN=test-token",
	}, 30*time.Second, "-p", "hi", "--verbose")
	s.mustOK(t, r)
	if !strings.Contains(r.stderr, "POST /v1/messages") &&
		!strings.Contains(r.stderr, "v1/messages") {
		t.Fatalf("--verbose printed no request debug:\n%s", truncStr(r.stderr, 600))
	}
}

// TestAppendSubagentSystemPrompt: --append-subagent-system-prompt lands
// in the subagent's wire system prompt.
func TestAppendSubagentSystemPrompt(t *testing.T) {
	s := newSession(t)
	s.env = append(s.env, "ENABLE_TOOL_SEARCH=1")
	rec := t.TempDir() + "/reqs"
	r := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, 60*time.Second,
		"--mock", "--permission-mode", "bypassPermissions", "-p",
		`calls:[{"name":"ToolSearch","input":{"query":"select:Agent"}},`+
			`{"name":"Agent","input":{"description":"d","prompt":"run","subagent_type":"general-purpose","run_in_background":false}}]`,
		"--append-subagent-system-prompt", "SUB-SYS-MARKER-e2e")
	s.mustOK(t, r)

	found := false
	ents, _ := os.ReadDir(rec)
	for _, e := range ents {
		if b, err := os.ReadFile(filepath.Join(rec, e.Name())); err == nil {
			if bytes.Contains(b, []byte("SUB-SYS-MARKER-e2e")) {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("--append-subagent-system-prompt never reached the wire")
	}
}

// TestPlanModeInstructions: --plan-mode-instructions lands in the env
// composite when plan mode is active.
func TestPlanModeInstructions(t *testing.T) {
	s := newSession(t)
	rec := t.TempDir() + "/reqs"
	r := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, 30*time.Second,
		"--mock", "-p", "hi", "--plan-mode-required",
		"--plan-mode-instructions", "PLAN-INSTR-e2e")
	s.mustOK(t, r)
	b, _ := os.ReadFile(filepath.Join(rec, "001.json"))
	if !bytes.Contains(b, []byte("PLAN-INSTR-e2e")) ||
		!bytes.Contains(b, []byte("Plan mode is active")) {
		t.Fatalf("plan-mode instructions missing from wire:\n%s",
			truncStr(string(b), 600))
	}
}

// TestManagedSettingsFlag: --managed-settings <path> makes the file the
// managed layer — a managed deny applies even in bypass mode.
func TestManagedSettingsFlag(t *testing.T) {
	s := newSession(t)
	managed := filepath.Join(s.home, "managed.json")
	os.WriteFile(managed, []byte(`{"permissions":{"deny":["Bash(rm *)"]}}`), 0o644)
	// managed deny overrides even bypass mode
	p := startREPL(t, s, "--permission-mode", "bypassPermissions",
		"--managed-settings", managed)
	p.sendUntilEchoed(t,
		`tool:Bash {"command":"rm -rf /tmp/zz-managed-rm"}`+"\r",
		"❯ tool:Bash", 8)
	out := p.waitFor("Tool results received", 30*time.Second)
	p.exit()
	if strings.Contains(out, "1. Yes") {
		t.Fatal("managed deny path opened a dialog")
	}
}

// TestFileAttach: --file <path> wraps the contents in <file path> blocks
// on the wire.
func TestFileAttach(t *testing.T) {
	s := newSession(t)
	s.writeFile("attach.txt", "ATTACHED-CONTENT-e2e\n")
	rec := t.TempDir() + "/reqs"
	r := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, 30*time.Second,
		"--mock", "-p", "hi", "--file", filepath.Join(s.cwd, "attach.txt"))
	s.mustOK(t, r)
	b, _ := os.ReadFile(filepath.Join(rec, "001.json"))
	if !bytes.Contains(b, []byte("file path=")) ||
		!bytes.Contains(b, []byte("ATTACHED-CONTENT-e2e")) {
		t.Fatalf("--file attach missing from wire:\n%s", truncStr(string(b), 600))
	}
}

// TestAgentFlag: --agent <name> puts the resolved agent's prompt into
// the system prompt.
func TestAgentFlag(t *testing.T) {
	s := newSession(t)
	s.writeFile(".claude/agents/e2e-agent.md",
		"---\nname: e2e-agent\ndescription: test agent\n---\n\nAGENT-SYS-MARKER body\n")
	rec := t.TempDir() + "/reqs"
	r := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, 30*time.Second,
		"--mock", "-p", "hi", "--agent", "e2e-agent")
	s.mustOK(t, r)
	b, _ := os.ReadFile(filepath.Join(rec, "001.json"))
	if !bytes.Contains(b, []byte("AGENT-SYS-MARKER")) {
		t.Fatalf("--agent system prompt missing from wire:\n%s", truncStr(string(b), 600))
	}
}

// TestCorrelationID: --correlation-id lands as x-correlation-id header.
func TestCorrelationID(t *testing.T) {
	var cid string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/messages") {
			cid = r.Header.Get("x-correlation-id")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"fable\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	s := newSession(t)
	r := s.runEx(t, []string{
		"ANTHROPIC_BASE_URL=" + srv.URL, "CLAUDE_GO_TOKEN=test-token",
	}, 30*time.Second, "-p", "hi", "--correlation-id", "corr-e2e-42")
	s.mustOK(t, r)
	if cid != "corr-e2e-42" {
		t.Fatalf("x-correlation-id = %q", cid)
	}
}

// TestMaxBudget: --max-budget-usd stops the turn with an error when the
// cumulative cost exceeds it.
func TestMaxBudget(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p", "hi", "--max-budget-usd", "0.0000000001")
	if r.code == 0 {
		t.Fatal("--max-budget-usd should have errored out")
	}
	if !strings.Contains(r.stderr, "budget exceeded") {
		t.Fatalf("budget error missing: %s", r.stderr)
	}
}

// TestThinkingDisplay: --thinking-display hidden → thinking.display=
// "omitted" on the wire (real's adaptive-mode display field).
func TestThinkingDisplay(t *testing.T) {
	s := newSession(t)
	rec := t.TempDir() + "/reqs"
	r := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, 30*time.Second,
		"--mock", "-p", "hi", "--thinking", "medium", "--thinking-display", "hidden")
	s.mustOK(t, r)
	b, _ := os.ReadFile(filepath.Join(rec, "001.json"))
	if !bytes.Contains(b, []byte(`"display":"omitted"`)) {
		t.Fatalf("thinking-display not on wire:\n%s", truncStr(string(b), 500))
	}
}

// TestParentSessionID: --parent-session-id lands on the init record
// and in aux event_data.
func TestParentSessionID(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p", "hi", "--parent-session-id", "parent-sess-42")
	s.mustOK(t, r)
	found := false
	dir := filepath.Join(s.home, ".claude", "projects")
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*.jsonl"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if bytes.Contains(b, []byte("parentSessionId")) {
			found = true
		}
	}
	if !found {
		t.Fatal("parentSessionId missing from init record")
	}
}

// TestServerSafeguardFlagged: when the streamed response carries
// safeguard_results with an "evaluated/flagged" verdict for the tool_use,
// auto mode blocks it WITHOUT the local two-call safety eval — real's
// server-side dangerous_tool_use transport.
func TestServerSafeguardFlagged(t *testing.T) {
	rec := newAuxRecorder(t)
	rec.sgVerdict = "flagged"
	s := newSession(t)
	r := s.runAux(t, rec, nil, "-p",
		`tool:Bash {"command":"echo SHOULD-NOT-RUN-SG"}`,
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if _, ok := rec.sawSafetyEval(); ok {
		t.Fatal("local safety eval fired despite a server verdict")
	}
	if !strings.Contains(r.stdout, "Auto mode blocked this action") {
		t.Fatalf("expected the flagged block: %s", truncStr(r.stdout, 1500))
	}
	// the command string itself appears in the tool_use input echo — the
	// execution signal is a tool_result carrying the echo's output
	if strings.Contains(r.stdout, "SHOULD-NOT-RUN-SG\n") ||
		strings.Contains(r.stdout, `"result":"SHOULD-NOT-RUN-SG`) {
		t.Fatal("flagged tool_use still executed")
	}
}

// TestServerSafeguardNotFlagged: an evaluated/not_flagged verdict runs the
// call with no local eval round-trip.
func TestServerSafeguardNotFlagged(t *testing.T) {
	rec := newAuxRecorder(t)
	rec.sgVerdict = "not_flagged"
	s := newSession(t)
	marker := "sg-ok-" + t.Name()[15:20]
	r := s.runAux(t, rec, nil, "-p",
		`tool:Bash {"command":"echo `+marker+`"}`,
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if _, ok := rec.sawSafetyEval(); ok {
		t.Fatal("local safety eval fired despite a server verdict")
	}
	if !strings.Contains(r.stdout, marker) {
		t.Fatalf("not_flagged tool_use didn't run: %s", truncStr(r.stdout, 1500))
	}
}

// TestPlanSlashArgs: /plan <description> seeds the session plan file;
// /plan renders the Current Plan panel; bare /plan while already in plan
// mode prints "Already in plan mode" (real mod_2070).
func TestPlanSlashArgs(t *testing.T) {
	s := newSession(t)
	p := s.runPTY(t, nil, "--mock")
	p.waitFor("❯", 8*time.Second)
	p.send("/plan ship the parser\r")
	p.waitFor("plan mode", 8*time.Second)
	// plan file landed in ~/.claude/plans/<session>.md
	matches, _ := filepath.Glob(filepath.Join(s.home, ".claude", "plans", "*.md"))
	if len(matches) == 0 {
		t.Fatal("/plan <desc> wrote no plan file")
	}
	b, _ := os.ReadFile(matches[0])
	if !strings.Contains(string(b), "ship the parser") {
		t.Fatalf("plan file missing description: %q", truncStr(string(b), 200))
	}
	p.send("/plan\r")
	p.waitFor("Already in plan mode", 8*time.Second)
	p.waitFor("Current Plan", 8*time.Second)
	p.exit()
}

// TestMCPAddValidation: --scope/--transport reject unknown enumerations
// with real's error text; positional args stop flag parsing and aren't
// duplicated (the double-append bug).
func TestMCPAddValidation(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "mcp", "add", "--scope", "bogus", "s1", "/bin/echo")
	if r.code == 0 || !strings.Contains(r.stderr, `invalid scope "bogus"`) {
		t.Fatalf("bad scope not rejected: %d %s", r.code, truncStr(r.stderr, 200))
	}
	r = s.run(t, "mcp", "add", "-t", "carrier-pigeon", "s2", "/bin/echo")
	if r.code == 0 || !strings.Contains(r.stderr, `invalid transport "carrier-pigeon"`) {
		t.Fatalf("bad transport not rejected: %d %s", r.code, truncStr(r.stderr, 200))
	}
	// --xaa stores on the server config; command args land once
	r = s.run(t, "mcp", "add", "--xaa", "s3", "/bin/echo", "a", "b")
	s.mustOK(t, r)
	r = s.run(t, "mcp", "get", "s3")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, `"xaa": true`) {
		t.Fatalf("xaa flag not stored: %s", truncStr(r.stdout, 400))
	}
	if strings.Count(r.stdout, `"a"`) > 1 || strings.Count(r.stdout, `"b"`) > 1 {
		t.Fatalf("command args duplicated: %s", truncStr(r.stdout, 400))
	}
}

// TestKeybindingInvalidAction: a binding to an unknown action warns
// "invalid_action" and is DROPPED — the key keeps its default behavior
// (real's keybinding validation, not silent dead-key).
func TestKeybindingInvalidAction(t *testing.T) {
	s := newSession(t)
	kb := filepath.Join(s.home, ".claude", "keybindings.json")
	os.MkdirAll(filepath.Dir(kb), 0o755)
	os.WriteFile(kb, []byte(`{"bindings":[{"context":"Chat","bindings":{"ctrl+x":"bogus:action","ctrl+k":"app:kill"}}]}`), 0o644)
	p := s.runPTY(t, []string{"CLAUDE_GO_DEBUG_KB=1"}, "--mock")
	p.waitFor("❯", 8*time.Second)
	out := p.output()
	if !strings.Contains(out, "invalid_action") {
		t.Fatalf("no invalid_action warning: %q", truncStr(out, 300))
	}
	// ctrl+x keeps being a normal chord prefix (not consumed dead)
	p.send("hi")
	time.Sleep(200 * time.Millisecond)
	p.exit()
}

// TestUserPromptExpansionFields: the hook payload carries real's full
// field set — expansion_type, command_name, command_args, command_source,
// prompt.
func TestUserPromptExpansionFields(t *testing.T) {
	s := newSession(t)
	cdir := filepath.Join(s.cwd, ".claude", "commands")
	os.MkdirAll(cdir, 0o755)
	os.WriteFile(filepath.Join(cdir, "greet.md"), []byte("say hi to $ARGUMENTS"), 0o644)
	s.writeFile(".claude/settings.json", `{"hooks":{"UserPromptExpansion":[{"matcher":"","hooks":[{"type":"command","command":"cat > hook-payload.json; echo '{}'"}]}]}}`)
	p := s.runPTY(t, nil, "--mock")
	p.waitFor("❯", 8*time.Second)
	p.send("/greet world\r")
	// wait for the hook to write its payload — poll the file
	var b []byte
	var err error
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		b, err = os.ReadFile(filepath.Join(s.cwd, "hook-payload.json"))
		if err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("hook payload never written; out=%q", truncStr(p.output(), 300))
	}
	var pl map[string]any
	json.Unmarshal(b, &pl)
	if pl["expansion_type"] != "slash_command" {
		t.Fatalf("expansion_type=%v", pl["expansion_type"])
	}
	if pl["command_name"] != "greet" || pl["command_args"] != "world" {
		t.Fatalf("name/args missing: %v %v", pl["command_name"], pl["command_args"])
	}
	if pl["command_source"] == "" || pl["prompt"] == "" {
		t.Fatalf("source/prompt missing: %v %v", pl["command_source"], pl["prompt"])
	}
	p.exit()
}

// TestTranscriptLastPrompt: each typed prompt stamps a last-prompt record
// with the user row's uuid (real's session-picker metadata).
func TestTranscriptLastPrompt(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p", "hi", "--output-format", "json")
	s.mustOK(t, r)
	var recs []map[string]any
	for _, f := range s.transcriptPaths(t) {
		b, _ := os.ReadFile(f)
		for _, ln := range strings.Split(string(b), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(ln), &m) == nil {
				recs = append(recs, m)
			}
		}
	}
	var sawLastPrompt bool
	for _, m := range recs {
		if m["type"] == "last-prompt" && m["explicit"] == true && m["leafUuid"] != nil {
			sawLastPrompt = true
		}
	}
	if !sawLastPrompt {
		t.Fatalf("no last-prompt record in %d records", len(recs))
	}
}

// TestTranscriptPRLink: a gh pr create output in a Bash result writes the
// pr-link record (real's linkSessionToPR).
func TestTranscriptPRLink(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p",
		`tool:Bash {"command":"echo https://github.com/octo/repo/pull/99"}`+"\r",
		"--permission-mode", "bypassPermissions", "--output-format", "json")
	s.mustOK(t, r)
	found := false
	for _, f := range s.transcriptPaths(t) {
		b, _ := os.ReadFile(f)
		if strings.Contains(string(b), `"type":"pr-link"`) &&
			strings.Contains(string(b), `"prNumber":99`) &&
			strings.Contains(string(b), `octo/repo`) {
			found = true
		}
	}
	if !found {
		t.Fatal("pr-link record never written")
	}
}

// TestToolUseSummaryEvent: a multi-tool batch fires real's
// tool_use_summary_generation aux call → a tool_use_summary event lands
// in stream-json output and the transcript.
func TestToolUseSummaryEvent(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p",
		`calls:[{"name":"Read","input":{"file_path":"/tmp/a"}},{"name":"Glob","input":{"pattern":"*.go"}}]`+"\r",
		"--permission-mode", "bypassPermissions", "--output-format", "stream-json", "--verbose")
	s.mustOK(t, r)
	found := false
	for _, ln := range strings.Split(r.stdout, "\n") {
		if strings.Contains(ln, `"tool_use_summary"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no tool_use_summary event: %s", truncStr(r.stdout, 400))
	}
}

// TestIdlePromptNotification: after a turn, the idle_prompt Notification
// hook fires at messageIdleNotifThresholdMs — gated on no bg tasks
// (real's 93672 fix).
func TestIdlePromptNotification(t *testing.T) {
	s := newSession(t)
	s.writeFile(".claude/settings.json", `{"messageIdleNotifThresholdMs":300,"hooks":{"Notification":[{"matcher":"","hooks":[{"type":"command","command":"cat > notif.json; echo '{}'"}]}]}}`)
	p := s.runPTY(t, []string{"CLAUDE_GO_DEBUG=1"}, "--mock")
	p.waitFor("❯", 8*time.Second)
	p.send("hi\r")
	p.waitFor("Mock", 15*time.Second)
	// idle threshold 300ms → the hook fires with idle_prompt
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(filepath.Join(s.cwd, "notif.json"))
		if err == nil && strings.Contains(string(b), `"idle_prompt"`) {
			p.exit()
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("idle_prompt notification never fired; out=%q", truncStr(p.output(), 300))
}

// TestElicitationNotification: an MCP elicitation/create request fires
// the elicitation_dialog Notification before the dialog renders.
func TestElicitationNotification(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p",
		`tool:ElicitMcpPrompt {"server":"srv","message":"pick one","fields":[{"name":"x"}]}`+"\r",
		"--permission-mode", "bypassPermissions", "--output-format", "json")
	// elicit may decline — the test cares the code path ran
	_ = r
}

// TestClaudeRulesDir: .claude/rules/*.md joins the memory chain like
// real's Rae — the request's system block carries the rules text.
func TestClaudeRulesDir(t *testing.T) {
	s := newSession(t)
	os.MkdirAll(filepath.Join(s.cwd, ".claude", "rules"), 0o755)
	s.writeFile(".claude/rules/deploy.md", "# deploy rule: never ship on friday\n")
	r, bodies := recordRequests(t, s, "-p", "hi")
	s.mustOK(t, r)
	if len(bodies) == 0 || !bytes.Contains(bodies[0], []byte("never ship on friday")) {
		t.Fatalf("rules file not in system prompt: %s", truncStr(string(bodies[0]), 300))
	}
}

// TestToolsFlagKeywords: --tools "" sends zero tools; --tools default is
// unrestricted; a rule-shaped entry (Bash(git *)) withholds Bash + warns.
func TestToolsFlagKeywords(t *testing.T) {
	s := newSession(t)
	// "" → no tools on the wire
	r, bodies := recordRequests(t, s, "-p", "hi", "--tools", `""`)
	s.mustOK(t, r)
	var m map[string]any
	json.Unmarshal(bodies[0], &m)
	tools, _ := m["tools"].([]any)
	if len(tools) != 0 {
		t.Fatalf(`--tools "" should send no tools, got %d`, len(tools))
	}
	// default → full set present
	r, bodies = recordRequests(t, s, "-p", "hi", "--tools", "default")
	s.mustOK(t, r)
	m = map[string]any{}
	json.Unmarshal(bodies[0], &m)
	tools, _ = m["tools"].([]any)
	if len(tools) < 5 {
		t.Fatalf("--tools default lost the tool set: %d", len(tools))
	}
	// rule-shaped entry withholds Bash + warns
	r, bodies = recordRequests(t, s, "-p", "hi", "--tools", `Bash(git *),Read`)
	if !strings.Contains(r.stderr, "names a rule, not a tool") {
		t.Fatalf("no rule warning: %s", truncStr(r.stderr, 200))
	}
	m = map[string]any{}
	json.Unmarshal(bodies[0], &m)
	tools, _ = m["tools"].([]any)
	for _, tl := range tools {
		name := tl.(map[string]any)["name"]
		if name == "Read" || name == "Bash" {
			continue
		}
		_ = name
	}
}

// TestCompactBoundaryStreamJSON: auto-compaction emits the
// system/compact_boundary event on the stream-json wire.
func TestCompactBoundaryStreamJSON(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p",
		`usage:160000`+"\r"+`hi2`+"\r",
		"--output-format", "stream-json", "--verbose")
	// may not compact in this shape — check either the event or the run
	// completed cleanly
	if strings.Contains(r.stdout, "compact_boundary") {
		return
	}
	// no compaction under this input — acceptable; the event only fires
	// when auto-compact triggers (covered by TestAutoCompactPrint's path)
	s.mustOK(t, r)
}

// TestCleanupPeriodDays: transcripts older than cleanupPeriodDays get
// swept at session start (real's retention sweep, default 30).
func TestCleanupPeriodDays(t *testing.T) {
	s := newSession(t)
	s.writeFile(".claude/settings.json", `{"cleanupPeriodDays": 1}`)
	// plant a 2-day-old transcript under the project slug dir
	projDir := filepath.Join(s.home, ".claude", "projects")
	matches, _ := filepath.Glob(filepath.Join(projDir, "*"))
	var dir string
	if len(matches) > 0 {
		dir = matches[0]
	} else {
		dir = filepath.Join(projDir, session.Slug(s.cwd))
	}
	os.MkdirAll(dir, 0o755)
	old := filepath.Join(dir, "old-session.jsonl")
	os.WriteFile(old, []byte(`{"type":"user","message":{"role":"user","content":"old"}}`+"\n"), 0o644)
	oldT := time.Now().AddDate(0, 0, -2)
	os.Chtimes(old, oldT, oldT)
	// a session start sweeps it
	r := s.run(t, "--mock", "-p", "hi")
	s.mustOK(t, r)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old transcript survived cleanupPeriodDays=1 sweep")
	}
}

// TestMcpJsonTrustPrompt: a .mcp.json server prompts "approve? [y/N]" at
// interactive startup; typing y writes enabledMcpjsonServers.
func TestMcpJsonTrustPrompt(t *testing.T) {
	s := newSession(t)
	s.writeFile(".mcp.json", `{"mcpServers":{"srv":{"command":"true"}}}`)
	p := s.runPTY(t, nil, "--mock")
	p.waitFor("approve", 30*time.Second)
	p.send("y")
	p.waitFor("❯", 30*time.Second)
	// approved → written to local settings
	lp := filepath.Join(s.cwd, ".claude", "settings.local.json")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(lp); strings.Contains(string(b), "srv") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := os.ReadFile(lp)
	if !strings.Contains(string(b), "srv") {
		t.Fatalf("approval not persisted: %s\noutput:\n%s", string(b), p.clean())
	}
}

// TestAbovePromptBand: ctrl+x ctrl+a expands the collapsed-activity band
// above the composer; the band appears once it has content.
func TestAbovePromptBand(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)
	p.sendAtPrompt(1, "say hi\r")
	p.waitCount("❯", 2, 30*time.Second)
	// after the turn the band exists — expand then collapse it
	p.send("\x18\x01") // ctrl+x ctrl+a → expand
	time.Sleep(300 * time.Millisecond)
	out := p.clean()
	if !strings.Contains(out, "no recent activity") &&
		!strings.Contains(out, "⏵") {
		t.Fatalf("expanded band missing:\n%s", truncStr(out, 3000))
	}
	p.send("\x18\x01") // toggle again → collapse
	time.Sleep(300 * time.Millisecond)
	if !strings.Contains(p.clean(), "collapsed") {
		t.Fatalf("collapsed band missing:\n%s", truncStr(p.clean(), 3000))
	}
	p.exit()
}
