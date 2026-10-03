package tools

import (
	"claude-go/internal/config"
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
	dir       string
	legacyDir string
	seq       int
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
	// real keeps file history under the config dir
	cs.dir = filepath.Join(config.ClaudeDir(), "file-history", sessionID)
	// legacy location fallback for /rewind on older sessions
	cs.legacyDir = filepath.Join(home, ".claude-go", "checkpoints", sessionID)
	os.MkdirAll(cs.dir, 0o755)
}

// Snapshot records a file's pre-write state.
// OnSnapshot is invoked after each snapshot so callers can log a
// file-history-snapshot transcript record (real rewind interop).
var OnSnapshot func(path, backupFile string, version int)

func (cs *CheckpointStore) Snapshot(path string) {
	if cs.dir == "" {
		return
	}
	cs.seq++
	abs, _ := filepath.Abs(path)
	meta := ckMeta{Path: abs, TS: time.Now().Format(time.RFC3339)}
	if b, err := os.ReadFile(abs); err == nil {
		// real naming: <sha-of-path>@v<version>
		pSum := sha256.Sum256([]byte(abs))
		snap := filepath.Join(cs.dir, fmt.Sprintf("%s@v%d",
			hex.EncodeToString(pSum[:8]), cs.seq))
		os.WriteFile(snap, b, 0o644)
		meta.Before = snap
		meta.Existed = true
	}
	mb, _ := json.Marshal(meta)
	os.WriteFile(filepath.Join(cs.dir, fmt.Sprintf("%03d.json", cs.seq)), mb, 0o644)
	if OnSnapshot != nil {
		OnSnapshot(abs, meta.Before, cs.seq)
	}
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
