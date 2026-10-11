package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gbasin/claude-go/internal/api"
	"github.com/gbasin/claude-go/internal/session"
)

func TestCanonicalJSONSortedKeys(t *testing.T) {
	v := map[string]any{"b": 1.0, "a": "x", "m": map[string]any{"z": true, "y": 2.0}}
	got := canonicalJSON(v, false, nil)
	if got != `{"a":"x","b":1,"m":{"y":2,"z":true}}` {
		t.Fatalf("canonicalJSON = %s", got)
	}
}

func TestCanonicalJSONOmitCacheControl(t *testing.T) {
	v := map[string]any{
		"type": "text", "text": "hi",
		"cache_control": map[string]any{"type": "ephemeral"},
	}
	var seen bool
	full := canonicalJSON(v, false, &seen)
	if !seen {
		t.Fatal("ccSeen not set")
	}
	if !strings.Contains(full, `"cache_control"`) {
		t.Fatalf("full form dropped cache_control: %s", full)
	}
	if stripped := canonicalJSON(v, true, nil); stripped != `{"text":"hi","type":"text"}` {
		t.Fatalf("omitCC form = %s", stripped)
	}
}

func TestStripRecord(t *testing.T) {
	v := map[string]any{
		"role": "user",
		"content": []any{map[string]any{
			"type": "text", "text": "x",
			"cache_control": map[string]any{"type": "ephemeral"},
		}},
		"gone": nil,
	}
	out := stripRecord(v).(map[string]any)
	if _, ok := out["gone"]; ok {
		t.Fatal("nil member kept")
	}
	blk := out["content"].([]any)[0].(map[string]any)
	if _, ok := blk["cache_control"]; ok {
		t.Fatal("cache_control kept")
	}
	if blk["text"] != "x" {
		t.Fatalf("text = %v", blk["text"])
	}
}

func TestMessageDigestNormalizesStringContent(t *testing.T) {
	a := messageDigest(map[string]any{"role": "user", "content": "hello"})
	b := messageDigest(map[string]any{
		"role":    "user",
		"content": []any{map[string]any{"type": "text", "text": "hello"}},
	})
	if a.hash != b.hash {
		t.Fatalf("string vs block content hash mismatch: %s != %s", a.hash, b.hash)
	}
	if a.hasCC || b.hasCC {
		t.Fatal("unexpected hasCC")
	}
	c := messageDigest(map[string]any{
		"role":    "user",
		"content": []any{map[string]any{"type": "text", "text": "hello", "cache_control": map[string]any{"type": "ephemeral"}}},
	})
	if !c.hasCC {
		t.Fatal("hasCC not detected")
	}
	// real hashes the cache_control-stripped form — identical content +
	// different cache_control tags collapse to one blob hash
	if c.hash != b.hash {
		t.Fatal("cache_control-tagged message should hash identically")
	}
	if !strings.Contains(c.json, "cache_control") {
		t.Fatal("json should retain cache_control")
	}
}

func TestCollectCacheControl(t *testing.T) {
	v := map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "text", "cache_control": map[string]any{"type": "ephemeral"}},
			map[string]any{"type": "text"},
		},
	}
	var out []map[string]any
	collectCacheControl(v, []any{"messages", 0}, &out)
	if len(out) != 1 {
		t.Fatalf("got %d entries", len(out))
	}
	path := out[0]["path"].([]any)
	if len(path) != 4 || path[0] != "messages" || path[1] != 0 || path[2] != "content" || path[3] != 0 {
		t.Fatalf("path = %v", path)
	}
	if out[0]["value"].(map[string]any)["type"] != "ephemeral" {
		t.Fatalf("value = %v", out[0]["value"])
	}
}

// replaySink stands in for the session store — captures the records
// recordAPIRequest hands to OnRequestReplay.
type replaySink struct {
	shapeHash string
	shape     any
	request   any
	blobs     []session.ReplayBlob
	status    string
	calls     int
}

func (s *replaySink) append(shapeHash string, shape, request any, blobs []session.ReplayBlob) string {
	s.shapeHash, s.shape, s.request, s.blobs = shapeHash, shape, request, blobs
	s.calls++
	if s.status == "" {
		return "written"
	}
	return s.status
}

func replayLoop(t *testing.T, sink *replaySink) *Loop {
	t.Helper()
	t.Setenv("CLAUDE_CODE_ELEGANT_MEADOW", "1")
	l := &Loop{
		Client:       &api.Client{SessionID: "sess-test"},
		ReplaySource: "repl_main_thread",
	}
	l.OnRequestReplay = sink.append
	return l
}

func TestRecordAPIRequestEmitsAllThreeRows(t *testing.T) {
	sink := &replaySink{}
	l := replayLoop(t, sink)
	req := &api.Request{
		Model:     "claude-fable-5-1",
		MaxTokens: 128000,
		System: []api.SystemBlock{
			{Type: "text", Text: "x-anthropic-billing-header: cc_version=2.1.296;"},
			{Type: "text", Text: "real prompt", CacheControl: &api.CacheControl{Type: "ephemeral"}},
		},
		Messages: []api.Message{
			api.StringMessage("user", "hello"),
		},
	}
	l.recordAPIRequest(req)
	if sink.calls != 1 {
		t.Fatalf("calls = %d", sink.calls)
	}
	rec, _ := json.Marshal(sink.request)
	var m map[string]any
	json.Unmarshal(rec, &m)
	if m["type"] != "api-request" {
		t.Fatalf("type = %v", m["type"])
	}
	for _, k := range []string{"sessionId", "id", "timestamp", "version",
		"querySource", "shapeHash", "params", "keep", "tail",
		"cacheControl", "requestDigest"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("api-request missing %q", k)
		}
	}
	if m["querySource"] != "repl_main_thread" || m["sessionId"] != "sess-test" ||
		m["version"] != api.Version {
		t.Fatalf("bad ids: %v", rec)
	}
	if m["keep"].(float64) != 0 {
		t.Fatalf("keep = %v", m["keep"])
	}
	if _, ok := m["base"]; ok {
		t.Fatal("base on a chain head")
	}
	// params must not contain messages/system/tools
	p := m["params"].(map[string]any)
	for _, k := range []string{"messages", "system", "tools", "stream", "metadata"} {
		if _, ok := p[k]; ok {
			t.Fatalf("params leaked %q", k)
		}
	}
	if p["model"] != "claude-fable-5-1" || p["max_tokens"] == nil {
		t.Fatalf("params = %v", p)
	}
	// shape row: billing block filtered, cache_control stripped
	srec, _ := json.Marshal(sink.shape)
	var sm map[string]any
	json.Unmarshal(srec, &sm)
	if sm["type"] != "api-request-shape" || sm["shapeHash"] != sink.shapeHash {
		t.Fatalf("shape row = %s", srec)
	}
	shape := sm["shape"].(map[string]any)
	sys := shape["system"].([]any)
	if len(sys) != 1 {
		t.Fatalf("shape.system = %v — billing block not filtered", sys)
	}
	if _, ok := sys[0].(map[string]any)["cache_control"]; ok {
		t.Fatal("shape kept cache_control")
	}
	// blob row
	if len(sink.blobs) != 1 {
		t.Fatalf("blobs = %d", len(sink.blobs))
	}
	brec, _ := json.Marshal(sink.blobs[0].Record)
	var bm map[string]any
	json.Unmarshal(brec, &bm)
	if bm["type"] != "api-request-blob" || bm["hash"] != sink.blobs[0].Hash {
		t.Fatalf("blob row = %s", brec)
	}
	// blob keeps the RAW message (string content un-normalized)
	msg := bm["message"].(map[string]any)
	if msg["content"] != "hello" {
		t.Fatalf("blob message = %v", msg)
	}
	// cacheControl lists the system block's cc
	cc := m["cacheControl"].([]any)
	if len(cc) != 1 {
		t.Fatalf("cacheControl = %v", cc)
	}
}

func TestRecordAPIRequestChainBaseKeep(t *testing.T) {
	sink := &replaySink{}
	l := replayLoop(t, sink)
	mk := func(msgs ...api.Message) *api.Request {
		return &api.Request{
			Model: "claude-fable-5-1", MaxTokens: 128000,
			System:   []api.SystemBlock{{Type: "text", Text: "sys"}},
			Messages: msgs,
		}
	}
	m1 := api.StringMessage("user", "one")
	l.recordAPIRequest(mk(m1))
	firstID := sink.request.(*apiRequestRecord).ID

	sink2 := &replaySink{}
	l.OnRequestReplay = sink2.append
	l.recordAPIRequest(mk(m1, api.StringMessage("assistant", "two")))
	rec := sink2.request.(*apiRequestRecord)
	if rec.Keep != 1 {
		t.Fatalf("keep = %d", rec.Keep)
	}
	if rec.Base != firstID {
		t.Fatalf("base = %q want %q", rec.Base, firstID)
	}
	if len(rec.Tail) != 1 {
		t.Fatalf("tail = %v", rec.Tail)
	}
	if len(sink2.blobs) != 1 {
		t.Fatalf("blobs = %d (kept prefix must not re-blob)", len(sink2.blobs))
	}
}

func TestRecordAPIRequestGateOff(t *testing.T) {
	t.Setenv("CLAUDE_CODE_ELEGANT_MEADOW", "")
	sink := &replaySink{}
	l := &Loop{
		Client:       &api.Client{SessionID: "s"},
		ReplaySource: "repl_main_thread",
	}
	l.OnRequestReplay = sink.append
	l.recordAPIRequest(&api.Request{Model: "m", Messages: []api.Message{api.StringMessage("user", "x")}})
	if sink.calls != 0 {
		t.Fatal("recorded with gate off")
	}
	// aux loops have no ReplaySource → never record
	t.Setenv("CLAUDE_CODE_ELEGANT_MEADOW", "1")
	l.ReplaySource = ""
	l.recordAPIRequest(&api.Request{Model: "m", Messages: []api.Message{api.StringMessage("user", "x")}})
	if sink.calls != 0 {
		t.Fatal("recorded without a main/subagent source")
	}
}

func TestRecordAPIRequestShapeTooLarge(t *testing.T) {
	sink := &replaySink{status: "shape_too_large"}
	l := replayLoop(t, sink)
	req := &api.Request{
		Model: "m", System: []api.SystemBlock{{Type: "text", Text: "s"}},
		Messages: []api.Message{api.StringMessage("user", "x")},
	}
	l.recordAPIRequest(req)
	if len(l.rrBadShapes) != 1 {
		t.Fatal("oversize shape not remembered")
	}
	if l.rrLastID != "" {
		t.Fatal("chain kept after failed append")
	}
}
