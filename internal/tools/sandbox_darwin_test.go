//go:build darwin

package tools

import (
	"strings"
	"testing"
)

func TestSeatbeltProfile(t *testing.T) {
	cwd := t.TempDir()
	s := &SandboxSettings{
		Enabled: true,
		locked:  map[string]bool{},
		Filesystem: SandboxFilesystem{
			DenyRead:   []string{"~/secret"},
			AllowWrite: []string{"extra-writable"},
			DenyWrite:  []string{"/no-write"},
		},
		Network: &SandboxNetwork{DeniedDomains: []string{"*"}},
	}
	p := buildSeatbeltProfile(s, cwd)
	for _, want := range []string{
		"(version 1)", "(allow default)",
		`(subpath "/etc")`,        // deny-read list
		`(literal "/etc/passwd")`, // read exception
		`(subpath "/Users")`,      // deny-read list
		`(literal "/dev/null")`,   // device node
		"(deny file-write*",       // write deny-all
		"(deny network*)",         // deny-all egress
	} {
		if !strings.Contains(p, want) {
			t.Errorf("profile missing %q\n%s", want, p)
		}
	}
	if !strings.Contains(p, cwd) {
		t.Error("profile should allow writes under cwd")
	}
	// denyWrite emitted after the allow-write block so it wins
	denyIdx := strings.Index(p, "(deny file-write*")
	allowIdx := strings.LastIndex(p, "(allow file-write*")
	reDeny := strings.LastIndex(p, "(deny file-write*")
	if denyIdx < 0 || allowIdx < denyIdx || reDeny < allowIdx || !strings.Contains(p[reDeny:], "/no-write") {
		t.Error("denyWrite must come after allowWrite in the profile")
	}
}

func TestSeatbeltProfileFilesystemDisabled(t *testing.T) {
	s := &SandboxSettings{
		Filesystem: SandboxFilesystem{Disabled: true},
		Network:    &SandboxNetwork{DeniedDomains: []string{"*"}},
	}
	p := buildSeatbeltProfile(s, t.TempDir())
	if strings.Contains(p, "file-read") || strings.Contains(p, "file-write") {
		t.Error("filesystem.disabled should skip all fs rules")
	}
	if !strings.Contains(p, "(deny network*)") {
		t.Error("network rules should still apply")
	}
}

func TestSeatbeltProfileManagedReadOnly(t *testing.T) {
	s := &SandboxSettings{
		locked:          map[string]bool{},
		policyAllowRead: []string{"/policy-only"},
		Filesystem: SandboxFilesystem{
			AllowManagedReadPathsOnly: true,
			AllowRead:                 []string{"/user-allow"},
		},
	}
	p := buildSeatbeltProfile(s, t.TempDir())
	if strings.Contains(p, "/user-allow") {
		t.Error("allowManagedReadPathsOnly should drop non-policy allowRead")
	}
	if !strings.Contains(p, "/policy-only") {
		t.Error("policy allowRead should be kept")
	}
}
