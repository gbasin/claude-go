package tools

import (
	"context"
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
	}, true)
	// later user/project/local files cannot relax locked scalars,
	// but lists still merge
	s.apply(&sandboxFileJSON{
		Enabled:                  boolp(false),
		AllowUnsandboxedCommands: boolp(true),
		AutoAllowBashIfSandboxed: boolp(false),
		ExcludedCommands:         []string{"docker *"},
		Network:                  &sandboxNetJSON{AllowedDomains: []string{"b.com"}},
	}, false)
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
