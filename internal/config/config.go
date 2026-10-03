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
	// policy-tier rules — always evaluated, never overridden
	ManagedDeny []string `json:"-"`
	ManagedAsk  []string `json:"-"`
	managedMode string   `json:"-"`
}

type Permissions struct {
	Allow       []string `json:"allow"`
	Deny        []string `json:"deny"`
	Ask         []string `json:"ask"`
	DefaultMode string   `json:"defaultMode"`
}

// Load merges user + project settings (project wins).
// Load merges settings. policySettings (managed) is highest-precedence:
// its deny/ask rules can't be overridden by user or project settings, and
// its defaultMode wins. Track it separately.
func Load(cwd string) *Settings {
	merged := &Settings{}
	managed := &Settings{}
	if b, err := os.ReadFile(managedSettingsPath()); err == nil {
		json.Unmarshal(b, managed)
	}
	merged.ManagedDeny = append(merged.ManagedDeny, managedPerms(managed, "deny")...)
	merged.ManagedAsk = append(merged.ManagedAsk, managedPerms(managed, "ask")...)
	if managed.Permissions != nil && managed.Permissions.DefaultMode != "" {
		merged.managedMode = managed.Permissions.DefaultMode
		merged.Permissions = &Permissions{DefaultMode: managed.Permissions.DefaultMode}
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
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
					_ = s
					merged.Permissions.Allow = append(merged.Permissions.Allow, s.Permissions.Allow...)
					merged.Permissions.Deny = append(merged.Permissions.Deny, s.Permissions.Deny...)
					merged.Permissions.Ask = append(merged.Permissions.Ask, s.Permissions.Ask...)
					if s.Permissions.DefaultMode != "" && merged.ManagedMode() == "" {
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

func managedPerms(s *Settings, kind string) []string {
	if s.Permissions == nil {
		return nil
	}
	switch kind {
	case "deny":
		return s.Permissions.Deny
	case "ask":
		return s.Permissions.Ask
	}
	return nil
}

// ManagedMode returns the policy-mandated defaultMode, if any.
func (s *Settings) ManagedMode() string {
	return s.managedMode
}
