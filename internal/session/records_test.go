package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gbasin/claude-go/internal/api"
	"github.com/gbasin/claude-go/internal/config"
)

// keys returns the sorted top-level key set of a record.
func keys(r map[string]any) []string {
	var out []string
	for k := range r {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func keySet(r map[string]any) string { return strings.Join(keys(r), ",") }

// Real's bookkeeping rows carry exactly their emit-site literal — no
// uuid/parentUuid/auto timestamp (captured 2.1.296 transcripts).
func TestBareRecordSchemas(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	st, err := Open(cwd, "sess-bare")
	if err != nil {
		t.Fatal(err)
	}
	st.Log(api.TextMessage("user", "hi"))
	st.LogAITitle("the title")
	st.LogEndedByModel()
	st.LogPRLink(7, "https://github.com/o/r/pull/7", "o/r")
	st.LogContinuedIn("sess-next")
	st.LogWorktreeState(map[string]any{
		"originalCwd": "/orig", "preEnterOriginalCwd": "/orig",
		"worktreePath": "/orig/.claude/worktrees/w", "worktreeName": "w",
		"worktreeBranch": "worktree-w", "originalBranch": "main",
		"originalHeadCommit": "abc", "sessionId": "sess-bare",
		"hookBased": false, "enteredExisting": false,
	})
	st.LogForkContextRef("ag1", "sess-bare", "leaf-uuid", 9)
	st.LogContentReplacement([]any{
		map[string]any{"kind": "tool-result", "toolUseId": "tu1", "replacement": "REDACTED"},
	}, "")
	st.Close()

	recs, err := readRecords(st.Path)
	if err != nil {
		t.Fatal(err)
	}
	byType := map[string]map[string]any{}
	for _, r := range recs {
		t_, _ := r["type"].(string)
		byType[t_] = r
	}
	// exact key sets (real's literals; captures verified)
	cases := map[string]string{
		"ai-title":            "aiTitle,sessionId,type",
		"ended-by-model":      "sessionId,timestamp,type",
		"pr-link":             "prNumber,prRepository,prUrl,sessionId,timestamp,type",
		"continued-in":        "continuedInSessionId,sessionId,timestamp,type",
		"worktree-state":      "sessionId,type,worktreeSession",
		"fork-context-ref":    "agentId,contextLength,parentLastUuid,parentSessionId,type",
		"content-replacement": "replacements,sessionId,type",
	}
	for typ, want := range cases {
		r := byType[typ]
		if r == nil {
			t.Fatalf("missing record type %s", typ)
		}
		if got := keySet(r); got != want {
			t.Fatalf("%s keys = %s, want %s", typ, got, want)
		}
	}
	if r := byType["ai-title"]; r["aiTitle"] != "the title" {
		t.Fatalf("aiTitle = %v", r["aiTitle"])
	}
	if r := byType["continued-in"]; r["continuedInSessionId"] != "sess-next" {
		t.Fatalf("continuedInSessionId = %v", r["continuedInSessionId"])
	}
	ws := byType["worktree-state"]["worktreeSession"].(map[string]any)
	if ws["worktreePath"] != "/orig/.claude/worktrees/w" || ws["hookBased"] != false {
		t.Fatalf("worktreeSession = %v", ws)
	}
	fcr := byType["fork-context-ref"]
	if fcr["contextLength"].(float64) != 9 || fcr["parentLastUuid"] != "leaf-uuid" ||
		fcr["agentId"] != "ag1" || fcr["parentSessionId"] != "sess-bare" {
		t.Fatalf("fork-context-ref = %v", fcr)
	}
	cr := byType["content-replacement"]
	rs := cr["replacements"].([]any)
	if len(rs) != 1 || rs[0].(map[string]any)["toolUseId"] != "tu1" {
		t.Fatalf("replacements = %v", rs)
	}
	// agentId only present when the writer is an agent
	if _, ok := cr["agentId"]; ok {
		t.Fatalf("agentId should be omitted, got %v", cr["agentId"])
	}
	pr := byType["pr-link"]
	if pr["prUrl"] != "https://github.com/o/r/pull/7" || pr["timestamp"] == nil {
		t.Fatalf("pr-link = %v", pr)
	}
}

// attachment rows are full chain records: uuid, parentUuid, session_id +
// sessionId, envelope fields, and they advance the chain.
func TestAttachmentEnvelope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	st, _ := Open(cwd, "sess-att")
	st.Log(api.TextMessage("user", "before"))
	if err := st.LogAttachmentMeta(map[string]any{
		"type": "queued_command", "prompt": "qp", "commandMode": "prompt",
		"origin": map[string]any{"kind": "human"},
	}, map[string]any{"rendered": []any{map[string]any{"content": "<r>"}},
		"renderedInHumanTurn": true}); err != nil {
		t.Fatal(err)
	}
	st.Log(api.TextMessage("user", "after"))
	st.Close()

	recs, _ := readRecords(st.Path)
	var att map[string]any
	for _, r := range recs {
		if r["type"] == "attachment" {
			att = r
		}
	}
	if att == nil {
		t.Fatal("no attachment row")
	}
	for _, k := range []string{"uuid", "timestamp", "sessionId", "session_id",
		"userType", "entrypoint", "cwd", "version", "isSidechain"} {
		if att[k] == nil {
			t.Fatalf("attachment missing %s: %v", k, att)
		}
	}
	if att["attachment"].(map[string]any)["type"] != "queued_command" {
		t.Fatalf("attachment.type = %v", att["attachment"])
	}
	if att["renderedInHumanTurn"] != true || att["rendered"] == nil {
		t.Fatalf("extras missing: %v", att)
	}
	// chain: attachment's parentUuid is the first user row's uuid; the
	// second user row's parentUuid is the attachment's uuid
	var u1, u2 map[string]any
	for _, r := range recs {
		if r["type"] == "user" && u1 == nil {
			u1 = r
		} else if r["type"] == "user" {
			u2 = r
		}
	}
	if att["parentUuid"] != u1["uuid"] {
		t.Fatalf("attachment parentUuid = %v, want %v", att["parentUuid"], u1["uuid"])
	}
	if u2["parentUuid"] != att["uuid"] {
		t.Fatalf("next message parentUuid = %v, want attachment uuid %v",
			u2["parentUuid"], att["uuid"])
	}
}

// --fork-session replays the source's content-replacement rows as one
// consolidated record (real's copySession path).
func TestForkReplaysContentReplacements(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	src, _ := Open(cwd, "sess-cr-src")
	src.Log(api.TextMessage("user", "p1"))
	src.Log(api.TextMessage("assistant", "a1"))
	// rows on the source: two ours, one foreign sessionId
	src.LogContentReplacement([]any{
		map[string]any{"kind": "tool-result", "toolUseId": "t1", "replacement": "R1"},
	}, "")
	src.LogContentReplacement([]any{
		map[string]any{"kind": "tool-result", "toolUseId": "t2", "replacement": "R2"},
	}, "")
	// foreign row appended raw (other session's leftovers)
	src.LogBare(map[string]any{"type": "content-replacement", "sessionId": "other",
		"replacements": []any{map[string]any{"kind": "x", "toolUseId": "t9"}}})
	src.Close()

	dst, _ := Open(cwd, "sess-cr-dst")
	dst.LogInit("m")
	msgs, err := dst.ForkInto(src.Path, "sess-cr-src", "")
	if err != nil {
		t.Fatal(err)
	}
	dst.Close()
	if len(msgs) != 2 {
		t.Fatalf("msgs = %d", len(msgs))
	}
	recs, _ := readRecords(dst.Path)
	var crs []map[string]any
	for _, r := range recs {
		if r["type"] == "content-replacement" {
			crs = append(crs, r)
		}
	}
	if len(crs) != 1 {
		t.Fatalf("content-replacement rows on fork = %d, want 1 consolidated", len(crs))
	}
	r := crs[0]
	if r["sessionId"] != "sess-cr-dst" {
		t.Fatalf("sessionId = %v", r["sessionId"])
	}
	rs := r["replacements"].([]any)
	if len(rs) != 2 || rs[0].(map[string]any)["toolUseId"] != "t1" ||
		rs[1].(map[string]any)["toolUseId"] != "t2" {
		t.Fatalf("replacements = %v", rs)
	}
	if r["uuid"] != nil || r["parentUuid"] != nil {
		t.Fatalf("content-replacement must not join the chain: %v", r)
	}
}

// file-history rows: snapshot keyed to the user message uuid + delta rows
// for newly tracked files — real's Tke + insertFileHistoryDelta.
func TestFileHistoryRows(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	st, _ := Open(cwd, "sess-fh")

	var promptUUIDs []string
	st.OnUserPrompt = func(u string) { promptUUIDs = append(promptUUIDs, u) }
	st.Log(api.TextMessage("user", "first"))
	st.Close()

	if len(promptUUIDs) != 1 {
		t.Fatalf("OnUserPrompt calls = %d", len(promptUUIDs))
	}
	// the hook must get the USER row's uuid — real keys the checkpoint to it
	recs, _ := readRecords(st.Path)
	var u1 string
	for _, r := range recs {
		if r["type"] == "user" {
			u1 = r["uuid"].(string)
		}
	}
	if promptUUIDs[0] != u1 {
		t.Fatalf("OnUserPrompt uuid = %v, want %v", promptUUIDs[0], u1)
	}

	// tool_result rows ride the user role but aren't prompts — no hook
	st2, _ := Open(cwd, "sess-fh2")
	fired := 0
	st2.OnUserPrompt = func(string) { fired++ }
	st2.Log(api.Message{Role: "user", Content: json.RawMessage(
		`[{"type":"tool_result","tool_use_id":"x","content":"y"}]`)})
	st2.Close()
	if fired != 0 {
		t.Fatalf("OnUserPrompt fired %d times for a tool_result row", fired)
	}

	// schema of the emit helpers themselves
	st3, _ := Open(cwd, "sess-fh3")
	st3.LogFileHistorySnapshot("u-uuid", map[string]any{
		"/p": map[string]any{"backupFileName": "abc@v1", "version": 1,
			"backupTime": "T", "realParentDir": "/"},
	}, false)
	st3.LogFileHistoryDelta("u-uuid", "u-uuid", "/p", map[string]any{
		"backupFileName": "abc@v1", "version": 1, "backupTime": "T",
	})
	st3.Close()
	recs, _ = readRecords(st3.Path)
	var snap, delta map[string]any
	for _, r := range recs {
		switch r["type"] {
		case "file-history-snapshot":
			snap = r
		case "file-history-delta":
			delta = r
		}
	}
	if snap == nil || delta == nil {
		t.Fatalf("missing rows: %v", recs)
	}
	if snap["messageId"] != "u-uuid" || snap["isSnapshotUpdate"] != false {
		t.Fatalf("snapshot = %v", snap)
	}
	inner := snap["snapshot"].(map[string]any)
	if inner["messageId"] != "u-uuid" || inner["trackedFileBackups"] == nil ||
		inner["timestamp"] == nil {
		t.Fatalf("snapshot inner = %v", inner)
	}
	if delta["messageId"] != "u-uuid" || delta["snapshotMessageId"] != "u-uuid" ||
		delta["trackingPath"] != "/p" || delta["backup"] == nil ||
		delta["timestamp"] == nil {
		t.Fatalf("delta = %v", delta)
	}
	// neither joins the chain
	for _, r := range []map[string]any{snap, delta} {
		if r["uuid"] != nil || r["parentUuid"] != nil {
			t.Fatalf("file-history row must be bare: %v", r)
		}
	}
}

// AppendContinuedIn stamps the marker onto an existing foreign
// transcript; a missing file stays missing (real's onlyIfExists).
func TestAppendContinuedIn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	src, _ := Open(cwd, "sess-src")
	src.Log(api.TextMessage("user", "work"))
	src.Close()
	if err := LogContinuedInTo(cwd, "sess-src", "sess-new"); err != nil {
		t.Fatal(err)
	}
	recs, _ := readRecords(src.Path)
	last := recs[len(recs)-1]
	if last["type"] != "continued-in" ||
		last["continuedInSessionId"] != "sess-new" ||
		last["sessionId"] != "sess-src" || last["timestamp"] == nil {
		t.Fatalf("continued-in = %v", last)
	}
	// picker behavior: sess-src now hides
	if err := LogContinuedInTo(cwd, "nonexistent", "sess-new"); err == nil {
		t.Fatal("missing transcript should not create a marker")
	}
	if _, err := os.Stat(filepath.Join(dir(cwd), "nonexistent.jsonl")); !os.IsNotExist(err) {
		t.Fatal("continued-in must not create the transcript file")
	}
}

// --rewind-files against messageId-linked rows: the snapshot keyed to the
// anchor user message carries the state to restore; deltas merge in
// first-tracked files.
func TestRewindFilesLinked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	target := filepath.Join(cwd, "f.txt")
	os.WriteFile(target, []byte("current"), 0o644)

	st, _ := Open(cwd, "sess-rw")
	st.Log(api.TextMessage("user", "m1"))
	// snapshot keyed to m1's uuid — real writes the row via Tke
	recs, _ := readRecords(st.Path)
	var u1 string
	for _, r := range recs {
		if r["type"] == "user" {
			u1 = r["uuid"].(string)
		}
	}
	histDir := filepath.Join(config.ClaudeDir(), "file-history", "sess-rw")
	os.MkdirAll(histDir, 0o755)
	os.WriteFile(filepath.Join(histDir, "aa@v1"), []byte("v0"), 0o644)
	st.LogFileHistorySnapshot(u1, map[string]any{
		target: map[string]any{"backupFileName": "aa@v1", "version": 1,
			"backupTime": "t", "realParentDir": cwd},
	}, false)
	// a new file first tracked during m1's turn: delta carries its backup
	newFile := filepath.Join(cwd, "new.txt")
	os.WriteFile(newFile, []byte("exists"), 0o644)
	st.LogFileHistoryDelta(u1, u1, newFile, map[string]any{
		"backupFileName": nil, "version": 1, "backupTime": "t",
		"realParentDir": cwd})
	st.Log(api.TextMessage("assistant", "done"))
	st.Close()

	restored, removed, err := RewindFiles(cwd, "sess-rw", u1)
	if err != nil {
		t.Fatal(err)
	}
	if restored != 1 || removed != 1 {
		t.Fatalf("restored=%d removed=%d", restored, removed)
	}
	if b, _ := os.ReadFile(target); string(b) != "v0" {
		t.Fatalf("restored content = %q", b)
	}
	if _, err := os.Stat(newFile); !os.IsNotExist(err) {
		t.Fatal("delta file (no backup at anchor) should be removed")
	}
}

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
