package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSettingSourcesFilter(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_GO_SETTING_SOURCES", "project")

	write := func(p, model string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(`{"model":"`+model+`"}`), 0o644)
	}
	write(filepath.Join(home, ".claude", "settings.json"), "user-model")
	write(filepath.Join(cwd, ".claude", "settings.json"), "project-model")
	write(filepath.Join(cwd, ".claude", "settings.local.json"), "local-model")

	// project-only: user + local tiers excluded
	if got := Load(cwd).Model; got != "project-model" {
		t.Fatalf("source filter not applied: %q", got)
	}
	t.Setenv("CLAUDE_GO_SETTING_SOURCES", "user,local")
	// later tier wins → local
	if got := Load(cwd).Model; got != "local-model" {
		t.Fatalf("user,local should yield local-model: %q", got)
	}
	t.Setenv("CLAUDE_GO_SETTING_SOURCES", "")
	if got := Load(cwd).Model; got != "local-model" {
		t.Fatalf("default should read all tiers: %q", got)
	}
}
