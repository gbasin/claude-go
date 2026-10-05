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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
				reply, _ := json.Marshal(r.nonStreamText)
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
	p.waitFor("╭─╮", 30*time.Second)
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

	evals := 0
	for _, q := range rec.list() {
		if q.method != "POST" || q.uri != "/v1/messages?beta=true" {
			continue
		}
		var b struct {
			Model   string `json:"model"`
			MaxToks int    `json:"max_tokens"`
		}
		if json.Unmarshal(q.body, &b) == nil &&
			b.Model == "claude-sonnet-5" && b.MaxToks == 64 {
			evals++
		}
	}
	if evals != 1 {
		t.Fatalf("auto mode should eval exactly the non-readonly call, got %d evals", evals)
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
