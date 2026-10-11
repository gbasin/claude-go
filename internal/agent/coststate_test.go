package agent

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gbasin/claude-go/internal/api"
)

func TestCostStateSchema(t *testing.T) {
	l := &Loop{Client: &api.Client{SessionID: "sess-1"}, Model: "claude-fable-5-1"}
	l.sessionStarted = time.Now().Add(-time.Second)
	l.LinesAdded.Store(12)
	l.LinesRemoved.Store(3)
	l.apiNanos.Store(int64(1500 * time.Millisecond))
	l.apiNanosNoRetry.Store(int64(1400 * time.Millisecond))
	l.toolNanos.Store(int64(2000 * time.Millisecond))
	l.costUSD = 0.42
	l.costModels = map[string]*costModelUsage{
		"claude-fable-5-1": {
			InputTokens: 100, OutputTokens: 50, ThinkingTokens: 7,
			CacheReadInputTokens: 900, CacheCreationInputTokens: 30,
			WebSearchRequests: 2, CostUSD: 0.42,
		},
	}
	rec := l.CostState()
	if rec == nil {
		t.Fatal("nil record")
	}
	b, _ := json.Marshal(rec)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	// real's s3t row — exact key set (verified against a real transcript)
	want := []string{
		"type", "sessionId", "totalCostUSD", "totalAPIDuration",
		"totalAPIDurationWithoutRetries", "totalToolDuration",
		"totalLinesAdded", "totalLinesRemoved", "totalDuration",
		"startTime", "modelUsage", "hasUnknownModelCost",
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing %q in %s", k, b)
		}
	}
	if len(m) != len(want) {
		t.Fatalf("extra keys in %s", b)
	}
	if m["type"] != "cost-state" || m["sessionId"] != "sess-1" {
		t.Fatalf("%s", b)
	}
	if m["totalAPIDuration"].(float64) != 1500 ||
		m["totalAPIDurationWithoutRetries"].(float64) != 1400 ||
		m["totalToolDuration"].(float64) != 2000 {
		t.Fatalf("durations: %s", b)
	}
	if m["totalLinesAdded"].(float64) != 12 || m["totalLinesRemoved"].(float64) != 3 {
		t.Fatalf("lines: %s", b)
	}
	mu := m["modelUsage"].(map[string]any)["claude-fable-5-1"].(map[string]any)
	for _, k := range []string{"inputTokens", "outputTokens", "thinkingTokens",
		"cacheReadInputTokens", "cacheCreationInputTokens",
		"webSearchRequests", "costUSD"} {
		if _, ok := mu[k]; !ok {
			t.Fatalf("modelUsage missing %q", k)
		}
	}
	if mu["webSearchRequests"].(float64) != 2 || mu["thinkingTokens"].(float64) != 7 {
		t.Fatalf("modelUsage = %v", mu)
	}
	if m["hasUnknownModelCost"].(bool) {
		t.Fatal("known model flagged unknown")
	}
}

func TestCostStateNilWithoutSession(t *testing.T) {
	l := &Loop{Client: &api.Client{}}
	if l.CostState() != nil {
		t.Fatal("record without a session id")
	}
}

func TestRecordCostUsageUnknownModel(t *testing.T) {
	l := &Loop{Client: &api.Client{SessionID: "s"}, Model: "claude-mystery-9"}
	u := api.Usage{InputTokens: 10, OutputTokens: 5}
	_, known := modelCostUSD("claude-mystery-9", u)
	if known {
		t.Fatal("mystery model should be unknown")
	}
	l.recordCostUsage(&api.Response{Model: "claude-mystery-9", Usage: u}, 0.01, known)
	rec := l.CostState()
	if !rec.HasUnknownModelCost {
		t.Fatal("hasUnknownModelCost not set")
	}
	mu := rec.ModelUsage["claude-mystery-9"]
	if mu.InputTokens != 10 || mu.OutputTokens != 5 {
		t.Fatalf("entry = %+v", mu)
	}
	u2 := api.Usage{
		InputTokens: 1,
		OutputTokensDetails: &struct {
			ThinkingTokens int `json:"thinking_tokens,omitempty"`
		}{ThinkingTokens: 4},
		ServerToolUse: &struct {
			WebSearchRequests int `json:"web_search_requests,omitempty"`
		}{WebSearchRequests: 1},
	}
	l.recordCostUsage(&api.Response{Model: "claude-mystery-9", Usage: u2}, 0.01, known)
	mu = l.CostState().ModelUsage["claude-mystery-9"]
	if mu.ThinkingTokens != 4 || mu.WebSearchRequests != 1 || mu.InputTokens != 11 {
		t.Fatalf("entry = %+v", mu)
	}
}
