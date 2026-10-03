// Package session persists transcripts as JSONL under
// ~/.claude/projects/<slug>/<session-id>.jsonl, matching Claude Code.
package session

import (
	"bufio"
	"encoding/json"
	"os"
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
}

type event struct {
	Type       string          `json:"type"`
	UUID       string          `json:"uuid"`
	ParentUUID string          `json:"parentUuid,omitempty"`
	SessionID  string          `json:"sessionId"`
	CWD        string          `json:"cwd"`
	Timestamp  string          `json:"timestamp"`
	Message    json.RawMessage `json:"message,omitempty"`
	Version    string          `json:"version"`
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

var lastUUID string

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
		Type: role, UUID: uuid, ParentUUID: lastUUID,
		SessionID: s.SessionID, CWD: s.CWD,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Message:   raw, Version: "2.1.288",
	}
	lastUUID = uuid
	b, _ := json.Marshal(e)
	_, err := s.f.Write(append(b, '\n'))
	return err
}

func (s *Store) Close() { s.f.Close() }

// Load reads all messages from an existing transcript.
func Load(cwd, sessionID string) ([]api.Message, error) {
	p := filepath.Join(dir(cwd), sessionID+".jsonl")
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var msgs []api.Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var e struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Message.Role == "" {
			continue
		}
		msgs = append(msgs, api.Message{Role: e.Message.Role, Content: e.Message.Content})
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
