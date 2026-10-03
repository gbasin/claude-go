package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Checkpoints keeps a per-session stack of file snapshots for /rewind.
type CheckpointStore struct {
	dir string
	seq int
}

var Checkpoints = &CheckpointStore{}

type ckMeta struct {
	Path    string `json:"path"`
	Before  string `json:"before"` // snapshot path (empty = file didn't exist)
	Existed bool   `json:"existed"`
	TS      string `json:"ts"`
}

func (cs *CheckpointStore) Init(sessionID string) {
	home, _ := os.UserHomeDir()
	cs.dir = filepath.Join(home, ".claude-go", "checkpoints", sessionID)
	os.MkdirAll(cs.dir, 0o755)
}

// Snapshot records a file's pre-write state.
func (cs *CheckpointStore) Snapshot(path string) {
	if cs.dir == "" {
		return
	}
	cs.seq++
	abs, _ := filepath.Abs(path)
	meta := ckMeta{Path: abs, TS: time.Now().Format(time.RFC3339)}
	if b, err := os.ReadFile(abs); err == nil {
		sum := sha256.Sum256(b)
		snap := filepath.Join(cs.dir, fmt.Sprintf("%03d-%s", cs.seq, hex.EncodeToString(sum[:6])))
		os.WriteFile(snap, b, 0o644)
		meta.Before = snap
		meta.Existed = true
	}
	mb, _ := json.Marshal(meta)
	os.WriteFile(filepath.Join(cs.dir, fmt.Sprintf("%03d.json", cs.seq)), mb, 0o644)
}

// Rewind restores the most recent snapshot. Returns the restored path.
func (cs *CheckpointStore) Rewind() (string, error) {
	if cs.dir == "" {
		return "", fmt.Errorf("no checkpoints")
	}
	for i := cs.seq; i > 0; i-- {
		mp := filepath.Join(cs.dir, fmt.Sprintf("%03d.json", i))
		b, err := os.ReadFile(mp)
		if err != nil {
			continue
		}
		var meta ckMeta
		if json.Unmarshal(b, &meta) != nil {
			continue
		}
		if meta.Existed {
			orig, err := os.ReadFile(meta.Before)
			if err != nil {
				continue
			}
			os.WriteFile(meta.Path, orig, 0o644)
		} else {
			os.Remove(meta.Path)
		}
		os.Remove(mp)
		os.Remove(meta.Before)
		cs.seq--
		return meta.Path, nil
	}
	return "", fmt.Errorf("no checkpoints")
}
