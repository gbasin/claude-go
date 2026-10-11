package agent

import (
	"encoding/json"
	"testing"

	"github.com/gbasin/claude-go/internal/api"
	"github.com/gbasin/claude-go/internal/auth"
)

func budgetLoop(t *testing.T, model string) *Loop {
	t.Helper()
	l := NewLoop(api.NewClient(&auth.Credentials{AccessToken: "mock"}, "test-session", api.EntrySDKCLI), t.TempDir())
	l.Model = model
	return l
}

func TestTaskBudgetWireShape(t *testing.T) {
	l := budgetLoop(t, "claude-fable-5-1")
	l.TaskBudget = 50000
	l.Effort = "medium"
	req := l.buildRequest()
	var oc map[string]any
	if err := json.Unmarshal(req.OutputConfig, &oc); err != nil || oc["task_budget"] == nil {
		t.Fatalf("task_budget missing: %s", req.OutputConfig)
	}
	tb := oc["task_budget"].(map[string]any)
	if tb["type"] != "tokens" || tb["total"].(float64) != 50000 {
		t.Fatalf("task_budget shape: %v", tb)
	}
	if _, ok := tb["remaining"]; ok {
		t.Fatal("remaining must be absent on the first request (real parity)")
	}
	// second request carries remaining after usage decrement
	l.taskBudgetRemaining = 40000
	req = l.buildRequest()
	json.Unmarshal(req.OutputConfig, &oc)
	if oc["task_budget"].(map[string]any)["remaining"].(float64) != 40000 {
		t.Fatalf("remaining: %s", oc["task_budget"])
	}
}

func TestTaskBudgetBetaHeader(t *testing.T) {
	l := budgetLoop(t, "claude-fable-5-1")
	l.TaskBudget = 50000
	l.buildRequest()
	if !containsStr(l.Client.ExtraBetas, "task-budgets-2026-03-13") {
		t.Fatalf("extra beta missing: %v", l.Client.ExtraBetas)
	}
}

func TestExtraBodyMerges(t *testing.T) {
	t.Setenv("CLAUDE_CODE_EXTRA_BODY", `{"custom_field": 42, "output_config": {"task_budget": {"type":"tokens","total":9}, "extra_oc": "yes"}}`)
	l := budgetLoop(t, "claude-fable-5-1")
	l.TaskBudget = 50000 // flag loses — env seeded first (real's "task_budget" in n guard)
	l.Effort = "medium"
	req := l.buildRequest()
	raw, _ := json.Marshal(req)
	var m map[string]any
	json.Unmarshal(raw, &m)
	if m["custom_field"].(float64) != 42 {
		t.Fatalf("extra body field missing: %s", raw)
	}
	oc := m["output_config"].(map[string]any)
	// env's task_budget wins over the flag; env extra key survives
	if oc["task_budget"].(map[string]any)["total"].(float64) != 9 {
		t.Fatalf("env task_budget should win: %v", oc)
	}
	if oc["extra_oc"] != "yes" {
		t.Fatalf("seeded oc key dropped: %v", oc)
	}
	if _, present := m["extraBody"]; present {
		t.Fatal("ExtraBody leaked into the wire body")
	}
}
