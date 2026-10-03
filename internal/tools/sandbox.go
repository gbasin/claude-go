// Sandbox settings + command wrapping for the Bash tool.
//
// Reimplements the `sandbox` settings block of Claude Code 2.1.288:
//   - settings schema: mod_0056.js GTn / mod_1164.js (network, filesystem,
//     credentials, ignoreViolations)
//   - availability modes: mod_0386.js oee (macos seatbelt / linux bubblewrap)
//   - seatbelt path lists: mod_0386.js _jn (deny-read), kjn (read exceptions),
//     Sct (device nodes)
//
// Files are merged in CC precedence order: managed/policy settings are applied
// first and pin ("lock") any field they set so later sources cannot relax
// them; then user (~/.claude/settings.json), project (.claude/settings.json)
// and local (.claude/settings.local.json). List fields merge across sources;
// scalar booleans take the last non-locked value.
package tools

import (
	"claude-go/internal/config"
	"claude-go/internal/netproxy"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// ---------- settings schema ----------

// SandboxSettings is the resolved `sandbox` block from all settings files.
type SandboxSettings struct {
	Enabled                  bool
	FailIfUnavailable        bool
	AutoAllowBashIfSandboxed bool // default true
	AllowUnsandboxedCommands bool // default true
	ExcludedCommands         []string
	IgnoreViolations         map[string][]string // command pattern -> paths; parsed, not yet enforced
	Filesystem               SandboxFilesystem
	Network                  *SandboxNetwork // nil = no network block in settings
	Credentials              SandboxCredentials
	BwrapPath                string // Linux only: absolute path to the bwrap binary

	locked          map[string]bool // scalar keys pinned by managed/policy settings
	policyAllowRead []string        // allowRead entries contributed by the policy tier
}

// SandboxFilesystem mirrors sandbox.filesystem (mod_1164.js G).
type SandboxFilesystem struct {
	Disabled                  bool // skip filesystem isolation entirely
	AllowRead                 []string
	DenyRead                  []string
	AllowWrite                []string
	DenyWrite                 []string
	AllowManagedReadPathsOnly bool // only policy-tier allowRead applies
}

// SandboxNetwork mirrors sandbox.network (mod_1164.js q, trimmed).
type SandboxNetwork struct {
	AllowedDomains      []string
	DeniedDomains       []string // a bare "*" denies all egress
	AllowedCidrs        []string
	AllowUnixSockets    []string // macOS only
	AllowAllUnixSockets bool
	AllowAllLocal       bool // claude-go extension: like real's allowLocalBinding
	// InteractiveHostApproval upgrades "not on the allow list" denials into
	// an interactive prompt (real's non-strictAllowlist ask callback). Hosts
	// approved this way land on the proxy's session allowlist.
	InteractiveHostApproval bool
}

// SandboxCredentials mirrors sandbox.credentials (mod_1164.js U, files only).
type SandboxCredentials struct {
	Files []string
}

// raw JSON mirror of one settings file's sandbox block.
type sandboxFileJSON struct {
	Enabled                  *bool               `json:"enabled"`
	FailIfUnavailable        *bool               `json:"failIfUnavailable"`
	AutoAllowBashIfSandboxed *bool               `json:"autoAllowBashIfSandboxed"`
	AllowUnsandboxedCommands *bool               `json:"allowUnsandboxedCommands"`
	ExcludedCommands         []string            `json:"excludedCommands"`
	IgnoreViolations         map[string][]string `json:"ignoreViolations"`
	Filesystem               *sandboxFSJSON      `json:"filesystem"`
	Network                  *sandboxNetJSON     `json:"network"`
	Credentials              *sandboxCredJSON    `json:"credentials"`
	BwrapPath                string              `json:"bwrapPath"`
}

type sandboxFSJSON struct {
	Disabled                  *bool    `json:"disabled"`
	AllowRead                 []string `json:"allowRead"`
	DenyRead                  []string `json:"denyRead"`
	AllowWrite                []string `json:"allowWrite"`
	DenyWrite                 []string `json:"denyWrite"`
	AllowManagedReadPathsOnly *bool    `json:"allowManagedReadPathsOnly"`
}

type sandboxNetJSON struct {
	AllowedDomains          []string `json:"allowedDomains"`
	DeniedDomains           []string `json:"deniedDomains"`
	AllowedCidrs            []string `json:"allowedCidrs"`
	AllowUnixSockets        []string `json:"allowUnixSockets"`
	AllowAllUnixSockets     *bool    `json:"allowAllUnixSockets"`
	AllowAllLocal           *bool    `json:"allowAllLocal"`
	InteractiveHostApproval *bool    `json:"interactiveHostApproval"`
}

type sandboxCredJSON struct {
	Files []string `json:"files"`
}

func managedSandboxSettingsPath() string {
	if runtime.GOOS == "darwin" {
		return "/Library/Application Support/ClaudeCode/managed-settings.json"
	}
	return "/etc/claude-code/managed-settings.json"
}

// apply folds one file's sandbox block into s. policy=true marks every scalar
// it sets as locked so user/project/local files cannot change it later.
func (s *SandboxSettings) apply(f *sandboxFileJSON, policy bool) {
	set := func(key string, v *bool, dst *bool) {
		if v == nil || s.locked[key] {
			return
		}
		*dst = *v
		if policy {
			s.locked[key] = true
		}
	}
	set("enabled", f.Enabled, &s.Enabled)
	set("failIfUnavailable", f.FailIfUnavailable, &s.FailIfUnavailable)
	set("autoAllowBashIfSandboxed", f.AutoAllowBashIfSandboxed, &s.AutoAllowBashIfSandboxed)
	set("allowUnsandboxedCommands", f.AllowUnsandboxedCommands, &s.AllowUnsandboxedCommands)
	s.ExcludedCommands = append(s.ExcludedCommands, f.ExcludedCommands...)
	if len(f.IgnoreViolations) > 0 && s.IgnoreViolations == nil {
		s.IgnoreViolations = map[string][]string{}
	}
	for k, v := range f.IgnoreViolations {
		s.IgnoreViolations[k] = append(s.IgnoreViolations[k], v...)
	}
	if fs := f.Filesystem; fs != nil {
		set("filesystem.disabled", fs.Disabled, &s.Filesystem.Disabled)
		set("filesystem.allowManagedReadPathsOnly", fs.AllowManagedReadPathsOnly, &s.Filesystem.AllowManagedReadPathsOnly)
		s.Filesystem.AllowRead = append(s.Filesystem.AllowRead, fs.AllowRead...)
		s.Filesystem.DenyRead = append(s.Filesystem.DenyRead, fs.DenyRead...)
		s.Filesystem.AllowWrite = append(s.Filesystem.AllowWrite, fs.AllowWrite...)
		s.Filesystem.DenyWrite = append(s.Filesystem.DenyWrite, fs.DenyWrite...)
		if policy {
			s.policyAllowRead = append(s.policyAllowRead, fs.AllowRead...)
		}
	}
	if n := f.Network; n != nil {
		if s.Network == nil {
			s.Network = &SandboxNetwork{}
		}
		s.Network.AllowedDomains = append(s.Network.AllowedDomains, n.AllowedDomains...)
		s.Network.DeniedDomains = append(s.Network.DeniedDomains, n.DeniedDomains...)
		s.Network.AllowedCidrs = append(s.Network.AllowedCidrs, n.AllowedCidrs...)
		s.Network.AllowUnixSockets = append(s.Network.AllowUnixSockets, n.AllowUnixSockets...)
		set("network.allowAllUnixSockets", n.AllowAllUnixSockets, &s.Network.AllowAllUnixSockets)
		set("network.allowAllLocal", n.AllowAllLocal, &s.Network.AllowAllLocal)
		set("network.interactiveHostApproval", n.InteractiveHostApproval, &s.Network.InteractiveHostApproval)
	}
	if c := f.Credentials; c != nil {
		s.Credentials.Files = append(s.Credentials.Files, c.Files...)
	}
	if f.BwrapPath != "" && !s.locked["bwrapPath"] {
		s.BwrapPath = f.BwrapPath
		if policy {
			s.locked["bwrapPath"] = true
		}
	}
}

// loadSandboxSettings reads every settings tier and merges their sandbox
// blocks. Policy (managed-settings.json) is applied first so the fields it
// sets are locked before user/project/local files are consulted.
func loadSandboxSettings(cwd string) *SandboxSettings {
	s := &SandboxSettings{
		AutoAllowBashIfSandboxed: true,
		AllowUnsandboxedCommands: true,
		locked:                   map[string]bool{},
	}
	files := []struct {
		path   string
		policy bool
	}{
		{managedSandboxSettingsPath(), true},
		{filepath.Join(config.ClaudeDir(), "settings.json"), false},
		{filepath.Join(cwd, ".claude", "settings.json"), false},
		{filepath.Join(cwd, ".claude", "settings.local.json"), false},
	}
	for _, f := range files {
		b, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		var outer struct {
			Sandbox sandboxFileJSON `json:"sandbox"`
		}
		if json.Unmarshal(b, &outer) == nil {
			s.apply(&outer.Sandbox, f.policy)
		}
	}
	return s
}

// SandboxCfg is the settings source consulted by the Bash tool. The agent
// layer may replace it (e.g. to share a cached config.Context); by default it
// loads the settings files directly.
var SandboxCfg = loadSandboxSettings

// enabled reports whether Bash sandboxing is on. CLAUDE_GO_SANDBOX=1 is an
// alias for sandbox.enabled=true unless policy locked the field.
func (s *SandboxSettings) enabled() bool {
	if !s.locked["enabled"] && os.Getenv("CLAUDE_GO_SANDBOX") == "1" {
		return true
	}
	return s.Enabled
}

// networkDenied reports whether outbound network is fully disallowed: a
// network block is present and either denies "*" or grants no domains.
// (Per-domain filtering needs the SRT proxy; without it we can only
// distinguish allow-all from deny-all.)
func (s *SandboxSettings) networkDenied() bool {
	if s.Network == nil {
		return false
	}
	for _, d := range s.Network.DeniedDomains {
		if d == "*" {
			return true
		}
	}
	return len(s.Network.AllowedDomains) == 0
}

// needsNetworkProxy reports whether egress goes through the netproxy MITM
// proxy rather than a blunt deny-all: a network block exists and grants at
// least one destination (real: the SRT proxy only starts when allowedDomains
// is set — deny-only configs collapse to deny-all without it).
func (s *SandboxSettings) needsNetworkProxy() bool {
	n := s.Network
	return n != nil && (len(n.AllowedDomains) > 0 || len(n.AllowedCidrs) > 0 || n.AllowAllLocal)
}

// allowRead returns the effective allowRead list, honoring
// allowManagedReadPathsOnly (mod_0056.js Pc).
func (s *SandboxSettings) allowRead() []string {
	if s.Filesystem.AllowManagedReadPathsOnly {
		return s.policyAllowRead
	}
	return s.Filesystem.AllowRead
}

// SandboxAutoAllow reports whether a sandboxed Bash call may skip the
// permission prompt (sandbox.enabled && sandbox.autoAllowBashIfSandboxed).
// The permission layer in agent/ consults this.
func SandboxAutoAllow(cwd string) bool {
	s := sandboxSettings(cwd)
	return s != nil && s.enabled() && s.AutoAllowBashIfSandboxed
}

func sandboxSettings(cwd string) *SandboxSettings {
	if SandboxCfg == nil {
		return nil
	}
	return SandboxCfg(cwd)
}

// ---------- excludedCommands ----------

// shellSegments splits a command line on pipeline/list separators so
// excludedCommands can match any command in a compound line.
func shellSegments(command string) []string {
	f := func(r rune) bool { return r == '|' || r == ';' || r == '&' || r == '\n' }
	out := strings.FieldsFunc(command, f)
	for i := range out {
		out[i] = strings.TrimSpace(out[i])
	}
	return out
}

// matchSandboxPattern implements Bash permission-rule matching for
// excludedCommands: optional Bash(...) wrapper, trailing ":*" or "*" for
// prefix/glob match, exact otherwise. Matched against the whole command and
// each pipeline segment.
func matchSandboxPattern(command, pat string) bool {
	pat = strings.TrimSpace(pat)
	if strings.HasPrefix(pat, "Bash(") && strings.HasSuffix(pat, ")") {
		pat = strings.TrimSpace(pat[5 : len(pat)-1])
	}
	if pat == "" {
		return false
	}
	targets := append([]string{strings.TrimSpace(command)}, shellSegments(command)...)
	if strings.HasSuffix(pat, ":*") {
		prefix := strings.TrimSuffix(pat, ":*")
		for _, t := range targets {
			if t == prefix || strings.HasPrefix(t, prefix+" ") {
				return true
			}
		}
		return false
	}
	if strings.ContainsAny(pat, "*?[") {
		re := globToRegexp(pat)
		if re == nil {
			return false
		}
		for _, t := range targets {
			if re.MatchString(t) {
				return true
			}
		}
		return false
	}
	for _, t := range targets {
		if t == pat {
			return true
		}
	}
	return false
}

// globToRegexp converts a shell glob (* ? [..]) to an anchored regexp.
func globToRegexp(glob string) *regexp.Regexp {
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(glob); i++ {
		switch c := glob[i]; c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteByte('.')
		case '[':
			j := i + 1
			if j < len(glob) && (glob[j] == '!' || glob[j] == '^') {
				j++
			}
			for j < len(glob) && (glob[j] != ']' || j == i+1) {
				j++
			}
			if j >= len(glob) {
				b.WriteString(`\[`)
				break
			}
			inner := glob[i+1 : j]
			if strings.HasPrefix(inner, "!") {
				inner = "^" + inner[1:]
			}
			b.WriteByte('[')
			b.WriteString(inner)
			b.WriteByte(']')
			i = j
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteByte('$')
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}

// excluded reports whether command matches sandbox.excludedCommands and so
// runs unsandboxed.
func (s *SandboxSettings) excluded(command string) bool {
	for _, p := range s.ExcludedCommands {
		if matchSandboxPattern(command, p) {
			return true
		}
	}
	return false
}

// ---------- path helpers ----------

// expandSandboxPath expands ~ and resolves relative paths against cwd.
func expandSandboxPath(p, cwd string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[1:])
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p)
}

func expandSandboxPaths(paths []string, cwd string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, expandSandboxPath(p, cwd))
	}
	return out
}

// shellQuote POSIX-quotes one argument for embedding in a shell -c string.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// ---------- dispatch ----------

// sandboxWrap decides whether script runs sandboxed and returns the final
// command string plus extra env vars for the child process. command is the
// raw user command (for excludedCommands); script is command plus the cwd
// sentinel trailer. Returns (wrapped, env, warn, err): env is appended to
// the command environment (proxy vars when network rules apply); warn is a
// non-fatal note to surface; err aborts the call.
func (c *Context) sandboxWrap(command, script string, disable bool) (string, []string, string, error) {
	s := sandboxSettings(c.CWD)
	if s == nil || !s.enabled() {
		return script, nil, "", nil
	}
	if disable {
		switch {
		case s.AllowUnsandboxedCommands:
			return script, nil, "", nil // honored: run outside the sandbox
		case s.locked["allowUnsandboxedCommands"]:
			return "", nil, "", fmt.Errorf("dangerouslyDisableSandbox refused: sandbox.allowUnsandboxedCommands is false (set by managed policy)")
		default:
			// CC: when false, the parameter is ignored and the command
			// still runs sandboxed.
			return c.sandboxedScript(command, script, s,
				"note: dangerouslyDisableSandbox ignored (sandbox.allowUnsandboxedCommands is false)")
		}
	}
	if s.excluded(command) {
		return script, nil, "", nil
	}
	return c.sandboxedScript(command, script, s, "")
}

// addNote folds a non-fatal note into the warn string.
func addNote(warn, msg string) string {
	if warn == "" {
		return msg
	}
	return warn + "\n" + msg
}

// sandboxedScript wraps script for the platform sandbox, degrading per
// failIfUnavailable when the sandbox cannot be applied. When sandbox.network
// grants any destination, it also brings up the session MITM proxy and
// exports its env vars; seatbelt then denies all egress except the proxy.
func (c *Context) sandboxedScript(command, script string, s *SandboxSettings, warn string) (string, []string, string, error) {
	if err := platformSandboxAvailable(s); err != nil {
		if s.FailIfUnavailable {
			return "", nil, "", fmt.Errorf("sandbox unavailable: %v (sandbox.failIfUnavailable is set)", err)
		}
		note := fmt.Sprintf("sandbox unavailable: %v; running unsandboxed", err)
		return script, nil, addNote(warn, note), nil
	}
	var env []string
	var px *netproxy.Proxy
	if s.needsNetworkProxy() {
		if p2, note, err := sharedNetProxy(s.Network); err != nil {
			warn = addNote(warn, fmt.Sprintf("sandbox network proxy failed: %v; denying all egress", err))
		} else {
			px = p2
			env = px.Env(command)
			// interactive host approval: unmatched-host denials become a
			// prompt when the tool context can ask (real's ask callback);
			// approved hosts join the session allowlist.
			if s.Network.InteractiveHostApproval && c.AskStructured != nil {
				px.SetPromptAllow(func(host string, port int) bool {
					sel, err := c.AskStructured("Blocked host",
						fmt.Sprintf("The sandboxed command tried to reach %s:%d, which is not in sandbox.network.allowedDomains.", host, port),
						[]string{"Allow for this session", "Deny"}, false)
					return err == nil && len(sel) > 0 && strings.HasPrefix(sel[0], "Allow")
				})
			} else {
				px.SetPromptAllow(nil)
			}
			if note != "" {
				warn = addNote(warn, note)
			}
		}
	}
	wrapped, err := platformSandboxWrap(script, s, c.CWD, px)
	if err != nil {
		if s.FailIfUnavailable {
			return "", nil, "", fmt.Errorf("sandbox failed to start: %v (sandbox.failIfUnavailable is set)", err)
		}
		note := fmt.Sprintf("sandbox failed to start: %v; running unsandboxed", err)
		return script, nil, addNote(warn, note), nil
	}
	return wrapped, env, warn, nil
}
