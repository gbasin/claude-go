package session

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gbasin/claude-go/internal/config"
)

// RewindFiles is real's --rewind-files <user-msg-uuid>: restore every
// tracked file to the state recorded in the transcript's
// file-history-snapshot records AT that message. The anchor must be a
// user message; the newest backup for each path before it wins. A path
// with no backup at the anchor didn't exist yet — it gets removed.
//
// Returns (restored, removed, error) — errors are real's refusal strings.
func RewindFiles(cwd, sessionID, msgUUID string) (int, int, error) {
	path := filepath.Join(dir(cwd), sessionID+".jsonl")
	recs, err := readRecords(path)
	if err != nil {
		return 0, 0, err
	}
	anchor := -1
	for i, r := range recs {
		if u, _ := r["uuid"].(string); u == msgUUID {
			if t, _ := r["type"].(string); t != "user" {
				return 0, 0, fmt.Errorf("--rewind-files requires a user message UUID, but %s is not a user message in this session", msgUUID)
			}
			anchor = i
			break
		}
	}
	if anchor < 0 {
		return 0, 0, fmt.Errorf("message %s not found in %s", msgUUID, path)
	}

	// file order isn't turn order — the user record is written at turn
	// end while snapshots land mid-turn, so the turn starting at the
	// anchor leaves its snapshots just BEFORE the anchor record. The
	// anchor-state backup for each path is the FIRST snapshot after the
	// previous message boundary (that turn's pre-write content — and a
	// later turn's first backup is equally the unmodified anchor state).
	boundary := 0
	for i := anchor - 1; i >= 0; i-- {
		if t, _ := recs[i]["type"].(string); t == "user" || t == "assistant" {
			boundary = i + 1
			break
		}
	}
	histDir := filepath.Join(config.ClaudeDir(), "file-history", sessionID)
	backups := map[string]string{}
	for _, r := range recs[boundary:] {
		if t, _ := r["type"].(string); t != "file-history-snapshot" {
			continue
		}
		snap, _ := r["snapshot"].(map[string]any)
		tfb, _ := snap["trackedFileBackups"].(map[string]any)
		for p, v := range tfb {
			if _, seen := backups[p]; seen {
				continue
			}
			if m, ok := v.(map[string]any); ok {
				bn, _ := m["backupFileName"].(string)
				backups[p] = bn
			}
		}
	}

	restored, removed := 0, 0
	for p, bn := range backups {
		if bn == "" {
			// no backup existed at the anchor — the file didn't exist yet
			if err := os.Remove(p); err == nil || os.IsNotExist(err) {
				removed++
			}
			continue
		}
		b, err := os.ReadFile(filepath.Join(histDir, bn))
		if err != nil {
			continue // backup blob pruned — real reports it as skipped
		}
		if err := os.WriteFile(p, b, 0o644); err == nil {
			restored++
		}
	}
	return restored, removed, nil
}
