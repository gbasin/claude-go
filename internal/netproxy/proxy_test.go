package netproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testCA(t *testing.T) *CA {
	t.Helper()
	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	return ca
}

func TestCAPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	ca1, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	ca2, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !ca1.Cert.Equal(ca2.Cert) {
		t.Error("reloaded CA differs from persisted one")
	}
	if !ca1.Cert.IsCA {
		t.Error("root should be a CA")
	}
	if got := ca1.Cert.NotAfter.Sub(ca1.Cert.NotBefore); got < 9*365*24*time.Hour {
		t.Error("CA validity should be ~10 years")
	}
}

func TestCAMintVerifies(t *testing.T) {
	ca := testCA(t)
	leaf, err := ca.Mint("API.Example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{
		DNSName: "api.example.com",
		Roots:   ca.Pool(),
	}); err != nil {
		t.Errorf("minted leaf fails verify: %v", err)
	}
	// cached
	leaf2, _ := ca.Mint("api.example.com")
	if leaf != leaf2 {
		t.Error("mint cache should return the same cert")
	}
	// IP SAN
	leafIP, err := ca.Mint("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leafIP.Leaf.Verify(x509.VerifyOptions{
		DNSName: "127.0.0.1",
		Roots:   ca.Pool(),
	}); err != nil {
		t.Errorf("IP leaf fails verify: %v", err)
	}
}

// ---------- matching (real CC hBe/Dyn semantics) ----------

func TestMatchDomain(t *testing.T) {
	cases := []struct {
		host, pat string
		want      bool
	}{
		{"example.com", "example.com", true},
		{"EXAMPLE.com", "example.com", true},
		{"example.com.", "example.com", true},
		{"sub.example.com", "example.com", false}, // exact does not cover subs
		{"a.example.com", "*.example.com", true},
		{"a.b.example.com", "*.example.com", true}, // wildcard spans labels
		{"example.com", "*.example.com", false},    // apex NOT covered
		{"badexample.com", "*.example.com", false}, // needs the dot
		{"127.0.0.1", "*.0.0.1", false},            // IPs never match wildcards
		{"anything.io", "*", true},
		{"1.2.3.4", "1.2.3.4", true},
	}
	for _, c := range cases {
		if got := matchDomain(c.host, c.pat); got != c.want {
			t.Errorf("matchDomain(%q,%q)=%v want %v", c.host, c.pat, got, c.want)
		}
	}
}

func TestDecideOrder(t *testing.T) {
	p := New(nil)
	p.Resolve = func(context.Context, string) ([]netip.Addr, error) { return nil, nil }
	p.SetRules(Rules{
		AllowedDomains: []string{"*.example.com", "good.io:8443", "10.0.0.0/8"},
		DeniedDomains:  []string{"bad.example.com", "192.168.0.0/16"},
	})
	cases := []struct {
		host string
		port int
		want bool
	}{
		{"a.example.com", 443, true},
		{"bad.example.com", 443, false}, // deny beats wildcard allow
		{"other.org", 443, false},       // unmatched
		{"good.io", 8443, true},         // port-qualified rule
		{"good.io", 443, false},         // wrong port
		{"10.1.2.3", 443, true},         // CIDR grant via IP literal
		{"192.168.1.1", 443, false},     // CIDR deny
	}
	for _, c := range cases {
		got, _ := p.decide(context.Background(), c.host, c.port)
		if got != c.want {
			t.Errorf("decide(%q,%d)=%v want %v", c.host, c.port, got, c.want)
		}
	}
}

func TestDecideResolvedCIDR(t *testing.T) {
	p := New(nil)
	p.Resolve = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "evil.test" {
			return []netip.Addr{netip.MustParseAddr("192.168.9.9")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	p.SetRules(Rules{
		AllowedDomains: []string{"*"},
		DeniedDomains:  []string{"192.168.0.0/16"},
	})
	if ok, _ := p.decide(context.Background(), "evil.test", 443); ok {
		t.Error("hostname resolving into denied CIDR must be denied")
	}
	if ok, _ := p.decide(context.Background(), "fine.test", 443); !ok {
		t.Error("hostname resolving outside denied CIDR should pass")
	}
}

func TestDecideAllowAllLocal(t *testing.T) {
	p := New(nil)
	p.Resolve = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "printer.lan" {
			return []netip.Addr{netip.MustParseAddr("10.0.0.5")}, nil
		}
		return nil, fmt.Errorf("no resolve")
	}
	p.SetRules(Rules{AllowAllLocal: true})
	for host, want := range map[string]bool{
		"localhost":     true,
		"db.localhost":  true,
		"127.0.0.1":     true,
		"10.1.1.1":      true,  // private
		"169.254.1.2":   true,  // link-local
		"8.8.8.8":       false, // public
		"printer.lan":   true,  // resolves private
		"unresolved.io": false,
	} {
		if got, _ := p.decide(context.Background(), host, 443); got != want {
			t.Errorf("decide(%q)=%v want %v", host, got, want)
		}
	}
}

// ---------- end-to-end ----------

// startProxy starts a rules-configured proxy whose upstream dials all map to
// the given address (lets fake hostnames reach a test server).
func startProxy(t *testing.T, rules Rules, upstreamAddr string, insecureTLS bool) *Proxy {
	t.Helper()
	ca := testCA(t)
	p := New(ca)
	p.SetRules(rules)
	if upstreamAddr != "" {
		p.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", upstreamAddr)
		}
	}
	if insecureTLS {
		p.UpstreamTLS = &tls.Config{InsecureSkipVerify: true}
	}
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func proxyClient(p *Proxy, tlsCfg *tls.Config) *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(&url.URL{Scheme: "http", Host: p.Addr()}),
		TLSClientConfig: tlsCfg,
	}}
}

func TestConnectMITMAllowed(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "secret-body")
	}))
	defer upstream.Close()

	p := startProxy(t, Rules{AllowedDomains: []string{"*.example.test"}},
		strings.TrimPrefix(upstream.URL, "https://"), true)

	client := proxyClient(p, &tls.Config{RootCAs: p.CA.Pool()})
	resp, err := client.Get("https://app.example.test/")
	if err != nil {
		t.Fatalf("GET through MITM proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "secret-body" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
}

func TestConnectDenied(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()
	p := startProxy(t, Rules{AllowedDomains: []string{"ok.test"}},
		strings.TrimPrefix(upstream.URL, "https://"), true)

	// manual CONNECT so we can read the raw status line
	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT evil.test:443 HTTP/1.1\r\nHost: evil.test:443\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "not on the allow list") {
		t.Errorf("deny body should carry the reason: %q", body)
	}
}

func TestConnectWildcardAndApex(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	p := startProxy(t, Rules{AllowedDomains: []string{"*.wild.test"}},
		strings.TrimPrefix(upstream.URL, "https://"), true)
	client := proxyClient(p, &tls.Config{RootCAs: p.CA.Pool()})

	if resp, err := client.Get("https://a.b.wild.test/"); err != nil {
		t.Errorf("deep subdomain should be allowed: %v", err)
	} else {
		resp.Body.Close()
	}

	// apex is not covered by *.wild.test → CONNECT refused
	if _, err := client.Get("https://wild.test/"); err == nil {
		t.Error("apex should be denied by *.wild.test")
	}
}

func TestDeniedDomainBeatsWildcard(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()
	p := startProxy(t, Rules{
		AllowedDomains: []string{"*.example.test"},
		DeniedDomains:  []string{"evil.example.test"},
	}, strings.TrimPrefix(upstream.URL, "https://"), true)

	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT evil.example.test:443 HTTP/1.1\r\nHost: evil.example.test:443\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied domain should 403, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// Inner per-request Host check: CONNECT to an allowed host but send requests
// for a different (unlisted) host inside the tunnel → 403.
func TestMITMInnerHostChecked(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	p := startProxy(t, Rules{AllowedDomains: []string{"ok.example.test"}},
		strings.TrimPrefix(upstream.URL, "https://"), true)

	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT ok.example.test:443 HTTP/1.1\r\nHost: ok.example.test:443\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("CONNECT should be 200, got %d", resp.StatusCode)
	}
	tlsConn := tls.Client(conn, &tls.Config{
		RootCAs:    p.CA.Pool(),
		ServerName: "ok.example.test",
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("client handshake with minted cert: %v", err)
	}
	// request a different, unlisted host inside the tunnel
	fmt.Fprintf(tlsConn, "GET / HTTP/1.1\r\nHost: sneaky.test\r\nConnection: close\r\n\r\n")
	inner, err := http.ReadResponse(bufio.NewReader(tlsConn), &http.Request{Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	if inner.StatusCode != http.StatusForbidden {
		t.Fatalf("inner request to unlisted host should 403, got %d", inner.StatusCode)
	}
	inner.Body.Close()
}

func TestPlainHTTPForwardAndDeny(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "plain-ok")
	}))
	defer upstream.Close()
	p := startProxy(t, Rules{AllowedDomains: []string{"plain.example.test"}},
		strings.TrimPrefix(upstream.URL, "http://"), false)

	client := proxyClient(p, nil)
	resp, err := client.Get("http://plain.example.test/path")
	if err != nil {
		t.Fatalf("plain GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "plain-ok" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}

	resp2, err := client.Get("http://denied.test/")
	if err != nil {
		t.Fatalf("denied plain GET should return a response, not error: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("plain HTTP deny should be 403, got %d", resp2.StatusCode)
	}
}

// Passthrough mode (Intercept off / no CA): the client's TLS goes straight to
// upstream — the minted cert is never involved.
func TestConnectPassthrough(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "tunnel-ok")
	}))
	defer upstream.Close()
	upAddr := strings.TrimPrefix(upstream.URL, "https://")

	p := New(nil) // no CA → passthrough
	p.SetRules(Rules{AllowedDomains: []string{"127.0.0.1"}})
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	tr := upstream.Client().Transport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyURL(&url.URL{Scheme: "http", Host: p.Addr()})
	client := &http.Client{Transport: tr}
	resp, err := client.Get(upstream.URL) // https://127.0.0.1:port → CONNECT 127.0.0.1:port
	if err != nil {
		t.Fatalf("passthrough GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "tunnel-ok" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	_ = upAddr
}

func TestProxyEnv(t *testing.T) {
	env := ProxyEnv(4321, "/ca/cert.pem")
	m := map[string]string{}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		m[k] = v
	}
	for _, k := range []string{
		"SANDBOX_RUNTIME", "HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy",
		"ALL_PROXY", "GRPC_PROXY", "NO_PROXY", "no_proxy",
		"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "GIT_SSL_CAINFO",
	} {
		if _, ok := m[k]; !ok {
			t.Errorf("env missing %s", k)
		}
	}
	if m["HTTPS_PROXY"] != "http://localhost:4321" {
		t.Errorf("HTTPS_PROXY = %q", m["HTTPS_PROXY"])
	}
	if m["SSL_CERT_FILE"] != "/ca/cert.pem" {
		t.Errorf("SSL_CERT_FILE = %q", m["SSL_CERT_FILE"])
	}
	if !strings.Contains(m["NO_PROXY"], "127.0.0.1") {
		t.Errorf("NO_PROXY should cover local ranges: %q", m["NO_PROXY"])
	}
}
