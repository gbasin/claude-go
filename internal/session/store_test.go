package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gbasin/claude-go/internal/api"
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

// ForkInto is the --fork-session path: history must land in the new
// transcript so a later resume sees it (the old code wrote only a
// system/fork marker).
func TestForkInto(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	src, err := Open(cwd, "sess-fork-src")
	if err != nil {
		t.Fatal(err)
	}
	src.Log(api.TextMessage("user", "prompt one"))
	src.Log(api.TextMessage("assistant", "answer one"))
	src.Close()

	dst, err := Open(cwd, "sess-fork-dst")
	if err != nil {
		t.Fatal(err)
	}
	if err := dst.LogInit("claude-test"); err != nil {
		t.Fatal(err)
	}
	msgs, err := dst.ForkInto(src.Path, "sess-fork-src", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("ForkInto messages = %d, want 2", len(msgs))
	}
	// new writes chain onto the copied leaf
	if err := dst.Log(api.TextMessage("user", "new turn")); err != nil {
		t.Fatal(err)
	}
	dst.Close()

	recs, _ := readRecords(dst.Path)
	var copied, chained int
	leaf := ""
	for _, r := range recs {
		if ff, _ := r["forkedFrom"].(map[string]any); ff != nil {
			copied++
			if ff["sessionId"] != "sess-fork-src" {
				t.Fatalf("forkedFrom.sessionId = %v", ff["sessionId"])
			}
			leaf, _ = r["uuid"].(string)
		}
	}
	if copied != 2 {
		t.Fatalf("copied records = %d, want 2", copied)
	}
	// the post-fork record's parentUuid must be the copied leaf
	for _, r := range recs {
		if r["type"] == "user" && r["forkedFrom"] == nil {
			if r["parentUuid"] != leaf {
				t.Fatalf("new record parentUuid = %v, want leaf %v", r["parentUuid"], leaf)
			}
			chained++
		}
	}
	if chained != 1 {
		t.Fatalf("post-fork user records = %d", chained)
	}
	// resume of the fork sees the whole history
	back, err := Load(cwd, "sess-fork-dst")
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 3 {
		t.Fatalf("resumed fork has %d messages, want 3", len(back))
	}
}

// ForkInto with a leaf cutoff truncates at the resume point
// (--fork-session --resume-session-at).
func TestForkIntoTruncatesAtLeaf(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	src, err := Open(cwd, "sess-trunc-src")
	if err != nil {
		t.Fatal(err)
	}
	src.Log(api.TextMessage("user", "u1"))
	src.Log(api.TextMessage("assistant", "a1"))
	src.Log(api.TextMessage("user", "u2"))
	src.Log(api.TextMessage("assistant", "a2"))
	src.Close()
	// leaf of "u1..a1" is a1's uuid — the second record's uuid
	recs, _ := readRecords(src.Path)
	var uuids []string
	for _, r := range recs {
		if r["type"] == "user" || r["type"] == "assistant" {
			uuids = append(uuids, r["uuid"].(string))
		}
	}
	dst, err := Open(cwd, "sess-trunc-dst")
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := dst.ForkInto(src.Path, "sess-trunc-src", uuids[1])
	if err != nil {
		t.Fatal(err)
	}
	dst.Close()
	if len(msgs) != 2 {
		t.Fatalf("truncated fork messages = %d, want 2", len(msgs))
	}
}

// Branch-title dedup: repeated forks of the same prompt get
// "(Branch)", "(Branch 2)", "(Branch 3)".
func TestForkBranchTitleDedup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	src, err := Open(cwd, "sess-title-src")
	if err != nil {
		t.Fatal(err)
	}
	src.Log(api.TextMessage("user", "same prompt"))
	src.Close()

	var titles []string
	for i := 0; i < 3; i++ {
		fst, _, err := Fork(cwd, "sess-title-src")
		if err != nil {
			t.Fatal(err)
		}
		titles = append(titles, ReadTitle(dir(cwd), fst.SessionID))
		fst.Close()
	}
	want := []string{"same prompt (Branch)", "same prompt (Branch 2)", "same prompt (Branch 3)"}
	for i, w := range want {
		if titles[i] != w {
			t.Fatalf("title[%d] = %q, want %q (all: %v)", i, titles[i], w, titles)
		}
	}
}

// ResolveArg matches titles like real's -r session resolution.
func TestResolveArg(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	st, _ := Open(cwd, "sess-byid")
	st.Log(api.TextMessage("user", "hi"))
	st.Close()
	titled, _ := Open(cwd, "sess-titled")
	titled.Log(api.TextMessage("user", "yo"))
	titled.LogTitle("My Project Work")
	titled.Close()
	ai, _ := Open(cwd, "sess-ai")
	ai.Log(api.TextMessage("user", "yo"))
	ai.LogRecord(map[string]any{"type": "ai-title", "aiTitle": "Refactor Auth Module"})
	ai.Close()

	if got := ResolveArg(cwd, "sess-byid"); got != "sess-byid" {
		t.Fatalf("id resolution = %q", got)
	}
	if got := ResolveArg(cwd, "My Project Work"); got != "sess-titled" {
		t.Fatalf("exact title = %q", got)
	}
	if got := ResolveArg(cwd, "project work"); got != "sess-titled" {
		t.Fatalf("substring title = %q", got)
	}
	if got := ResolveArg(cwd, "auth module"); got != "sess-ai" {
		t.Fatalf("aiTitle substring = %q", got)
	}
	if got := ResolveArg(cwd, "nonexistent"); got != "" {
		t.Fatalf("miss = %q", got)
	}
}

func TestNewRecordWriters(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	st, _ := Open(cwd, "sess-recs")
	st.Log(api.TextMessage("user", "hi"))
	st.LogAITitle("auto title")
	st.LogEndedByModel()
	st.LogPRLink(42, "https://github.com/o/r/pull/42", "o/r")
	st.LogContinuedIn("sess-next")
	st.LogAgentMeta("v2", "", "", "")
	st.LogAgentMeta("", "helper-bot", "cyan", "")
	st.Close()

	recs, _ := readRecords(st.Path)
	want := map[string]bool{
		"ai-title": false, "ended-by-model": false, "pr-link": false,
		"continued-in": false, "tag": false, "agent-name": false,
	}
	for _, r := range recs {
		t_, _ := r["type"].(string)
		if _, ok := want[t_]; ok {
			want[t_] = true
			if t_ == "pr-link" && r["prNumber"].(float64) != 42 {
				t.Fatalf("prNumber = %v", r["prNumber"])
			}
			if t_ == "continued-in" && r["continuedInSessionId"] != "sess-next" {
				t.Fatalf("continuedInSessionId = %v", r["continuedInSessionId"])
			}
		}
	}
	for k, v := range want {
		if !v {
			t.Fatalf("missing record type %s", k)
		}
	}
}

// Picker filter: empty transcripts, ended-by-model, and superseded
// (continued-in) sessions are hidden — real's /resume listing rules.
func TestPickerFilters(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	// empty transcript — hidden
	empty, _ := Open(cwd, "sess-empty")
	empty.LogInit("m")
	empty.Close()
	// normal — shown
	ok, _ := Open(cwd, "sess-ok")
	ok.Log(api.TextMessage("user", "work"))
	ok.Close()
	// ended by model — hidden
	end, _ := Open(cwd, "sess-ended")
	end.Log(api.TextMessage("user", "done"))
	end.LogEndedByModel()
	end.Close()
	// superseded by a live sibling — hidden
	old, _ := Open(cwd, "sess-sup")
	old.Log(api.TextMessage("user", "old"))
	old.LogContinuedIn("sess-ok")
	old.Close()

	infos, err := List(cwd)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, i := range infos {
		ids[i.ID] = true
	}
	if len(infos) != 1 || !ids["sess-ok"] {
		t.Fatalf("listed %v", ids)
	}
}
