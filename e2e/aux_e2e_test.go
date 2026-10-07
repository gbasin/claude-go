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
	"strings"
	"sync"
	"testing"
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
	// nonStreamByModel overrides nonStreamText keyed on the request's model
	// field — lets the two eval stages answer differently.
	nonStreamByModel map[string]string

	mu   sync.Mutex
	reqs []recReq
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
					writeToolUseSSE(w, calls)
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
func writeToolUseSSE(w http.ResponseWriter, calls []auxCall) {
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
	emit("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": "tool_use"}, "usage": usage})
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
		case q.method == "POST" && strings.Contains(string(q.body), `"max_tokens":1`):
			sawProbe = true
			if q.class != "auxiliary" {
				t.Fatalf("quota probe request-class = %q", q.class)
			}
			if !strings.Contains(string(q.body), "claude-haiku-4-5") {
				t.Fatalf("quota probe should use haiku, got %s", q.body)
			}
		case q.method == "POST" && strings.HasPrefix(q.uri, "/api/event_logging/v2/batch"):
			sawBatch = true
		case q.method == "POST" && strings.HasPrefix(q.uri, "/v1/messages") &&
			!strings.Contains(string(q.body), `"max_tokens":1`):
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
		if strings.Contains(string(q.body), `"max_tokens":1`) {
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
		if strings.Contains(string(q.body), `"max_tokens":1`) {
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
	if !strings.Contains(req.System[0].Text, "cc_prompt_id=") {
		t.Fatalf("subagent billing missing its own prompt_id: %s", req.System[0].Text)
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

	// env cap wins over the flag when smaller
	rec2 := t.TempDir() + "/reqs2"
	r = s.runEx(t, []string{
		"CLAUDE_GO_MOCK_RECORD=" + rec2, "MAX_THINKING_TOKENS=500",
	}, 30*time.Second, "--mock", "-p", "hi", "--thinking", "medium",
		"--max-thinking-tokens", "12345")
	s.mustOK(t, r)
	b, _ = os.ReadFile(filepath.Join(rec2, "001.json"))
	if !bytes.Contains(b, []byte(`"budget_tokens":500`)) {
		t.Fatalf("MAX_THINKING_TOKENS cap missing:\n%s", truncStr(string(b), 500))
	}
}
