//go:build darwin

package tools

import (
	"claude-go/internal/netproxy"
	"fmt"
	"os/exec"
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
	p := buildSeatbeltProfile(s, cwd, nil, nil)
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
	p := buildSeatbeltProfile(s, t.TempDir(), nil, nil)
	if strings.Contains(p, "file-read") || strings.Contains(p, "file-write") {
		t.Error("filesystem.disabled should skip all fs rules")
	}
	if !strings.Contains(p, "(deny network*)") {
		t.Error("network rules should still apply")
	}
}

func TestSeatbeltProfileProxy(t *testing.T) {
	s := &SandboxSettings{
		Network: &SandboxNetwork{AllowedDomains: []string{"*.example.com"}},
	}
	ca, err := netproxy.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	px := netproxy.New(ca)
	if err := px.Start(); err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	p := buildSeatbeltProfile(s, t.TempDir(), px, nil)
	for _, want := range []string{
		"(deny network*)",
		fmt.Sprintf(`(allow network-outbound (remote ip "localhost:%d"))`, px.Port()),
		fmt.Sprintf(`(allow network-outbound (remote ip "localhost:%d"))`, px.SocksPort()),
		`(literal "` + ca.CertPath + `")`, // CA readable inside the deny-read $HOME
	} {
		if !strings.Contains(p, want) {
			t.Errorf("proxy profile missing %q\n%s", want, p)
		}
	}
	// missing proxy on an allow-list config must still deny egress
	p = buildSeatbeltProfile(s, t.TempDir(), nil, nil)
	if !strings.Contains(p, "(deny network*)") || strings.Contains(p, "localhost:") {
		t.Error("failed proxy should fall back to blunt deny-all")
	}
	// allowAllLocal opens loopback + binds like real's allowLocalBinding
	s.Network.AllowAllLocal = true
	p = buildSeatbeltProfile(s, t.TempDir(), px, nil)
	for _, want := range []string{
		`(allow network-bind (local ip "*:*"))`,
		`(allow network-outbound (remote ip "localhost:*"))`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("allowAllLocal profile missing %q\n%s", want, p)
		}
	}
}

// TestSandboxWrapProxyEnv exercises the full path: enabled sandbox + network
// allow rules → shared proxy starts, env vars come back, and the generated
// seatbelt profile admits only the proxy port.
func TestSandboxWrapProxyEnv(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("no sandbox-exec")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // keep the CA out of ~/.claude
	t.Cleanup(resetNetProxyForTest)
	s := &SandboxSettings{
		Enabled: true,
		Network: &SandboxNetwork{AllowedDomains: []string{"*.example.com"}},
		locked:  map[string]bool{},
	}
	c := withSandboxCfg(t, s)
	got, env, warn, err := c.sandboxWrap("curl https://a.example.com", "curl x\nsentinel", false)
	if err != nil {
		t.Fatal(err)
	}
	if warn != "" {
		t.Log("warn:", warn)
	}
	var httpProxy, sslCert string
	for _, e := range env.Set {
		if strings.HasPrefix(e, "HTTP_PROXY=") {
			httpProxy = e
		}
		if strings.HasPrefix(e, "SSL_CERT_FILE=") {
			sslCert = e
		}
	}
	if httpProxy == "" || sslCert == "" {
		t.Fatalf("expected proxy env vars, got %v", env.Set)
	}
	if !strings.Contains(got, "sandbox-exec") {
		t.Fatalf("expected sandbox-exec wrap, got %q", got)
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
	p := buildSeatbeltProfile(s, t.TempDir(), nil, nil)
	if strings.Contains(p, "/user-allow") {
		t.Error("allowManagedReadPathsOnly should drop non-policy allowRead")
	}
	if !strings.Contains(p, "/policy-only") {
		t.Error("policy allowRead should be kept")
	}
}
