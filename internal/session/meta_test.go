package session

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/gbasin/claude-go/internal/api"
)

// readLines returns the raw transcript lines.
func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

func keyOrder(t *testing.T, line string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(line))
	var keys []string
	tok, err := dec.Token() // '{'
	if err != nil {
		t.Fatal(err)
	}
	_ = tok
	for dec.More() {
		tk, _ := dec.Token()
		keys = append(keys, tk.(string))
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// Real's streaming scanner anchors on {"parentUuid": at byte 0 of
// user/assistant rows — ours emitted "type" first.
func TestMessageRowsParentUuidFirst(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	st, err := Open(cwd, "sess-order")
	if err != nil {
		t.Fatal(err)
	}
	st.Log(api.TextMessage("user", "first"))
	st.Log(api.TextMessage("assistant", "one"))
	st.Log(api.TextMessage("user", "second"))
	st.Close()

	lines := readLines(t, st.Path)
	var userLines, asstLines []string
	for _, l := range lines {
		if strings.Contains(l, `"type":"user"`) {
			userLines = append(userLines, l)
		}
		if strings.Contains(l, `"type":"assistant"`) {
			asstLines = append(asstLines, l)
		}
	}
	if len(userLines) != 2 || len(asstLines) != 1 {
		t.Fatalf("user=%d assistant=%d", len(userLines), len(asstLines))
	}
	// root row: real writes "parentUuid":null — the anchor holds either way
	if !strings.HasPrefix(userLines[0], `{"parentUuid":`) {
		t.Fatalf("user row does not start with parentUuid: %.80s", userLines[0])
	}
	if !strings.HasPrefix(asstLines[0], `{"parentUuid":`) {
		t.Fatalf("assistant row does not start with parentUuid: %.80s", asstLines[0])
	}
	// real user-row order: parentUuid, isSidechain, promptId, type, message,
	// uuid, timestamp, then the session envelope
	keys := keyOrder(t, userLines[0])
	wantPrefix := []string{"parentUuid", "isSidechain", "promptId", "type", "message", "uuid", "timestamp"}
	if len(keys) < len(wantPrefix) {
		t.Fatalf("keys = %v", keys)
	}
	for i, w := range wantPrefix {
		if keys[i] != w {
			t.Fatalf("key[%d] = %q, want %q (all: %v)", i, keys[i], w, keys)
		}
	}
	// real's tail: userType, entrypoint, cwd, sessionId, version
	tail := []string{"userType", "entrypoint", "cwd", "sessionId", "version"}
	got := keys[len(keys)-len(tail):]
	for i, w := range tail {
		if got[i] != w {
			t.Fatalf("tail key[%d] = %q, want %q (all: %v)", i, got[i], w, keys)
		}
	}
	// chained rows: second user's parentUuid is the assistant row's uuid
	var a map[string]any
	json.Unmarshal([]byte(asstLines[0]), &a)
	var u2 map[string]any
	json.Unmarshal([]byte(userLines[1]), &u2)
	if u2["parentUuid"] != a["uuid"] {
		t.Fatalf("chain broken: %v != %v", u2["parentUuid"], a["uuid"])
	}
}

// promptId is minted per typed prompt and shared by that turn's
// tool_result user rows (real's requestJournal.promptId).
func TestPromptIDRotation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	st, _ := Open(cwd, "sess-pid")
	st.Log(api.TextMessage("user", "p1"))
	tr := api.Message{Role: "user", Content: json.RawMessage(
		`[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]`)}
	st.Log(tr)
	st.Log(api.TextMessage("assistant", "a1"))
	st.Log(api.TextMessage("user", "p2"))
	st.Close()

	var pids []string
	for _, l := range readLines(t, st.Path) {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil || m["type"] != "user" {
			continue
		}
		pids = append(pids, m["promptId"].(string))
	}
	if len(pids) != 3 {
		t.Fatalf("pids = %v", pids)
	}
	if pids[0] != pids[1] {
		t.Fatalf("tool_result should share the turn's promptId: %q vs %q", pids[0], pids[1])
	}
	if pids[0] == pids[2] {
		t.Fatal("promptId did not rotate on the second prompt")
	}
}

// permissionMode rides typed-prompt user rows once the mode is latched
// (real stamps it inside the entry, after timestamp).
func TestUserRowPermissionMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	st, _ := Open(cwd, "sess-perm")
	st.StampPermMode("acceptEdits")
	st.Log(api.TextMessage("user", "hi"))
	st.Close()
	for _, l := range readLines(t, st.Path) {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil || m["type"] != "user" {
			continue
		}
		if m["permissionMode"] != "acceptEdits" {
			t.Fatalf("permissionMode = %v", m["permissionMode"])
		}
	}
}

// State records are bare {type,<fields>,sessionId} — no uuid/parentUuid —
// and land in real's literal key order.
func TestStateRecordShapes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	st, _ := Open(cwd, "sess-state")

	st.StampMode("normal")
	st.StampPermMode("bypassPermissions")
	st.LogAtisLatch("")
	st.LogAgentMeta("v2", "", "", "")
	st.LogAgentMeta("", "helper", "", "")
	st.LogAgentMeta("", "", "cyan", "")
	st.LogAgentMeta("", "", "", "code-reviewer")
	st.LogAITitle("auto title")
	st.LogEndedByModel()
	st.LogPRLink(42, "https://github.com/o/r/pull/42", "o/r")
	st.LogContinuedIn("sess-next")
	st.LogRelocated("/elsewhere")
	st.LogIsolationLatch("inside")
	st.LogDevModsFolder("/tmp/devmods")
	st.LogFrameLink("page.html", "https://claude.ai/a/xyz", "My Page", 3)
	st.LogObserverRef("agent-1", "obs-1", "watcher")
	st.Close()

	got := map[string][]string{}
	for _, l := range readLines(t, st.Path) {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil {
			continue
		}
		got[m["type"].(string)] = keyOrder(t, l)
	}

	want := map[string][]string{
		"mode":            {"type", "mode", "sessionId"},
		"permission-mode": {"type", "permissionMode", "sessionId"},
		"atis-latch":      {"type", "atis", "sessionId"},
		"tag":             {"type", "tag", "sessionId"},
		"agent-name":      {"type", "agentName", "sessionId"},
		"agent-color":     {"type", "agentColor", "sessionId"},
		"agent-setting":   {"type", "agentSetting", "sessionId"},
		"ai-title":        {"type", "aiTitle", "sessionId"},
		"ended-by-model":  {"type", "timestamp", "sessionId"},
		"pr-link":         {"type", "sessionId", "prNumber", "prUrl", "prRepository", "timestamp"},
		"continued-in":    {"type", "timestamp", "sessionId", "continuedInSessionId"},
		"relocated":       {"type", "sessionId", "relocatedCwd"},
		"isolation-latch": {"type", "side", "sessionId"},
		"dev-mods":        {"type", "folder", "sessionId"},
		"frame-link":      {"type", "sessionId", "path", "frameUrl", "title", "artifactCount", "timestamp"},
		"observer-ref":    {"type", "agentId", "observerTaskId", "observerAgentType", "timestamp", "sessionId"},
	}
	for typ, keys := range want {
		gotKeys, ok := got[typ]
		if !ok {
			t.Fatalf("missing %s record", typ)
		}
		if len(gotKeys) != len(keys) {
			t.Fatalf("%s keys = %v, want %v", typ, gotKeys, keys)
		}
		for i, k := range keys {
			if gotKeys[i] != k {
				t.Fatalf("%s key[%d] = %q, want %q (all: %v)", typ, i, gotKeys[i], k, gotKeys)
			}
		}
	}
}

// After each typed prompt real re-flushes the whole latched state set
// (planReAppendSessionMetadata): last-prompt first, then the rest in the
// fixed push order.
func TestRestampBlockPerPrompt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	st, _ := Open(cwd, "sess-flush")
	st.StampMode("normal")
	st.StampPermMode("default")
	st.LogAgentMeta("", "", "cyan", "")
	st.Log(api.TextMessage("user", "hello\nworld"))
	st.Close()

	lines := readLines(t, st.Path)
	var types []string
	for _, l := range lines {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil {
			types = append(types, m["type"].(string))
		}
	}
	// the tail block: last-prompt, agent-color, mode, permission-mode,
	// atis-latch (push order: identity before mode, latches after)
	var tail []string
	for i, ty := range types {
		if ty == "last-prompt" {
			tail = types[i:]
		}
	}
	want := []string{"last-prompt", "agent-color", "mode", "permission-mode", "atis-latch"}
	if len(tail) != len(want) {
		t.Fatalf("flush block = %v, want %v", tail, want)
	}
	for i, w := range want {
		if tail[i] != w {
			t.Fatalf("flush[%d] = %q, want %q (all %v)", i, tail[i], w, tail)
		}
	}
	// last-prompt carries the normalized text + leaf uuid, no uuid/ts
	var lp map[string]any
	for _, l := range lines {
		if strings.Contains(l, `"type":"last-prompt"`) {
			json.Unmarshal([]byte(l), &lp)
		}
	}
	if lp["lastPrompt"] != "hello world" || lp["leafUuid"] == "" {
		t.Fatalf("last-prompt = %v", lp)
	}
	if _, ok := lp["uuid"]; ok {
		t.Fatal("last-prompt must not carry uuid")
	}
}

// Reopening a transcript restores the latched state (real's adopt path)
// and continues the uuid chain from the last row.
func TestRestoreMetaOnOpen(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	st, _ := Open(cwd, "sess-restore")
	st.StampMode("coordinator")
	st.StampPermMode("plan")
	st.LogAgentMeta("", "helper", "", "")
	st.Log(api.TextMessage("user", "hey"))
	st.Log(api.TextMessage("assistant", "yo"))
	st.Close()

	st2, err := Open(cwd, "sess-restore")
	if err != nil {
		t.Fatal(err)
	}
	if st2.meta.mode != "coordinator" || st2.meta.permMode != "plan" ||
		st2.meta.agentName != "helper" {
		t.Fatalf("meta not restored: %+v", st2.meta)
	}
	if st2.lastUUID == "" || st2.chainParent == "" {
		t.Fatal("chain anchors not restored")
	}
	if err := st2.Log(api.TextMessage("user", "again")); err != nil {
		t.Fatal(err)
	}
	st2.Close()

	// the new user row's parentUuid is the assistant row's uuid
	recs, _ := readRecords(st2.Path)
	var lastAsst, lastUser map[string]any
	for _, r := range recs {
		switch r["type"] {
		case "assistant":
			lastAsst = r
		case "user":
			lastUser = r
		}
	}
	if lastUser["parentUuid"] != lastAsst["uuid"] {
		t.Fatalf("resumed chain broken: %v != %v", lastUser["parentUuid"], lastAsst["uuid"])
	}
}

// queue-operation is a state row: {type,operation,timestamp,sessionId,
// content} — no uuid/parentUuid (real's insertQueueOperation).
func TestQueueOperationShape(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	st, _ := Open(cwd, "sess-queue")
	st.LogRecord(map[string]any{
		"type": "queue-operation", "operation": "enqueue", "content": "later",
	})
	st.Close()
	for _, l := range readLines(t, st.Path) {
		if !strings.Contains(l, "queue-operation") {
			continue
		}
		keys := keyOrder(t, l)
		want := []string{"type", "operation", "timestamp", "sessionId", "content"}
		if len(keys) != len(want) {
			t.Fatalf("queue-operation keys = %v", keys)
		}
		for i, k := range want {
			if keys[i] != k {
				t.Fatalf("key[%d] = %q, want %q", i, keys[i], k)
			}
		}
		return
	}
	t.Fatal("no queue-operation row")
}

// Attachments and other chained records join the uuid chain — real
// parents each appended row on the previous uuid'd row.
func TestChainedRecordOrdering(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	st, _ := Open(cwd, "sess-chain")
	st.Log(api.TextMessage("user", "q"))
	st.LogAttachment(map[string]any{"type": "file", "path": "/x"})
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
	if att["parentUuid"] == nil || att["parentUuid"] == "" {
		t.Fatal("attachment should chain onto the user row")
	}
	if att["uuid"] == nil || att["isSidechain"] != false || att["userType"] != "external" {
		t.Fatalf("attachment missing envelope: %v", att)
	}
	// attachments advance the chain: a following message parents on them
	st2, _ := Open(cwd, "sess-chain")
	st2.restoreMeta()
	if st2.chainParent != att["uuid"] {
		t.Fatalf("chainParent = %v, want attachment uuid %v", st2.chainParent, att["uuid"])
	}
}
