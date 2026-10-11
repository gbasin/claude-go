package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func freshCheckpoints(t *testing.T) *CheckpointStore {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	cs := &CheckpointStore{}
	cs.Init("test-sess")
	return cs
}

// First track of a path lands in pending (the file-history-delta queue);
// re-writes don't — real's Fet emits a delta only for newly tracked files.
func TestCheckpointPendingTracking(t *testing.T) {
	cs := freshCheckpoints(t)
	p1 := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(p1, []byte("v0"), 0o644)
	p2 := filepath.Join(t.TempDir(), "b.txt")
	os.WriteFile(p2, []byte("x"), 0o644)

	cs.Snapshot(p1)
	cs.Snapshot(p1) // second write of a tracked file → touch, not a delta
	cs.Snapshot(p2)
	pending := cs.TakePending()
	if len(pending) != 2 {
		t.Fatalf("pending = %d, want 2 first-tracks", len(pending))
	}
	if pending[0].Path != p1 || pending[1].Path != p2 {
		t.Fatalf("pending order = %v, %v", pending[0].Path, pending[1].Path)
	}
	if pending[0].Version != 1 || pending[0].FileName == "" {
		t.Fatalf("backup ref = %+v", pending[0])
	}
	// drained
	if len(cs.TakePending()) != 0 {
		t.Fatal("TakePending should drain")
	}
	// tracked set holds the LATEST pre-write backup per path — this is
	// p1's third snapshot, so the tracked ref is version 3
	cs.Snapshot(p1)
	m := cs.BackupMap()
	if len(m) != 2 {
		t.Fatalf("BackupMap = %v", m)
	}
	ent := m[p1].(map[string]any)
	if ent["version"] != 3 || ent["backupFileName"] == nil ||
		ent["backupTime"] == nil || ent["realParentDir"] == nil {
		t.Fatalf("BackupMap[%s] = %v", p1, ent)
	}
}

// A backup of a not-yet-existing file carries backupFileName:null — the
// delta row marks "file didn't exist at anchor" (real's null).
func TestCheckpointBackupNullForNewFile(t *testing.T) {
	cs := freshCheckpoints(t)
	p := filepath.Join(t.TempDir(), "nope.txt")
	cs.Snapshot(p)
	pending := cs.TakePending()
	if len(pending) != 1 {
		t.Fatalf("pending = %d", len(pending))
	}
	if pending[0].FileName != "" {
		t.Fatalf("FileName = %q, want empty", pending[0].FileName)
	}
	w := pending[0].Wire()
	if v, ok := w["backupFileName"]; !ok || v != nil {
		t.Fatalf("wire backupFileName = %v, want explicit null", v)
	}
	if w["version"] != 1 || w["backupTime"] == nil || w["realParentDir"] == nil {
		t.Fatalf("wire = %v", w)
	}
}
