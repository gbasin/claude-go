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
	Permissions                *Permissions      `json:"permissions"`
	Model                      string            `json:"model"`
	Env                        map[string]string `json:"env"`
	EffortLevel                string            `json:"effortLevel"`
	FastMode                   string            `json:"fastMode"`
	AlwaysThinkingEnabled      bool              `json:"alwaysThinkingEnabled"`
	EnableAllProjectMcpServers bool              `json:"enableAllProjectMcpServers"`
	EnabledMcpjsonServers      []string          `json:"enabledMcpjsonServers"`
	DisabledMcpjsonServers     []string          `json:"disabledMcpjsonServers"`
	IncludeCoAuthoredBy        *bool             `json:"includeCoAuthoredBy"`
	AutoCompactWindow          string            `json:"autoCompactWindow"`
	// policy-tier rules — always evaluated, never overridden
	ManagedDeny  []string `json:"-"`
	ManagedAsk   []string `json:"-"`
	ManagedAllow []string `json:"-"`
	managedMode  string   `json:"-"`
}

type Permissions struct {
	Allow                 []string `json:"allow"`
	Deny                  []string `json:"deny"`
	Ask                   []string `json:"ask"`
	DefaultMode           string   `json:"defaultMode"`
	AdditionalDirectories []string `json:"additionalDirectories"`
	// managed-only gates
	DisableBypassPermissionsMode string `json:"disableBypassPermissionsMode"`
	DisableAutoMode              string `json:"disableAutoMode"`
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
	// managed allow rules also apply (they can't be removed by lower tiers)
	if managed.Permissions != nil {
		merged.ManagedAllow = append(merged.ManagedAllow, managed.Permissions.Allow...)
		if managed.Permissions.DefaultMode != "" {
			merged.managedMode = managed.Permissions.DefaultMode
			merged.Permissions = &Permissions{DefaultMode: managed.Permissions.DefaultMode}
		}
	}
	for _, p := range []string{
		filepath.Join(ClaudeDir(), "settings.json"),
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
				for k, v := range s.Env {
					if merged.Env == nil {
						merged.Env = map[string]string{}
					}
					merged.Env[k] = v // later tier wins per key
				}
				if s.Permissions != nil {
					merged.Permissions.AdditionalDirectories = append(
						merged.Permissions.AdditionalDirectories, s.Permissions.AdditionalDirectories...)
				}
				// other honored keys
				if s.EffortLevel != "" {
					merged.EffortLevel = s.EffortLevel
				}
				if s.FastMode != "" {
					merged.FastMode = s.FastMode
				}
				if s.EnableAllProjectMcpServers {
					merged.EnableAllProjectMcpServers = true
				}
				merged.EnabledMcpjsonServers = append(merged.EnabledMcpjsonServers, s.EnabledMcpjsonServers...)
				merged.DisabledMcpjsonServers = append(merged.DisabledMcpjsonServers, s.DisabledMcpjsonServers...)
				merged.IncludeCoAuthoredBy = s.IncludeCoAuthoredBy
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

// ClaudeDir is the user config home — CLAUDE_CONFIG_DIR overrides ~/.claude.
func ClaudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".claude"
	}
	return filepath.Join(home, ".claude")
}

// Home returns the OS home dir ("" on error).
func Home() string {
	h, _ := os.UserHomeDir()
	return h
}

// ApplyEnv injects the settings env blocks into the process environment —
// real CC sets each declared key before tools/hooks/shells run.
func (s *Settings) ApplyEnv() {
	for k, v := range s.Env {
		if k == "" {
			continue
		}
		os.Setenv(k, v)
	}
}

// CoAuthoredBy reports whether the trailer should be added (default true).
func (s *Settings) CoAuthoredBy() bool {
	return s.IncludeCoAuthoredBy == nil || *s.IncludeCoAuthoredBy
}
