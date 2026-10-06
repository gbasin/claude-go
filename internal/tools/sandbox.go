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

// SandboxNetwork mirrors sandbox.network (mod_1164.js q).
type SandboxNetwork struct {
	AllowedDomains []string
	DeniedDomains  []string // a bare "*" denies all egress
	// DeniedDomainReasons maps an exact deniedDomains entry to the
	// model-facing reason shown when that entry denies a connection.
	DeniedDomainReasons map[string]string
	AllowedCidrs        []string
	// DeniedResolvedAddresses holds IPs/CIDRs a permitted hostname must not
	// resolve to (real's network.deniedResolvedAddresses).
	DeniedResolvedAddresses []string
	AllowUnixSockets        []string // macOS only
	AllowAllUnixSockets     bool
	AllowAllLocal           bool // claude-go extension: like real's allowLocalBinding
	// InteractiveHostApproval upgrades "not on the allow list" denials into
	// an interactive prompt (real's non-strictAllowlist ask callback). Hosts
	// approved this way land on the proxy's session allowlist.
	InteractiveHostApproval bool
	// StrictAllowlist deterministically denies hosts not in allowedDomains
	// — no prompt, no session allowlist. Honored only from policy and user
	// settings (real ignores project settings, and drops their
	// allowedDomains while it's on).
	StrictAllowlist bool
	// AllowManagedDomainsOnly (policy tier only): only managed-settings
	// allowedDomains apply; user/project/local entries are ignored.
	AllowManagedDomainsOnly bool
	// ParentProxy routes upstream traffic through an outer proxy; unset
	// fields fall back to HTTP(S)_PROXY/NO_PROXY env vars.
	ParentProxy *netproxy.ParentProxyConfig

	policyAllowed   []string // allowedDomains contributed by the policy tier
	nonProjectAllow []string // allowedDomains from policy + user tiers
}

// effectiveAllowed is the allowlist the proxy enforces:
// allowManagedDomainsOnly → policy tier only; strictAllowlist → policy+user
// (project/local entries are left out); otherwise the merged list.
func (n *SandboxNetwork) effectiveAllowed() []string {
	if n.AllowManagedDomainsOnly {
		return n.policyAllowed
	}
	if n.StrictAllowlist {
		return n.nonProjectAllow
	}
	return n.AllowedDomains
}

// SandboxCredentials mirrors sandbox.credentials (mod_1164.js U).
// mode "deny" entries unshare the file/env var; mode "mask" entries replace
// the real value inside the sandbox with a "fake_value_..." sentinel the
// egress proxy swaps back for the real value on hosts matching injectHosts
// (default: network.allowedDomains). AWS SigV4 re-signing (awsPairs) is
// implemented on the proxy: whole-value masked AWS env vars are grouped
// into credential pairs (explicit awsPairs plus the conventional
// AWS_ACCESS_KEY_ID/SECRET/TOKEN auto-pair) and the MITM path re-signs
// sentinel-credentialed requests with the real secret; sigv4 holds the
// policies for the shapes that can't be re-signed.
type SandboxCredentials struct {
	Files   []SandboxCredFile
	EnvVars []SandboxCredVar
	// AllowPlaintextInject permits sentinel→real substitution on the
	// plain-HTTP proxy path (default false — TLS-terminated only).
	AllowPlaintextInject bool
	AWSPairs             []SandboxAWSPair
	SigV4                *SandboxSigV4
}

// SandboxCredFile is one credentials.files entry.
type SandboxCredFile struct {
	Path             string
	Mode             string // "deny" | "mask"
	Extract          string // regex; group 1 is the credential
	OnExtractNoMatch string // "warn" (default) | "deny" | "error"
	Decode           string // "jwt" enables JWT-aware masking
	MaskClaims       []string
	MaskDuplicates   bool
	InjectHosts      []string
}

// SandboxCredVar is one credentials.envVars entry.
type SandboxCredVar struct {
	Name             string
	Mode             string // "deny" | "mask"
	Extract          string
	OnExtractNoMatch string
	Decode           string
	MaskClaims       []string
	MaskDuplicates   bool
	InjectHosts      []string
}

// SandboxAWSPair groups masked env vars into a SigV4 credential set.
type SandboxAWSPair struct {
	AccessKeyIDVar     string `json:"accessKeyIdVar"`
	SecretAccessKeyVar string `json:"secretAccessKeyVar"`
	SessionTokenVar    string `json:"sessionTokenVar,omitempty"`
}

// SandboxSigV4 holds the policies for request shapes the proxy cannot
// re-sign: "deny" (default) or "passthrough".
type SandboxSigV4 struct {
	Streaming string `json:"streaming,omitempty"`
	Presigned string `json:"presigned,omitempty"`
	SigV4a    string `json:"sigv4a,omitempty"`
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
	AllowedDomains          []string                    `json:"allowedDomains"`
	DeniedDomains           []string                    `json:"deniedDomains"`
	DeniedDomainReasons     map[string]string           `json:"deniedDomainReasons"`
	AllowedCidrs            []string                    `json:"allowedCidrs"`
	DeniedResolvedAddresses []string                    `json:"deniedResolvedAddresses"`
	AllowUnixSockets        []string                    `json:"allowUnixSockets"`
	AllowAllUnixSockets     *bool                       `json:"allowAllUnixSockets"`
	AllowAllLocal           *bool                       `json:"allowAllLocal"`
	InteractiveHostApproval *bool                       `json:"interactiveHostApproval"`
	StrictAllowlist         *bool                       `json:"strictAllowlist"`
	AllowManagedDomainsOnly *bool                       `json:"allowManagedDomainsOnly"`
	ParentProxy             *netproxy.ParentProxyConfig `json:"parentProxy"`
}

type sandboxCredJSON struct {
	Files                []sandboxCredEntryJSON `json:"files"`
	EnvVars              []sandboxCredEntryJSON `json:"envVars"`
	AllowPlaintextInject *bool                  `json:"allowPlaintextInject"`
	AWSPairs             []SandboxAWSPair       `json:"awsPairs"`
	SigV4                *SandboxSigV4          `json:"sigv4"`
}

// sandboxCredEntryJSON covers both files (path) and envVars (name) entries.
type sandboxCredEntryJSON struct {
	Path             string   `json:"path"`
	Name             string   `json:"name"`
	Mode             string   `json:"mode"`
	Extract          string   `json:"extract"`
	OnExtractNoMatch string   `json:"onExtractNoMatch"`
	Decode           string   `json:"decode"`
	MaskClaims       []string `json:"maskClaims"`
	MaskDuplicates   *bool    `json:"maskDuplicates"`
	InjectHosts      []string `json:"injectHosts"`
}

func managedSandboxSettingsPath() string {
	if runtime.GOOS == "darwin" {
		return "/Library/Application Support/ClaudeCode/managed-settings.json"
	}
	return "/etc/claude-code/managed-settings.json"
}

// settings tiers in merge order (policy first — it locks what it sets).
const (
	tierPolicy = iota
	tierUser
	tierProject
	tierLocal
)

// apply folds one file's sandbox block into s. The policy tier marks every
// scalar it sets as locked so user/project/local files cannot change it
// later. Tier restrictions mirror real: strictAllowlist is honored only
// from policy/user, allowManagedDomainsOnly only from policy.
func (s *SandboxSettings) apply(f *sandboxFileJSON, tier int) {
	policy := tier == tierPolicy
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
		if policy {
			s.Network.policyAllowed = append(s.Network.policyAllowed, n.AllowedDomains...)
		}
		if tier <= tierUser {
			s.Network.nonProjectAllow = append(s.Network.nonProjectAllow, n.AllowedDomains...)
		}
		s.Network.DeniedDomains = append(s.Network.DeniedDomains, n.DeniedDomains...)
		s.Network.AllowedCidrs = append(s.Network.AllowedCidrs, n.AllowedCidrs...)
		s.Network.DeniedResolvedAddresses = append(s.Network.DeniedResolvedAddresses, n.DeniedResolvedAddresses...)
		s.Network.AllowUnixSockets = append(s.Network.AllowUnixSockets, n.AllowUnixSockets...)
		if len(n.DeniedDomainReasons) > 0 {
			if s.Network.DeniedDomainReasons == nil {
				s.Network.DeniedDomainReasons = map[string]string{}
			}
			for k, v := range n.DeniedDomainReasons {
				s.Network.DeniedDomainReasons[k] = v
			}
		}
		set("network.allowAllUnixSockets", n.AllowAllUnixSockets, &s.Network.AllowAllUnixSockets)
		set("network.allowAllLocal", n.AllowAllLocal, &s.Network.AllowAllLocal)
		set("network.interactiveHostApproval", n.InteractiveHostApproval, &s.Network.InteractiveHostApproval)
		// real honors strictAllowlist from managed/policy, user, and
		// --settings only — project settings are ignored entirely.
		if n.StrictAllowlist != nil && tier <= tierUser {
			set("network.strictAllowlist", n.StrictAllowlist, &s.Network.StrictAllowlist)
		}
		// allowManagedDomainsOnly takes effect only from managed settings.
		if n.AllowManagedDomainsOnly != nil && policy {
			set("network.allowManagedDomainsOnly", n.AllowManagedDomainsOnly, &s.Network.AllowManagedDomainsOnly)
		}
		if n.ParentProxy != nil && !s.locked["network.parentProxy"] {
			s.Network.ParentProxy = n.ParentProxy
			if policy {
				s.locked["network.parentProxy"] = true
			}
		}
	}
	if c := f.Credentials; c != nil {
		for _, e := range c.Files {
			s.Credentials.Files = append(s.Credentials.Files, SandboxCredFile{
				Path: e.Path, Mode: e.Mode, Extract: e.Extract,
				OnExtractNoMatch: e.OnExtractNoMatch, Decode: e.Decode,
				MaskClaims: e.MaskClaims, InjectHosts: e.InjectHosts,
				MaskDuplicates: e.MaskDuplicates != nil && *e.MaskDuplicates,
			})
		}
		for _, e := range c.EnvVars {
			s.Credentials.EnvVars = append(s.Credentials.EnvVars, SandboxCredVar{
				Name: e.Name, Mode: e.Mode, Extract: e.Extract,
				OnExtractNoMatch: e.OnExtractNoMatch, Decode: e.Decode,
				MaskClaims: e.MaskClaims, InjectHosts: e.InjectHosts,
				MaskDuplicates: e.MaskDuplicates != nil && *e.MaskDuplicates,
			})
		}
		set("credentials.allowPlaintextInject", c.AllowPlaintextInject, &s.Credentials.AllowPlaintextInject)
		s.Credentials.AWSPairs = append(s.Credentials.AWSPairs, c.AWSPairs...)
		if c.SigV4 != nil && !s.locked["credentials.sigv4"] {
			s.Credentials.SigV4 = c.SigV4
			if policy {
				s.locked["credentials.sigv4"] = true
			}
		}
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
		path string
		tier int
	}{
		{managedSandboxSettingsPath(), tierPolicy},
		{filepath.Join(config.ClaudeDir(), "settings.json"), tierUser},
		{filepath.Join(cwd, ".claude", "settings.json"), tierProject},
		{filepath.Join(cwd, ".claude", "settings.local.json"), tierLocal},
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
			s.apply(&outer.Sandbox, f.tier)
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
	return len(s.Network.effectiveAllowed()) == 0
}

// needsNetworkProxy reports whether egress goes through the netproxy MITM
// proxy rather than a blunt deny-all: a network block exists and grants at
// least one destination (real: the SRT proxy only starts when allowedDomains
// is set — deny-only configs collapse to deny-all without it).
func (s *SandboxSettings) needsNetworkProxy() bool {
	n := s.Network
	return n != nil && (len(n.effectiveAllowed()) > 0 || len(n.AllowedCidrs) > 0 || n.AllowAllLocal)
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

// sandboxEnv carries the env deltas a sandboxed command needs: Set entries
// ("K=V") are appended to the inherited environment; Unset names are removed
// from it (credential mode:"deny" variables, masked-extract degradations).
type sandboxEnv struct {
	Set   []string
	Unset []string
}

// applyTo builds the child environment: inherited vars minus Unset, plus Set.
func (e *sandboxEnv) applyTo(base []string) []string {
	if e == nil {
		return base
	}
	if len(e.Unset) > 0 {
		drop := make(map[string]bool, len(e.Unset))
		for _, n := range e.Unset {
			drop[n] = true
		}
		kept := make([]string, 0, len(base))
		for _, kv := range base {
			if k, _, _ := strings.Cut(kv, "="); !drop[k] {
				kept = append(kept, kv)
			}
		}
		base = kept
	}
	return append(base, e.Set...)
}

// sandboxWrap decides whether script runs sandboxed and returns the final
// command string plus env deltas for the child process. command is the
// raw user command (for excludedCommands); script is command plus the cwd
// sentinel trailer. Returns (wrapped, env, warn, err): env is applied over
// the command's inherited environment (proxy vars, credential masking);
// warn is a non-fatal note to surface; err aborts the call.
func (c *Context) sandboxWrap(command, script string, disable bool) (string, *sandboxEnv, string, error) {
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
func (c *Context) sandboxedScript(command, script string, s *SandboxSettings, warn string) (string, *sandboxEnv, string, error) {
	if err := platformSandboxAvailable(s); err != nil {
		if s.FailIfUnavailable {
			return "", nil, "", fmt.Errorf("sandbox unavailable: %v (sandbox.failIfUnavailable is set)", err)
		}
		note := fmt.Sprintf("sandbox unavailable: %v; running unsandboxed", err)
		return script, nil, addNote(warn, note), nil
	}
	env := &sandboxEnv{}
	var px *netproxy.Proxy
	if s.needsNetworkProxy() {
		if p2, note, err := sharedNetProxy(s); err != nil {
			warn = addNote(warn, fmt.Sprintf("sandbox network proxy failed: %v; denying all egress", err))
		} else {
			px = p2
			env.Set = append(env.Set, px.Env(command)...)
			// interactive host approval: unmatched-host denials become a
			// prompt when the tool context can ask (real's ask callback);
			// approved hosts join the session allowlist. strictAllowlist
			// disables this path (the proxy also refuses to prompt).
			if s.Network.InteractiveHostApproval && !s.Network.StrictAllowlist && c.AskStructured != nil {
				px.SetPromptAllow(func(host string, port int) bool {
					sel, err := c.AskStructured("Blocked host",
						fmt.Sprintf("The sandboxed command tried to reach %s:%d, which is not in sandbox.network.allowedDomains.", host, port),
						[]AskOption{{Label: "Allow for this session"}, {Label: "Deny"}}, false)
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
	// sandbox.credentials: deny-mode env vars drop out of the child env,
	// mask-mode vars/files become sentinels the proxy swaps back on egress.
	var cred *credPlan
	if len(s.Credentials.Files) > 0 || len(s.Credentials.EnvVars) > 0 {
		cp, err := s.prepareCredentials(px, c.CWD)
		if err != nil {
			return "", nil, "", err // onExtractNoMatch "error" aborts the wrap
		}
		cred = cp
		env.Set = append(env.Set, cred.setEnv...)
		env.Unset = append(env.Unset, cred.unsetEnv...)
		for _, w := range cred.warns {
			warn = addNote(warn, w)
		}
	}
	wrapped, err := platformSandboxWrap(script, s, c.CWD, px, cred)
	if err != nil {
		if s.FailIfUnavailable {
			return "", nil, "", fmt.Errorf("sandbox failed to start: %v (sandbox.failIfUnavailable is set)", err)
		}
		note := fmt.Sprintf("sandbox failed to start: %v; running unsandboxed", err)
		return script, nil, addNote(warn, note), nil
	}
	return wrapped, env, warn, nil
}
