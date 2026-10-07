package e2e

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// waitFileHas polls for a substring inside a file.
func waitFileHas(t *testing.T, path, sub string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), sub) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// InstructionsLoaded fires per loaded CLAUDE.md at startup.
func TestInstructionsLoadedHook(t *testing.T) {
	s := newSession(t)
	s.writeFile("CLAUDE.md", "# test memory\n")
	s.writeFile(".claude/settings.json", `{"hooks":{"InstructionsLoaded":[{"hooks":[{"type":"command","command":"echo INSTR-LOADED >> .hooklog"}]}]}}`)
	p := startREPL(t, s)
	p.waitFor("❯", 15*time.Second)
	time.Sleep(500 * time.Millisecond)
	p.exit()
	if !waitFileHas(t, s.cwd+"/.hooklog", "INSTR-LOADED", 8*time.Second) {
		t.Fatal("InstructionsLoaded hook did not fire")
	}
}

// ConfigChange fires when /config writes a setting.
func TestConfigChangeHook(t *testing.T) {
	s := newSession(t)
	s.writeFile(".claude/settings.json", `{"hooks":{"ConfigChange":[{"hooks":[{"type":"command","command":"echo CONFIG-CHANGED >> .hooklog"}]}]}}`)
	p := startREPL(t, s)
	p.waitFor("❯", 15*time.Second)
	p.sendUntilEchoed(t, "/config keybindings vim\r", "❯ /config", 8)
	p.waitFor("set keybindings", 10*time.Second) // prove the write path ran
	if !waitFileHas(t, s.cwd+"/.hooklog", "CONFIG-CHANGED", 8*time.Second) {
		ents, _ := os.ReadDir(s.cwd)
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("ConfigChange hook did not fire; cwd files=%v settings=%s",
			names, func() string { b, _ := os.ReadFile(s.cwd + "/.claude/settings.json"); return string(b) }())
	}
	p.exit()
}

// Setup fires once on first run (onboarding completion).
func TestSetupHook(t *testing.T) {
	s := newSession(t)
	s.writeFile(".claude/settings.json", `{"hooks":{"Setup":[{"hooks":[{"type":"command","command":"echo SETUP-RAN >> .hooklog"}]}]}}`)
	// clear the harness's pre-seeded hasCompletedOnboarding → onboarding
	// path runs → Setup hook fires
	var cj map[string]any
	if b, err := os.ReadFile(s.home + "/.claude.json"); err == nil {
		_ = json.Unmarshal(b, &cj)
		delete(cj, "hasCompletedOnboarding")
		b, _ = json.Marshal(cj)
		_ = os.WriteFile(s.home+"/.claude.json", b, 0o644)
	}
	r := s.run(t, "--mock", "-p", "hi")
	if !waitFileHas(t, s.cwd+"/.hooklog", "SETUP-RAN", 8*time.Second) {
		t.Fatalf("Setup hook did not fire on first run: %s", r.stdout)
	}
}
