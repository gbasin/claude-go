//go:build darwin

package tools

import (
	"claude-go/internal/netproxy"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Live verification: real sandbox-exec + real curl through the session
// proxy with the generated env — auth URL creds, MITM via CA env vars.
// Run: go test ./internal/tools -run TestLiveSandboxCurl -v
func TestLiveSandboxCurl(t *testing.T) {
	if os.Getenv("LIVE") == "" {
		t.Skip("set LIVE=1")
	}
	dir := t.TempDir()
	ca, err := netproxy.LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	px := netproxy.New(ca)
	px.SetRules(netproxy.Rules{AllowedDomains: []string{"example.com", "*.example.com"}})
	if err := px.Start(); err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	t.Cleanup(func() {
		cert, key := ca.CertPath, ca.KeyPath
		_ = cert
		_ = key
	})

	s := &SandboxSettings{Enabled: true, Network: &SandboxNetwork{AllowedDomains: []string{"example.com"}}}
	profile := buildSeatbeltProfile(s, "/tmp", px)
	pf := dir + "/p.sb"
	if err := os.WriteFile(pf, []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}

	env := px.Env("curl https://example.com/")
	// show the env surface
	for _, e := range env {
		if strings.HasPrefix(e, "HTTP_PROXY") || strings.HasPrefix(e, "GIT_SSH") ||
			strings.HasPrefix(e, "ALL_PROXY") || strings.HasPrefix(e, "CLOUDSDK") ||
			strings.HasPrefix(e, "GIT_CONFIG") {
			t.Log("env:", e)
		}
	}

	// allowed host through MITM
	cmd := exec.Command("sandbox-exec", "-p", profile, "/bin/sh", "-c",
		"curl -s -o /dev/null -w '%{http_code}' https://example.com/")
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	t.Logf("allowed curl: %q err=%v", out, err)
	if strings.TrimSpace(string(out)) != "200" {
		t.Errorf("expected 200, got %q (%v)", out, err)
	}

	// denied host (HTTPS): CONNECT is refused with 403 → curl exit 56 and a
	// "HTTP code 403 from proxy" line on stderr.
	cmd2 := exec.Command("sandbox-exec", "-p", profile, "/bin/sh", "-c",
		"curl -sv https://denied.example.org/ 2>&1 | tail -8")
	cmd2.Env = append(os.Environ(), env...)
	out2, err2 := cmd2.CombinedOutput()
	t.Logf("denied curl: %q err=%v", out2, err2)
	if !strings.Contains(string(out2), "403") && !strings.Contains(string(out2), "Blocked by") {
		t.Errorf("expected proxy 403 for denied host, got %q", out2)
	}

	// denied plain-HTTP: proxy forwards nothing → real 403 with body.
	cmd2b := exec.Command("sandbox-exec", "-p", profile, "/bin/sh", "-c",
		"curl -s http://denied.example.org/")
	cmd2b.Env = append(os.Environ(), env...)
	out2b, _ := cmd2b.CombinedOutput()
	t.Logf("denied http: %q", out2b)
	if !strings.Contains(string(out2b), "Blocked by") {
		t.Errorf("expected deny body, got %q", out2b)
	}

	// direct egress must be denied by seatbelt (no proxy env for this one)
	cmd3 := exec.Command("sandbox-exec", "-p", profile, "/bin/sh", "-c",
		"env -u HTTP_PROXY -u HTTPS_PROXY -u http_proxy -u https_proxy -u ALL_PROXY -u all_proxy curl -s -o /dev/null -w '%{http_code}' --max-time 5 https://example.com/")
	cmd3.Env = os.Environ()
	out3, _ := cmd3.CombinedOutput()
	t.Logf("unproxied curl: %q", out3)
	if strings.TrimSpace(string(out3)) == "200" {
		t.Error("seatbelt should have blocked direct egress")
	}

	// socks listener reachability via nc (unauthenticated → refusal, port 443)
	nc := exec.Command("nc", "-X", "5", "-x", fmt.Sprintf("localhost:%d", px.SocksPort()), "example.com", "443")
	nc.Env = append(os.Environ(), env...)
	nout, nerr := nc.CombinedOutput()
	t.Logf("nc socks unauth: %q err=%v", nout, nerr)
}
