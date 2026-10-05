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
