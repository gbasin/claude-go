// Package session persists transcripts as JSONL under
// ~/.claude/projects/<slug>/<session-id>.jsonl, matching Claude Code.
package session

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gbasin/claude-go/internal/config"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gbasin/claude-go/internal/api"
)

type Store struct {
	Path      string
	SessionID string
	CWD       string
	f         *os.File
	mu        sync.Mutex // serializes writers (OnTurnEnd goroutine, etc)
	lastUUID  string     // uuid of the last user/assistant row (leaf for last-prompt)
	// chainParent is the uuid of the last chained row — every transcript
	// entry real appends carries parentUuid=<previous row's uuid>, so the
	// chain threads through attachments and system rows, not just messages.
	chainParent string
	sidechain   bool   // subagent transcript — records get isSidechain:true
	agentID     string // subagent transcripts stamp agentId on each row
	// curPromptID is real's requestJournal.promptId — minted per typed
	// prompt and shared by that turn's tool_result user rows.
	curPromptID string
	gitBranch   string
	gitDone     bool
	meta        sessionMeta // latched session state, re-stamped per flush
	// OnUserPrompt fires after a real user prompt row is written (its uuid
	// is the leaf just logged). Real keys file-history-snapshot rows to the
	// user message uuid — the host wires this to emit them.
	OnUserPrompt  func(uuid string)
	replayIdx     map[string]bool   // api-request-shape/api-request-blob hashes persisted
	replaySeeded  bool              // replayIdx populated from the file once
	replayForkRef bool              // file carries a fork-context-ref row
	metaStamps    map[string]string // last re-stamped value per meta row type
	relocatedCwd  string            // last cwd a {type:"relocated"} row named
}

// sessionMeta mirrors real's currentSession* fields (mod_0416 AFn): the
// metadata rows are last-wins state — the writer latches each value and
// planReAppendSessionMetadata re-stamps the whole set after appends.
type sessionMeta struct {
	customTitle   string
	aiTitle       string
	lastPrompt    string
	tag           string
	relocatedCwd  string
	agentName     string
	agentColor    string
	agentSetting  string
	mode          string // "normal"|"coordinator" — NOT the permission mode
	permMode      string
	isolationSide string
	devModsFolder string
	atis          string
	atisSet       bool
	prSet         bool
	prNumber      int
	prURL         string
	prRepo        string
	artifactCount int
}

// ISONow formats a timestamp like real's new Date().toISOString() —
// UTC, millisecond precision, trailing Z. Transcript bookkeeping rows
// (queue-operation, file-history-*, worktree-state …) use it; the
// message envelope keeps RFC3339Nano.
func ISONow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func isoNow() string { return ISONow() }

// ForkedFrom marks a message copied into a forked session.
type ForkedFrom struct {
	SessionID   string `json:"sessionId"`
	MessageUUID string `json:"messageUuid"`
}

// isoNow matches real's new Date().toISOString() — millisecond precision.

// kv is one ordered JSON member — Go maps marshal keys alphabetically but
// real's JSON.stringify keeps insertion order, and its streaming scanner
// anchors on `{"parentUuid":` at byte 0 of message rows.
type kv struct {
	k string
	v any
}

// marshalKV emits {"k":v,...} in pair order.
func marshalKV(pairs []kv) []byte {
	var b strings.Builder
	b.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(p.k)
		vb, _ := json.Marshal(p.v)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return []byte(b.String())
}

// orderedMap emits ev with the keys in order first (when present), then
// any remaining keys alphabetically — matching real's per-type wire order.
func orderedMap(order []string, ev map[string]any) []byte {
	seen := make(map[string]bool, len(order))
	var b strings.Builder
	b.WriteByte('{')
	n := 0
	put := func(k string) {
		v, ok := ev[k]
		if !ok {
			return
		}
		if n > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(v)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
		n++
		seen[k] = true
	}
	for _, k := range order {
		put(k)
	}
	var rest []string
	for k := range ev {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		put(k)
	}
	b.WriteByte('}')
	return []byte(b.String())
}

// stateKeyOrder is real's session-metadata record shapes — the rows
// planReAppendSessionMetadata pushes (mod_0416): {type, <fields>,
// sessionId} with no uuid/parentUuid (plus timestamp on the handful real
// stamps). Order mirrors each emit site's literal.
var stateKeyOrder = map[string][]string{
	"mode":            {"type", "mode", "sessionId"},
	"permission-mode": {"type", "permissionMode", "sessionId"},
	"tag":             {"type", "tag", "sessionId"},
	"agent-name":      {"type", "agentName", "sessionId"},
	"agent-color":     {"type", "agentColor", "sessionId"},
	"agent-setting":   {"type", "agentSetting", "sessionId"},
	"ai-title":        {"type", "aiTitle", "sessionId"},
	"custom-title":    {"type", "customTitle", "sessionId"},
	"last-prompt":     {"type", "lastPrompt", "leafUuid", "explicit", "rewound", "sessionId"},
	"summary":         {"type", "summary", "leafUuid"},
	"relocated":       {"type", "sessionId", "relocatedCwd"},
	"isolation-latch": {"type", "side", "sessionId"},
	"dev-mods":        {"type", "folder", "sessionId"},
	"atis-latch":      {"type", "atis", "sessionId"},
	"worktree-state":  {"type", "worktreeSession", "sessionId"},
	"ended-by-model":  {"type", "timestamp", "sessionId"},
	"continued-in":    {"type", "timestamp", "sessionId", "continuedInSessionId"},
	"pr-link":         {"type", "sessionId", "prNumber", "prUrl", "prRepository", "timestamp"},
	"frame-link":      {"type", "sessionId", "path", "frameUrl", "title", "artifactCount", "timestamp"},
	"observer-ref":    {"type", "agentId", "observerTaskId", "observerAgentType", "timestamp", "sessionId"},
	"queue-operation": {"type", "operation", "timestamp", "sessionId", "content"},
	// file-history rows are written bare too ({type,messageId,snapshot,…})
	"file-history-snapshot": {"type", "messageId", "snapshot", "isSnapshotUpdate"},
	"file-history-delta":    {"type", "messageId", "snapshotMessageId", "trackingPath", "backup", "timestamp"},
}

// stateTimestamped lists the state records real stamps a timestamp on.
var stateTimestamped = map[string]bool{
	"ended-by-model": true, "continued-in": true, "pr-link": true,
	"frame-link": true, "observer-ref": true, "queue-operation": true,
	"file-history-delta": true,
}

// chainedKeyOrder is the wire order of real's transcript entries
// (appendEntry, mod_0416): parentUuid first, then the row body, then the
// session envelope (Xar) and version/gitBranch tail.
var chainedKeyOrder = []string{
	"parentUuid", "logicalParentUuid", "isSidechain", "teamName", "agentName",
	"promptId", "agentId", "type", "subtype", "attachment", "message",
	"requestId", "content", "level", "data", "hookCount", "hookInfos",
	"hookErrors", "hookAdditionalContext", "preventedContinuation",
	"stopReason", "hasOutput", "durationMs", "messageCount", "taskId",
	"cron", "prompt", "taskKind", "cronKind", "error", "retryInMs",
	"retryAttempt", "maxRetries", "source", "compactBoundary",
	"compactMetadata", "isMeta", "isVisibleInTranscriptOnly",
	"isCompactSummary", "modelOnlyText", "uuid", "timestamp", "toolUseID",
	"parentToolUseID", "toolUseResult", "sourceToolAssistantUUID",
	"permissionMode", "origin", "promptSource", "turnOrigin", "turnPosition",
	"userType", "entrypoint", "cwd", "sessionId", "version", "gitBranch",
	"slug", "forkedFrom",
}

func Slug(cwd string) string {
	// real resolves symlinks for the project slug (/tmp → /private/tmp
	// on macOS — captured session dirs use -private-tmp-…)
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = r
	}
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, filepath.Clean(cwd))
}

func dir(cwd string) string {
	return filepath.Join(config.ClaudeDir(), "projects", Slug(cwd))
}

// ProjectsDir exposes the transcript dir for a cwd (resume uses it to
// locate the leaf anchor in the same file it continues).
func ProjectsDir(cwd string) string { return dir(cwd) }

// Open creates (or appends to) the transcript for sessionID in cwd.
// Existing content is tail-scanned so the session state real persists
// (mode/permission-mode/atis-latch/titles/tag/agent identity, the
// last-prompt leaf, and the uuid chain anchor) survives a resume — the
// same restore pass real's planReAppendSessionMetadata runs on adopt.
func Open(cwd, sessionID string) (*Store, error) {
	d := dir(cwd)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return nil, err
	}
	p := filepath.Join(d, sessionID+".jsonl")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	s := &Store{Path: p, SessionID: sessionID, CWD: cwd, f: f}
	s.meta.atisSet = true // standard sessions latch atis:"" (FY())
	s.restoreMeta()
	return s, nil
}

// restoreMeta tail-scans the existing transcript for the state rows real
// re-reads on adopt (mod_0416 y2/planReAppendSessionMetadata) plus the
// chain anchors the writer continues from.
func (s *Store) restoreMeta() {
	b, err := os.ReadFile(s.Path)
	if err != nil || len(b) == 0 {
		return
	}
	const tail = 512 << 10 // real scans a bounded tail for the same purpose
	if len(b) > tail {
		b = b[len(b)-tail:]
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		ln := sc.Bytes()
		if !bytes.Contains(ln, []byte(`"type"`)) {
			continue
		}
		var m map[string]any
		if json.Unmarshal(ln, &m) != nil {
			continue
		}
		s.applyMetaRow(m)
	}
}

// applyMetaRow folds one scanned row into the latched session state and
// chain anchors.
func (s *Store) applyMetaRow(m map[string]any) {
	str := func(k string) string { v, _ := m[k].(string); return v }
	switch t := str("type"); t {
	case "mode":
		s.meta.mode = str("mode")
	case "permission-mode":
		s.meta.permMode = str("permissionMode")
	case "tag":
		s.meta.tag = str("tag")
	case "agent-name":
		s.meta.agentName = str("agentName")
	case "agent-color":
		s.meta.agentColor = str("agentColor")
	case "agent-setting":
		s.meta.agentSetting = str("agentSetting")
	case "relocated":
		if s.meta.relocatedCwd == "" { // real: restored value is ??= (first wins)
			s.meta.relocatedCwd = str("relocatedCwd")
		}
	case "isolation-latch":
		s.meta.isolationSide = str("side")
	case "dev-mods":
		s.meta.devModsFolder = str("folder")
	case "atis-latch":
		s.meta.atis, s.meta.atisSet = str("atis"), true
	case "custom-title":
		if s.meta.customTitle == "" {
			s.meta.customTitle = str("customTitle")
		}
	case "ai-title":
		if s.meta.aiTitle == "" {
			s.meta.aiTitle = str("aiTitle")
		}
	case "pr-link":
		s.meta.prSet = true
		s.meta.prURL, s.meta.prRepo = str("prUrl"), str("prRepository")
		if n, ok := m["prNumber"].(float64); ok {
			s.meta.prNumber = int(n)
		}
	case "frame-link":
		if n, ok := m["artifactCount"].(float64); ok && int(n) > s.meta.artifactCount {
			s.meta.artifactCount = int(n)
		}
	case "last-prompt":
		if p := str("lastPrompt"); p != "" && s.meta.lastPrompt == "" {
			s.meta.lastPrompt = p
		}
	}
	// chain anchors: any uuid'd row can parent the next append; the leaf
	// semantic tracks user/assistant rows only (real's Cle).
	if u := str("uuid"); u != "" {
		s.chainParent = u
		t := str("type")
		if !truthy(m["isSidechain"]) && (t == "user" || t == "assistant") {
			s.lastUUID = u
		}
	}
}

// isoMillis renders a timestamp the way real's new Date().toISOString()
// does: millisecond precision, trailing Z.
func isoMillis(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// NewID generates a session/message uuid.
func NewID() string { return newUUID() }

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b)
}

func formatUUID(b [16]byte) string {
	const hex = "0123456789abcdef"
	var s [36]byte
	j := 0
	for i, c := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			s[j] = '-'
			j++
		}
		s[j] = hex[c>>4]
		s[j+1] = hex[c&0xf]
		j += 2
	}
	return string(s[:])
}

// Log is LogMessage (satisfies repl.Store).
func (s *Store) Log(m api.Message) error { return s.LogMessage(m) }

// LogMessage appends a user/assistant message event.
func (s *Store) LogMessage(m api.Message) error {
	if m.IsCompactSummary {
		return s.logMessage(m, true, "Compact summary")
	}
	err := s.logMessage(m, false, "")
	// real flushes session metadata after appends (insertMessageChain →
	// reAppendSessionMetadata): the last-prompt stamp plus every latched
	// state row re-lands per prompt. last-prompt stamps only on real user
	// prompts — tool_result rows ride the user role but aren't prompts
	// (real's leafUuid semantic).
	if err == nil && m.Role == "user" && isPromptContent(m.Content) {
		s.restampMeta(m)
	}
	return err
}

// normalizeLastPrompt mirrors real's normalizeLastPrompt: newlines
// collapse to spaces, then truncate at 200 chars with an ellipsis.
func normalizeLastPrompt(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\n", " "))
	if len(p) > 200 {
		p = strings.TrimSpace(p[:200]) + "…"
	}
	return p
}

// restampMeta re-appends the session-state rows real flushes per prompt
// (planReAppendSessionMetadata's V.push order): last-prompt first, then
// titles/tag/relocated/agent identity/mode/permission-mode/latches, then
// pr-link and the frame-link count variant.
func (s *Store) restampMeta(m api.Message) {
	s.mu.Lock()
	leaf, meta := s.lastUUID, s.meta
	s.mu.Unlock()
	var prompt string
	if prompt = normalizeLastPrompt(contentText(m.Content)); prompt == "" {
		prompt = meta.lastPrompt
	} else {
		s.mu.Lock()
		s.meta.lastPrompt = prompt
		s.mu.Unlock()
		s.mu.Lock()
		leaf := s.lastUUID
		s.mu.Unlock()
		if s.OnUserPrompt != nil {
			s.OnUserPrompt(leaf)
		}
	}
	emit := func(rec map[string]any) {
		t, _ := rec["type"].(string)
		_ = s.logState(rec, stateKeyOrder[t])
	}
	if leaf != "" {
		rec := map[string]any{"type": "last-prompt", "leafUuid": leaf, "explicit": true}
		if prompt != "" {
			rec["lastPrompt"] = prompt
		}
		emit(rec)
	}
	if meta.customTitle != "" {
		emit(map[string]any{"type": "custom-title", "customTitle": meta.customTitle})
	}
	if meta.aiTitle != "" {
		emit(map[string]any{"type": "ai-title", "aiTitle": meta.aiTitle})
	}
	if meta.tag != "" {
		emit(map[string]any{"type": "tag", "tag": meta.tag})
	}
	if meta.relocatedCwd != "" {
		emit(map[string]any{"type": "relocated", "relocatedCwd": meta.relocatedCwd})
	}
	if meta.agentName != "" {
		emit(map[string]any{"type": "agent-name", "agentName": meta.agentName})
	}
	if meta.agentColor != "" {
		emit(map[string]any{"type": "agent-color", "agentColor": meta.agentColor})
	}
	if meta.agentSetting != "" {
		emit(map[string]any{"type": "agent-setting", "agentSetting": meta.agentSetting})
	}
	if meta.mode != "" {
		emit(map[string]any{"type": "mode", "mode": meta.mode})
	}
	if meta.permMode != "" {
		emit(map[string]any{"type": "permission-mode", "permissionMode": meta.permMode})
	}
	if meta.isolationSide != "" {
		emit(map[string]any{"type": "isolation-latch", "side": meta.isolationSide})
	}
	if meta.devModsFolder != "" {
		emit(map[string]any{"type": "dev-mods", "folder": meta.devModsFolder})
	}
	if meta.atisSet {
		emit(map[string]any{"type": "atis-latch", "atis": meta.atis})
	}
	if meta.prSet {
		emit(map[string]any{"type": "pr-link", "prNumber": meta.prNumber,
			"prUrl": meta.prURL, "prRepository": meta.prRepo})
	}
	// the flush's frame-link variant carries only the artifact count
	if meta.artifactCount > 0 {
		emit(map[string]any{"type": "frame-link", "artifactCount": meta.artifactCount})
	}
}

// isPromptContent reports whether a user-role message is a typed prompt
// (text) rather than a tool_result carrier.
func isPromptContent(c json.RawMessage) bool {
	var str string
	if json.Unmarshal(c, &str) == nil {
		return true // string content = typed text
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(c, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return false
		}
	}
	return len(blocks) > 0
}

// LogCompactSummary appends the user message carrying the post-compact
// summary, flagged isCompactSummary with modelOnlyText like real CC.
func (s *Store) LogCompactSummary(m api.Message, modelOnlyText string) error {
	return s.logMessage(m, true, modelOnlyText)
}

func (s *Store) logMessage(m api.Message, compact bool, modelOnlyText string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var raw json.RawMessage
	role := m.Role
	if role == "assistant" && len(m.Raw) > 0 {
		// real persists the full API message: {id,type:"message",role,model,
		// content,stop_reason,stop_sequence,usage}
		raw = m.Raw
	}
	if len(raw) == 0 {
		mb, _ := json.Marshal(struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}{role, m.Content})
		raw = mb
	}
	uuid := newUUID()
	// real rotates promptId per typed prompt; that turn's tool_result user
	// rows share it
	if role == "user" && isPromptContent(m.Content) {
		s.curPromptID = newUUID()
	}
	if s.curPromptID == "" {
		s.curPromptID = newUUID()
	}
	prompt := role == "user" && isPromptContent(m.Content)
	var parent any // real writes "parentUuid":null at the chain root
	if s.chainParent != "" {
		parent = s.chainParent
	}
	pairs := []kv{
		{"parentUuid", parent},
		{"isSidechain", s.sidechain},
	}
	if role == "user" {
		pairs = append(pairs, kv{"promptId", s.curPromptID})
	}
	if s.agentID != "" {
		pairs = append(pairs, kv{"agentId", s.agentID})
	}
	if role == "assistant" {
		// real's assistant rows order message/requestId before type
		pairs = append(pairs, kv{"message", raw}, kv{"type", role})
	} else {
		pairs = append(pairs, kv{"type", role}, kv{"message", raw})
	}
	if compact {
		pairs = append(pairs, kv{"isCompactSummary", true})
		if modelOnlyText != "" {
			pairs = append(pairs, kv{"modelOnlyText", modelOnlyText})
		}
	}
	pairs = append(pairs, kv{"uuid", uuid}, kv{"timestamp", isoNow()})
	if prompt && s.meta.permMode != "" {
		pairs = append(pairs, kv{"permissionMode", s.meta.permMode})
	}
	pairs = append(pairs,
		kv{"userType", "external"}, kv{"entrypoint", "cli"},
		kv{"cwd", s.CWD}, kv{"sessionId", s.SessionID},
		kv{"version", "2.1.296"})
	if gb := s.branch(); gb != "" {
		pairs = append(pairs, kv{"gitBranch", gb})
	}
	b := marshalKV(pairs)
	if _, err := s.f.Write(append(b, '\n')); err != nil {
		return err
	}
	s.lastUUID = uuid
	s.chainParent = uuid
	return nil
}

// logState appends a session-state record — the rows real writes outside
// the uuid chain (no parentUuid/uuid; sessionId and, on the timestamped
// subset, a millis timestamp get filled in when absent).
func (s *Store) logState(ev map[string]any, order []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, _ := ev["type"].(string)
	delete(ev, "uuid")
	delete(ev, "parentUuid")
	if _, ok := ev["sessionId"]; !ok {
		ev["sessionId"] = s.SessionID
	}
	if stateTimestamped[t] {
		if _, ok := ev["timestamp"]; !ok {
			ev["timestamp"] = isoNow()
		}
	}
	b := orderedMap(order, ev)
	_, err := s.f.Write(append(b, '\n'))
	return err
}

// branch caches the git branch probe (real resolves it once per write
// batch; once per store is close enough and avoids a git fork per row).
func (s *Store) branch() string {
	if !s.gitDone {
		s.gitBranch = currentGitBranch(s.CWD)
		s.gitDone = true
	}
	return s.gitBranch
}

// LogRecord writes an arbitrary transcript record. Two emit paths match
// real's writer:
//   - session-state types (stateKeyOrder — mode, permission-mode,
//     last-prompt, titles, latches, pr-link, queue-operation, …) land
//     bare: {type, <fields>, sessionId}, no uuid/parentUuid.
//   - everything else is a chained transcript entry: uuid, timestamp,
//     sessionId, the session envelope, and parentUuid = the previous
//     row's uuid (null at the root) — real chains every append.
//
// progress rows stamp parentUuid but do not advance the chain (real's
// boundary-cleared semantic).

// LogBare appends a record exactly as given — real's non-transcript rows
// (last-wins state, boundary-cleared, accumulate) carry only their
// emit-site literal: no uuid, no parentUuid, no synthesized timestamp.
// Verified against captured 296 transcripts: ai-title, queue-operation,
// file-history-*, fork-context-ref, cost-state, atis-latch all write bare.
func (s *Store) LogBare(ev map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b []byte
	if order, ok := stateKeyOrder[ev["type"].(string)]; ok {
		// real's emit-site key order is known — write in it verbatim
		b = orderedMap(order, ev)
	} else {
		b, _ = json.Marshal(ev)
	}
	_, err := s.f.Write(append(b, '\n'))
	return err
}

// logBare appends a session-metadata record verbatim. Real writes these
// rows ({type:"mode"}, {type:"permission-mode"}, {type:"ai-title"},

// LogRecord writes an arbitrary transcript record (summary/system/etc).
func (s *Store) LogRecord(ev map[string]any) error {
	t, _ := ev["type"].(string)
	if order, ok := stateKeyOrder[t]; ok {
		return s.logState(ev, order)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev["uuid"] == nil {
		ev["uuid"] = newUUID()
	}
	if ev["sessionId"] == nil {
		ev["sessionId"] = s.SessionID
	}
	if ev["timestamp"] == nil {
		ev["timestamp"] = isoNow()
	}
	if _, has := ev["parentUuid"]; !has {
		var parent any // real writes "parentUuid":null at the root
		if s.chainParent != "" {
			parent = s.chainParent
		}
		ev["parentUuid"] = parent
	}
	if ev["isSidechain"] == nil {
		ev["isSidechain"] = s.sidechain
	}
	if ev["userType"] == nil {
		ev["userType"] = "external"
	}
	if ev["entrypoint"] == nil {
		ev["entrypoint"] = "cli"
	}
	if ev["cwd"] == nil {
		ev["cwd"] = s.CWD
	}
	if ev["version"] == nil {
		ev["version"] = "2.1.296"
	}
	if ev["gitBranch"] == nil {
		if gb := s.branch(); gb != "" {
			ev["gitBranch"] = gb
		}
	}
	b := orderedMap(chainedKeyOrder, ev)
	if _, err := s.f.Write(append(b, '\n')); err != nil {
		return err
	}
	if t != "progress" {
		if u, _ := ev["uuid"].(string); u != "" {
			s.chainParent = u
		}
	}
	return nil
}

// LogSummary writes a summary record (branch leaf marker/title) — real's
// {type:"summary",summary,leafUuid} lands bare, off the uuid chain.
func (s *Store) LogSummary(summary, leafUUID string) error {
	return s.LogRecord(map[string]any{
		"type": "summary", "summary": summary, "leafUuid": leafUUID,
	})
}

// LogCompactBoundary marks where compaction replaced history.
func (s *Store) LogCompactBoundary(preTokens, kept int) error {
	return s.LogCompactBoundaryTrig(preTokens, kept, "auto")
}

// LogCompactBoundaryTrig is LogCompactBoundary with the real trigger field.
// Real stamps parentUuid:null + logicalParentUuid=<last message uuid> —
// the boundary breaks the uuid chain but keeps the logical link.
func (s *Store) LogCompactBoundaryTrig(preTokens, kept int, trigger string) error {
	s.mu.Lock()
	leaf := s.lastUUID
	s.mu.Unlock()
	return s.LogRecord(map[string]any{
		"type": "system", "subtype": "compact_boundary",
		"parentUuid": nil, "compactBoundary": true, "isMeta": false,
		"logicalParentUuid": leaf,
		"compactMetadata": map[string]any{
			"trigger": trigger, "preTokens": preTokens, "messagesKept": kept,
		},
	})
}

// LogInit writes the {type:"system",subtype:"init"} record CC emits at
// session start.
func (s *Store) LogInit(model string) error {
	return s.LogInitMeta(model, "", "", "", "")
}

// LogInitMeta is LogInit carrying optional metadata when set: spawn
// lineage (--parent-session-id), agent type, sys-prompt snapshot, and the
// session's permission mode (--inherit-permission-mode restores it on
// resume).
func (s *Store) LogInitMeta(model, parentSessionID, agentType, sysSnap, permMode string) error {
	rec := map[string]any{
		"type": "system", "subtype": "init",
		"model": model, "cwd": s.CWD,
		"version": "2.1.296", "userType": "external",
		"gitBranch": currentGitBranch(s.CWD), "entrypoint": "cli",
	}
	if parentSessionID != "" {
		rec["parentSessionId"] = parentSessionID
	}
	if p := os.Getenv("CLAUDE_GO_PARENT_SESSION_ID"); p != "" {
		rec["parentSessionId"] = p // --parent-session-id lineage
	}
	if agentType != "" {
		rec["agentType"] = agentType
	}
	if sysSnap != "" {
		rec["systemPromptSnapshot"] = sysSnap
	}
	if permMode != "" {
		rec["permissionMode"] = permMode
	}
	return s.LogRecord(rec)
}

// InitSysPromptSnapshot reads the init record's systemPromptSnapshot
// (resume restore — real reuses it verbatim until compaction).
func InitSysPromptSnapshot(cwd, sessionID string) string {
	b, err := os.ReadFile(filepath.Join(dir(cwd), sessionID+".jsonl"))
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(string(b), "\n") {
		var rec struct {
			Type     string `json:"type"`
			Subtype  string `json:"subtype"`
			Snapshot string `json:"systemPromptSnapshot"`
		}
		if json.Unmarshal([]byte(ln), &rec) != nil {
			continue
		}
		if rec.Type == "system" && rec.Subtype == "init" && rec.Snapshot != "" {
			return rec.Snapshot
		}
	}
	return ""
}

// InitMode reads the init record's permissionMode back (resume restore).
// The standalone {type:"permission-mode"} state row wins when present —
// it's real's last-wins record, stamped after the init row.
func InitMode(cwd, sessionID string) string {
	b, err := os.ReadFile(filepath.Join(dir(cwd), sessionID+".jsonl"))
	if err != nil {
		return ""
	}
	var mode, initMode string
	for _, ln := range strings.Split(string(b), "\n") {
		var rec struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			Mode    string `json:"permissionMode"`
		}
		if json.Unmarshal([]byte(ln), &rec) != nil {
			continue
		}
		if rec.Type == "permission-mode" && rec.Mode != "" {
			mode = rec.Mode
		}
		if rec.Subtype == "init" && initMode == "" {
			initMode = rec.Mode
		}
	}
	if mode != "" {
		return mode
	}
	return initMode
}

// LogAttachment appends an attachment record — queued commands, hook
// system messages, file mentions, memory context, etc. Real's
// {type:"attachment"} rows are full transcript records: they carry the
// message envelope (uuid, parentUuid, isSidechain, timestamp, sessionId +
// session_id, cwd, version, gitBranch, userType, entrypoint) and DO join
// the parentUuid chain — the next message's parentUuid is the attachment
// uuid (verified on captured rows).
func (s *Store) LogAttachment(att map[string]any) error {
	return s.LogAttachmentMeta(att, nil)
}

// LogAttachmentMeta is LogAttachment plus top-level extras real stamps
// beside the envelope (rendered, renderedInHumanTurn, isMeta).
func (s *Store) LogAttachmentMeta(att, extra map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	uuid := newUUID()
	e := map[string]any{
		"type":        "attachment",
		"attachment":  att,
		"uuid":        uuid,
		"timestamp":   ISONow(),
		"isSidechain": s.sidechain,
		"sessionId":   s.SessionID,
		"session_id":  s.SessionID,
		"userType":    "external",
		"entrypoint":  "cli",
		"cwd":         s.CWD,
		"version":     "2.1.296",
	}
	if s.chainParent != "" {
		e["parentUuid"] = s.chainParent
	} else {
		e["parentUuid"] = nil // real writes an explicit null
	}
	if b := currentGitBranch(s.CWD); b != "" {
		e["gitBranch"] = b
	}
	for k, v := range extra {
		e[k] = v
	}
	s.chainParent = uuid
	b, _ := json.Marshal(e)
	_, err := s.f.Write(append(b, '\n'))
	return err
}

// LogProgress appends a progress record (tool progress, heartbeat, hook
// progress). Progress records carry toolUseID/parentToolUseID and do not
// join the parentUuid message chain.
func (s *Store) LogProgress(toolUseID, parentToolUseID string, data map[string]any) error {
	ev := map[string]any{"type": "progress", "data": data}
	if toolUseID != "" {
		ev["toolUseID"] = toolUseID
	}
	if parentToolUseID != "" {
		ev["parentToolUseID"] = parentToolUseID
	}
	return s.LogRecord(ev)
}

// StampMode latches the session's interaction mode and stamps
// {type:"mode",mode,sessionId} — real's saveMode/_2 ("coordinator" for
// coordinator sessions, "normal" otherwise; NOT the permission mode,
// which is a separate permission-mode record).
func (s *Store) StampMode(mode string) error {
	s.mu.Lock()
	s.meta.mode = mode
	s.mu.Unlock()
	return s.LogRecord(map[string]any{"type": "mode", "mode": mode})
}

// StampPermMode latches and stamps {type:"permission-mode",
// permissionMode,sessionId} — real's savePermissionMode/mlr. Called on
// --permission-mode, the shift+tab cycle, and programmatic mode changes.
func (s *Store) StampPermMode(mode string) error {
	s.mu.Lock()
	s.meta.permMode = mode
	s.mu.Unlock()
	return s.LogRecord(map[string]any{"type": "permission-mode", "permissionMode": mode})
}

// LatchPermMode updates the latched permission mode without writing a
// row — real stamps {type:"permission-mode"} only via the metadata
// re-append (prompt flush / materialize / exit), not on the cycle.
func (s *Store) LatchPermMode(mode string) {
	s.mu.Lock()
	s.meta.permMode = mode
	s.mu.Unlock()
}

// LogAtisLatch stamps {type:"atis-latch",atis,sessionId} — real's
// conversationAtisLatch (FY/G1e): a per-session latch re-stamped in every
// metadata flush; "" for standard sessions.
func (s *Store) LogAtisLatch(atis string) error {
	s.mu.Lock()
	s.meta.atis, s.meta.atisSet = atis, true
	s.mu.Unlock()
	return s.LogRecord(map[string]any{"type": "atis-latch", "atis": atis})
}

// LogTitle sets the session's custom title. It writes both a transcript
// record ({type:"custom-title"}) and <projectDir>/<sessionID>/custom-title.json
// ({customTitle}), matching real CC. A summary record is also appended as a
// named leaf marker.
func (s *Store) LogTitle(title string) error {
	s.mu.Lock()
	s.meta.customTitle = title
	leaf := s.lastUUID
	s.mu.Unlock()
	if err := s.LogSummary(title, leaf); err != nil {
		return err
	}
	if err := s.LogRecord(map[string]any{
		"type": "custom-title", "customTitle": title,
	}); err != nil {
		return err
	}
	return writeTitleFile(filepath.Dir(s.Path), s.SessionID, title)
}

// LogAITitle writes {type:"ai-title",aiTitle,sessionId} — real's
// saveAiGeneratedTitle (mod_0416): auto-generated from the first turn and
// re-stamped in every metadata flush.
func (s *Store) LogAITitle(title string) error {
	s.mu.Lock()
	s.meta.aiTitle = title
	s.mu.Unlock()
	return s.LogRecord(map[string]any{
		"type": "ai-title", "aiTitle": title,
	})
}

// LogEndedByModel writes {type:"ended-by-model",timestamp,sessionId} —
// real's markSessionEndedByModel (TJr): EndConversation's call site
// stamps it before aborting; the picker filters ended sessions.
func (s *Store) LogEndedByModel() error {
	return s.LogBare(map[string]any{
		"type":      "ended-by-model",
		"timestamp": isoMillis(time.Now()),
		"sessionId": s.SessionID,
	})
}

// LogPRLink writes {type:"pr-link",sessionId,prNumber,prUrl,prRepository,
// timestamp} — real's linkSessionToPR record when a session is
// associated with a PR (re-stamped in metadata flushes).
func (s *Store) LogPRLink(number int, url, repo string) error {
	s.mu.Lock()
	s.meta.prSet, s.meta.prNumber, s.meta.prURL, s.meta.prRepo = true, number, url, repo
	s.mu.Unlock()
	return s.LogRecord(map[string]any{
		"type": "pr-link", "prNumber": number,
		"prUrl": url, "prRepository": repo,
	})
}

// LogContinuedIn writes {type:"continued-in",timestamp,sessionId,
// continuedInSessionId} — real's Jns/TRe: stamped on the source
// transcript when the session's continuation moves to a different
// session id (background/handoff spawn). Picker: the superseded
// transcript is hidden.
func (s *Store) LogContinuedIn(newSessionID string) error {
	return s.LogBare(map[string]any{
		"type": "continued-in", "timestamp": ISONow(),
		"sessionId": s.SessionID, "continuedInSessionId": newSessionID,
	})
}

// LogContinuedInTo appends the continued-in marker to another session's
// transcript (the --bg -r handoff: the resumed source continues under the
// new bg session id). onlyIfExists — a missing source file stays missing.
func LogContinuedInTo(cwd, srcID, dstID string) error {
	return AppendContinuedIn(filepath.Join(dir(cwd), srcID+".jsonl"), srcID, dstID)
}

// AppendContinuedIn appends the continued-in marker to an arbitrary
// transcript path (real's TRe writes it onto the source session file).
func AppendContinuedIn(path, srcID, dstID string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]any{
		"type": "continued-in", "timestamp": ISONow(),
		"sessionId": srcID, "continuedInSessionId": dstID,
	})
	_, werr := f.Write(append(b, '\n'))
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// LogWorktreeState writes {type:"worktree-state",worktreeSession,sessionId}
// — real's saveWorktreeState (AM), stamped when the session enters a
// worktree; ws is nil on exit (real emits worktreeSession:null).
func (s *Store) LogWorktreeState(ws map[string]any) error {
	return s.LogBare(map[string]any{
		"type": "worktree-state", "worktreeSession": ws,
		"sessionId": s.SessionID,
	})
}

// LogForkContextRef writes {type:"fork-context-ref",agentId,
// parentSessionId,parentLastUuid,contextLength} — real's fJr: a fork
// subagent that references the parent's conversation instead of carrying
// copies stamps it into its own (subagent) transcript so a resume can
// hydrate the prefix. Captured rows carry no sessionId/uuid/timestamp.
func (s *Store) LogForkContextRef(agentID, parentSessionID, parentLastUUID string, contextLength int) error {
	return s.LogBare(map[string]any{
		"type": "fork-context-ref", "agentId": agentID,
		"parentSessionId": parentSessionID, "parentLastUuid": parentLastUUID,
		"contextLength": contextLength,
	})
}

// LogContentReplacement writes {type:"content-replacement",sessionId,
// agentId?,replacements:[{kind,toolUseId,replacement}]} — real's
// insertContentReplacement: tool-result content the wire rewrote
// (persisted-message budget) is recorded so resume can re-apply it.
// Route-by-agent: agentID non-empty targets the subagent transcript.
func (s *Store) LogContentReplacement(replacements []any, agentID string) error {
	rec := map[string]any{
		"type": "content-replacement", "sessionId": s.SessionID,
		"replacements": replacements,
	}
	if agentID != "" {
		rec["agentId"] = agentID
	}
	return s.LogBare(rec)
}

// LogFileHistorySnapshot writes real's per-user-message checkpoint row:
// {type:"file-history-snapshot",messageId:<user-msg-uuid>,snapshot:
// {messageId,trackedFileBackups,timestamp},isSnapshotUpdate}.
func (s *Store) LogFileHistorySnapshot(messageID string, trackedFileBackups map[string]any, isSnapshotUpdate bool) error {
	return s.LogBare(map[string]any{
		"type": "file-history-snapshot", "messageId": messageID,
		"snapshot": map[string]any{
			"messageId":          messageID,
			"trackedFileBackups": trackedFileBackups,
			"timestamp":          ISONow(),
		},
		"isSnapshotUpdate": isSnapshotUpdate,
	})
}

// LogFileHistoryDelta writes {type:"file-history-delta",messageId,
// snapshotMessageId,trackingPath,backup,timestamp} — real's Jes: a file
// newly tracked mid-turn merges its pre-edit backup into the latest
// snapshot.
func (s *Store) LogFileHistoryDelta(messageID, snapshotMessageID, trackingPath string, backup map[string]any) error {
	return s.LogBare(map[string]any{
		"type": "file-history-delta", "messageId": messageID,
		"snapshotMessageId": snapshotMessageID, "trackingPath": trackingPath,
		"backup": backup, "timestamp": ISONow(),
	})
}

// LogRelocated writes {type:"relocated",sessionId,relocatedCwd} — real's
// relocateSessionTranscript (mod_0416 cnt) stamps it when the session's
// cwd changes (the transcript relocates to the new project dir; ours
// keeps the file in place but stamps the same marker).
func (s *Store) LogRelocated(newCwd string) error {
	s.mu.Lock()
	if s.meta.relocatedCwd == newCwd {
		s.mu.Unlock()
		return nil
	}
	s.meta.relocatedCwd = newCwd
	s.mu.Unlock()
	return s.LogRecord(map[string]any{
		"type": "relocated", "relocatedCwd": newCwd,
	})
}

// LogIsolationLatch writes {type:"isolation-latch",side,sessionId} —
// real's saveIsolationLatch (Xhe): stamps the tool-isolation latch side
// when it first engages. No-op-safe: call only when a side is known.
func (s *Store) LogIsolationLatch(side string) error {
	s.mu.Lock()
	if s.meta.isolationSide == side {
		s.mu.Unlock()
		return nil
	}
	s.meta.isolationSide = side
	s.mu.Unlock()
	return s.LogRecord(map[string]any{
		"type": "isolation-latch", "side": side,
	})
}

// LogDevModsFolder writes {type:"dev-mods",folder,sessionId} — real's
// saveDevModsFolder (NJr): the dev-mods folder a session loaded.
func (s *Store) LogDevModsFolder(folder string) error {
	s.mu.Lock()
	if s.meta.devModsFolder == folder {
		s.mu.Unlock()
		return nil
	}
	s.meta.devModsFolder = folder
	s.mu.Unlock()
	return s.LogRecord(map[string]any{
		"type": "dev-mods", "folder": folder,
	})
}

// LogFrameLink writes {type:"frame-link",sessionId,path,frameUrl,title,
// artifactCount,timestamp} — real's stamp when the Artifact tool
// publishes a frame page (mod_1663). artifactCount is the session's
// known-frame count.
func (s *Store) LogFrameLink(path, frameURL, title string, artifactCount int) error {
	s.mu.Lock()
	s.meta.artifactCount = artifactCount
	s.mu.Unlock()
	return s.LogRecord(map[string]any{
		"type": "frame-link", "path": path, "frameUrl": frameURL,
		"title": title, "artifactCount": artifactCount,
	})
}

// LogObserverRef writes {type:"observer-ref",agentId?,observerTaskId,
// observerAgentType,timestamp} — real's recordObserverRef (gJr): stamped
// when an agent-observer pairing delivers a batch. claude-go has no
// observer feature; the writer exists for parity callers.
func (s *Store) LogObserverRef(agentID, observerTaskID, observerAgentType string) error {
	rec := map[string]any{
		"type": "observer-ref", "observerTaskId": observerTaskID,
		"observerAgentType": observerAgentType,
	}
	if agentID != "" {
		rec["agentId"] = agentID
	}
	return s.LogRecord(rec)
}

// LogAgentMeta writes the teammate/agent identity records real persists
// ("always" merge policy): tag, agent-name, agent-color, agent-setting.
// Real stamps agent-name/agent-color immediately (fAe/XKt) and latches
// all four for the metadata flush; ours stamps the set record and
// latches it for re-stamps.
func (s *Store) LogAgentMeta(tag, agentName, agentColor, agentSetting string) error {
	rec := map[string]any{}
	s.mu.Lock()
	switch {
	case tag != "":
		rec["type"] = "tag"
		rec["tag"] = tag
		s.meta.tag = tag
	case agentName != "":
		rec["type"] = "agent-name"
		rec["agentName"] = agentName
		s.meta.agentName = agentName
	case agentColor != "":
		rec["type"] = "agent-color"
		rec["agentColor"] = agentColor
		s.meta.agentColor = agentColor
	case agentSetting != "":
		rec["type"] = "agent-setting"
		rec["agentSetting"] = agentSetting
		s.meta.agentSetting = agentSetting
	default:
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	return s.LogRecord(rec)
}

// LogMode writes {type:"mode",mode,sessionId} — real's saveMode row. The
// value is the session mode ("normal" for a plain REPL, "coordinator" for
// a coordinator session), NOT the permission mode. Real stamps it through
// the metadata re-append (file materialize, resume adopt, post-compact,
// exit).
func (s *Store) LogMode(mode string) error {
	return s.LogBare(map[string]any{
		"type": "mode", "mode": mode, "sessionId": s.SessionID,
	})
}

// LogPermissionMode writes {type:"permission-mode",permissionMode,
// sessionId} — real's savePermissionMode row (default|acceptEdits|plan|
// bypassPermissions|auto|dontAsk), also via the metadata re-append. Real
// stamps nothing at mode-change time.
func (s *Store) LogPermissionMode(mode string) error {
	return s.LogBare(map[string]any{
		"type": "permission-mode", "permissionMode": mode,
		"sessionId": s.SessionID,
	})
}

// StampMeta mirrors real's reAppendSessionMetadata: the metadata rows that
// get re-written when the session file materializes, on resume adopt,
// after compaction, and at exit. Rows identical to the last stamp are
// skipped (real dedupes against a tail scan of the file).
func (s *Store) StampMeta(mode, permMode string) error {
	s.mu.Lock()
	if s.metaStamps == nil {
		s.metaStamps = map[string]string{}
	}
	writeMode := mode != "" && s.metaStamps["mode"] != mode
	writePerm := permMode != "" && s.metaStamps["permission-mode"] != permMode
	s.metaStamps["mode"] = mode
	s.metaStamps["permission-mode"] = permMode
	s.mu.Unlock()
	if writeMode {
		if err := s.LogMode(mode); err != nil {
			return err
		}
	}
	if writePerm {
		return s.LogPermissionMode(permMode)
	}
	return nil
}

// Relocate mirrors real's relocateSessionTranscript (mod_0416 cnt): /cd
// carries the session with it — the <oldslug>/<sid>.jsonl transcript (and
// the <sid>/ sidecar dir holding custom-title.json etc.) moves to the new
// cwd's project dir, then a bare {type:"relocated"} row is stamped. When
// the project dir is unchanged the file stays put and only the row lands
// — and only when the relocated cwd actually changed.
func (s *Store) Relocate(newCWD string) error {
	s.mu.Lock()
	oldPath := s.Path
	dest := filepath.Join(dir(newCWD), s.SessionID+".jsonl")
	move := filepath.Clean(dest) != filepath.Clean(oldPath)
	s.mu.Unlock()

	if move {
		newDir := dir(newCWD)
		if err := os.MkdirAll(newDir, 0o755); err != nil {
			return err
		}
		// real sets an existing destination aside as
		// <file>.superseded-<unixms> before moving (LZe)
		if _, err := os.Stat(dest); err == nil {
			setAside := fmt.Sprintf("%s.superseded-%d", dest, time.Now().UnixMilli())
			if err := os.Rename(dest, setAside); err != nil {
				return err
			}
		}
		if err := os.Rename(oldPath, dest); err != nil {
			// old file missing (ENOENT): real logs "old file missing" and
			// skips the stamp; the session still repoints
			if !os.IsNotExist(err) {
				return err
			}
		}
		// move the per-session sidecar dir (custom-title.json et al)
		oldSub := filepath.Join(filepath.Dir(oldPath), s.SessionID)
		newSub := filepath.Join(newDir, s.SessionID)
		_ = os.Rename(oldSub, newSub) // absent on most sessions — ignore
		s.mu.Lock()
		s.Path = dest
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.CWD = newCWD
	changed := s.relocatedCwd != newCWD
	if changed {
		s.relocatedCwd = newCWD
	}
	s.mu.Unlock()
	if !changed {
		return nil
	}
	return s.LogRelocated(newCWD)
}

// ResolveArg resolves a -r/--resume argument to a session id in cwd's
// project dir. Accepts a session id directly; otherwise matches custom
// and ai titles — exact first, then case-insensitive substring (real's
// title resolution for session args). Returns "" on no match.
func ResolveArg(cwd, arg string) string {
	if arg == "" {
		return ""
	}
	d := dir(cwd)
	if _, err := os.Stat(filepath.Join(d, arg+".jsonl")); err == nil {
		return arg
	}
	type cand struct {
		id    string
		title string
	}
	var cands []cand
	ents, err := os.ReadDir(d)
	if err != nil {
		return ""
	}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		p := filepath.Join(d, e.Name())
		if t := ReadTitle(d, id); t != "" {
			cands = append(cands, cand{id, t})
		}
		if t := transcriptTitle(p); t != "" {
			cands = append(cands, cand{id, t})
		}
	}
	// exact match first
	for _, c := range cands {
		if c.title == arg {
			return c.id
		}
	}
	lower := strings.ToLower(arg)
	for _, c := range cands {
		if strings.Contains(strings.ToLower(c.title), lower) {
			return c.id
		}
	}
	return ""
}

// writeRaw appends an already-complete record verbatim (used by Fork, which
// controls every field including a nil parentUuid).
func (s *Store) writeRaw(ev any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeRawLocked(ev)
}

func (s *Store) writeRawLocked(ev any) error {
	b, _ := json.Marshal(ev)
	_, err := s.f.Write(append(b, '\n'))
	return err
}

// StampContinuedIn appends a {type:"continued-in"} record to an arbitrary
// transcript file — real's TRe stamps the SOURCE session file when the
// conversation continues under a different session id (background
// handoff / --fork-session), hiding the superseded file from the picker.
func StampContinuedIn(path, sessionID, continuedInSessionID string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b := orderedMap(stateKeyOrder["continued-in"], map[string]any{
		"type": "continued-in", "timestamp": isoNow(),
		"sessionId": sessionID, "continuedInSessionId": continuedInSessionID,
	})
	_, err = f.Write(append(b, '\n'))
	return err
}

// LogRawRecord appends a record verbatim — no uuid/timestamp/parentUuid
// stamps. cost-state, history-suppression, and the api-request* family are
// side-channel rows real writes without a message-chain position.
func (s *Store) LogRawRecord(ev any) error { return s.writeRaw(ev) }

// LogHistorySuppression writes real's {type:"history-suppression"} row
// (mod_0092 c8e → mod_0416 writeHistorySuppression): sessionId, cause,
// optional vetoedAgainstAccountUuid, ts — deliberately no uuid/chain fields.
func (s *Store) LogHistorySuppression(cause, vetoedAccountUUID string) error {
	rec := map[string]any{
		"type":      "history-suppression",
		"sessionId": s.SessionID,
		"cause":     cause,
		"ts":        isoNow(),
	}
	if vetoedAccountUUID != "" {
		rec["vetoedAgainstAccountUuid"] = vetoedAccountUUID
	}
	return s.writeRaw(rec)
}

// ReplayBlob pairs a message hash with its complete api-request-blob record.
type ReplayBlob struct {
	Hash   string
	Record any
}

// replayShapeCap is real's XKr — an api-request-shape row over this many
// bytes means the system+tools payload is too big to record; requests
// using that shape are skipped entirely (return "shape_too_large").
const replayShapeCap = 2000000

// seedReplayIndex scans the transcript once for api-request-shape /
// api-request-blob / fork-context-ref rows so a resumed session doesn't
// rewrite shapes/blobs already persisted (real's replayIndexFor).
func (s *Store) seedReplayIndex() {
	if s.replaySeeded {
		return
	}
	s.replaySeeded = true
	s.replayIdx = map[string]bool{}
	f, err := os.Open(s.Path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	var rec struct {
		Type      string `json:"type"`
		ShapeHash string `json:"shapeHash"`
		Hash      string `json:"hash"`
	}
	for sc.Scan() {
		ln := sc.Bytes()
		if len(ln) == 0 {
			continue
		}
		switch {
		case bytes.Contains(ln, []byte(`"type":"fork-context-ref"`)):
			s.replayForkRef = true
		case bytes.Contains(ln, []byte(`"type":"api-request-shape"`)),
			bytes.Contains(ln, []byte(`"type":"api-request-blob"`)):
			rec.Type, rec.ShapeHash, rec.Hash = "", "", ""
			if json.Unmarshal(ln, &rec) == nil {
				if h := rec.ShapeHash; h != "" {
					s.replayIdx[h] = true
				}
				if h := rec.Hash; h != "" {
					s.replayIdx[h] = true
				}
			}
		}
	}
}

// AppendReplayRecords is real's appendRequestReplayRecords (mod_0416): the
// api-request-shape row lands once per shapeHash, one api-request-blob row
// per unseen message hash, then the api-request row — replay rebuilds a
// request as shape → blobs → request. shape/request/blob.Record must be
// fully-formed rows (the loop builds them with real's field order).
// Returns "written", "shape_too_large", or "no_transcript" like real.
func (s *Store) AppendReplayRecords(shapeHash string, shape, request any, blobs []ReplayBlob) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seedReplayIndex()
	if s.replayForkRef {
		return "no_transcript"
	}
	if !s.replayIdx[shapeHash] {
		b, _ := json.Marshal(shape)
		if len(b) > replayShapeCap {
			return "shape_too_large"
		}
		if err := s.writeRawLocked(shape); err != nil {
			return "no_transcript"
		}
		s.replayIdx[shapeHash] = true
	}
	for _, bl := range blobs {
		if s.replayIdx[bl.Hash] {
			continue
		}
		if err := s.writeRawLocked(bl.Record); err != nil {
			return "no_transcript"
		}
		s.replayIdx[bl.Hash] = true
	}
	if err := s.writeRawLocked(request); err != nil {
		return "no_transcript"
	}
	return "written"
}

// WriteTitle sets the custom title on a stored (possibly closed) transcript:
// appends a {type:"custom-title"} record and writes custom-title.json.
// Used by the resume picker's rename mode.
func WriteTitle(projectDir, sessionID, title string) error {
	p := filepath.Join(projectDir, sessionID+".jsonl")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	// real's picker-rename write is the same bare custom-title record
	// saveCustomTitle appends: {type,customTitle,sessionId} — no uuid/ts
	b := orderedMap(stateKeyOrder["custom-title"], map[string]any{
		"type": "custom-title", "customTitle": title,
		"sessionId": sessionID,
	})
	_, werr := f.Write(append(b, '\n'))
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	if cerr != nil {
		return cerr
	}
	return writeTitleFile(projectDir, sessionID, title)
}

// writeTitleFile writes <projectDir>/<sessionID>/custom-title.json. Real CC
// stores {customTitle}; "title" is included for forwards compatibility.
func writeTitleFile(projectDir, sessionID, title string) error {
	d := filepath.Join(projectDir, sessionID)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]string{
		"customTitle": title,
		"title":       title,
	})
	return os.WriteFile(filepath.Join(d, "custom-title.json"), b, 0o644)
}

// ReadTitle returns the custom title for <projectDir>/<sessionID>.jsonl,
// or "" if none.
func ReadTitle(projectDir, sessionID string) string {
	b, err := os.ReadFile(filepath.Join(projectDir, sessionID, "custom-title.json"))
	if err != nil {
		return ""
	}
	var v struct {
		CustomTitle string `json:"customTitle"`
		Title       string `json:"title"`
	}
	if json.Unmarshal(b, &v) != nil {
		return ""
	}
	if v.CustomTitle != "" {
		return v.CustomTitle
	}
	return v.Title
}

// CurrentGitBranch is the exported branch probe — the picker filters on it.
func CurrentGitBranch(cwd string) string { return currentGitBranch(cwd) }

func currentGitBranch(cwd string) string {
	out, err := execCommand("git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.f.Close()
}

// SeedParent primes the parentUuid chain — on same-transcript resume the
// next logged record must link to the loaded branch's leaf, not start a
// new root.
func (s *Store) SeedParent(uuid string) {
	s.mu.Lock()
	s.lastUUID = uuid
	s.chainParent = uuid
	s.mu.Unlock()
}

// LeafUUID returns the uuid at the active branch's leaf in a transcript —
// the parentUuid anchor a same-transcript resume continues from.
func LeafUUID(path string) string {
	recs, err := readRecords(path)
	if err != nil {
		return ""
	}
	branch := activeBranch(recs)
	if branch == nil {
		// no uuid chain — last uuid'd record is the best anchor
		for i := len(recs) - 1; i >= 0; i-- {
			if u, _ := recs[i]["uuid"].(string); u != "" {
				return u
			}
		}
		return ""
	}
	u, _ := branch[len(branch)-1]["uuid"].(string)
	return u
}

// Load reconstructs the active branch: walk parentUuid from the leaf
// backwards, excluding sidechains and non-message records. This mirrors
// real resume semantics where a rewound transcript keeps its tail but
// resumes from the marked leaf.
func Load(cwd, sessionID string) ([]api.Message, error) {
	return LoadPath(filepath.Join(dir(cwd), sessionID+".jsonl"))
}

// LoadPath loads an arbitrary .jsonl transcript file (for `claude --resume
// <file>`), applying the same active-branch reconstruction as Load.
func LoadPath(path string) ([]api.Message, error) {
	recs, err := readRecords(path)
	if err != nil {
		return nil, err
	}
	branch := activeBranch(recs)
	if branch == nil {
		// no uuid chain — fall back to in-order messages
		var msgs []api.Message
		for _, r := range recs {
			if m, ok := recToMessage(r); ok {
				msgs = append(msgs, m)
			}
		}
		return msgs, nil
	}
	var msgs []api.Message
	for _, r := range branch {
		if m, ok := recToMessage(r); ok {
			msgs = append(msgs, m)
		}
	}
	return msgs, nil
}

// readRecords parses every JSONL record in a transcript file.
func readRecords(path string) ([]map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		out = append(out, m)
	}
	return out, sc.Err()
}

func truthy(v any) bool {
	b, _ := v.(bool)
	return b
}

// activeBranch returns the main-chain records: the parentUuid walk from the
// last non-sidechain user/assistant record back to the root, ordered
// oldest-first. Returns nil when no uuid chain exists.
func activeBranch(recs []map[string]any) []map[string]any {
	byUUID := map[string]map[string]any{}
	leaf := -1
	for i, r := range recs {
		u, _ := r["uuid"].(string)
		if u == "" {
			continue
		}
		byUUID[u] = r
		t, _ := r["type"].(string)
		if !truthy(r["isSidechain"]) && (t == "user" || t == "assistant") {
			leaf = i
		}
	}
	if leaf < 0 {
		return nil
	}
	var chain []map[string]any
	seen := map[string]bool{}
	for cur := recs[leaf]; cur != nil; {
		u, _ := cur["uuid"].(string)
		if seen[u] {
			break // cyclic parentUuid chain (corrupt transcript) — stop
		}
		seen[u] = true
		chain = append(chain, cur)
		p, _ := cur["parentUuid"].(string)
		cur = byUUID[p]
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// recToMessage extracts an api.Message from a transcript record. Sidechain,
// meta, and message-less records are skipped.
func recToMessage(r map[string]any) (api.Message, bool) {
	if truthy(r["isSidechain"]) || truthy(r["isMeta"]) {
		return api.Message{}, false
	}
	raw, _ := r["message"].(map[string]any)
	role, _ := raw["role"].(string)
	if role == "" {
		return api.Message{}, false
	}
	content, _ := json.Marshal(raw["content"])
	return api.Message{Role: role, Content: content}, true
}

// Fork copies the active branch of session srcID into a fresh session id in
// the same project dir, like real CC branching: copied records keep their
// uuids, get sessionId=<new>, isSidechain=false, the parentUuid chain is
// rebuilt across non-progress records, and every record is stamped
// forkedFrom:{sessionId:<srcID>,messageUuid:<orig uuid>}. The returned Store
// is open and chained so subsequent Log calls continue the branch. The fork
// is titled "<first prompt> (Branch)" via LogTitle.
func Fork(cwd, srcID string) (*Store, []api.Message, error) {
	srcPath := filepath.Join(dir(cwd), srcID+".jsonl")
	recs, err := readRecords(srcPath)
	if err != nil {
		return nil, nil, err
	}
	branch := activeBranch(recs)
	if len(branch) == 0 {
		return nil, nil, errors.New("session has no message chain to fork")
	}
	newID := NewID()
	st, err := Open(cwd, newID)
	if err != nil {
		return nil, nil, err
	}
	// real's fork writes {type:"history-suppression",cause:"fork_inherit"}
	// into the new transcript ahead of the copied messages when the source
	// carried the row (mod_1044) — suppressed history stays suppressed.
	st.writeSuppressionOnFork(recs)
	msgs, err := st.forkCopy(branch, srcID)
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	st.replayContentReplacements(recs, srcID)
	if p := firstPromptText(msgs); p != "" {
		// best-effort title, matching CC's "<prompt> (Branch N)" naming —
		// N dedups against existing branch titles of the same prompt
		_ = st.LogTitle(nextBranchTitle(filepath.Dir(srcPath), trunc(p, 60)))
	}
	return st, msgs, nil
}

// ForkInto copies the active branch of the transcript at srcPath into
// this (already-open) store — --fork-session's path: real copies the
// session file to <newid>.jsonl stamping every record
// forkedFrom:{sessionId:<srcID>,messageUuid:<orig uuid>}, so a later -r
// of the fork sees the full history. leafUUID, when non-empty, truncates
// the copy at that record (--fork-session + --resume-session-at forks at
// the resume point, not the tail). Returns the copied messages.
func (s *Store) ForkInto(srcPath, srcID, leafUUID string) ([]api.Message, error) {
	recs, err := readRecords(srcPath)
	if err != nil {
		return nil, err
	}
	branch := activeBranch(recs)
	if leafUUID != "" {
		for i, r := range branch {
			if u, _ := r["uuid"].(string); u == leafUUID ||
				(len(leafUUID) < len(u) && strings.HasPrefix(u, leafUUID)) {
				branch = branch[:i+1]
				break
			}
		}
	}
	if len(branch) == 0 {
		return nil, errors.New("session has no message chain to fork")
	}
	// fork_inherit suppression row ahead of the copied records (same as Fork)
	s.writeSuppressionOnFork(recs)
	msgs, err := s.forkCopy(branch, srcID)
	if err != nil {
		return nil, err
	}
	s.replayContentReplacements(recs, srcID)
	return msgs, nil
}

// replayContentReplacements re-emits the source transcript's
// content-replacement rows as one consolidated record on the fork —
// real's session-copy path (mod_1044 ne) collects every {type:
// "content-replacement",sessionId:<src>} row's replacements and appends
// {type:"content-replacement",sessionId:<new>,replacements:[...]} after
// the copied messages.
func (s *Store) replayContentReplacements(recs []map[string]any, srcID string) {
	var all []any
	for _, r := range recs {
		if t, _ := r["type"].(string); t != "content-replacement" {
			continue
		}
		if sid, _ := r["sessionId"].(string); sid != srcID {
			continue
		}
		if rs, ok := r["replacements"].([]any); ok {
			all = append(all, rs...)
		}
	}
	if len(all) == 0 {
		return
	}
	_ = s.LogContentReplacement(all, "")
}

// writeSuppressionOnFork appends a {type:"history-suppression",
// cause:"fork_inherit"} row when the source transcript records carry one —
// real's createFork rewrites the taint onto the branched session.
func (s *Store) writeSuppressionOnFork(recs []map[string]any) {
	for _, r := range recs {
		if t, _ := r["type"].(string); t == "history-suppression" {
			_ = s.LogHistorySuppression("fork_inherit", "")
			return
		}
	}
}

// forkCopy writes the branch records into s with forkedFrom stamps and a
// rebuilt parentUuid chain (progress records don't join the chain),
// leaving s.lastUUID at the copied leaf so subsequent Log calls continue
// the branch.
func (s *Store) forkCopy(branch []map[string]any, srcID string) ([]api.Message, error) {
	parent := ""
	var msgs []api.Message
	for _, r := range branch {
		cp := make(map[string]any, len(r)+3)
		for k, v := range r {
			cp[k] = v
		}
		origUUID, _ := cp["uuid"].(string)
		if t, _ := cp["type"].(string); t == "system" {
			if sb, _ := cp["subtype"].(string); sb == "model_refusal_fallback" {
				cp["neutralizedByFork"] = true
			}
		}
		cp["sessionId"] = s.SessionID
		cp["isSidechain"] = false
		delete(cp, "sessionKind")
		cp["forkedFrom"] = map[string]string{
			"sessionId":   srcID,
			"messageUuid": origUUID,
		}
		if parent == "" {
			cp["parentUuid"] = nil
		} else {
			cp["parentUuid"] = parent
		}
		if err := s.writeRaw(cp); err != nil {
			return nil, err
		}
		if t, _ := cp["type"].(string); t != "progress" {
			parent = origUUID
		}
		if m, ok := recToMessage(cp); ok {
			msgs = append(msgs, m)
		}
	}
	s.mu.Lock()
	s.lastUUID = parent
	s.chainParent = parent
	s.mu.Unlock()
	return msgs, nil
}

// nextBranchTitle mirrors real's `re()`: "<prompt> (Branch)", then
// "(Branch 2)", "(Branch 3)", … deduped against every session title in
// the project dir (regex /\(Branch(?: (\d+))?\)$/ on custom titles).
func nextBranchTitle(projectDir, base string) string {
	plain := base + " (Branch)"
	taken := map[int]bool{}
	for _, t := range projectTitles(projectDir) {
		m := branchTitleRe.FindStringSubmatch(t)
		if m == nil || !strings.HasPrefix(t, base+" (Branch") {
			continue
		}
		if m[1] == "" {
			taken[1] = true
		} else if n, err := strconv.Atoi(m[1]); err == nil {
			taken[n] = true
		}
	}
	if !taken[1] {
		return plain
	}
	n := 2
	for taken[n] {
		n++
	}
	return fmt.Sprintf("%s (Branch %d)", base, n)
}

var branchTitleRe = regexp.MustCompile(` \(Branch(?: (\d+))?\)$`)

// projectTitles returns every custom/ai title recorded in a project dir —
// real's I4(title) lookup for branch-name dedup.
func projectTitles(projectDir string) []string {
	var out []string
	ents, err := os.ReadDir(projectDir)
	if err != nil {
		return nil
	}
	for _, e := range ents {
		if e.IsDir() {
			if t := ReadTitle(projectDir, e.Name()); t != "" {
				out = append(out, t)
			}
			continue
		}
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		p := filepath.Join(projectDir, e.Name())
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		if t := ReadTitle(projectDir, id); t != "" {
			out = append(out, t)
		}
		if t := transcriptTitle(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// transcriptTitle scans a transcript for its latest custom-title or
// ai-title record (the title real's resume surfaces).
func transcriptTitle(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	title := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var e struct {
			Type        string `json:"type"`
			CustomTitle string `json:"customTitle"`
			AITitle     string `json:"aiTitle"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.Type == "custom-title" && e.CustomTitle != "" {
			title = e.CustomTitle
		} else if e.Type == "ai-title" && e.AITitle != "" && title == "" {
			title = e.AITitle
		}
	}
	return title
}

// firstPromptText returns the first real user text in msgs (CC's
// deriveFirstPrompt), skipping system-reminder/bash wrapper text.
func firstPromptText(msgs []api.Message) string {
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		t := contentText(m.Content)
		if t == "" || strings.HasPrefix(t, "<") {
			continue
		}
		return t
	}
	return ""
}

// contentText joins the text blocks of a message content payload.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, " ")
}

// Latest returns the newest session id for cwd (for --continue). Real
// continues the most recent conversation — an empty transcript (freshly
// created this session, or a fork with no messages) doesn't qualify.
func Latest(cwd string) (string, error) {
	ents, err := os.ReadDir(dir(cwd))
	if err != nil {
		return "", err
	}
	type cand struct {
		id string
		t  time.Time
	}
	var cands []cand
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if fi, err := e.Info(); err == nil {
			cands = append(cands, cand{strings.TrimSuffix(e.Name(), ".jsonl"), fi.ModTime()})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].t.After(cands[j].t) })
	for _, c := range cands {
		if hasConversation(filepath.Join(dir(cwd), c.id+".jsonl")) {
			return c.id, nil
		}
	}
	return "", nil
}

// hasConversation reports whether a transcript holds a user or assistant
// message record (skips init/system-only files).
func hasConversation(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if bytes.Contains(line, []byte(`"type":"user"`)) ||
			bytes.Contains(line, []byte(`"type":"assistant"`)) {
			return true
		}
	}
	return false
}

var execCommand = func(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}

// OpenSubagent opens a transcript for a subagent under
// <slug>/<sid>/subagents/<agentID>.jsonl and writes the .meta.json sidecar —
// the layout real CC uses (keeps subagent turns out of the parent file but
// resumable). Records get isSidechain:true + agentId stamps.
func OpenSubagent(parentPath, agentID, cwd string) (*Store, error) {
	parentSid := strings.TrimSuffix(filepath.Base(parentPath), ".jsonl")
	dir := filepath.Join(filepath.Dir(parentPath), parentSid, "subagents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	st := &Store{
		Path:      filepath.Join(dir, agentID+".jsonl"),
		SessionID: parentSid, CWD: cwd,
	}
	f, err := os.OpenFile(st.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	st.f = f
	meta, _ := json.Marshal(map[string]any{"agentId": agentID, "sessionId": parentSid})
	os.WriteFile(filepath.Join(dir, agentID+".meta.json"), meta, 0o600)
	st.sidechain = true
	st.agentID = agentID // real stamps agentId on every sidechain row
	return st, nil
}

// SweepRetention deletes transcripts older than cleanupPeriodDays
// (default 30 — real's sbn/lS sweep; skipped on settings errors or when
// userSettings is disabled via --setting-sources).
func SweepRetention(cwd string) {
	if os.Getenv("CLAUDE_GO_NO_SWEEP") != "" {
		return
	}
	days := 30
	for _, p := range []string{
		filepath.Join(cwd, ".claude", "settings.json"),
		filepath.Join(config.ClaudeDir(), "settings.json"),
	} {
		if b, err := os.ReadFile(p); err == nil {
			var m map[string]any
			if json.Unmarshal(b, &m) == nil {
				if v, ok := m["cleanupPeriodDays"].(float64); ok {
					days = int(v)
					break
				}
			}
		}
	}
	if days <= 0 {
		return // 0/negative disables the sweep
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	root := filepath.Join(config.ClaudeDir(), "projects")
	ents, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, proj := range ents {
		if !proj.IsDir() {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(root, proj.Name()))
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			if fi, err := f.Info(); err == nil && fi.ModTime().Before(cutoff) {
				os.Remove(filepath.Join(root, proj.Name(), f.Name()))
			}
		}
	}
}
