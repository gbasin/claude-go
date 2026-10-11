package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gbasin/claude-go/internal/api"
)

func TestLogHistorySuppression(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	st, err := Open(cwd, "sess-hs")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LogHistorySuppression("restored_owner_mismatch", "acct-1"); err != nil {
		t.Fatal(err)
	}
	if err := st.LogHistorySuppression("migration", ""); err != nil {
		t.Fatal(err)
	}
	st.Close()

	recs, err := readRecords(st.Path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	for _, r := range recs {
		if r["type"] == "history-suppression" {
			rows = append(rows, r)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	// real's schema (mod_0092 c8e): sessionId, cause, optional
	// vetoedAgainstAccountUuid, ts — no uuid/parentUuid/timestamp
	r0 := rows[0]
	if r0["sessionId"] != "sess-hs" || r0["cause"] != "restored_owner_mismatch" ||
		r0["vetoedAgainstAccountUuid"] != "acct-1" {
		t.Fatalf("row = %v", r0)
	}
	if r0["ts"] == nil {
		t.Fatal("missing ts")
	}
	for _, k := range []string{"uuid", "parentUuid", "timestamp"} {
		if _, ok := r0[k]; ok {
			t.Fatalf("stamped %q", k)
		}
	}
	if _, ok := rows[1]["vetoedAgainstAccountUuid"]; ok {
		t.Fatal("empty veto uuid emitted")
	}
}

func TestForkWritesHistorySuppression(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	src, err := Open(cwd, "sess-sup")
	if err != nil {
		t.Fatal(err)
	}
	src.LogInit("claude-test")
	src.Log(api.TextMessage("user", "u1"))
	src.Log(api.TextMessage("assistant", "a1"))
	if err := src.LogHistorySuppression("chokepoint_veto", ""); err != nil {
		t.Fatal(err)
	}
	src.Close()

	fst, msgs, err := Fork(cwd, "sess-sup")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 {
		t.Fatal("no messages forked")
	}
	fst.Close()

	recs, err := readRecords(fst.Path)
	if err != nil {
		t.Fatal(err)
	}
	// the fork_inherit row lands ahead of the copied messages
	if recs[0]["type"] != "history-suppression" || recs[0]["cause"] != "fork_inherit" {
		t.Fatalf("first record = %v", recs[0])
	}
}

func TestForkNoSuppressionWhenSourceClean(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	src, err := Open(cwd, "sess-clean")
	if err != nil {
		t.Fatal(err)
	}
	src.Log(api.TextMessage("user", "u1"))
	src.Close()

	fst, _, err := Fork(cwd, "sess-clean")
	if err != nil {
		t.Fatal(err)
	}
	fst.Close()
	recs, err := readRecords(fst.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r["type"] == "history-suppression" {
			t.Fatal("suppression row on a clean fork")
		}
	}
}

func TestAppendReplayRecordsOrderAndDedup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	st, err := Open(cwd, "sess-rr")
	if err != nil {
		t.Fatal(err)
	}
	shape := map[string]any{"type": "api-request-shape", "sessionId": "sess-rr",
		"shapeHash": "sh1", "shape": map[string]any{"system": []any{}}}
	req := map[string]any{"type": "api-request", "sessionId": "sess-rr", "id": "r1"}
	blob := ReplayBlob{Hash: "h1", Record: map[string]any{
		"type": "api-request-blob", "sessionId": "sess-rr", "hash": "h1"}}
	if s := st.AppendReplayRecords("sh1", shape, req, []ReplayBlob{blob}); s != "written" {
		t.Fatalf("status = %s", s)
	}
	// second request reusing shape+blob writes only the api-request row
	req2 := map[string]any{"type": "api-request", "sessionId": "sess-rr", "id": "r2"}
	if s := st.AppendReplayRecords("sh1", shape, req2, []ReplayBlob{blob}); s != "written" {
		t.Fatalf("status = %s", s)
	}
	st.Close()

	recs, err := readRecords(st.Path)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, r := range recs {
		if s, ok := r["type"].(string); ok && strings.HasPrefix(s, "api-request") {
			types = append(types, s)
		}
	}
	want := []string{"api-request-shape", "api-request-blob", "api-request", "api-request"}
	if len(types) != len(want) {
		t.Fatalf("rows = %v", types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("rows = %v", types)
		}
	}
	// a reopened store dedups against the on-disk index (resume case)
	st2, err := Open(cwd, "sess-rr")
	if err != nil {
		t.Fatal(err)
	}
	req3 := map[string]any{"type": "api-request", "sessionId": "sess-rr", "id": "r3"}
	if s := st2.AppendReplayRecords("sh1", shape, req3, []ReplayBlob{blob}); s != "written" {
		t.Fatalf("status = %s", s)
	}
	st2.Close()
	recs, _ = readRecords(st2.Path)
	count := 0
	for _, r := range recs {
		if r["type"] == "api-request-shape" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("shape rewritten after reopen: %d", count)
	}
}

func TestAppendReplayRecordsShapeTooLarge(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	st, err := Open(cwd, "sess-big")
	if err != nil {
		t.Fatal(err)
	}
	big := map[string]any{"type": "api-request-shape", "sessionId": "sess-big",
		"shapeHash": "shbig", "shape": map[string]any{"blob": strings.Repeat("x", replayShapeCap)}}
	s := st.AppendReplayRecords("shbig", big, map[string]any{"type": "api-request"}, nil)
	if s != "shape_too_large" {
		t.Fatalf("status = %s", s)
	}
	st.Close()
	b, _ := json.Marshal(map[string]any{"ok": true})
	_ = b
	recs, _ := readRecords(st.Path)
	for _, r := range recs {
		if s, _ := r["type"].(string); strings.HasPrefix(s, "api-request") {
			t.Fatalf("row written despite oversize shape: %v", r)
		}
	}
}
