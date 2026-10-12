package repl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gbasin/claude-go/internal/agent"
	"github.com/gbasin/claude-go/internal/api"
	"github.com/gbasin/claude-go/internal/tools"
)

// foldResultRows: 3 wrapped rows above the fold, 4-row grace shows all.
func TestFoldResultRows(t *testing.T) {
	above, rem := foldResultRows("a\nb\nc\nd", 80)
	if rem != 0 || len(above) != 4 {
		t.Fatalf("4-row grace should show all: above=%d rem=%d", len(above), rem)
	}
	above, rem = foldResultRows("a\nb\nc\nd\ne", 80)
	if rem != 2 || len(above) != 3 {
		t.Fatalf("5 rows should fold to 3 + 2: above=%d rem=%d", len(above), rem)
	}
	// a long line wraps at w-10 → wrapped rows count toward the fold
	long := strings.Repeat("x", 300) // 300/70 → 5 wrapped rows + tail = 6
	above, rem = foldResultRows(long+"\ntail", 80)
	if rem != 3 || len(above) != 3 {
		t.Fatalf("wrapped fold: above=%d rem=%d", len(above), rem)
	}
}

// Transcript splice: an Agent tool_use replays the sidechain transcript
// under its row when the view is expanded (showAll).
func TestTranscriptAgentSplice(t *testing.T) {
	dir := t.TempDir()
	sid := "s123"
	transcript := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(transcript, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, sid, "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// minimal sidechain: one assistant text row
	rec := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"inner answer"}]},"uuid":"u1","sessionId":"` + sid + `"}`
	if err := os.WriteFile(filepath.Join(sub, "a1-1.jsonl"), []byte(rec+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tools.RegisterAgent(&tools.AgentRecord{
		Name: "probe agent", AgentID: "a1-1", Done: make(chan struct{}),
		Inbox: make(chan string, 1),
	})
	call, _ := json.Marshal([]api.ContentBlock{{
		Type: "tool_use", ID: "toolu_1", Name: "Agent",
		Input: json.RawMessage(`{"description":"probe agent","prompt":"go"}`),
	}})
	l := &agent.Loop{Transcript: transcript}
	l.Messages = []api.Message{{Role: "assistant", Content: call}}
	rows := transcriptLines(l, true)
	var found bool
	for _, r := range rows {
		if strings.Contains(r, "inner answer") {
			found = true
		}
	}
	if !found {
		t.Fatalf("agent inner transcript not spliced:\n%s", strings.Join(rows, "\n"))
	}
	// collapsed view (showAll=false) does not splice
	rows = transcriptLines(l, false)
	for _, r := range rows {
		if strings.Contains(r, "inner answer") {
			t.Fatal("spliced rows should hide when collapsed")
		}
	}
}
