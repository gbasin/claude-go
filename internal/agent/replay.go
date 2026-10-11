package agent

// request-replay transcript records: api-request / api-request-shape /
// api-request-blob — real's "elegant meadow" capture (mod_0416 ust →
// appendRequestReplayRecords). Per API request, real writes a shape row
// once per unique {system,tools} hash, one blob row per unseen message
// hash, then the api-request row that chains them (base/keep/tail).
// Gated on CLAUDE_CODE_ELEGANT_MEADOW / the tengu_elegant_meadow GB flag.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"time"

	"github.com/gbasin/claude-go/internal/api"
	"github.com/gbasin/claude-go/internal/config"
	"github.com/gbasin/claude-go/internal/session"
)

// replayChainCap is real's i0n — the per-host chain map keeps at most this
// many agent/session entries before evicting the oldest.
const replayChainCap = 64

// digestExclude is real's BCe — params that never enter the api-request
// record's params object or the requestDigest (metadata, stream, thread,
// diagnostics are wire-only fields).
var digestExclude = map[string]bool{
	"metadata": true, "stream": true, "thread": true, "diagnostics": true,
}

// billingHeaderPrefix is real's VKe — system blocks that only carry the
// billing header are filtered out of the recorded shape (mod_0170 dhs).
const billingHeaderPrefix = "x-anthropic-billing-header:"

// elegantMeadow is real's yK() — env var wins when set (any non-empty
// value, even "0"), else the tengu_elegant_meadow GB flag.
func elegantMeadow() bool {
	if v, ok := os.LookupEnv("CLAUDE_CODE_ELEGANT_MEADOW"); ok {
		return v != ""
	}
	v, _ := config.GBFeature("tengu_elegant_meadow").(bool)
	return v
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// primJSON renders a scalar/object key like JSON.stringify — no HTML
// escaping, and Go's unconditional U+2028/U+2029 escapes reverted to the
// literal runes (JS leaves them raw).
func primJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "null"
	}
	s := bytes.TrimRight(buf.Bytes(), "\n")
	s = bytes.ReplaceAll(s, []byte("\\u2028"), []byte("\u2028"))
	s = bytes.ReplaceAll(s, []byte("\\u2029"), []byte("\u2029"))
	return string(s)
}

// canonicalJSON renders v compactly with sorted object keys — real's
// cee/Wge, the hash substrate for shapeHash/message hashes. With omitCC,
// cache_control keys are skipped; ccSeen, when non-nil, is set true if a
// cache_control key was encountered (whether or not it was emitted).
func canonicalJSON(v any, omitCC bool, ccSeen *bool) string {
	var b bytes.Buffer
	writeCanonical(&b, v, omitCC, ccSeen)
	return b.String()
}

func writeCanonical(b *bytes.Buffer, v any, omitCC bool, ccSeen *bool) {
	switch t := v.(type) {
	case map[string]any:
		b.WriteByte('{')
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		first := true
		for _, k := range keys {
			val := t[k]
			if k == "cache_control" && ccSeen != nil {
				*ccSeen = true
			}
			if omitCC && k == "cache_control" {
				continue
			}
			if !first {
				b.WriteByte(',')
			}
			first = false
			b.WriteString(primJSON(k))
			b.WriteByte(':')
			writeCanonical(b, val, omitCC, ccSeen)
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonical(b, e, omitCC, ccSeen)
		}
		b.WriteByte(']')
	default:
		b.WriteString(primJSON(v))
	}
}

// stripRecord is real's zD — recursively drop cache_control keys and
// (JSON-unmarshal can't produce them, but) nil members from objects.
func stripRecord(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			if k == "cache_control" || e == nil {
				continue
			}
			out[k] = stripRecord(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = stripRecord(e)
		}
		return out
	default:
		return v
	}
}

// normMsgContent is real's t0n — a string-content message is normalized
// to [{type:"text",text}] before hashing/digesting.
func normMsgContent(m any) any {
	obj, ok := m.(map[string]any)
	if !ok {
		return m
	}
	if s, isStr := obj["content"].(string); isStr {
		cp := make(map[string]any, len(obj))
		for k, v := range obj {
			cp[k] = v
		}
		cp["content"] = []any{map[string]any{"type": "text", "text": s}}
		return cp
	}
	return m
}

// msgDigest is real's GCe: {hash, json, hasCacheControl}. The hash is
// sha256 of the canonical message WITHOUT cache_control when the message
// carries one, else of the full canonical form; json keeps cache_control.
type msgDigest struct {
	hash  string
	json  string
	hasCC bool
}

func messageDigest(m any) msgDigest {
	n := normMsgContent(m)
	var seen bool
	j := canonicalJSON(n, false, &seen)
	if seen {
		return msgDigest{hash: sha256hex(canonicalJSON(n, true, nil)), json: j, hasCC: true}
	}
	return msgDigest{hash: sha256hex(j), json: j}
}

// collectCacheControl is real's Uq — every cache_control key under v is
// recorded as {path:[...to containing object], value:<cache_control>}.
func collectCacheControl(v any, path []any, out *[]map[string]any) {
	switch t := v.(type) {
	case []any:
		for i, e := range t {
			collectCacheControl(e, append(path, i), out)
		}
	case map[string]any:
		for k, e := range t {
			if k == "cache_control" {
				cp := make([]any, len(path))
				copy(cp, path)
				*out = append(*out, map[string]any{"path": cp, "value": e})
			} else {
				collectCacheControl(e, append(path, k), out)
			}
		}
	}
}

// requestDigest is real's qCe — sha256 over the request params written
// sorted-key compact, with the messages array replaced by each message's
// canonical json (cache_control included). metadata/stream/thread/
// diagnostics never enter the digest.
func requestDigest(params map[string]any, msgJSON []string) string {
	h := bytes.NewBuffer(nil)
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	first := true
	for _, k := range keys {
		v := params[k]
		if digestExclude[k] {
			continue
		}
		if first {
			h.Write([]byte("{"))
		} else {
			h.Write([]byte(","))
		}
		first = false
		h.WriteString(primJSON(k))
		h.WriteByte(':')
		if k != "messages" {
			h.WriteString(canonicalJSON(v, false, nil))
			continue
		}
		arr, ok := v.([]any)
		if !ok {
			h.WriteString(canonicalJSON(v, false, nil))
			continue
		}
		h.WriteByte('[')
		for i, m := range arr {
			if i > 0 {
				h.WriteByte(',')
			}
			if i < len(msgJSON) {
				h.WriteString(msgJSON[i])
			} else {
				h.WriteString(messageDigest(m).json)
			}
		}
		h.WriteByte(']')
	}
	if first {
		h.WriteString("{}")
	} else {
		h.WriteString("}")
	}
	sum := sha256.Sum256(h.Bytes())
	return hex.EncodeToString(sum[:])
}

// apiRequestRecord mirrors real's api-request row field order (mod_0416
// ust). Params/CacheControl carry decoded wire values.
type apiRequestRecord struct {
	Type          string           `json:"type"`
	SessionID     string           `json:"sessionId"`
	AgentID       string           `json:"agentId,omitempty"`
	ID            string           `json:"id"`
	Timestamp     string           `json:"timestamp"`
	Version       string           `json:"version"`
	QuerySource   string           `json:"querySource"`
	ShapeHash     string           `json:"shapeHash"`
	Params        any              `json:"params"`
	Withheld      []string         `json:"withheld,omitempty"`
	Base          string           `json:"base,omitempty"`
	Keep          int              `json:"keep"`
	Tail          []string         `json:"tail"`
	CacheControl  []map[string]any `json:"cacheControl"`
	RequestDigest string           `json:"requestDigest"`
}

// apiRequestShapeRecord mirrors real's api-request-shape row — written
// once per shapeHash; shape is the {system?,tools?} pair cache_control-
// stripped (zD).
type apiRequestShapeRecord struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	AgentID   string `json:"agentId,omitempty"`
	ShapeHash string `json:"shapeHash"`
	Timestamp string `json:"timestamp"`
	Version   string `json:"version"`
	Shape     any    `json:"shape"`
}

// apiRequestBlobRecord mirrors real's api-request-blob row — the raw wire
// message minus cache_control keys, deduped by hash.
type apiRequestBlobRecord struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	AgentID   string `json:"agentId,omitempty"`
	Hash      string `json:"hash"`
	Message   any    `json:"message"`
}

// recordAPIRequest is real's ust — called once per request attempt with
// the final wire params. Emits api-request-shape/api-request-blob/
// api-request rows through OnRequestReplay when the elegant-meadow gate
// is open and this loop is a main/subagent source. The laptop-memory cut
// variant (cut/cutRequestDigest) is unreachable — we ship no account
// memory, so Neo() always returns empty.
func (l *Loop) recordAPIRequest(req *api.Request) {
	if l.OnRequestReplay == nil || l.ReplaySource == "" || !elegantMeadow() {
		return
	}
	raw, err := req.MarshalJSON()
	if err != nil {
		return
	}
	var params map[string]any
	if err := json.Unmarshal(raw, &params); err != nil {
		return
	}
	msgs, _ := params["messages"].([]any)
	// real filters billing-header system blocks out of the shape (dhs)
	var sys any
	if arr, ok := params["system"].([]any); ok {
		kept := arr[:0]
		for _, b := range arr {
			if m, ok := b.(map[string]any); ok {
				if t, ok := m["text"].(string); ok &&
					len(t) >= len(billingHeaderPrefix) &&
					t[:len(billingHeaderPrefix)] == billingHeaderPrefix {
					continue
				}
			}
			kept = append(kept, b)
		}
		sys = kept
	} else {
		sys = params["system"]
	}
	shape := map[string]any{}
	if sys != nil {
		shape["system"] = sys
	}
	if tv, ok := params["tools"]; ok {
		shape["tools"] = tv
	}
	shapeHash := sha256hex(canonicalJSON(shape, true, nil))
	if l.rrBadShapes == nil {
		l.rrBadShapes = map[string]bool{}
	}
	if l.rrBadShapes[shapeHash] {
		return
	}
	// withheld: CLAUDE_CODE_EXTRA_BODY keys that merely restated a param
	// (real compares canonical values; system/tools/messages never count)
	var withheld []string
	for k, v := range extraBodyFields() {
		if k == "system" || k == "tools" || k == "messages" {
			continue
		}
		if pv, ok := params[k]; ok &&
			canonicalJSON(pv, false, nil) == canonicalJSON(v, false, nil) {
			withheld = append(withheld, k)
		}
	}
	scrubbed := map[string]any{}
	for k, v := range params {
		if k == "system" || k == "tools" || k == "messages" || digestExclude[k] {
			continue
		}
		if containsStr(withheld, k) {
			continue
		}
		scrubbed[k] = stripRecord(v)
	}
	digests := make([]msgDigest, len(msgs))
	hashes := make([]string, len(msgs))
	msgJSON := make([]string, len(msgs))
	for i, m := range msgs {
		digests[i] = messageDigest(m)
		hashes[i] = digests[i].hash
		msgJSON[i] = digests[i].json
	}
	var ccPaths []map[string]any
	collectCacheControl(sys, []any{"system"}, &ccPaths)
	if tv, ok := params["tools"]; ok {
		collectCacheControl(tv, []any{"tools"}, &ccPaths)
	}
	for i, m := range msgs {
		if digests[i].hasCC {
			collectCacheControl(m, []any{"messages", i}, &ccPaths)
		}
	}
	// chain: keep = leading message-hash prefix shared with this loop's
	// previous recorded request; base = that request's id
	keep := 0
	for keep < len(l.rrLastHashes) && keep < len(hashes) &&
		l.rrLastHashes[keep] == hashes[keep] {
		keep++
	}
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	id := api.NewUUID()
	// digest input Ve = params with the filtered system + (uncut) messages,
	// withheld keys deleted
	ve := make(map[string]any, len(params)+2)
	for k, v := range params {
		ve[k] = v
	}
	ve["system"] = sys
	ve["messages"] = msgs
	for _, k := range withheld {
		delete(ve, k)
	}
	rec := &apiRequestRecord{
		Type:        "api-request",
		SessionID:   l.Client.SessionID,
		AgentID:     l.LoopAgentID,
		ID:          id,
		Timestamp:   ts,
		Version:     api.Version,
		QuerySource: l.ReplaySource,
		ShapeHash:   shapeHash,
		Params:      scrubbed,
		Withheld:    withheld,
		Keep:        keep,
		Tail:        append([]string{}, hashes[keep:]...),
		CacheControl: func() []map[string]any {
			if ccPaths == nil {
				return []map[string]any{}
			}
			return ccPaths
		}(),
		RequestDigest: requestDigest(ve, msgJSON),
	}
	if keep > 0 {
		rec.Base = l.rrLastID
	}
	shapeRec := &apiRequestShapeRecord{
		Type: "api-request-shape", SessionID: l.Client.SessionID,
		AgentID: l.LoopAgentID, ShapeHash: shapeHash,
		Timestamp: ts, Version: api.Version,
		Shape: stripRecord(shape),
	}
	blobs := make([]session.ReplayBlob, 0, len(msgs)-keep)
	for i := keep; i < len(msgs); i++ {
		blobs = append(blobs, session.ReplayBlob{
			Hash: hashes[i],
			Record: &apiRequestBlobRecord{
				Type: "api-request-blob", SessionID: l.Client.SessionID,
				AgentID: l.LoopAgentID, Hash: hashes[i],
				Message: stripRecord(msgs[i]),
			},
		})
	}
	// real updates the chain before the async write resolves and rolls it
	// back on a non-"written" result — same here, synchronously
	l.rrLastID, l.rrLastHashes = id, hashes
	if st := l.OnRequestReplay(shapeHash, shapeRec, rec, blobs); st != "written" {
		if l.rrLastID == id {
			l.rrLastID, l.rrLastHashes = "", nil
		}
		if st == "shape_too_large" {
			l.rrBadShapes[shapeHash] = true
		}
	}
}
