package plugins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// setHome redirects ~/.claude/plugins at a temp dir.
func setHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, ".claude"))
	return dir
}

// mkMarketplace builds a local marketplace dir with one plugin entry
// whose source is a sibling dir containing plugin.json.
func mkMarketplace(t *testing.T) (mktDir, pluginSrc string) {
	t.Helper()
	mktDir = t.TempDir()
	pluginSrc = filepath.Join(mktDir, "plugins", "hello")
	manifest := filepath.Join(pluginSrc, ".claude-plugin")
	if err := os.MkdirAll(manifest, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(manifest, "plugin.json"),
		[]byte(`{"name":"hello","version":"1.2.3"}`), 0o644)
	os.WriteFile(filepath.Join(pluginSrc, "marker.txt"), []byte("x"), 0o644)

	pmDir := filepath.Join(mktDir, ".claude-plugin")
	if err := os.MkdirAll(pmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mkt := `{"name":"test-mkt","source":{"source":"local"},"plugins":[
		{"name":"hello","description":"d","source":"plugins/hello","version":"1.2.3"}]}`
	if err := os.WriteFile(filepath.Join(pmDir, "marketplace.json"), []byte(mkt), 0o644); err != nil {
		t.Fatal(err)
	}
	return mktDir, pluginSrc
}

func TestInstalledRoundTrip(t *testing.T) {
	setHome(t)
	in := []InstalledPlugin{{
		Name: "p", Version: "1.0.0", Marketplace: "m",
		Path: "/tmp/x", Enabled: true, InstalledAt: "2026-01-01T00:00:00Z",
	}}
	saveInstalled(in)
	got := loadInstalled()
	if len(got) != 1 || got[0].Name != "p" || got[0].Marketplace != "m" || !got[0].Enabled {
		t.Fatalf("round trip: %+v", got)
	}
}

func TestLoadInstalledFlatArray(t *testing.T) {
	setHome(t)
	// legacy schema: bare array instead of {"plugins": {name: [...]}}
	os.MkdirAll(pluginsDir(), 0o755)
	b, _ := json.Marshal([]InstalledPlugin{{Name: "old", Marketplace: "m", Enabled: true}})
	if err := os.WriteFile(installedFile(), b, 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadInstalled()
	if len(got) != 1 || got[0].Name != "old" {
		t.Fatalf("flat schema: %+v", got)
	}
}

func TestAddMarketplaceLocalDir(t *testing.T) {
	setHome(t)
	mktDir, _ := mkMarketplace(t)
	name, err := AddMarketplace(mktDir)
	if err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}
	if name != filepath.Base(mktDir) {
		t.Fatalf("name = %q", name)
	}
	mkts := loadMarketplaces()
	if mkts[name] != mktDir {
		t.Fatalf("marketplaces: %v", mkts)
	}
	if _, err := Marketplace(name); err != nil {
		t.Fatalf("Marketplace: %v", err)
	}
}

func TestAddMarketplaceNoManifest(t *testing.T) {
	setHome(t)
	dir := t.TempDir() // no .claude-plugin/marketplace.json
	if _, err := AddMarketplace(dir); err == nil {
		t.Fatal("expected error for missing marketplace.json")
	}
}

func TestMarketplaceUnknown(t *testing.T) {
	setHome(t)
	if _, err := Marketplace("nope"); err == nil {
		t.Fatal("expected unknown-marketplace error")
	}
}

func TestInstallRemoveList(t *testing.T) {
	setHome(t)
	mktDir, pluginSrc := mkMarketplace(t)
	name, err := AddMarketplace(mktDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Install(name, "hello"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	lst := List()
	if len(lst) != 1 || lst[0].Name != "hello" || !lst[0].Enabled || lst[0].Version != "1.2.3" {
		t.Fatalf("List: %+v", lst)
	}
	// copyTree actually copied the plugin contents
	if _, err := os.Stat(filepath.Join(lst[0].Path, "marker.txt")); err != nil {
		t.Fatalf("installed tree missing marker: %v", err)
	}
	dirs := EnabledPluginDirs()
	if len(dirs) != 1 || dirs[0] != lst[0].Path {
		t.Fatalf("EnabledPluginDirs: %v", dirs)
	}
	// reinstall replaces, doesn't duplicate
	if err := Install(name, "hello"); err != nil {
		t.Fatal(err)
	}
	if n := len(List()); n != 1 {
		t.Fatalf("reinstall left %d records", n)
	}
	if err := Remove("hello"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if n := len(List()); n != 0 {
		t.Fatalf("after remove: %d", n)
	}
	if _, err := os.Stat(filepath.Join(pluginSrc, "marker.txt")); err != nil {
		t.Fatal("remove shouldn't touch the marketplace source")
	}
	if err := Remove("hello"); err == nil {
		t.Fatal("second remove should fail")
	}
}

func TestSettingsGateOverrides(t *testing.T) {
	setHome(t)
	mktDir, _ := mkMarketplace(t)
	name, _ := AddMarketplace(mktDir)
	if err := Install(name, "hello"); err != nil {
		t.Fatal(err)
	}
	key := "hello@" + name
	// disabledPlugins in settings.local.json blocks the entry
	sj, _ := json.Marshal(map[string]any{
		"disabledPlugins": map[string]bool{key: true}})
	cfg := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.local.json")
	os.MkdirAll(filepath.Dir(cfg), 0o755)
	if err := os.WriteFile(cfg, sj, 0o644); err != nil {
		t.Fatal(err)
	}
	ents := EnabledPluginEntries()
	if len(ents) != 0 {
		t.Fatalf("disabledPlugins ignored: %+v", ents)
	}
	// enabledPlugins can't resurrect it (disabled wins)
	sj, _ = json.Marshal(map[string]any{
		"enabledPlugins":  map[string]bool{key: true},
		"disabledPlugins": map[string]bool{key: true}})
	os.WriteFile(cfg, sj, 0o644)
	if n := len(EnabledPluginEntries()); n != 0 {
		t.Fatalf("disabled should win over enabled: %d", n)
	}
}

func TestUnreadableTracked(t *testing.T) {
	setHome(t)
	markUnreadable("/etc/passwd")
	if v, ok := unreadableFiles.Load("/etc/passwd"); !ok || v != true {
		t.Fatal("markUnreadable didn't record")
	}
}
