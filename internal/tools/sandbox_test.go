package tools

import (
	"claude-go/internal/netproxy"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func boolp(b bool) *bool { return &b }

func TestSandboxApplyMerging(t *testing.T) {
	s := &SandboxSettings{
		AutoAllowBashIfSandboxed: true,
		AllowUnsandboxedCommands: true,
		locked:                   map[string]bool{},
	}
	// policy tier sets + locks enabled and allowUnsandboxedCommands
	s.apply(&sandboxFileJSON{
		Enabled:                  boolp(true),
		AllowUnsandboxedCommands: boolp(false),
		Network:                  &sandboxNetJSON{AllowedDomains: []string{"a.com"}},
	}, tierPolicy)
	// later user/project/local files cannot relax locked scalars,
	// but lists still merge
	s.apply(&sandboxFileJSON{
		Enabled:                  boolp(false),
		AllowUnsandboxedCommands: boolp(true),
		AutoAllowBashIfSandboxed: boolp(false),
		ExcludedCommands:         []string{"docker *"},
		Network:                  &sandboxNetJSON{AllowedDomains: []string{"b.com"}},
	}, tierUser)
	if !s.Enabled {
		t.Error("policy-enabled was overridden by user file")
	}
	if s.AllowUnsandboxedCommands {
		t.Error("policy allowUnsandboxedCommands=false was overridden")
	}
	if s.AutoAllowBashIfSandboxed {
		t.Error("non-locked autoAllowBashIfSandboxed should be overridable")
	}
	if len(s.ExcludedCommands) != 1 || len(s.Network.AllowedDomains) != 2 {
		t.Error("list fields should merge across sources")
	}
}

func TestSandboxEnvAlias(t *testing.T) {
	s := &SandboxSettings{locked: map[string]bool{}}
	t.Setenv("CLAUDE_GO_SANDBOX", "1")
	if !s.enabled() {
		t.Error("CLAUDE_GO_SANDBOX=1 should enable the sandbox")
	}
	s2 := &SandboxSettings{locked: map[string]bool{"enabled": true}}
	if s2.enabled() {
		t.Error("policy-locked enabled=false must beat CLAUDE_GO_SANDBOX")
	}
	t.Setenv("CLAUDE_GO_SANDBOX", "")
	if s.enabled() {
		t.Error("sandbox should be disabled without env or setting")
	}
}

func TestSandboxNetworkDenied(t *testing.T) {
	cases := []struct {
		name string
		net  *SandboxNetwork
		want bool
	}{
		{"no block", nil, false},
		{"deny all", &SandboxNetwork{DeniedDomains: []string{"*"}}, true},
		{"no allowed domains", &SandboxNetwork{}, true},
		{"allowed domains", &SandboxNetwork{AllowedDomains: []string{"github.com"}}, false},
	}
	for _, c := range cases {
		s := &SandboxSettings{Network: c.net}
		if got := s.networkDenied(); got != c.want {
			t.Errorf("%s: networkDenied=%v want %v", c.name, got, c.want)
		}
	}
}

func TestMatchSandboxPattern(t *testing.T) {
	cases := []struct {
		cmd, pat string
		want     bool
	}{
		{"git push origin main", "git push:*", true},
		{"git push origin main", "Bash(git push:*)", true},
		{"git push origin main", "git pull:*", false},
		{"ls", "ls", true},
		{"ls -la", "ls", false},
		{"rm -rf /", "rm *", true},
		{"echo a && docker ps", "docker *", true}, // matches second segment
		{"echo a | docker ps", "docker ps", true}, // exact on segment
		{"docker info", "docker ps", false},
		{"sh -c 'x'", "sh *", true},
	}
	for _, c := range cases {
		if got := matchSandboxPattern(c.cmd, c.pat); got != c.want {
			t.Errorf("matchSandboxPattern(%q,%q)=%v want %v", c.cmd, c.pat, got, c.want)
		}
	}
}

func TestSandboxExcluded(t *testing.T) {
	s := &SandboxSettings{ExcludedCommands: []string{"docker *", "Bash(sudo:*)"}}
	if !s.excluded("docker ps -a") {
		t.Error("docker * should match")
	}
	if !s.excluded("echo hi && sudo reboot") {
		t.Error("sudo:* should match a segment")
	}
	if s.excluded("dockerfile lint") {
		t.Error("dockerfile should not match docker *")
	}
}

// testCtx + a stubbed SandboxCfg exercise sandboxWrap's decision paths
// without depending on sandbox-exec/bwrap being usable.
func withSandboxCfg(t *testing.T, s *SandboxSettings) *Context {
	t.Helper()
	old := SandboxCfg
	SandboxCfg = func(string) *SandboxSettings { return s }
	t.Cleanup(func() { SandboxCfg = old })
	return &Context{Ctx: context.Background(), CWD: t.TempDir()}
}

func TestSandboxWrapDisabled(t *testing.T) {
	c := withSandboxCfg(t, &SandboxSettings{locked: map[string]bool{}})
	got, _, warn, err := c.sandboxWrap("ls", "ls\nsentinel", true)
	if err != nil || warn != "" || got == "" {
		t.Fatalf("disabled sandbox should pass through: %q %q %v", got, warn, err)
	}
}

func TestSandboxWrapDangerouslyDisable(t *testing.T) {
	s := &SandboxSettings{
		Enabled:                  true,
		AllowUnsandboxedCommands: true,
		locked:                   map[string]bool{},
	}
	c := withSandboxCfg(t, s)
	got, _, _, err := c.sandboxWrap("ls", "ls\nsentinel", true)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got != "ls\nsentinel" {
		t.Error("dangerouslyDisableSandbox should skip wrapping")
	}

	// non-policy false: ignored, command still sandboxed
	s.AllowUnsandboxedCommands = false
	got, _, warn, err := c.sandboxWrap("ls", "ls\nsentinel", true)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if warn == "" {
		t.Error("expected a note that dangerouslyDisableSandbox was ignored")
	}
	_ = got // wrap attempt itself is platform-dependent

	// policy-locked false: refused
	s.locked["allowUnsandboxedCommands"] = true
	if _, _, _, err := c.sandboxWrap("ls", "s", true); err == nil {
		t.Error("policy-locked allowUnsandboxedCommands=false must refuse")
	}
}

func TestSandboxWrapExcluded(t *testing.T) {
	s := &SandboxSettings{
		Enabled:          true,
		ExcludedCommands: []string{"docker *"},
		locked:           map[string]bool{},
	}
	c := withSandboxCfg(t, s)
	got, _, _, err := c.sandboxWrap("docker ps", "docker ps\nsentinel", false)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got != "docker ps\nsentinel" {
		t.Error("excluded command should run unsandboxed")
	}
}

func TestSandboxAutoAllow(t *testing.T) {
	old := SandboxCfg
	defer func() { SandboxCfg = old }()
	SandboxCfg = func(string) *SandboxSettings {
		return &SandboxSettings{Enabled: true, AutoAllowBashIfSandboxed: true}
	}
	if !SandboxAutoAllow("/x") {
		t.Error("enabled + autoAllow should report true")
	}
	SandboxCfg = func(string) *SandboxSettings {
		return &SandboxSettings{Enabled: true, AutoAllowBashIfSandboxed: false}
	}
	if SandboxAutoAllow("/x") {
		t.Error("autoAllowBashIfSandboxed=false should report false")
	}
	SandboxCfg = func(string) *SandboxSettings {
		return &SandboxSettings{}
	}
	if SandboxAutoAllow("/x") {
		t.Error("disabled sandbox should report false")
	}
}

// ---------- credentials + tiered network merge ----------

func TestSandboxManagedDomainsOnly(t *testing.T) {
	s := &SandboxSettings{locked: map[string]bool{}}
	// policy tier pins allowManagedDomainsOnly + its own allowedDomains
	s.apply(&sandboxFileJSON{Network: &sandboxNetJSON{
		AllowedDomains:          []string{"policy.com"},
		AllowManagedDomainsOnly: boolp(true),
	}}, tierPolicy)
	// user + project domains are dropped from the effective allowlist
	s.apply(&sandboxFileJSON{Network: &sandboxNetJSON{
		AllowedDomains:          []string{"user.com"},
		AllowManagedDomainsOnly: boolp(false), // non-policy can't unset
	}}, tierUser)
	s.apply(&sandboxFileJSON{Network: &sandboxNetJSON{
		AllowedDomains: []string{"proj.com"},
	}}, tierProject)
	got := s.Network.effectiveAllowed()
	if len(got) != 1 || got[0] != "policy.com" {
		t.Fatalf("allowManagedDomainsOnly effective = %v", got)
	}
	if !s.Network.AllowManagedDomainsOnly {
		t.Error("user tier should not clear a policy-managed flag")
	}
	// a user-tier allowManagedDomainsOnly:true is ignored (policy only)
	s2 := &SandboxSettings{locked: map[string]bool{}}
	s2.apply(&sandboxFileJSON{Network: &sandboxNetJSON{
		AllowedDomains:          []string{"u.com"},
		AllowManagedDomainsOnly: boolp(true),
	}}, tierUser)
	if s2.Network.AllowManagedDomainsOnly {
		t.Error("allowManagedDomainsOnly from user settings must be ignored")
	}
	if len(s2.Network.effectiveAllowed()) != 1 || s2.Network.effectiveAllowed()[0] != "u.com" {
		t.Error("without the flag, user domains apply")
	}
}

func TestSandboxStrictAllowlistTiers(t *testing.T) {
	s := &SandboxSettings{locked: map[string]bool{}}
	// project-tier strictAllowlist is ignored entirely (real semantics)
	s.apply(&sandboxFileJSON{Network: &sandboxNetJSON{
		StrictAllowlist: boolp(true),
		AllowedDomains:  []string{"proj.com"},
	}}, tierProject)
	if s.Network.StrictAllowlist {
		t.Error("project-tier strictAllowlist must be ignored")
	}
	// user-tier sets it — and project-tier allowedDomains drop out
	s2 := &SandboxSettings{locked: map[string]bool{}}
	s2.apply(&sandboxFileJSON{Network: &sandboxNetJSON{
		AllowedDomains: []string{"user.com"},
	}}, tierUser)
	s2.apply(&sandboxFileJSON{Network: &sandboxNetJSON{
		StrictAllowlist: boolp(true),
	}}, tierUser)
	s2.apply(&sandboxFileJSON{Network: &sandboxNetJSON{
		AllowedDomains: []string{"proj.com"},
	}}, tierProject)
	if !s2.Network.StrictAllowlist {
		t.Error("user-tier strictAllowlist should apply")
	}
	got := s2.Network.effectiveAllowed()
	if len(got) != 1 || got[0] != "user.com" {
		t.Fatalf("strict allowlist should drop project domains: %v", got)
	}
}

func TestSandboxDeniedDomainReasonsMerge(t *testing.T) {
	s := &SandboxSettings{locked: map[string]bool{}}
	s.apply(&sandboxFileJSON{Network: &sandboxNetJSON{
		DeniedDomains:       []string{"github.com:22"},
		DeniedDomainReasons: map[string]string{"github.com:22": "ssh blocked"},
	}}, tierProject)
	if s.Network.DeniedDomainReasons["github.com:22"] != "ssh blocked" {
		t.Fatalf("reasons = %v", s.Network.DeniedDomainReasons)
	}
	s.apply(&sandboxFileJSON{Network: &sandboxNetJSON{
		DeniedDomainReasons: map[string]string{"github.com:22": "later wins"},
	}}, tierLocal)
	if s.Network.DeniedDomainReasons["github.com:22"] != "later wins" {
		t.Error("later sources should overwrite per-key")
	}
}

func TestSandboxCredentialsSchemaParse(t *testing.T) {
	s := &SandboxSettings{locked: map[string]bool{}}
	raw := `{"sandbox": {
		"enabled": true,
		"credentials": {
			"files": [{"path": "~/.aws/credentials", "mode": "mask", "extract": "key=(\\S+)", "injectHosts": ["*.aws.internal"]}],
			"envVars": [
				{"name": "MY_TOKEN", "mode": "mask"},
				{"name": "AWS_SECRET_ACCESS_KEY", "mode": "deny"}
			],
			"allowPlaintextInject": true,
			"awsPairs": [{"accessKeyIdVar": "AK", "secretAccessKeyVar": "SK", "sessionTokenVar": "ST"}],
			"sigv4": {"streaming": "passthrough"}
		}
	}}`
	var outer struct {
		Sandbox sandboxFileJSON `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(raw), &outer); err != nil {
		t.Fatal(err)
	}
	s.apply(&outer.Sandbox, tierProject)
	if len(s.Credentials.Files) != 1 || s.Credentials.Files[0].Mode != "mask" ||
		s.Credentials.Files[0].Extract != `key=(\S+)` {
		t.Fatalf("files = %+v", s.Credentials.Files)
	}
	if len(s.Credentials.EnvVars) != 2 || s.Credentials.EnvVars[1].Mode != "deny" {
		t.Fatalf("envVars = %+v", s.Credentials.EnvVars)
	}
	if !s.Credentials.AllowPlaintextInject {
		t.Error("allowPlaintextInject not parsed")
	}
	if len(s.Credentials.AWSPairs) != 1 || s.Credentials.AWSPairs[0].SessionTokenVar != "ST" {
		t.Fatalf("awsPairs = %+v", s.Credentials.AWSPairs)
	}
	if s.Credentials.SigV4 == nil || s.Credentials.SigV4.Streaming != "passthrough" {
		t.Fatalf("sigv4 = %+v", s.Credentials.SigV4)
	}
}

// prepareCredentials masks env values into registered sentinels and drops
// deny-mode vars; the proxy store resolves sentinel→real for allowed hosts.
func TestPrepareCredentialsEnv(t *testing.T) {
	t.Setenv("MY_TOKEN", "real-token-abc")
	t.Setenv("DENIED_KEY", "shhh")
	px := netproxy.New(nil)
	s := &SandboxSettings{
		locked:  map[string]bool{},
		Network: &SandboxNetwork{AllowedDomains: []string{"api.example.com:443"}},
		Credentials: SandboxCredentials{
			EnvVars: []SandboxCredVar{
				{Name: "MY_TOKEN", Mode: "mask"},
				{Name: "DENIED_KEY", Mode: "deny"},
			},
		},
	}
	plan, err := s.prepareCredentials(px, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var masked string
	for _, e := range plan.setEnv {
		if strings.HasPrefix(e, "MY_TOKEN=") {
			masked = strings.TrimPrefix(e, "MY_TOKEN=")
		}
	}
	if masked == "" || masked == "real-token-abc" || !strings.HasPrefix(masked, "fake_value_") {
		t.Fatalf("masked env = %q", masked)
	}
	if !containsStr(plan.unsetEnv, "DENIED_KEY") {
		t.Fatalf("deny-mode var not unset: %v", plan.unsetEnv)
	}
	// the sentinel is registered, injectable at the allowed host
	// (default injectHosts = allowedDomains host patterns — port stripped)
	if real, ok := px.Creds.LookupReal(masked); !ok || real != "real-token-abc" {
		t.Fatalf("sentinel not registered: %v", ok)
	}
	if got := px.Creds.SubstituteString("Authorization: Bearer "+masked, "api.example.com"); got != "Authorization: Bearer real-token-abc" {
		t.Fatalf("substitution at allowed host = %q", got)
	}
	if got := px.Creds.SubstituteString(masked, "evil.com"); got != masked {
		t.Fatal("sentinel substituted at a non-injectable host")
	}
}

// extract masks only the captured group inside a larger value; a no-match
// honors onExtractNoMatch.
func TestPrepareCredentialsExtract(t *testing.T) {
	t.Setenv("CONF", "user=bob token:tok_12345 end")
	px := netproxy.New(nil)
	s := &SandboxSettings{
		locked:  map[string]bool{},
		Network: &SandboxNetwork{AllowedDomains: []string{"*"}},
		Credentials: SandboxCredentials{EnvVars: []SandboxCredVar{
			{Name: "CONF", Mode: "mask", Extract: `token:(\S+)`},
		}},
	}
	plan, err := s.prepareCredentials(px, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.setEnv) != 1 {
		t.Fatalf("setEnv = %v warns=%v", plan.setEnv, plan.warns)
	}
	masked := strings.TrimPrefix(plan.setEnv[0], "CONF=")
	if strings.Contains(masked, "tok_12345") || !strings.Contains(masked, "fake_value_") ||
		!strings.HasPrefix(masked, "user=bob token:") || !strings.HasSuffix(masked, " end") {
		t.Fatalf("partial mask wrong: %q", masked)
	}

	// onExtractNoMatch=error aborts the wrap
	s.Credentials.EnvVars[0].Extract = `nope:(\S+)`
	s.Credentials.EnvVars[0].OnExtractNoMatch = "error"
	if _, err := s.prepareCredentials(px, t.TempDir()); err == nil {
		t.Error("onExtractNoMatch=error should fail the wrap")
	}
	// deny → unset
	s.Credentials.EnvVars[0].OnExtractNoMatch = "deny"
	plan, err = s.prepareCredentials(px, t.TempDir())
	if err != nil || !containsStr(plan.unsetEnv, "CONF") {
		t.Fatalf("deny degrade: %v %+v", err, plan)
	}
	// warn → passes through, warning recorded
	s.Credentials.EnvVars[0].OnExtractNoMatch = "warn"
	plan, err = s.prepareCredentials(px, t.TempDir())
	if err != nil || len(plan.setEnv) != 0 || len(plan.warns) == 0 {
		t.Fatalf("warn degrade: %v %+v", err, plan)
	}
}

// Masked credential files: on darwin they degrade to denyRead (real's
// documented limitation), on linux they produce bind pairs.
func TestPrepareCredentialsFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "creds.txt")
	os.WriteFile(fp, []byte("token: file_secret_42"), 0o600)
	px := netproxy.New(nil)
	s := &SandboxSettings{
		locked:  map[string]bool{},
		Network: &SandboxNetwork{AllowedDomains: []string{"*"}},
		Credentials: SandboxCredentials{Files: []SandboxCredFile{
			{Path: fp, Mode: "mask", Extract: `token:\s*(\S+)`},
			{Path: filepath.Join(dir, "other"), Mode: "deny"},
		}},
	}
	plan, err := s.prepareCredentials(px, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(plan.denyRead, filepath.Join(dir, "other")) {
		t.Fatalf("deny-mode file not in denyRead: %v", plan.denyRead)
	}
	if runtime.GOOS == "darwin" {
		if !containsStr(plan.denyRead, fp) {
			t.Fatalf("masked file should degrade to deny on darwin: %v", plan.denyRead)
		}
		found := false
		for _, w := range plan.warns {
			if strings.Contains(w, "degrades to deny on macOS") {
				found = true
			}
		}
		if !found {
			t.Error("expected the macOS degradation warning")
		}
	} else {
		if len(plan.binds) != 1 || plan.binds[0][0] != fp {
			t.Fatalf("binds = %v", plan.binds)
		}
		// the fake file carries the masked content, not the secret
		b, _ := os.ReadFile(plan.binds[0][1])
		if strings.Contains(string(b), "file_secret_42") ||
			!strings.Contains(string(b), "fake_value_") {
			t.Fatalf("fake file content = %q", b)
		}
	}
	// the sentinel is registered either way
	subs := px.Creds.SentinelsForHost("anything.test")
	if len(subs) == 0 {
		t.Fatal("masked file sentinel not registered")
	}
}

func containsStr(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}
