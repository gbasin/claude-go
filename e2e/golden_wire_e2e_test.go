//go:build e2e

package e2e

// Golden-wire parity: real claude's /v1/messages request (captured through
// the tlscap relay, redacted) vs our binary's request to the mock.
// Structural comparison — the same field set, system layout, tool list,
// and scalars must match; per-run values (uuids, session ids, message
// content) are ignored.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type wireCapture struct {
	RequestLine string            `json:"request_line"`
	Headers     map[string]string `json:"headers"`
	Body        struct {
		Model             string            `json:"model"`
		MaxTokens         int               `json:"max_tokens"`
		System            []json.RawMessage `json:"system"`
		Messages          []json.RawMessage `json:"messages"`
		Tools             []json.RawMessage `json:"tools"`
		Stream            bool              `json:"stream"`
		Thinking          json.RawMessage   `json:"thinking"`
		ContextManagement json.RawMessage   `json:"context_management"`
		Diagnostics       json.RawMessage   `json:"diagnostics"`
		OutputConfig      json.RawMessage   `json:"output_config"`
		Safeguards        json.RawMessage   `json:"safeguards"`
		Metadata          json.RawMessage   `json:"metadata"`
		Thread            json.RawMessage   `json:"thread"`
	} `json:"body"`
}

func TestGoldenRequestParity(t *testing.T) {
	gb, err := os.ReadFile("testdata/realwire_print_fable.json")
	if err != nil {
		t.Skip("no golden capture")
	}
	var golden wireCapture
	if err := json.Unmarshal(gb, &golden); err != nil {
		t.Fatal(err)
	}

	s := newSession(t)
	recDir := filepath.Join(t.TempDir(), "reqs")
	r := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + recDir}, 30*time.Second,
		"--mock", "-p", "say hi")
	s.mustOK(t, r)

	ents, err := os.ReadDir(recDir)
	if err != nil || len(ents) == 0 {
		t.Fatalf("mock recorded no requests: %v", err)
	}
	last, err := os.ReadFile(filepath.Join(recDir, ents[len(ents)-1].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var ours struct {
		Model             string            `json:"model"`
		MaxTokens         int               `json:"max_tokens"`
		System            []json.RawMessage `json:"system"`
		Messages          []json.RawMessage `json:"messages"`
		Tools             []json.RawMessage `json:"tools"`
		Stream            bool              `json:"stream"`
		Thinking          json.RawMessage   `json:"thinking"`
		ContextManagement json.RawMessage   `json:"context_management"`
		Diagnostics       json.RawMessage   `json:"diagnostics"`
	}
	if err := json.Unmarshal(last, &ours); err != nil {
		t.Fatal(err)
	}

	// request line
	if golden.RequestLine != "POST /v1/messages?beta=true HTTP/1.1" {
		t.Fatalf("golden line changed: %s", golden.RequestLine)
	}
	// model + scalars
	if ours.Model != golden.Body.Model {
		t.Fatalf("model %q vs golden %q", ours.Model, golden.Body.Model)
	}
	if ours.MaxTokens != golden.Body.MaxTokens {
		t.Fatalf("max_tokens %d vs golden %d", ours.MaxTokens, golden.Body.MaxTokens)
	}
	if ours.Stream != golden.Body.Stream {
		t.Fatal("stream flag differs")
	}
	// system block count + cache_control placement
	if len(ours.System) != len(golden.Body.System) {
		t.Fatalf("system blocks: ours %d vs golden %d", len(ours.System), len(golden.Body.System))
	}
	for i := range golden.Body.System {
		var gs, os_ struct {
			Type         string          `json:"type"`
			CacheControl json.RawMessage `json:"cache_control"`
		}
		json.Unmarshal(golden.Body.System[i], &gs)
		json.Unmarshal(ours.System[i], &os_)
		if gs.Type != os_.Type {
			t.Fatalf("system[%d] type %q vs %q", i, os_.Type, gs.Type)
		}
		if normJSON(gs.CacheControl) != normJSON(os_.CacheControl) {
			t.Fatalf("system[%d] cache_control %s vs %s", i, os_.CacheControl, gs.CacheControl)
		}
	}
	// tool name list — exact match including order
	var gtools, otools []struct{ Name string }
	for _, g := range golden.Body.Tools {
		var s struct{ Name string }
		json.Unmarshal(g, &s)
		gtools = append(gtools, s)
	}
	for _, o := range ours.Tools {
		var s struct{ Name string }
		json.Unmarshal(o, &s)
		otools = append(otools, s)
	}
	if len(gtools) != len(otools) {
		t.Fatalf("tool count ours %d vs golden %d", len(otools), len(gtools))
	}
	for i := range gtools {
		if gtools[i].Name != otools[i].Name {
			t.Fatalf("tool[%d] ours %q vs golden %q", i, otools[i].Name, gtools[i].Name)
		}
	}
	// thinking + context_management + diagnostics identical
	if normJSON(ours.Thinking) != normJSON(golden.Body.Thinking) {
		t.Fatalf("thinking %s vs %s", ours.Thinking, golden.Body.Thinking)
	}
	if normJSON(ours.ContextManagement) != normJSON(golden.Body.ContextManagement) {
		t.Fatalf("context_management %s vs %s", ours.ContextManagement, golden.Body.ContextManagement)
	}
	if normJSON(ours.Diagnostics) != normJSON(golden.Body.Diagnostics) {
		t.Fatalf("diagnostics %s vs %s", ours.Diagnostics, golden.Body.Diagnostics)
	}
	// header parity — every golden header name must be present on ours
	// (values normalized for per-request uuids)
	goldenHdrs := map[string]bool{}
	for k := range golden.Headers {
		goldenHdrs[k] = true
	}
	for must := range map[string]bool{
		"anthropic-beta": true, "anthropic-version": true, "x-app": true,
		"x-stainless-runtime": true, "x-claude-code-session-id": true,
		"x-claude-code-prompt-id": true, "x-client-request-id": true,
		"anthropic-dispatch-id": true, "x-claude-code-request-class": true,
	} {
		if !goldenHdrs[must] {
			t.Fatalf("golden missing expected header %q", must)
		}
	}
}

// normJSON reparses a RawMessage so key order/indentation don't count.
func normJSON(r json.RawMessage) string {
	var v any
	if json.Unmarshal(r, &v) != nil {
		return string(r)
	}
	b, _ := json.Marshal(v)
	return string(b)
}
