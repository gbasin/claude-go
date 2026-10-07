package tools

import (
	"strings"
	"testing"
)

func TestEnterPlanModeInstructions(t *testing.T) {
	t.Setenv("CLAUDE_GO_PLAN_MODE_INSTRUCTIONS", "WORKFLOW: sketch first, verify twice")
	c := &Context{CWD: t.TempDir(), SetPermMode: func(string) {}}
	r := execEnterPlan(c, nil)
	if r.IsError || !strings.Contains(r.Content, "WORKFLOW: sketch first, verify twice") {
		t.Fatalf("plan-mode instructions missing from result: %q", r.Content)
	}
	if !strings.Contains(r.Content, "read-only") {
		t.Fatalf("preamble lost: %q", r.Content)
	}
}
