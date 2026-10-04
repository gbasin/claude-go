package browsermcp

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
)

// wiring.go — the in-session enablement path, mirroring mod_0659.js
// (which calls q_e(dc, gme()) and W1e()) plus mod_0278.js (I$t, W1e, zG,
// UG):
//
//	Enabled(chromeFlag)         — I$t(): policy + scope + flag + setting
//	SessionSetup()              — W1e(): {mcpConfig, allowedTools,
//	                              systemPrompt} + native-host manifest
//	                              install
//	DynamicServerConfig()       — gme(): {type:"stdio", command:
//	                              <this binary>, args:[--claude-in-chrome-mcp],
//	                              scope:"dynamic"}
//
// Real's tengu_chrome_auto_enable GrowthBook check and the auto-enable
// dialog (Q$r/l5n) are not reproduced — opt-in is flag/setting only.

//go:embed prompt.txt
var chromeSystemPrompt string

// chromeScopesAccepted mirrors DQe(): the OAuth token must carry a scope
// /api/oauth/validate accepts for the bridge — user:profile, user:office,
// or user:ccr_inference.
var chromeScopesAccepted = []string{"user:profile", "user:office", "user:ccr_inference"}

// ErrScopeRejected mirrors the DQe() warning line.
const scopeRejectMsg = "[Claude in Chrome] Disabled: OAuth token has no scope accepted by /api/oauth/validate (needs user:profile, user:office, or user:ccr_inference; env-var and setup-token sessions default to user:inference only)"

// EnabledInput carries the bits I$t() reads.
type EnabledInput struct {
	ChromeFlag        *bool // --chrome → true, --no-chrome → false, absent → nil
	SafeMode          bool  // Ir(): --safe-mode / --restricted
	OAuthScopes       []string
	EnvTokenSession   bool // CLAUDE_CODE_OAUTH_TOKEN / CLAUDE_GO_TOKEN set
	PolicyDenied      bool // wk() outcome — caller injects the verdict
	DefaultEnabledSet bool // claudeInChromeDefaultEnabled present
	DefaultEnabled    bool // its value
	Warn              func(string)
}

// Enabled mirrors I$t(e) (mod_0278.js).
func Enabled(in EnabledInput) bool {
	warn := in.Warn
	if warn == nil {
		warn = func(string) {}
	}
	if in.PolicyDenied {
		return false
	}
	if in.EnvTokenSession {
		// env-var / setup-token sessions carry user:inference — rejected
		warn(scopeRejectMsg)
		return false
	}
	if len(in.OAuthScopes) > 0 {
		ok := false
		for _, s := range in.OAuthScopes {
			for _, want := range chromeScopesAccepted {
				if s == want {
					ok = true
				}
			}
		}
		if !ok {
			warn(scopeRejectMsg)
			return false
		}
	}
	if in.ChromeFlag != nil {
		return *in.ChromeFlag
	}
	if in.SafeMode {
		return false
	}
	if v := os.Getenv("CLAUDE_CODE_ENABLE_CFC"); v != "" {
		return v == "true"
	}
	if in.DefaultEnabledSet {
		return in.DefaultEnabled
	}
	return false
}

// DynamicServerConfig mirrors gme(): spawn this binary in MCP mode.
func DynamicServerConfig() map[string]any {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = os.Args[0]
	}
	return map[string]any{
		"type":    "stdio",
		"command": exe,
		"args":    []string{FlagClaudeInChromeMCP},
		"scope":   "dynamic",
	}
}

// SystemPrompt mirrors the systemPrompt half of W1e()/n1r() —
// CLAUDE_CODE_DISABLE_CFC_PROMPT suppresses it. Our tool surface is never
// deferred (no ToolSearch), so this is the Y8e(false) variant.
func SystemPrompt() string {
	if os.Getenv("CLAUDE_CODE_DISABLE_CFC_PROMPT") == "true" {
		return ""
	}
	return chromeSystemPrompt
}

// nativeHostManifest mirrors the manifest object in UG() (mod_0278.js).
type nativeHostManifest struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Path           string   `json:"path"`
	Type           string   `json:"type"`
	AllowedOrigins []string `json:"allowed_origins"`
}

const nativeHostName = "com.anthropic.claude_code_browser_extension" // jb

// chromeExtOrigin is the bundled allowed_origins entry.
const chromeExtOrigin = "chrome-extension://fcoeoabgfenejglbffodgkkbkcdhcgfn/"

// nativeHostDirs mirrors $G()/w6r(): where the manifest lands per-OS.
func nativeHostDirs() []string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return []string{filepath.Join(home, "Library", "Application Support", "Google", "Chrome", "NativeMessagingHosts")}
	case "linux":
		return []string{filepath.Join(home, ".config", "google-chrome", "NativeMessagingHosts")}
	default:
		return nil // windows uses registry + a .bat; not written here
	}
}

// wrapperPath mirrors zG's target: ~/.claude/chrome/chrome-native-host.
func wrapperPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "chrome", "chrome-native-host")
}

// InstallNativeHost mirrors zG+UG (unix): write the wrapper script that
// execs `<this binary> --chrome-native-host`, then drop the manifest into
// Chrome's NativeMessagingHosts dir. Returns paths written.
func InstallNativeHost(log Logger) []string {
	dirs := nativeHostDirs()
	if len(dirs) == 0 {
		return nil
	}
	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = os.Args[0]
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	wp := wrapperPath()
	if err := os.MkdirAll(filepath.Dir(wp), 0o755); err != nil {
		log.Warnf("[Claude in Chrome] Failed to install native host: %v", err)
		return nil
	}
	script := "#!/bin/sh\n# Chrome native host wrapper script\n# Generated by Claude Code - do not edit manually\nexec \"" +
		exe + "\" --chrome-native-host\n"
	if err := os.WriteFile(wp, []byte(script), 0o755); err != nil {
		log.Warnf("[Claude in Chrome] Failed to install native host: %v", err)
		return nil
	}
	manifest := nativeHostManifest{
		Name:           nativeHostName,
		Description:    "Claude Code Browser Extension Native Host",
		Path:           wp,
		Type:           "stdio",
		AllowedOrigins: []string{chromeExtOrigin},
	}
	body, _ := json.MarshalIndent(manifest, "", "  ")
	var written []string
	for _, dir := range dirs {
		p := filepath.Join(dir, nativeHostName+".json")
		existing, rerr := os.ReadFile(p)
		if rerr == nil && string(existing) == string(body) {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			continue
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			log.Warnf("[Claude in Chrome] Failed to install manifest at %s: %v", p, err)
			continue
		}
		log.Infof("[Claude in Chrome] Installed native host manifest at: %s", p)
		written = append(written, p)
	}
	return written
}

// SessionSetup mirrors W1e(): manifest install + the pieces a session
// consumes. AllowedTools is always empty (real: n=[]).
func SessionSetup(log Logger) (mcpConfig map[string]any, allowedTools []string, systemPrompt string) {
	if log == nil {
		log = StdLogger{}
	}
	go func() {
		defer func() { _ = recover() }()
		InstallNativeHost(log)
	}()
	return map[string]any{ServerName: DynamicServerConfig()}, nil, SystemPrompt()
}
