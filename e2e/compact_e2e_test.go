//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestAutoCompactPrint: a -p turn whose reported usage crosses the
// CLAUDE_CODE_AUTO_COMPACT_WINDOW threshold must trigger auto-compact —
// the transcript gains the pre-compact messages, a compact_boundary
// (trigger:auto) and an isCompactSummary user record, in that order.
func TestAutoCompactPrint(t *testing.T) {
	s := newSession(t)
	r := s.runEx(t, []string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW=40000"},
		30*time.Second, "--mock", "-p", "usage:9000")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "Mock response") {
		t.Fatalf("expected mock reply, got %q", r.stdout)
	}
	paths := s.transcriptPaths(t)
	if len(paths) != 1 {
		t.Fatalf("transcripts = %v", paths)
	}
	recs := readRecords(t, paths[0])
	var boundary, summary int = -1, -1
	var sawPreCompact bool
	for i, rec := range recs {
		if rec["type"] == "system" && rec["subtype"] == "compact_boundary" {
			boundary = i
			md, _ := rec["compactMetadata"].(map[string]any)
			if md["trigger"] != "auto" {
				t.Fatalf("boundary trigger = %v, want auto", md["trigger"])
			}
		}
		if rec["type"] == "user" && rec["isCompactSummary"] == true {
			summary = i
		}
		if rec["type"] == "user" && boundary < 0 {
			if msg, _ := rec["message"].(map[string]any); msg != nil {
				if strings.Contains(string(mustJSON(msg["content"])), "usage:9000") {
					sawPreCompact = true
				}
			}
		}
	}
	if boundary < 0 {
		t.Fatal("no compact_boundary record in transcript")
	}
	if summary < 0 {
		t.Fatal("no isCompactSummary record in transcript")
	}
	if !sawPreCompact {
		t.Fatal("pre-compact user message missing from transcript")
	}
	if !(boundary < summary) {
		t.Fatal("records out of order: boundary must precede the summary record")
	}
}

// TestAutoCompactInteractive: auto-compact mid-turn in the REPL must not
// crash the transcript writer (history shrinks under the append cursor)
// and the session must keep answering.
func TestAutoCompactInteractive(t *testing.T) {
	s := newSession(t)
	p := s.runPTY(t, []string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW=40000"}, "--mock")
	p.waitFor("╭─╮", 30*time.Second)
	p.send("usage:9000\r")
	// compact fires at end of this turn; the next prompt box must come back
	p.waitCount("╭─╮", 2, 30*time.Second)
	// keystrokes can land in the post-turn drain window — retry until one
	// reaches the editor (each send submits a prompt; one answer suffices)
	p.sendUntil("hi\r", "Mock response to: hi", 15)
	p.exit()

	// transcript: pre-compact messages once, boundary, isCompactSummary,
	// and the post-compact turn — with no duplicated records
	paths := s.transcriptPaths(t)
	if len(paths) != 1 {
		t.Fatalf("transcripts = %v", paths)
	}
	var boundary, summary, promptCount, hiCount int
	for _, rec := range readRecords(t, paths[0]) {
		switch {
		case rec["subtype"] == "compact_boundary":
			boundary++
		case rec["isCompactSummary"] == true:
			summary++
		default:
			c := string(mustJSON(rec["message"]))
			if rec["type"] == "user" && strings.Contains(c, `"text":"usage:9000"`) {
				promptCount++
			}
			if rec["type"] == "assistant" && strings.Contains(c, "Mock response to: hi") {
				hiCount++
			}
		}
	}
	if boundary != 1 {
		t.Fatalf("compact_boundary records = %d, want 1", boundary)
	}
	if summary != 1 {
		t.Fatalf("isCompactSummary records = %d, want 1", summary)
	}
	if promptCount != 1 {
		for i, rec := range readRecords(t, paths[0]) {
			t.Logf("rec %d: %s", i, truncStr(string(mustJSON(rec)), 300))
		}
		t.Fatalf("usage:9000 prompt logged %d times (dup logging), want 1", promptCount)
	}
	if hiCount != 1 {
		t.Fatalf("post-compact reply logged %d times, want 1", hiCount)
	}
}

// TestAutoCompactThreshold: usage below the window must NOT compact —
// no boundary record, and the plain user/assistant records only.
func TestAutoCompactThreshold(t *testing.T) {
	s := newSession(t)
	r := s.runEx(t, []string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW=40000"},
		30*time.Second, "--mock", "-p", "usage:2000")
	s.mustOK(t, r)
	paths := s.transcriptPaths(t)
	if len(paths) != 1 {
		t.Fatalf("transcripts = %v", paths)
	}
	for _, rec := range readRecords(t, paths[0]) {
		if rec["subtype"] == "compact_boundary" || rec["isCompactSummary"] == true {
			t.Fatal("compact fired below threshold")
		}
	}
}
