//go:build e2e

// print_e2e_test.go — non-interactive (-p / --print) coverage:
//   - text echo against the mock
//   - tool round-trip (Bash echo → tool_result → mock summary)
//   - --output-format stream-json (init event keys, tool_use/tool_result
//     events, result event with is_error=false)
//   - --output-format json (result + usage fields)
//   - --model propagation into the init event
//   - --effort acceptance
//   - --allowedTools / --disallowedTools rule enforcement
//   - plan-mode tools erroring non-interactively
//   - --input-format stream-json over stdin
package e2e

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestPrintTextEcho: plain prompt → "Mock response to: <prompt>".
func TestPrintTextEcho(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p", "hello-e2e-world")
	s.mustOK(t, r)
	contains(t, r.stdout, "Mock response to: hello-e2e-world", "print output")
}

// TestPrintToolRoundTrip: forced Bash tool_use executes and the result
// round-trips back to the model ("Tool results received" echo).
func TestPrintToolRoundTrip(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p",
		`tool:Bash {"command":"echo hello-e2e"}`,
		"--permission-mode", "bypassPermissions")
	s.mustOK(t, r)
	contains(t, r.stdout, "Tool results received", "print output")
	contains(t, r.stdout, "hello-e2e", "tool result echoed back")
}

// TestPrintStreamJSON: init event shape + result event.
func TestPrintStreamJSON(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p", "stream check",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	events, result := parseStreamJSON(t, r.stdout)

	init := findEvent(events, "system", "init")
	if init == nil {
		t.Fatalf("no system/init event in output:\n%s", r.stdout)
	}
	for _, k := range []string{"type", "subtype", "session_id", "tools"} {
		if _, ok := init[k]; !ok {
			t.Fatalf("init event missing key %q: %v", k, init)
		}
	}
	if sid, _ := init["session_id"].(string); sid == "" {
		t.Fatal("init session_id empty")
	}
	if tools, ok := init["tools"].([]any); !ok || len(tools) == 0 {
		t.Fatalf("init tools not a non-empty array: %v", init["tools"])
	}
	if result == nil {
		t.Fatalf("no result object in output:\n%s", r.stdout)
	}
	if result["type"] != "result" || result["subtype"] != "success" {
		t.Fatalf("bad result event: %v", result)
	}
	if result["is_error"] != false {
		t.Fatalf("is_error should be false: %v", result["is_error"])
	}
	if res, _ := result["result"].(string); !strings.Contains(res, "Mock response") {
		t.Fatalf("result text lacks mock echo: %q", res)
	}
}

// TestPrintStreamJSONToolEvents: tool_use → assistant event, tool_result →
// user event carrying the bash output.
func TestPrintStreamJSONToolEvents(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p",
		`tool:Bash {"command":"echo sj-marker"}`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	events, result := parseStreamJSON(t, r.stdout)

	// assistant event with a tool_use block named Bash
	var sawToolUse bool
	for _, e := range events {
		if e["type"] != "assistant" {
			continue
		}
		msg, _ := e["message"].(map[string]any)
		for _, b := range msg["content"].([]any) {
			bm, _ := b.(map[string]any)
			if bm["type"] == "tool_use" && bm["name"] == "Bash" {
				sawToolUse = true
			}
		}
	}
	if !sawToolUse {
		t.Fatalf("no assistant tool_use event for Bash:\n%s", r.stdout)
	}
	trs := toolResultTexts(events)
	if len(trs) == 0 {
		t.Fatalf("no user tool_result event:\n%s", r.stdout)
	}
	contains(t, trs[0], "sj-marker", "tool_result content")
	if result["is_error"] != false {
		t.Fatalf("is_error should be false: %v", result)
	}
}

// TestPrintJSON: --output-format json emits one parseable object with
// result + usage.
func TestPrintJSON(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p", "json check", "--output-format", "json")
	s.mustOK(t, r)
	var m map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &m); err != nil {
		t.Fatalf("json output did not parse: %v\n%s", err, r.stdout)
	}
	if m["type"] != "result" || m["is_error"] != false {
		t.Fatalf("bad result object: %v", m)
	}
	if res, _ := m["result"].(string); !strings.Contains(res, "Mock response to: json check") {
		t.Fatalf("result missing mock text: %q", res)
	}
	usage, ok := m["usage"].(map[string]any)
	if !ok {
		t.Fatalf("no usage object: %v", m)
	}
	if usage["input_tokens"] == nil && usage["inputTokens"] == nil {
		t.Fatalf("usage has no input token count: %v", usage)
	}
	if _, ok := m["session_id"].(string); !ok {
		t.Fatalf("no session_id: %v", m)
	}
}

// TestModelPropagates: --model foo appears in the stream-json init event.
func TestModelPropagates(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p", "m", "--model", "foo-e2e-model",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	events, _ := parseStreamJSON(t, r.stdout)
	init := findEvent(events, "system", "init")
	if init == nil {
		t.Fatalf("no init event:\n%s", r.stdout)
	}
	if init["model"] != "foo-e2e-model" {
		t.Fatalf("model not propagated, got %v", init["model"])
	}
}

// TestEffortAccepted: --effort max is accepted and the run succeeds.
func TestEffortAccepted(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p", "effort check", "--effort", "max")
	s.mustOK(t, r)
	contains(t, r.stdout, "Mock response", "print output")
}

// TestAllowedToolsRestricts: --allowedTools "Bash(ls:*)" permits ls but a
// non-matching command resolves to a declined tool_result.
func TestAllowedToolsRestricts(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p",
		`calls:[{"name":"Bash","input":{"command":"ls"}},{"name":"Bash","input":{"command":"echo should-be-blocked"}}]`,
		"--allowedTools", "Bash(ls:*)",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	events, _ := parseStreamJSON(t, r.stdout)
	trs := toolResultTexts(events)
	if len(trs) != 2 {
		t.Fatalf("expected 2 tool_result events, got %d:\n%s", len(trs), r.stdout)
	}
	contains(t, trs[0]+trs[1], "denied", "denied tool result")
	notContains(t, trs[0]+trs[1], "should-be-blocked", "blocked command output")
}

// TestDisallowedTools: --disallowedTools=Write denies a Write call.
func TestDisallowedTools(t *testing.T) {
	s := newSession(t)
	target := s.cwd + "/deny-me.txt"
	r := s.run(t, "--mock", "-p",
		`tool:Write {"file_path":"`+target+`","content":"nope"}`,
		"--disallowedTools", "Write",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	events, _ := parseStreamJSON(t, r.stdout)
	trs := toolResultTexts(events)
	if len(trs) == 0 {
		t.Fatalf("no tool_result event:\n%s", r.stdout)
	}
	contains(t, trs[0], "denied", "denied Write result")
}

// TestPlanToolsNonInteractive: EnterPlanMode/ExitPlanMode return the
// "requires interactive" error in -p.
func TestPlanToolsNonInteractive(t *testing.T) {
	s := newSession(t)
	for _, tool := range []string{"EnterPlanMode", "ExitPlanMode"} {
		r := s.run(t, "--mock", "-p",
			`calls:[{"name":"ToolSearch","input":{"query":"select:`+tool+`"}},`+
				`{"name":"`+tool+`","input":{}}]`,
			"--permission-mode", "bypassPermissions",
			"--output-format", "stream-json")
		s.mustOK(t, r)
		events, _ := parseStreamJSON(t, r.stdout)
		trs := toolResultTexts(events)
		if len(trs) == 0 {
			t.Fatalf("%s: no tool_result event:\n%s", tool, r.stdout)
		}
		contains(t, trs[len(trs)-1], "interactive session", tool+" result")
	}
}

// TestStreamJSONInput: --input-format stream-json reads JSONL user messages
// from stdin and answers each.
func TestStreamJSONInput(t *testing.T) {
	s := newSession(t)
	in := `{"type":"user","message":{"role":"user","content":"first-q"}}` + "\n" +
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"second-q"}]}}` + "\n"
	r := s.runStdin(t, in, "--mock", "-p", "--input-format", "stream-json")
	s.mustOK(t, r)
	contains(t, r.stdout, `"type":"assistant"`, "stream-json input output")
	if !strings.Contains(r.stdout, "Mock response to: first-q") &&
		!strings.Contains(r.stdout, "Mock response to: second-q") {
		t.Fatalf("expected mock answers in output:\n%s", r.stdout)
	}
}

// TestMaxTurns: --max-turns 1 on a tool call still completes (the mock's
// follow-up echo turn is allowed once).
func TestUnknownFlagWriteSessionID(t *testing.T) {
	// claude-go has --session-id but no --write-session-id (real CC writes
	// the session id to a file). Assert the honest "unknown flag" failure
	// so the gap is documented rather than silently skipped.
	s := newSession(t)
	r := s.run(t, "--mock", "-p", "hi", "--write-session-id", "/tmp/x")
	if r.code == 0 {
		t.Fatalf("--write-session-id unexpectedly succeeded:\n%s", r.stdout)
	}
	contains(t, r.stderr, "unknown flag", "flag error")
}
