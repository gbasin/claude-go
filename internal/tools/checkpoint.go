package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/gbasin/claude-go/internal/config"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Checkpoints keeps a per-session stack of file snapshots for /rewind.
type CheckpointStore struct {
	dir       string
	legacyDir string
	seq       int
	mu        sync.Mutex
	// tracked: path → latest pre-write backup — real's file-history
	// trackedFiles set, carrying the backup each transcript row names.
	tracked map[string]*FileBackup
	// pending: first-time tracks since the last transcript flush — real
	// merges them into the current message's snapshot as file-history-delta.
	pending  []*FileBackup
	versions map[string]int
}

var Checkpoints = &CheckpointStore{}

// FileBackup is one tracked file's backup descriptor — the shape real's
// file-history rows carry as trackedFileBackups[path] / delta.backup.
type FileBackup struct {
	Path      string // absolute trackingPath
	FileName  string // backup blob name; "" when the file didn't exist
	Version   int    // per-path backup counter (real's version)
	Time      string // ISO8601 millis, real's backupTime
	ParentDir string // realParentDir — resolved parent directory
}

// Wire renders the backup as the JSON object real's rows embed.
func (b *FileBackup) Wire() map[string]any {
	m := map[string]any{
		"backupFileName": nil,
		"version":        b.Version,
		"backupTime":     b.Time,
		"realParentDir":  b.ParentDir,
	}
	if b.FileName != "" {
		m["backupFileName"] = b.FileName
	}
	return m
}

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
	cs.tracked = map[string]*FileBackup{}
	cs.versions = map[string]int{}
	cs.pending = nil
	os.MkdirAll(cs.dir, 0o755)
}

// Snapshot records a file's pre-write state.
// OnSnapshot is invoked after each snapshot so callers can log a
// file-history transcript record (real rewind interop).
var OnSnapshot func(path, backupFile string, version int)

func (cs *CheckpointStore) Snapshot(path string) {
	if cs.dir == "" {
		return
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.seq++
	ref := cs.backupLocked(path)
	mb, _ := json.Marshal(ckMeta{Path: ref.Path, Before: cs.blobPath(ref.FileName), Existed: ref.FileName != "", TS: time.Now().Format(time.RFC3339)})
	os.WriteFile(filepath.Join(cs.dir, fmt.Sprintf("%03d.json", cs.seq)), mb, 0o644)
	if OnSnapshot != nil {
		OnSnapshot(ref.Path, cs.blobPath(ref.FileName), ref.Version)
	}
}

// backupLocked writes the pre-write backup blob for path (when it exists)
// and returns its descriptor. trackingPath is the absolute path;
// realParentDir is the symlink-resolved parent (real resolves before
// keying the backup dir).
func (cs *CheckpointStore) backupLocked(path string) *FileBackup {
	abs, _ := filepath.Abs(path)
	parent := filepath.Dir(abs)
	if rp, err := filepath.EvalSymlinks(parent); err == nil {
		parent = rp
	}
	cs.versions[abs]++
	ref := &FileBackup{
		Path: abs, Version: cs.versions[abs], ParentDir: parent,
		Time: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	if b, err := os.ReadFile(abs); err == nil {
		// real naming: <sha-of-path>@v<version>
		pSum := sha256.Sum256([]byte(abs))
		ref.FileName = fmt.Sprintf("%s@v%d", hex.EncodeToString(pSum[:8]), ref.Version)
		os.WriteFile(filepath.Join(cs.dir, ref.FileName), b, 0o644)
	}
	if _, ok := cs.tracked[abs]; !ok {
		// first track of this file — real records it as a
		// file-history-delta against the current message's snapshot
		cp := *ref
		cs.pending = append(cs.pending, &cp)
	}
	cs.tracked[abs] = ref
	return ref
}

// TakePending drains first-track deltas accumulated since the last call —
// the transcript writer flushes them beside each user-message snapshot.
func (cs *CheckpointStore) TakePending() []*FileBackup {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := cs.pending
	cs.pending = nil
	return out
}

// BackupMap renders the tracked set as real's snapshot.trackedFileBackups.
func (cs *CheckpointStore) BackupMap() map[string]any {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make(map[string]any, len(cs.tracked))
	for p, b := range cs.tracked {
		out[p] = b.Wire()
	}
	return out
}

func (cs *CheckpointStore) blobPath(name string) string {
	if name == "" {
		return ""
	}
	return filepath.Join(cs.dir, name)
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
