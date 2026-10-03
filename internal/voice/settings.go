package voice

import (
	"encoding/json"
	"os"
	"path/filepath"

	"claude-go/internal/config"
)

// Config is the resolved voice settings view: ~/.claude/settings.json
// then .claude/settings.json then .claude/settings.local.json (later wins)
// — the tiers real's userSettings merges over.
type Config struct {
	Enabled    bool   // voice.enabled (written by /voice)
	Mode       string // voice.mode: "hold"|"tap" (default "hold")
	AutoSubmit bool   // voice.autoSubmit
	TopLevel   bool   // voiceEnabled top-level flag
	Language   string // top-level language setting
}

// LoadConfig merges the voice fields across the settings tiers.
func LoadConfig(cwd string) Config {
	var out Config
	for _, p := range settingsPaths(cwd) {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var st struct {
			VoiceEnabled *bool  `json:"voiceEnabled"`
			Language     string `json:"language"`
			Voice        *struct {
				Enabled    *bool  `json:"enabled"`
				Mode       string `json:"mode"`
				AutoSubmit *bool  `json:"autoSubmit"`
			} `json:"voice"`
		}
		if json.Unmarshal(b, &st) != nil {
			continue
		}
		if st.VoiceEnabled != nil {
			out.TopLevel = *st.VoiceEnabled
		}
		if st.Language != "" {
			out.Language = st.Language
		}
		if st.Voice != nil {
			if st.Voice.Enabled != nil {
				out.Enabled = *st.Voice.Enabled
			}
			if st.Voice.Mode != "" {
				out.Mode = st.Voice.Mode
			}
			if st.Voice.AutoSubmit != nil {
				out.AutoSubmit = *st.Voice.AutoSubmit
			}
		}
	}
	if out.Mode == "" {
		out.Mode = "hold" // real: settings.voice?.mode ?? "hold"
	}
	return out
}

// Active reports the effective enablement — real gates on the user-intent
// flag (voiceEnabled) and the voice sub-object's enabled bit.
func (c Config) Active() bool {
	return c.Enabled || c.TopLevel
}

func settingsPaths(cwd string) []string {
	return []string{
		filepath.Join(config.ClaudeDir(), "settings.json"),
		filepath.Join(cwd, ".claude", "settings.json"),
		filepath.Join(cwd, ".claude", "settings.local.json"),
	}
}

// writeUserVoiceSettings mirrors _n("userSettings", {...}): merge the
// voice fields into ~/.claude/settings.json.
func writeUserVoiceSettings(enabled bool, mode string) error {
	p := filepath.Join(config.ClaudeDir(), "settings.json")
	var m map[string]any
	if b, err := os.ReadFile(p); err == nil {
		if json.Unmarshal(b, &m) != nil {
			m = nil
		}
	}
	if m == nil {
		m = map[string]any{}
	}
	m["voiceEnabled"] = enabled
	v, _ := m["voice"].(map[string]any)
	if v == nil {
		v = map[string]any{}
	}
	v["enabled"] = enabled
	if mode != "" {
		v["mode"] = mode
	}
	m["voice"] = v
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Dir(p), 0o755)
	return os.WriteFile(p, out, 0o644)
}

// Enable persists voiceEnabled:true + voice:{enabled:true, mode} — the
// _n("userSettings") call real's /voice makes.
func Enable(mode string) error {
	if mode == "" {
		mode = "hold"
	}
	return writeUserVoiceSettings(true, mode)
}

// Disable persists voiceEnabled:false + voice.enabled:false.
func Disable() error {
	return writeUserVoiceSettings(false, "")
}
