package session

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gbasin/claude-go/internal/config"
)

// RewindFiles is real's --rewind-files <user-msg-uuid>: restore every
// tracked file to the state recorded at that message. The anchor must be
// a user message. Real keys the checkpoint by messageId: the
// file-history-snapshot row whose messageId is the anchor carries the
// full trackedFileBackups state, extended by any file-history-delta rows
// whose snapshotMessageId matches. Transcripts from before messageId
// linkage (per-write snapshot rows with no messageId) fall back to the
// positional scan: the first snapshot after the previous message
// boundary holds each path's pre-write content for the anchor's turn.
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

	// messageId-linked restore (real's model): the LAST snapshot row keyed
	// to the anchor message is the checkpoint (real's findLast); deltas
	// pointing at it merge in files first tracked during that turn.
	backups := map[string]string{}
	linked := false
	for _, r := range recs {
		if t, _ := r["type"].(string); t != "file-history-snapshot" {
			continue
		}
		if mid, _ := r["messageId"].(string); mid == msgUUID {
			linked = true
			snap, _ := r["snapshot"].(map[string]any)
			if tfb, _ := snap["trackedFileBackups"].(map[string]any); tfb != nil {
				for p, v := range tfb {
					if m, ok := v.(map[string]any); ok {
						bn, _ := m["backupFileName"].(string)
						backups[p] = bn
					}
				}
			}
		}
	}
	if linked {
		for _, r := range recs {
			if t, _ := r["type"].(string); t != "file-history-delta" {
				continue
			}
			if sid, _ := r["snapshotMessageId"].(string); sid != msgUUID {
				continue
			}
			p, _ := r["trackingPath"].(string)
			if p == "" {
				continue
			}
			if _, seen := backups[p]; seen {
				continue
			}
			if b, ok := r["backup"].(map[string]any); ok {
				bn, _ := b["backupFileName"].(string)
				backups[p] = bn
			}
		}
	} else {
		// legacy: file order isn't turn order — the user record is written
		// at turn end while snapshots land mid-turn, so the turn starting
		// at the anchor leaves its snapshots just BEFORE the anchor record.
		boundary := 0
		for i := anchor - 1; i >= 0; i-- {
			if t, _ := recs[i]["type"].(string); t == "user" || t == "assistant" {
				boundary = i + 1
				break
			}
		}
		for _, r := range recs[boundary:] {
			if t, _ := r["type"].(string); t != "file-history-snapshot" {
				continue
			}
			snap, _ := r["snapshot"].(map[string]any)
			mergeBackups(backups, snap)
		}
	}

	histDir := filepath.Join(config.ClaudeDir(), "file-history", sessionID)
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

// mergeBackups folds a snapshot row's trackedFileBackups into backups —
// first-seen wins (the earliest record of a path is its anchor state).
func mergeBackups(backups map[string]string, snap map[string]any) {
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
