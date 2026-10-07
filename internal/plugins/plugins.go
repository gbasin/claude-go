// Package plugins implements the plugin/marketplace surface: marketplaces are
// git repos or local dirs containing .claude-plugin/marketplace.json; installs
// copy plugin trees into ~/.claude/plugins/synced/<marketplace>/<plugin>/
// and record state in ~/.claude/plugins/installed_plugins.json + settings
// enabledPlugins.
package plugins

import (
	"claude-go/internal/config"
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

func marketplacesFile() string { return filepath.Join(pluginsDir(), "known_marketplaces.json") }

// Real schema: {"plugins": {"<name>@<marketplace>": [{version, scope, ...}]}}.
// Load also tolerates the earlier flat array.
func loadInstalled() []InstalledPlugin {
	var out []InstalledPlugin
	b, err := os.ReadFile(installedFile())
	if err != nil {
		return out
	}
	var wrap struct {
		Plugins map[string][]struct {
			Version     string `json:"version"`
			Scope       string `json:"scope,omitempty"`
			InstallPath string `json:"installPath,omitempty"`
			Enabled     *bool  `json:"enabled,omitempty"`
		} `json:"plugins"`
	}
	if json.Unmarshal(b, &wrap) == nil && wrap.Plugins != nil {
		for key, vers := range wrap.Plugins {
			name, mkt, _ := strings.Cut(key, "@")
			for _, v := range vers {
				ip := InstalledPlugin{Name: name, Marketplace: mkt,
					Version: v.Version, Path: v.InstallPath, Enabled: true}
				if v.Enabled != nil {
					ip.Enabled = *v.Enabled
				}
				out = append(out, ip)
			}
		}
		return out
	}
	var flat []InstalledPlugin
	if json.Unmarshal(b, &flat) == nil {
		return flat
	}
	return out
}

func saveInstalled(p []InstalledPlugin) {
	os.MkdirAll(pluginsDir(), 0o755)
	wrap := map[string]any{"plugins": map[string]any{}}
	for _, ip := range p {
		key := ip.Name + "@" + ip.Marketplace
		wrap["plugins"].(map[string]any)[key] = []map[string]any{{
			"version": ip.Version, "scope": "user",
			"installPath": ip.Path, "installedAt": ip.InstalledAt,
			"enabled": ip.Enabled,
		}}
	}
	b, _ := json.MarshalIndent(wrap, "", "  ")
	os.WriteFile(installedFile(), b, 0o644)
}

// known_marketplaces.json: {name: {source: {...}, autoUpdate: bool}}.
// Reads both real objects and our older {name: path} flat map.
type marketplaceSource struct {
	Source string `json:"source"` // github|git|url|directory|file|pluginDirectory
	Repo   string `json:"repo,omitempty"`
	URL    string `json:"url,omitempty"`
	Path   string `json:"path,omitempty"`
}

type marketplaceEntry struct {
	Source     marketplaceSource `json:"source"`
	AutoUpdate bool              `json:"autoUpdate"`
}

func loadMarketplaces() map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(marketplacesFile())
	if err != nil {
		return out
	}
	var real map[string]marketplaceEntry
	if json.Unmarshal(b, &real) == nil {
		for n, e := range real {
			if e.Source.Path != "" {
				out[n] = e.Source.Path
			} else if p := marketplacePath(n); dirExists(p) {
				out[n] = p
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	var flat map[string]string
	if json.Unmarshal(b, &flat) == nil {
		return flat
	}
	return out
}

func marketplacePath(name string) string {
	return filepath.Join(pluginsDir(), "marketplaces", name)
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func saveMarketplaces(m map[string]string) {
	os.MkdirAll(pluginsDir(), 0o755)
	out := map[string]marketplaceEntry{}
	for name, path := range m {
		src := marketplaceSource{Source: "directory", Path: path}
		if strings.HasPrefix(path, pluginsDir()) {
			src = marketplaceSource{Source: "git", URL: path}
		}
		out[name] = marketplaceEntry{Source: src, AutoUpdate: true}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
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

// EnabledPluginDirs returns paths of enabled plugins (for skill/command/agents
// loading). The settings enabledPlugins/disabledPlugins maps override the
// installed-file flag like real CC (keyed "name@marketplace").
func EnabledPluginDirs() []string {
	enabled, disabled := settingsPluginGates()
	var out []string
	for _, p := range loadInstalled() {
		key := p.Name + "@" + p.Marketplace
		on := p.Enabled
		if v, ok := enabled[key]; ok {
			on = v
		}
		if disabled[key] {
			on = false
		}
		if on {
			out = append(out, p.Path)
		}
	}
	return out
}

func settingsPluginGates() (map[string]bool, map[string]bool) {
	en, dis := map[string]bool{}, map[string]bool{}
	var s struct {
		EnabledPlugins  map[string]bool `json:"enabledPlugins"`
		DisabledPlugins map[string]bool `json:"disabledPlugins"`
	}
	if b, err := json.Marshal(config.UserSettings()); err == nil {
		// a malformed value fails the whole file, like the old direct read
		if json.Unmarshal(b, &s) != nil {
			return en, dis
		}
	}
	for k, v := range s.EnabledPlugins {
		en[k] = v
	}
	for k, v := range s.DisabledPlugins {
		dis[k] = v
	}
	return en, dis
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
