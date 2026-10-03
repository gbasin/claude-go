// Package plugins implements the plugin/marketplace surface: marketplaces are
// git repos or local dirs containing .claude-plugin/marketplace.json; installs
// copy plugin trees into ~/.claude/plugins/synced/<marketplace>/<plugin>/
// and record state in ~/.claude/plugins/installed_plugins.json + settings
// enabledPlugins.
package plugins

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type MarketplaceEntry struct {
	Name   string `json:"name"`
	Source struct {
		Source string `json:"source"` // github|git|local|url
		Repo   string `json:"repo,omitempty"`
		Path   string `json:"path,omitempty"`
		URL    string `json:"url,omitempty"`
	} `json:"source"`
	Plugins []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Source      string `json:"source,omitempty"` // path inside marketplace
		Version     string `json:"version,omitempty"`
	} `json:"plugins"`
}

type InstalledPlugin struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Marketplace string `json:"marketplace"`
	Path        string `json:"path"`
	Enabled     bool   `json:"enabled"`
	InstalledAt string `json:"installed_at"`
}

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

func pluginsDir() string { return filepath.Join(home(), ".claude", "plugins") }
func syncedDir() string  { return filepath.Join(pluginsDir(), "synced") }

func installedFile() string { return filepath.Join(pluginsDir(), "installed_plugins.json") }

func marketplacesFile() string { return filepath.Join(pluginsDir(), "marketplaces.json") }

func loadInstalled() []InstalledPlugin {
	var out []InstalledPlugin
	if b, err := os.ReadFile(installedFile()); err == nil {
		json.Unmarshal(b, &out)
	}
	return out
}

func saveInstalled(p []InstalledPlugin) {
	os.MkdirAll(pluginsDir(), 0o755)
	b, _ := json.MarshalIndent(p, "", "  ")
	os.WriteFile(installedFile(), b, 0o644)
}

func loadMarketplaces() map[string]string {
	out := map[string]string{}
	if b, err := os.ReadFile(marketplacesFile()); err == nil {
		json.Unmarshal(b, &out)
	}
	return out
}

func saveMarketplaces(m map[string]string) {
	os.MkdirAll(pluginsDir(), 0o755)
	b, _ := json.MarshalIndent(m, "", "  ")
	os.WriteFile(marketplacesFile(), b, 0o644)
}

// AddMarketplace clones/copies a marketplace source and registers it.
// Sources: owner/repo (github), https://… , ./local/path
func AddMarketplace(src string) (string, error) {
	var name, path string
	switch {
	case strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") || strings.Contains(src, "@"):
		name = strings.TrimSuffix(filepath.Base(src), ".git")
		dst := filepath.Join(pluginsDir(), "marketplaces", name)
		if _, err := os.Stat(filepath.Join(dst, ".claude-plugin", "marketplace.json")); err == nil {
			path = dst // already cloned
			break
		}
		if out, err := exec.Command("git", "clone", "--depth", "1", src, dst).CombinedOutput(); err != nil {
			return "", fmt.Errorf("clone failed: %s", out)
		}
		path = dst
	case strings.Contains(src, "/") && !strings.HasPrefix(src, ".") && strings.Count(src, "/") == 1:
		// owner/repo → github
		name = strings.Split(src, "/")[1]
		dst := filepath.Join(pluginsDir(), "marketplaces", name)
		url := "https://github.com/" + src
		if out, err := exec.Command("git", "clone", "--depth", "1", url, dst).CombinedOutput(); err != nil {
			return "", fmt.Errorf("clone %s failed: %s", url, out)
		}
		path = dst
	default:
		abs, err := filepath.Abs(src)
		if err != nil {
			return "", err
		}
		name = filepath.Base(abs)
		path = abs
	}
	if _, err := os.Stat(filepath.Join(path, ".claude-plugin", "marketplace.json")); err != nil {
		return "", fmt.Errorf("no .claude-plugin/marketplace.json in %s", path)
	}
	mkts := loadMarketplaces()
	mkts[name] = path
	saveMarketplaces(mkts)
	return name, nil
}

// Marketplace lists a marketplace's plugins.
func Marketplace(name string) (*MarketplaceEntry, error) {
	mkts := loadMarketplaces()
	path, ok := mkts[name]
	if !ok {
		return nil, fmt.Errorf("unknown marketplace %q (have: %v)", name, keys(mkts))
	}
	b, err := os.ReadFile(filepath.Join(path, ".claude-plugin", "marketplace.json"))
	if err != nil {
		return nil, err
	}
	var m MarketplaceEntry
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Install copies a plugin into the synced dir and records it.
func Install(mktName, pluginName string) error {
	m, err := Marketplace(mktName)
	if err != nil {
		return err
	}
	mkts := loadMarketplaces()
	mktPath := mkts[mktName]
	var plugin *struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Source      string `json:"source,omitempty"`
		Version     string `json:"version,omitempty"`
	}
	for i := range m.Plugins {
		if m.Plugins[i].Name == pluginName {
			plugin = &m.Plugins[i]
			break
		}
	}
	if plugin == nil {
		return fmt.Errorf("no plugin %q in marketplace %s", pluginName, mktName)
	}
	src := mktPath
	if plugin.Source != "" {
		src = filepath.Join(mktPath, plugin.Source)
	}
	dst := filepath.Join(syncedDir(), mktName, pluginName)
	os.RemoveAll(dst)
	if err := copyTree(src, dst); err != nil {
		return err
	}
	inst := loadInstalled()
	ver := plugin.Version
	for i := range inst {
		if inst[i].Name == pluginName && inst[i].Marketplace == mktName {
			inst[i].Path = dst
			inst[i].Version = ver
			inst[i].Enabled = true
			saveInstalled(inst)
			return nil
		}
	}
	inst = append(inst, InstalledPlugin{
		Name: pluginName, Version: ver, Marketplace: mktName,
		Path: dst, Enabled: true, InstalledAt: nowISO(),
	})
	saveInstalled(inst)
	return nil
}

// Remove uninstalls a plugin.
func Remove(name string) error {
	inst := loadInstalled()
	out := inst[:0]
	var found *InstalledPlugin
	for _, p := range inst {
		if p.Name == name {
			cp := p
			found = &cp
			continue
		}
		out = append(out, p)
	}
	if found == nil {
		return fmt.Errorf("plugin not installed: %s", name)
	}
	os.RemoveAll(found.Path)
	saveInstalled(out)
	return nil
}

// List returns installed plugins.
func List() []InstalledPlugin { return loadInstalled() }

// EnabledPluginDirs returns paths of enabled plugins (for skill/command/agents loading).
func EnabledPluginDirs() []string {
	var out []string
	for _, p := range loadInstalled() {
		if p.Enabled {
			out = append(out, p.Path)
		}
	}
	return out
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode())
	})
}

func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// ListMarketplaces returns name → path.
func ListMarketplaces() map[string]string { return loadMarketplaces() }

// SyncedBase is the plugin install root.
func SyncedBase() string { return syncedDir() }
