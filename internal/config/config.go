// Package config loads Claude Code settings: ~/.claude.json,
// ~/.claude/settings.json, .claude/settings.json, CLAUDE.md paths.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
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
	AdvisorModel               string            `json:"advisorModel"`
	// disableRemoteControl gates `claude remote-control` / Remote Control;
	// true in ANY tier (managed, user, project) disables it (mod_1259.js Xe()).
	DisableRemoteControl bool `json:"disableRemoteControl"`
	// DisableClaudeAiConnectors gates claude.ai connector auto-mount
	// (mod_0386.js SLn: yme() check — "disabled_setting" eligibility state).
	DisableClaudeAiConnectors bool `json:"disableClaudeAiConnectors"`
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
	if managed.DisableRemoteControl {
		merged.DisableRemoteControl = true
	}
	if managed.DisableClaudeAiConnectors {
		merged.DisableClaudeAiConnectors = true
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
		if !SettingSourceOK(cwd, p) {
			continue
		}
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
				if s.AutoCompactWindow != "" {
					merged.AutoCompactWindow = s.AutoCompactWindow
				}
				if s.AdvisorModel != "" {
					merged.AdvisorModel = s.AdvisorModel
				}
				if s.EnableAllProjectMcpServers {
					merged.EnableAllProjectMcpServers = true
				}
				merged.EnabledMcpjsonServers = append(merged.EnabledMcpjsonServers, s.EnabledMcpjsonServers...)
				merged.DisabledMcpjsonServers = append(merged.DisabledMcpjsonServers, s.DisabledMcpjsonServers...)
				merged.IncludeCoAuthoredBy = s.IncludeCoAuthoredBy
				if s.DisableRemoteControl {
					merged.DisableRemoteControl = true
				}
				if s.DisableClaudeAiConnectors {
					merged.DisableClaudeAiConnectors = true
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

// OrganizationUUID reads oauthAccount.organizationUuid from ~/.claude.json
// (real's z2() — the org that owns :orgUUID-scoped endpoints such as
// /api/oauth/organizations/:orgUUID/sync/github/auth, mod_0963.js).
func OrganizationUUID() string {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	if v, ok := m["oauthAccount"].(map[string]any); ok {
		s, _ := v["organizationUuid"].(string)
		return s
	}
	return ""
}

// managedSettingsPath: enterprise-managed settings (CC reads these too).
func managedSettingsPath() string {
	if p := os.Getenv("CLAUDE_GO_MANAGED_SETTINGS"); p != "" {
		return p // --managed-settings
	}
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

// ReadClaudeJSON parses ~/.claude.json; on parse failure the file is copied
// to .claude.json.corrupted.<ts> (real CC's auto-repair) and an empty map
// returned. Returns the path too so callers can write back.
func ReadClaudeJSON() (map[string]any, string) {
	p := filepath.Join(Home(), ".claude.json")
	m := map[string]any{}
	b, err := os.ReadFile(p)
	if err != nil {
		return m, p
	}
	if json.Unmarshal(b, &m) != nil || m == nil {
		// preserve the corrupt file instead of silently clobbering it
		ts := time.Now().Unix()
		os.WriteFile(p+fmt.Sprintf(".corrupted.%d", ts), b, 0o600)
		m = map[string]any{}
	}
	return m, p
}

// WriteClaudeJSON writes the map back atomically (tmp + rename).
func WriteClaudeJSON(p string, m map[string]any) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err == nil {
		os.Rename(tmp, p)
	}
}

// OAuthAccount mirrors real's oauthAccount block in .claude.json — the
// fields the GB eval attributes payload reads.
type OAuthAccountInfo struct {
	OrganizationUUID          string
	OrganizationRole          string
	OrganizationRateLimitTier string
	SubscriptionCreatedAt     string
}

func OAuthAccount() OAuthAccountInfo {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return OAuthAccountInfo{}
	}
	var m struct {
		OAuthAccount struct {
			OrganizationUUID          string `json:"organizationUuid"`
			OrganizationRole          string `json:"organizationRole"`
			OrganizationRateLimitTier string `json:"organizationRateLimitTier"`
			SubscriptionCreatedAt     string `json:"subscriptionCreatedAt"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(b, &m) != nil {
		return OAuthAccountInfo{}
	}
	return OAuthAccountInfo{
		OrganizationUUID:          m.OAuthAccount.OrganizationUUID,
		OrganizationRole:          m.OAuthAccount.OrganizationRole,
		OrganizationRateLimitTier: m.OAuthAccount.OrganizationRateLimitTier,
		SubscriptionCreatedAt:     m.OAuthAccount.SubscriptionCreatedAt,
	}
}

// FirstTokenTime is statsig's firstTokenTime attr — earliest userID-
// stamped onboarding ms in .claude.json.
func FirstTokenTime() int64 {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return 0
	}
	var m struct {
		FirstStartTime string `json:"firstStartTime"`
		Onboarding     struct {
			FirstTokenTime int64 `json:"firstTokenTime"`
		} `json:"onboarding"`
	}
	if json.Unmarshal(b, &m) != nil {
		return 0
	}
	if m.Onboarding.FirstTokenTime != 0 {
		return m.Onboarding.FirstTokenTime
	}
	return 0
}

// MergeCachedGBFeatures folds an eval feature map into
// cachedGrowthBookFeatures — real's cache write.
func MergeCachedGBFeatures(feats map[string]any) {
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".claude.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return
	}
	cur, _ := m["cachedGrowthBookFeatures"].(map[string]any)
	if cur == nil {
		cur = map[string]any{}
	}
	for k, v := range feats {
		cur[k] = v
	}
	m["cachedGrowthBookFeatures"] = cur
	WriteClaudeJSON(p, m)
}

// GBFeature reads a cached GrowthBook feature value (real's
// cachedGrowthBookFeatures persisted to ~/.claude.json).
func GBFeature(name string) any {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	cur, _ := m["cachedGrowthBookFeatures"].(map[string]any)
	if cur == nil {
		return nil
	}
	return cur[name]
}

// SettingSourceOK reports whether a settings file's layer is enabled by
// --setting-sources (CLAUDE_GO_SETTING_SOURCES, comma list of
// user/project/local/managed). Unset = all layers load.
func SettingSourceOK(cwd, path string) bool {
	raw := os.Getenv("CLAUDE_GO_SETTING_SOURCES")
	if raw == "" {
		return true
	}
	layer := "project"
	switch path {
	case filepath.Join(ClaudeDir(), "settings.json"):
		layer = "user"
	case filepath.Join(cwd, ".claude", "settings.local.json"):
		layer = "local"
	case filepath.Join(cwd, ".claude", "settings.json"):
		layer = "project"
	default:
		layer = "managed"
	}
	for _, s := range strings.Split(raw, ",") {
		if strings.TrimSpace(s) == layer {
			return true
		}
	}
	return false
}

// LoadPermissions merges permissions across enabled settings layers.
