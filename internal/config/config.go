// Package config loads Claude Code settings: ~/.claude.json,
// ~/.claude/settings.json, .claude/settings.json, CLAUDE.md paths.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
)

type Settings struct {
	Permissions *Permissions      `json:"permissions"`
	Model       string            `json:"model"`
	Env         map[string]string `json:"env"`
}

type Permissions struct {
	Allow       []string `json:"allow"`
	Deny        []string `json:"deny"`
	Ask         []string `json:"ask"`
	DefaultMode string   `json:"defaultMode"`
}

// Load merges user + project settings (project wins).
func Load(cwd string) *Settings {
	merged := &Settings{}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		managedSettingsPath(),
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(cwd, ".claude", "settings.json"),
		filepath.Join(cwd, ".claude", "settings.local.json"),
	} {
		var s Settings
		if b, err := os.ReadFile(p); err == nil {
			if json.Unmarshal(b, &s) == nil {
				if s.Permissions != nil {
					if merged.Permissions == nil {
						merged.Permissions = &Permissions{}
					}
					merged.Permissions.Allow = append(merged.Permissions.Allow, s.Permissions.Allow...)
					merged.Permissions.Deny = append(merged.Permissions.Deny, s.Permissions.Deny...)
					merged.Permissions.Ask = append(merged.Permissions.Ask, s.Permissions.Ask...)
					if s.Permissions.DefaultMode != "" {
						merged.Permissions.DefaultMode = s.Permissions.DefaultMode
					}
				}
				if s.Model != "" {
					merged.Model = s.Model
				}
			}
		}
	}
	return merged
}

// Account reads account info from ~/.claude.json.
func Account() (email, accountUUID, deviceID string) {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return "", "", ""
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return "", "", ""
	}
	if v, ok := m["oauthAccount"].(map[string]any); ok {
		email, _ = v["emailAddress"].(string)
		accountUUID, _ = v["accountUuid"].(string)
	}
	if email == "" {
		email, _ = m["email"].(string)
	}
	if v, ok := m["userID"].(string); ok {
		deviceID = v
	}
	return email, accountUUID, deviceID
}

// managedSettingsPath: enterprise-managed settings (CC reads these too).
func managedSettingsPath() string {
	if runtime.GOOS == "darwin" {
		return "/Library/Application Support/ClaudeCode/managed-settings.json"
	}
	return "/etc/claude-code/managed-settings.json"
}
