// Package session persists transcripts as JSONL under
// ~/.claude/projects/<slug>/<session-id>.jsonl, matching Claude Code.
package session

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
}

type event struct {
	Type          string          `json:"type"` // user|assistant|system|summary|attachment|progress
	Subtype       string          `json:"subtype,omitempty"`
	UUID          string          `json:"uuid"`
	ParentUUID    string          `json:"parentUuid,omitempty"`
	SessionID     string          `json:"sessionId"`
	CWD           string          `json:"cwd"`
	Timestamp     string          `json:"timestamp"`
	Message       json.RawMessage `json:"message,omitempty"`
	Version       string          `json:"version"`
	UserType      string          `json:"userType"`
	GitBranch     string          `json:"gitBranch,omitempty"`
	IsSidechain   bool            `json:"isSidechain,omitempty"`
	IsMeta        bool            `json:"isMeta,omitempty"`
	RequestID     string          `json:"requestId,omitempty"`
	AgentID       string          `json:"agentId,omitempty"`
	Origin        string          `json:"origin,omitempty"`
	Entrypoint    string          `json:"entrypoint,omitempty"`
	ForkedFrom    *ForkedFrom     `json:"forkedFrom,omitempty"`
	ToolUseResult json.RawMessage `json:"toolUseResult,omitempty"`
}

// ForkedFrom marks a message copied into a forked session.
type ForkedFrom struct {
	SessionID   string `json:"sessionId"`
	MessageUUID string `json:"messageUuid"`
}

func Slug(cwd string) string {
	return strings.ReplaceAll(filepath.Clean(cwd), "/", "-")
}

func dir(cwd string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects", Slug(cwd))
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
	var raw json.RawMessage
	role := m.Role
	// re-encode as {role, content}
	mb, _ := json.Marshal(struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}{role, m.Content})
	raw = mb
	uuid := newUUID()
	e := event{
		Type: role, UUID: uuid, ParentUUID: s.lastUUID,
		SessionID: s.SessionID, CWD: s.CWD,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Message:   raw, Version: "2.1.288",
		UserType:   "external",
		GitBranch:  currentGitBranch(s.CWD),
		Entrypoint: "cli",
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
	return s.LogRecord(map[string]any{
		"type": "system", "subtype": "compact_boundary",
		"compactMetadata": map[string]any{
			"trigger": "auto", "preTokens": preTokens, "messagesKept": kept,
		},
	})
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
	p := filepath.Join(dir(cwd), sessionID+".jsonl")
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	type rec struct {
		Type        string `json:"type"`
		UUID        string `json:"uuid"`
		ParentUUID  string `json:"parentUuid"`
		IsSidechain bool   `json:"isSidechain"`
		IsMeta      bool   `json:"isMeta"`
		Message     struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	var records []rec
	byUUID := map[string]*rec{}
	var leaf *rec
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var r rec
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		records = append(records, r)
		if r.UUID != "" {
			cp := records[len(records)-1]
			byUUID[r.UUID] = &cp
			if !r.IsSidechain && (r.Type == "user" || r.Type == "assistant") {
				leaf = &records[len(records)-1]
			}
		}
	}
	if leaf == nil {
		// no uuid chain — fall back to in-order messages
		var msgs []api.Message
		for _, r := range records {
			if !r.IsSidechain && !r.IsMeta && r.Message.Role != "" {
				msgs = append(msgs, api.Message{Role: r.Message.Role, Content: r.Message.Content})
			}
		}
		return msgs, sc.Err()
	}
	var chain []rec
	for cur := leaf; cur != nil; {
		chain = append(chain, *cur)
		cur = byUUID[cur.ParentUUID]
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	var msgs []api.Message
	for _, r := range chain {
		if r.IsSidechain || r.IsMeta || r.Message.Role == "" {
			continue
		}
		msgs = append(msgs, api.Message{Role: r.Message.Role, Content: r.Message.Content})
	}
	return msgs, sc.Err()
}

// Latest returns the newest session id for cwd (for --continue).
func Latest(cwd string) (string, error) {
	ents, err := os.ReadDir(dir(cwd))
	if err != nil {
		return "", err
	}
	var best string
	var bestT time.Time
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.ModTime().After(bestT) {
			bestT = fi.ModTime()
			best = strings.TrimSuffix(e.Name(), ".jsonl")
		}
	}
	return best, nil
}

var execCommand = func(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}
