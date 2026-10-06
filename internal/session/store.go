// Package session persists transcripts as JSONL under
// ~/.claude/projects/<slug>/<session-id>.jsonl, matching Claude Code.
package session

import (
	"bufio"
	"bytes"
	"claude-go/internal/config"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"claude-go/internal/api"
)

type Store struct {
	Path      string
	SessionID string
	CWD       string
	f         *os.File
	lastUUID  string
	sidechain bool // subagent transcript — records get isSidechain:true
}

type event struct {
	Type             string          `json:"type"` // user|assistant|system|summary|attachment|progress
	Subtype          string          `json:"subtype,omitempty"`
	UUID             string          `json:"uuid"`
	ParentUUID       string          `json:"parentUuid,omitempty"`
	SessionID        string          `json:"sessionId"`
	CWD              string          `json:"cwd"`
	Timestamp        string          `json:"timestamp"`
	Message          json.RawMessage `json:"message,omitempty"`
	Version          string          `json:"version"`
	UserType         string          `json:"userType"`
	GitBranch        string          `json:"gitBranch,omitempty"`
	IsSidechain      bool            `json:"isSidechain"`
	IsMeta           bool            `json:"isMeta"`
	IsCompactSummary bool            `json:"isCompactSummary,omitempty"`
	ModelOnlyText    string          `json:"modelOnlyText,omitempty"`
	RequestID        string          `json:"requestId,omitempty"`
	AgentID          string          `json:"agentId,omitempty"`
	Origin           string          `json:"origin,omitempty"`
	Entrypoint       string          `json:"entrypoint,omitempty"`
	ForkedFrom       *ForkedFrom     `json:"forkedFrom,omitempty"`
	ToolUseResult    json.RawMessage `json:"toolUseResult,omitempty"`
}

// ForkedFrom marks a message copied into a forked session.
type ForkedFrom struct {
	SessionID   string `json:"sessionId"`
	MessageUUID string `json:"messageUuid"`
}

func Slug(cwd string) string {
	// real resolves symlinks for the project slug (/tmp → /private/tmp
	// on macOS — captured session dirs use -private-tmp-…)
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = r
	}
	return strings.ReplaceAll(filepath.Clean(cwd), "/", "-")
}

func dir(cwd string) string {
	return filepath.Join(config.ClaudeDir(), "projects", Slug(cwd))
}

// Open creates (or appends to) the transcript for sessionID in cwd.
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
	return &Store{Path: p, SessionID: sessionID, CWD: cwd, f: f}, nil
}

// Store fields additions

var lastUUID string

// NewID generates a session/message uuid.
func NewID() string { return newUUID() }

func newUUID() string {
	var b [16]byte
	f, _ := os.Open("/dev/urandom")
	f.Read(b[:])
	f.Close()
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
	return s.logMessage(m, false, "")
}

// LogCompactSummary appends the user message carrying the post-compact
// summary, flagged isCompactSummary with modelOnlyText like real CC.
func (s *Store) LogCompactSummary(m api.Message, modelOnlyText string) error {
	return s.logMessage(m, true, modelOnlyText)
}

func (s *Store) logMessage(m api.Message, compact bool, modelOnlyText string) error {
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
	e := event{
		Type: role, UUID: uuid, ParentUUID: s.lastUUID,
		SessionID: s.SessionID, CWD: s.CWD,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Message:   raw, Version: "2.1.292",
		UserType:         "external",
		GitBranch:        currentGitBranch(s.CWD),
		Entrypoint:       "cli",
		IsCompactSummary: compact,
		ModelOnlyText:    modelOnlyText,
		IsSidechain:      s.sidechain,
	}
	s.lastUUID = uuid
	b, _ := json.Marshal(e)
	_, err := s.f.Write(append(b, '\n'))
	return err
}

// LogRecord writes an arbitrary transcript record (summary/system/etc).
func (s *Store) LogRecord(ev map[string]any) error {
	if ev["uuid"] == nil {
		ev["uuid"] = newUUID()
	}
	if ev["sessionId"] == nil {
		ev["sessionId"] = s.SessionID
	}
	if ev["timestamp"] == nil {
		ev["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if ev["parentUuid"] == nil {
		ev["parentUuid"] = s.lastUUID
	}
	b, _ := json.Marshal(ev)
	_, err := s.f.Write(append(b, '\n'))
	return err
}

// LogSummary writes a summary record (branch leaf marker/title).
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
func (s *Store) LogCompactBoundaryTrig(preTokens, kept int, trigger string) error {
	return s.LogRecord(map[string]any{
		"type": "system", "subtype": "compact_boundary",
		"compactBoundary": true, "isMeta": false,
		"logicalParentUuid": s.lastUUID,
		"compactMetadata": map[string]any{
			"trigger": trigger, "preTokens": preTokens, "messagesKept": kept,
		},
	})
}

// LogInit writes the {type:"system",subtype:"init"} record CC emits at
// session start.
func (s *Store) LogInit(model string) error {
	return s.LogRecord(map[string]any{
		"type": "system", "subtype": "init",
		"model": model, "cwd": s.CWD,
		"version": "2.1.292", "userType": "external",
		"gitBranch": currentGitBranch(s.CWD), "entrypoint": "cli",
	})
}

// LogAttachment appends an attachment record
// ({type:"attachment",attachment:{...}}) — queued commands, file mentions,
// relevant memories, etc.
func (s *Store) LogAttachment(att map[string]any) error {
	return s.LogRecord(map[string]any{
		"type": "attachment", "attachment": att,
	})
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

// LogTitle sets the session's custom title. It writes both a transcript
// record ({type:"custom-title"}) and <projectDir>/<sessionID>/custom-title.json
// ({customTitle}), matching real CC. A summary record is also appended as a
// named leaf marker.
func (s *Store) LogTitle(title string) error {
	if err := s.LogSummary(title, s.lastUUID); err != nil {
		return err
	}
	if err := s.LogRecord(map[string]any{
		"type": "custom-title", "customTitle": title,
	}); err != nil {
		return err
	}
	return writeTitleFile(filepath.Dir(s.Path), s.SessionID, title)
}

// writeRaw appends an already-complete record verbatim (used by Fork, which
// controls every field including a nil parentUuid).
func (s *Store) writeRaw(ev map[string]any) error {
	b, _ := json.Marshal(ev)
	_, err := s.f.Write(append(b, '\n'))
	return err
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
	e := map[string]any{
		"type": "custom-title", "customTitle": title,
		"sessionId": sessionID, "uuid": newUUID(),
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
	}
	b, _ := json.Marshal(e)
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

func currentGitBranch(cwd string) string {
	out, err := execCommand("git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (s *Store) Close() { s.f.Close() }

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
		cp["sessionId"] = newID
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
		if err := st.writeRaw(cp); err != nil {
			st.Close()
			return nil, nil, err
		}
		if t, _ := cp["type"].(string); t != "progress" {
			parent = origUUID
		}
		if m, ok := recToMessage(cp); ok {
			msgs = append(msgs, m)
		}
	}
	st.lastUUID = parent
	if p := firstPromptText(msgs); p != "" {
		// best-effort title, matching CC's "<prompt> (Branch)" naming
		_ = st.LogTitle(trunc(p, 60) + " (Branch)")
	}
	return st, msgs, nil
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
	return st, nil
}
