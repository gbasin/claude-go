package repl

import (
	"strings"
	"testing"
)

// queued_command attachment schema — real's mod_0416 emit.
func TestQueuedCommandAttachment(t *testing.T) {
	att, extra := queuedCommandAttachment("fix the tests", false)
	if att["type"] != "queued_command" || att["prompt"] != "fix the tests" {
		t.Fatalf("att = %v", att)
	}
	for _, k := range []string{"source_uuid", "delivery_id", "commandMode", "origin", "timestamp"} {
		if att[k] == nil {
			t.Fatalf("missing %s in %v", k, att)
		}
	}
	if att["commandMode"] != "prompt" {
		t.Fatalf("commandMode = %v", att["commandMode"])
	}
	if att["origin"].(map[string]any)["kind"] != "human" {
		t.Fatalf("origin = %v", att["origin"])
	}
	// human prompts carry real's system-reminder rendered text
	r, _ := extra["rendered"].([]any)
	if len(r) != 1 {
		t.Fatalf("rendered = %v", extra["rendered"])
	}
	content := r[0].(map[string]any)["content"].(string)
	if !strings.Contains(content, "sent a new message while you were working:\nfix the tests") ||
		!strings.HasPrefix(content, "<system-reminder>") ||
		!strings.HasSuffix(content, "</system-reminder>") {
		t.Fatalf("rendered content = %q", content)
	}
	if extra["renderedInHumanTurn"] != true {
		t.Fatalf("renderedInHumanTurn = %v", extra["renderedInHumanTurn"])
	}

	// injected (cron/monitor) items are task-notification, no humanTurn
	att2, extra2 := queuedCommandAttachment("<scheduled/>", true)
	if att2["commandMode"] != "task-notification" {
		t.Fatalf("commandMode = %v", att2["commandMode"])
	}
	if att2["origin"].(map[string]any)["kind"] != "task-notification" {
		t.Fatalf("origin = %v", att2["origin"])
	}
	if extra2["rendered"] != nil || extra2["renderedInHumanTurn"] != nil {
		t.Fatalf("injected extras = %v", extra2)
	}
}
