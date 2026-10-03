package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"claude-go/internal/api"
)

func TestLogTitleWritesCustomTitleJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	st, err := Open(cwd, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Log(api.TextMessage("user", "hello there")); err != nil {
		t.Fatal(err)
	}
	if err := st.LogTitle("my title"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	p := filepath.Join(dir(cwd), "sess-1", "custom-title.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("custom-title.json missing: %v", err)
	}
	var v map[string]string
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if v["customTitle"] != "my title" {
		t.Fatalf("customTitle = %q", v["customTitle"])
	}
	if got := ReadTitle(dir(cwd), "sess-1"); got != "my title" {
		t.Fatalf("ReadTitle = %q", got)
	}
	infos, err := List(cwd)
	if err != nil || len(infos) != 1 {
		t.Fatalf("List: %v %v", infos, err)
	}
	if infos[0].Title != "my title" {
		t.Fatalf("Info.Title = %q", infos[0].Title)
	}
	// transcript should carry summary + custom-title records
	recs, err := readRecords(st.Path)
	if err != nil {
		t.Fatal(err)
	}
	var sawSummary, sawCustom bool
	for _, r := range recs {
		if r["type"] == "summary" && r["summary"] == "my title" {
			sawSummary = true
		}
		if r["type"] == "custom-title" && r["customTitle"] == "my title" {
			sawCustom = true
		}
	}
	if !sawSummary || !sawCustom {
		t.Fatalf("missing records: summary=%v custom-title=%v", sawSummary, sawCustom)
	}
}

func TestLoadPathAndRecordTypes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	st, err := Open(cwd, "sess-2")
	if err != nil {
		t.Fatal(err)
	}
	st.LogInit("claude-test")
	st.Log(api.TextMessage("user", "u1"))
	st.Log(api.TextMessage("assistant", "a1"))
	st.LogAttachment(map[string]any{"type": "file", "path": "/x"})
	st.LogProgress("tu1", "", map[string]any{"type": "tool_heartbeat"})
	st.LogCompactSummary(api.TextMessage("user", "summary of earlier"), "model only")
	st.Close()

	msgs, err := LoadPath(st.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	recs, _ := readRecords(st.Path)
	var init, att, prog, compact bool
	for _, r := range recs {
		switch r["type"] {
		case "system":
			if r["subtype"] == "init" {
				init = true
			}
		case "attachment":
			if a, ok := r["attachment"].(map[string]any); ok && a["type"] == "file" {
				att = true
			}
		case "progress":
			prog = true
		case "user":
			if r["isCompactSummary"] == true && r["modelOnlyText"] == "model only" {
				compact = true
			}
		}
	}
	if !init || !att || !prog || !compact {
		t.Fatalf("record types missing: init=%v att=%v prog=%v compact=%v", init, att, prog, compact)
	}
}

func TestFork(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	src, err := Open(cwd, "sess-3")
	if err != nil {
		t.Fatal(err)
	}
	src.Log(api.TextMessage("user", "first prompt"))
	src.Log(api.TextMessage("assistant", "reply one"))
	src.Log(api.TextMessage("user", "second"))
	src.Close()

	fst, msgs, err := Fork(cwd, "sess-3")
	if err != nil {
		t.Fatal(err)
	}
	defer fst.Close()
	if fst.SessionID == "sess-3" {
		t.Fatal("fork reused session id")
	}
	if len(msgs) != 3 {
		t.Fatalf("fork messages = %d, want 3", len(msgs))
	}
	// forked records must carry forkedFrom + rebuilt parentUuid chain
	recs, _ := readRecords(fst.Path)
	var last string
	seen := 0
	for _, r := range recs {
		ff, _ := r["forkedFrom"].(map[string]any)
		if ff == nil {
			continue // title/summary records appended by LogTitle
		}
		if ff["sessionId"] != "sess-3" || ff["messageUuid"] != r["uuid"] {
			t.Fatalf("bad forkedFrom: %v", ff)
		}
		if r["sessionId"] != fst.SessionID {
			t.Fatalf("sessionId = %v", r["sessionId"])
		}
		if seen == 0 {
			if r["parentUuid"] != nil {
				t.Fatalf("first parentUuid = %v", r["parentUuid"])
			}
		} else if r["parentUuid"] != last {
			t.Fatalf("parent chain broken at %d", seen)
		}
		last, _ = r["uuid"].(string)
		seen++
	}
	if seen != 3 {
		t.Fatalf("forked records = %d, want 3", seen)
	}
	// fork should be resumable: same messages back via Load
	back, err := Load(cwd, fst.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 3 {
		t.Fatalf("reloaded %d messages", len(back))
	}
	if got := ReadTitle(dir(cwd), fst.SessionID); got == "" {
		t.Fatal("fork missing (Branch) title")
	}
}
