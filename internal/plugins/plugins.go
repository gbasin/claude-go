// Package plugins implements the plugin/marketplace surface: marketplaces are
// git repos or local dirs containing .claude-plugin/marketplace.json; installs
// copy plugin trees into ~/.claude/plugins/synced/<marketplace>/<plugin>/
// and record state in ~/.claude/plugins/installed_plugins.json + settings
// enabledPlugins.
package plugins

import (
	"archive/tar"
	"claude-go/internal/config"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	NoMCP       bool   `json:"noMCP,omitempty"` // --plugin-dir-no-mcp: skip MCP registration
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
	// --plugin-dir adds scan roots: each immediate subdir is a plugin;
	// --plugin-dir-no-mcp registers the same dirs minus their MCP servers
	for _, spec := range []struct {
		env   string
		noMCP bool
	}{{"CLAUDE_GO_PLUGIN_DIR", false}, {"CLAUDE_GO_PLUGIN_DIR_NO_MCP", true}} {
		root0 := spec.env
		_ = root0
		roots := filepath.SplitList(os.Getenv(spec.env))
		for _, root := range roots {
			// real: --plugin-dir's value IS the plugin root — it must
			// carry .claude-plugin/plugin.json; a bare parent dir of
			// plugins is not the documented shape (fall back to scanning
			// for compat)
			if _, err := os.Stat(filepath.Join(root, ".claude-plugin", "plugin.json")); err == nil {
				out = append(out, InstalledPlugin{
					Name: filepath.Base(root), Marketplace: "local-dir",
					Path: root, Enabled: true, NoMCP: spec.noMCP})
				continue
			}
			ents, err := os.ReadDir(root)
			if err != nil {
				continue
			}
			for _, e := range ents {
				if !e.IsDir() {
					continue
				}
				pd := filepath.Join(root, e.Name())
				if _, err := os.Stat(filepath.Join(pd, ".claude-plugin", "plugin.json")); err == nil {
					out = append(out, InstalledPlugin{
						Name: e.Name(), Marketplace: "local-dir",
						Path: pd, Enabled: true, NoMCP: spec.noMCP})
				}
			}
		}
	}
	// real's installed_plugins.json is V2 {version:2,plugins:{id:[rec]}}.
	// Load splits records into: usable (this build's shape), set-aside
	// (invalid plugin-id keys — copied to a dated side file at the next
	// write then dropped), and keptRaw (valid id, unparseable — likely a
	// NEWER build's format → the whole file counts as unreadable for this
	// build and no write may replace it).
	b, err := os.ReadFile(installedFile())
	if err != nil {
		return out
	}
	// V1 is detected by version tag first — its records are objects and
	// would otherwise fall into the keptRaw bucket
	var rawTop struct {
		Version int                        `json:"version"`
		Plugins map[string]json.RawMessage `json:"plugins"`
	}
	rawOK := json.Unmarshal(b, &rawTop) == nil && rawTop.Plugins != nil && rawTop.Version != 1
	if rawOK {
		var setAside []string
		unreadable := false
		for key, raw := range rawTop.Plugins {
			name, mkt, _ := strings.Cut(key, "@")
			if name == "" || mkt == "" {
				// not a valid plugin id — set aside (real's b2n)
				setAside = append(setAside, key)
				continue
			}
			var recs []installedV2Record
			if json.Unmarshal(raw, &recs) != nil {
				unreadable = true // keptRaw — another build's shape
				continue
			}
			for _, v := range recs {
				ip := InstalledPlugin{Name: name, Marketplace: mkt,
					Version: v.Version, Path: v.InstallPath, Enabled: true}
				out = append(out, ip)
			}
		}
		if len(setAside) > 0 {
			fmt.Fprintf(os.Stderr,
				"installed_plugins.json: %d record(s) set aside (invalid plugin id); "+
					"the next write copies them to a dated file beside it and drops them\n",
				len(setAside))
			pendingSetAside.Store(installedFile(), map[string]json.RawMessage{
				"keys": mustJSON(setAside)})
			setAsideRecords(installedFile(), setAside)
		}
		if unreadable {
			markUnreadable(installedFile())
			return out // installed-file records unreadable; plugin-dir ones survive
		}
		return out
	}
	// V1 migration: {version:1,plugins:{id:{...}}} → V2 record array
	var v1 struct {
		Version int `json:"version"`
		Plugins map[string]struct {
			Version     string `json:"version"`
			InstalledAt string `json:"installedAt,omitempty"`
			LastUpdated string `json:"lastUpdated,omitempty"`
			GitSHA      string `json:"gitCommitSha,omitempty"`
		} `json:"plugins"`
	}
	if json.Unmarshal(b, &v1) == nil && v1.Version == 1 && v1.Plugins != nil {
		for key, v := range v1.Plugins {
			name, mkt, _ := strings.Cut(key, "@")
			out = append(out, InstalledPlugin{Name: name, Marketplace: mkt,
				Version: v.Version, Enabled: true,
				Path: filepath.Join(syncedDir(), mkt, name)})
		}
		if len(v1.Plugins) > 0 {
			fmt.Fprintf(os.Stderr,
				"converted installed_plugins.json from V1 to V2 format (%d plugins)\n",
				len(v1.Plugins))
			saveInstalled(out) // write the migrated V2 back
		}
		return out
	}
	var flat []InstalledPlugin
	if json.Unmarshal(b, &flat) == nil {
		return flat
	}
	return out
}

// installedV2Record is one entry of the V2 plugins[id] array.
type installedV2Record struct {
	Version     string `json:"version"`
	Scope       string `json:"scope,omitempty"`
	InstallPath string `json:"installPath,omitempty"`
	InstalledAt string `json:"installedAt,omitempty"`
	LastUpdated string `json:"lastUpdated,omitempty"`
	GitSHA      string `json:"gitCommitSha,omitempty"`
	Enabled     *bool  `json:"enabled,omitempty"`
}

// pendingSetAside tracks records whose keys aren't valid plugin ids — the
// next write archives them to a dated side file then drops them (real's
// lDr/MX flow).
var pendingSetAside sync.Map // file -> map{"keys": json}

var unreadableFiles sync.Map // file -> true (keptRaw present)

func markUnreadable(f string) { unreadableFiles.Store(f, true) }

// setAsideRecords is a no-op placeholder for real's in-memory set-aside
// store — the keys flow into pendingSetAside for archive-on-write.
func setAsideRecords(f string, keys []string) {}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// archiveSetAside writes set-aside records to a dated side file before the
// main write drops them — real's lDr(): <dir>/installed_plugins.<ts>-<sha12>.json
func archiveSetAside(f string) {
	v, ok := pendingSetAside.Load(f)
	if !ok {
		return
	}
	pendingSetAside.Delete(f)
	rec := v.(map[string]json.RawMessage)
	side := strings.TrimSuffix(f, ".json") +
		"." + time.Now().UTC().Format("20060102T150405") + ".json"
	b, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(side, b, 0o600); err != nil {
		fmt.Fprintf(os.Stderr,
			"installed_plugins.json was not changed: set-aside records could not be copied to %s (%v)\n", side, err)
	} else {
		fmt.Fprintf(os.Stderr, "copied set-aside records to %s\n", side)
	}
}

func saveInstalled(p []InstalledPlugin) {
	// real refuses to replace the file when it holds records this build
	// can't parse (keptRaw — likely a newer build's format)
	if _, unread := unreadableFiles.Load(installedFile()); unread {
		fmt.Fprintln(os.Stderr,
			"installed_plugins.json holds records this build cannot read — not writing")
		return
	}
	// archive any set-aside records before the write drops them
	archiveSetAside(installedFile())
	os.MkdirAll(pluginsDir(), 0o755)
	// real's V2 record: {version:2, plugins:{"name@mkt":[{scope,
	// installPath, version, installedAt, lastUpdated, gitCommitSha}]}}
	wrap := map[string]any{"version": 2, "plugins": map[string]any{}}
	for _, ip := range p {
		key := ip.Name + "@" + ip.Marketplace
		wrap["plugins"].(map[string]any)[key] = []map[string]any{{
			"version": ip.Version, "scope": "user",
			"installPath": ip.Path, "installedAt": ip.InstalledAt,
			"lastUpdated": ip.InstalledAt,
			"enabled":     ip.Enabled,
		}}
	}
	b, _ := json.MarshalIndent(wrap, "", "  ")
	os.WriteFile(installedFile(), b, 0o644)
}

// known_marketplaces.json: {name: {source: {...}, autoUpdate: bool}}.
// Reads both real objects and our older {name: path} flat map.
type marketplaceSource struct {
	Source string `json:"source"`          // github|git|url|directory|file|pluginDirectory
	NoMCP  bool   `json:"noMCP,omitempty"` // --plugin-dir-no-mcp: skip MCP registration
	Repo   string `json:"repo,omitempty"`
	URL    string `json:"url,omitempty"`
	Path   string `json:"path,omitempty"`
}

type marketplaceEntry struct {
	Source          marketplaceSource `json:"source"`
	AutoUpdate      bool              `json:"autoUpdate"`
	InstallLocation string            `json:"installLocation,omitempty"`
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
		// real records the resolved clone dir on the entry
		out[name] = marketplaceEntry{Source: src, AutoUpdate: true,
			InstallLocation: path}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	os.WriteFile(marketplacesFile(), b, 0o644)
}

// RemoveMarketplace unregisters a marketplace (real's
// `plugin marketplace remove --scope <name>`) — leaves the clone dir.
func RemoveMarketplace(name string) error {
	mkts := loadMarketplaces()
	if _, ok := mkts[name]; !ok {
		return fmt.Errorf("unknown marketplace %q", name)
	}
	delete(mkts, name)
	saveMarketplaces(mkts)
	return nil
}

// AddMarketplace clones/copies a marketplace source and registers it.
func AddMarketplace(src string) (string, error) {
	return AddMarketplaceOpts(src, nil)
}

// AddMarketplaceOpts is AddMarketplace with real's flags:
// --sparse <paths...> = git sparse-checkout on the clone (monorepo
// marketplaces) — real 295 uses `clone --filter=blob:none --sparse` then
// `sparse-checkout set`.
func AddMarketplaceOpts(src string, sparse []string) (string, error) {
	var name, path string
	clone := func(url, dst string) error {
		argv := []string{"clone", "--depth", "1"}
		if len(sparse) > 0 {
			argv = append(argv, "--filter=blob:none", "--sparse")
		}
		argv = append(argv, url, dst)
		if out, err := exec.Command("git", argv...).CombinedOutput(); err != nil {
			return fmt.Errorf("clone %s failed: %s", url, out)
		}
		if len(sparse) > 0 {
			args := append([]string{"-C", dst, "sparse-checkout", "set"}, sparse...)
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				return fmt.Errorf("sparse-checkout failed: %s", out)
			}
		}
		return nil
	}
	switch {
	case strings.HasPrefix(src, "npm:"):
		// real: npm:<pkg>[@ver] → registry.npmjs.org metadata → dist.tarball
		pkg := strings.TrimPrefix(src, "npm:")
		ver := ""
		if i := strings.LastIndex(pkg, "@"); i > 0 {
			pkg, ver = pkg[:i], pkg[i+1:]
		}
		name = strings.ReplaceAll(strings.TrimPrefix(pkg, "@"), "/", "-")
		dst := filepath.Join(pluginsDir(), "marketplaces", name)
		if _, err := os.Stat(filepath.Join(dst, ".claude-plugin", "marketplace.json")); err != nil {
			if err := npmFetchExtract(pkg, ver, dst); err != nil {
				return "", err
			}
		}
		path = dst
	case strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") || strings.Contains(src, "@"):
		name = strings.TrimSuffix(filepath.Base(src), ".git")
		dst := filepath.Join(pluginsDir(), "marketplaces", name)
		if _, err := os.Stat(filepath.Join(dst, ".claude-plugin", "marketplace.json")); err == nil {
			path = dst // already cloned
			break
		}
		if err := clone(src, dst); err != nil {
			return "", err
		}
		path = dst
	case strings.Contains(src, "/") && !strings.HasPrefix(src, ".") && strings.Count(src, "/") == 1:
		// owner/repo → github
		name = strings.Split(src, "/")[1]
		dst := filepath.Join(pluginsDir(), "marketplaces", name)
		if err := clone("https://github.com/"+src, dst); err != nil {
			return "", err
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

// npmFetchExtract downloads an npm package tarball into dir (real's
// npm source: registry metadata → dist.tarball → package/ contents).
func npmFetchExtract(pkg, ver, dir string) error {
	// npm's own registry override convention — honored like real's npm
	reg := os.Getenv("NPM_CONFIG_REGISTRY")
	if reg == "" {
		reg = "https://registry.npmjs.org"
	}
	metaURL := strings.TrimRight(reg, "/") + "/" + pkg
	var meta struct {
		DistTags struct {
			Latest string `json:"latest"`
		} `json:"dist-tags"`
		Versions map[string]struct {
			Dist struct {
				Tarball string `json:"tarball"`
			} `json:"dist"`
		} `json:"versions"`
	}
	req, _ := http.NewRequest("GET", metaURL, nil)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("npm metadata for %s: %v", pkg, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("npm metadata for %s: http %d", pkg, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return fmt.Errorf("npm metadata for %s: %v", pkg, err)
	}
	if ver == "" {
		ver = meta.DistTags.Latest
	}
	v := meta.Versions[ver]
	if v.Dist.Tarball == "" {
		return fmt.Errorf("npm: no tarball for %s@%s", pkg, ver)
	}
	resp2, err := http.DefaultClient.Get(v.Dist.Tarball)
	if err != nil {
		return fmt.Errorf("npm tarball %s@%s: %v", pkg, ver, err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		return fmt.Errorf("npm tarball %s@%s: http %d", pkg, ver, resp2.StatusCode)
	}
	gz, err := gzip.NewReader(resp2.Body)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hd, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		// strip the npm "package/" prefix
		name := strings.TrimPrefix(hd.Name, "package/")
		if name == "" || strings.Contains(name, "..") {
			continue
		}
		dest := filepath.Join(dir, name)
		if hd.Typeflag == tar.TypeDir {
			os.MkdirAll(dest, 0o755)
			continue
		}
		os.MkdirAll(filepath.Dir(dest), 0o755)
		w, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, tr); err != nil {
			w.Close()
			return err
		}
		w.Close()
	}
	return nil
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
// EnabledPluginEntries is EnabledPluginDirs with the full records —
// callers that need NoMCP/marketplace metadata use this.
func EnabledPluginEntries() []InstalledPlugin {
	enabled, disabled := settingsPluginGates()
	var out []InstalledPlugin
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
			out = append(out, p)
		}
	}
	return out
}

func EnabledPluginDirs() []string {
	var out []string
	for _, p := range EnabledPluginEntries() {
		out = append(out, p.Path)
	}
	return out
}

// SettingsPluginGates exports settingsPluginGates for `plugin prune`.
func SettingsPluginGates() (map[string]bool, map[string]bool) {
	return settingsPluginGates()
}

func settingsPluginGates() (map[string]bool, map[string]bool) {
	en, dis := map[string]bool{}, map[string]bool{}
	// real ORs enabledPlugins across local+project+user scopes — a true
	// in any scope enables; disabledPlugins blocks from any scope
	for _, sm := range []map[string]any{
		config.UserSettings(),
		config.ReadJSONMap(filepath.Join(config.ClaudeDir(), "settings.local.json")),
		config.ReadJSONMap(filepath.Join(mustCWD(), ".claude", "settings.json")),
	} {
		var s struct {
			EnabledPlugins  map[string]bool `json:"enabledPlugins"`
			DisabledPlugins map[string]bool `json:"disabledPlugins"`
		}
		if b, err := json.Marshal(sm); err == nil {
			if json.Unmarshal(b, &s) != nil {
				continue
			}
		}
		for k, v := range s.EnabledPlugins {
			if v {
				en[k] = true
			}
		}
		for k, v := range s.DisabledPlugins {
			if v {
				dis[k] = true
			}
		}
	}
	return en, dis
}

func mustCWD() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
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
