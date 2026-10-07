package agent

import (
	"strings"
	"testing"
)

func TestAppendSubagentSystemPrompt(t *testing.T) {
	t.Setenv("CLAUDE_GO_APPEND_SUBAGENT_SYS", "APPENDED RULES")
	d := AgentDef{Name: "x", SystemPrompt: "base"}
	if got := d.effectiveSystemPrompt(); !strings.HasSuffix(got, "\n\nAPPENDED RULES") {
		t.Fatalf("append missing: %q", got)
	}
}
