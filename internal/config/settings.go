package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// userSettingsCache memoizes ~/.claude/settings.json keyed by the resolved
// ClaudeDir — real reads it once at startup too (restart to pick up edits),
// but tests Setenv("HOME"/"CLAUDE_CONFIG_DIR") between checks, so a plain
// sync.Once would pin whichever HOME was live at first read.
var userSettingsCache struct {
	mu  sync.Mutex
	dir string
	m   map[string]any
}

// ReadJSONMap reads a JSON object file into a map — {} on a missing file or
// invalid JSON (partial parses are discarded, matching the read-then-skip
// pattern callers used before). Uncached; use UserSettings for the hot
// ~/.claude/settings.json path.
func ReadJSONMap(p string) map[string]any {
	m := map[string]any{}
	if b, err := os.ReadFile(p); err == nil {
		if json.Unmarshal(b, &m) != nil {
			m = map[string]any{}
		}
	}
	if m == nil {
		m = map[string]any{}
	}
	return m
}

// UserSettings returns ~/.claude/settings.json as a map ({} on error),
// cached until InvalidateUserSettings or ClaudeDir() resolves differently.
func UserSettings() map[string]any {
	dir := ClaudeDir()
	userSettingsCache.mu.Lock()
	defer userSettingsCache.mu.Unlock()
	if userSettingsCache.m == nil || userSettingsCache.dir != dir {
		userSettingsCache.dir = dir
		userSettingsCache.m = ReadJSONMap(filepath.Join(dir, "settings.json"))
	}
	return userSettingsCache.m
}

// UserSetting reads one top-level key from ~/.claude/settings.json.
func UserSetting(key string) any { return UserSettings()[key] }

// WriteUserSetting merges key=v into ~/.claude/settings.json (indented JSON)
// and drops the cache so the next read sees the change.
func WriteUserSetting(key string, v any) {
	p := filepath.Join(ClaudeDir(), "settings.json")
	m := ReadJSONMap(p)
	m[key] = v
	if out, err := json.MarshalIndent(m, "", "  "); err == nil {
		os.WriteFile(p, out, 0o644)
	}
	InvalidateUserSettings()
}

// InvalidateUserSettings drops the cache so edits made outside
// WriteUserSetting (or by it) are picked up by the next read.
func InvalidateUserSettings() {
	userSettingsCache.mu.Lock()
	userSettingsCache.m = nil
	userSettingsCache.mu.Unlock()
}
