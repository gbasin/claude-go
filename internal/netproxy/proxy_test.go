package netproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"runtime"
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
		Proxy: http.ProxyURL(&url.URL{
			Scheme: "http", Host: p.Addr(),
			User: url.UserPassword("srt", p.AuthToken),
		}),
		TLSClientConfig: tlsCfg,
	}}
}

// proxyAuthHeader returns the Proxy-Authorization header value the proxy
// expects — "Basic <b64 srt:token>", matching the env URLs Env() emits.
func proxyAuthHeader(p *Proxy) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("srt:"+p.AuthToken))
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
	fmt.Fprintf(conn, "CONNECT evil.test:443 HTTP/1.1\r\nHost: evil.test:443\r\nProxy-Authorization: %s\r\n\r\n", proxyAuthHeader(p))
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
	fmt.Fprintf(conn, "CONNECT evil.example.test:443 HTTP/1.1\r\nHost: evil.example.test:443\r\nProxy-Authorization: %s\r\n\r\n", proxyAuthHeader(p))
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
	fmt.Fprintf(conn, "CONNECT ok.example.test:443 HTTP/1.1\r\nHost: ok.example.test:443\r\nProxy-Authorization: %s\r\n\r\n", proxyAuthHeader(p))
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
	tr.Proxy = http.ProxyURL(&url.URL{
		Scheme: "http", Host: p.Addr(),
		User: url.UserPassword("srt", p.AuthToken),
	})
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
	env := ProxyEnv(4321, 0, "/ca/cert.pem", "", "")
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
	// no socks port / no auth → none of the socks-side vars, no userinfo
	for _, k := range []string{"GIT_SSH_COMMAND", "FTP_PROXY", "RSYNC_PROXY",
		"DOCKER_HTTP_PROXY", "CLOUDSDK_PROXY_TYPE", "GIT_CONFIG_PARAMETERS"} {
		if _, ok := m[k]; ok {
			t.Errorf("unexpected %s without socks port/auth: %q", k, m[k])
		}
	}
}

// The full CBt() surface: socks port + auth token + command tag.
func TestProxyEnvFull(t *testing.T) {
	env := ProxyEnv(4321, 5678, "/ca/cert.pem", "tok-123", "echo hi")
	m := map[string]string{}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		m[k] = v
	}
	user := authUser(commandTag("echo hi")) // srt.<base64>
	wantURL := fmt.Sprintf("http://%s:tok-123@localhost:4321",
		encodeURIComponent(user))
	if m["HTTPS_PROXY"] != wantURL {
		t.Errorf("HTTPS_PROXY = %q, want %q", m["HTTPS_PROXY"], wantURL)
	}
	if m["ALL_PROXY"] != wantURL {
		t.Errorf("ALL_PROXY = %q, want %q", m["ALL_PROXY"], wantURL)
	}
	su := "socks5h://" + encodeURIComponent(user) + ":tok-123@localhost:5678"
	if m["FTP_PROXY"] != su {
		t.Errorf("FTP_PROXY = %q, want %q", m["FTP_PROXY"], su)
	}
	if m["RSYNC_PROXY"] != "localhost:5678" {
		t.Errorf("RSYNC_PROXY = %q", m["RSYNC_PROXY"])
	}
	if m["DOCKER_HTTP_PROXY"] != wantURL || m["DOCKER_HTTPS_PROXY"] != wantURL {
		t.Errorf("DOCKER_*_PROXY = %q/%q", m["DOCKER_HTTP_PROXY"], m["DOCKER_HTTPS_PROXY"])
	}
	if m["GIT_CONFIG_PARAMETERS"] != "'http.proxyAuthMethod=basic'" {
		t.Errorf("GIT_CONFIG_PARAMETERS = %q", m["GIT_CONFIG_PARAMETERS"])
	}
	if m["CLOUDSDK_PROXY_TYPE"] != "http" || m["CLOUDSDK_PROXY_ADDRESS"] != "localhost" ||
		m["CLOUDSDK_PROXY_PORT"] != "4321" {
		t.Errorf("CLOUDSDK block wrong: %q/%q/%q",
			m["CLOUDSDK_PROXY_TYPE"], m["CLOUDSDK_PROXY_ADDRESS"], m["CLOUDSDK_PROXY_PORT"])
	}
	if m["CLOUDSDK_PROXY_USERNAME"] != user || m["CLOUDSDK_PROXY_PASSWORD"] != "tok-123" {
		t.Errorf("CLOUDSDK auth = %q/%q", m["CLOUDSDK_PROXY_USERNAME"], m["CLOUDSDK_PROXY_PASSWORD"])
	}
	if runtime.GOOS == "darwin" {
		want := "ssh -o ControlMaster=no -o ControlPath=none -o ProxyCommand='nc -X 5 -x localhost:5678 %h %p'"
		if m["GIT_SSH_COMMAND"] != want {
			t.Errorf("GIT_SSH_COMMAND = %q, want %q", m["GIT_SSH_COMMAND"], want)
		}
	}
	// command tag round-trips: decode the user back to the command
	if got := decodeTagCommand(decodeUserTag(user)); got != "echo hi" {
		t.Errorf("tag round-trip = %q", got)
	}
}

// ---------- proxy auth (real's proxyAuthToken + 407) ----------

func TestProxyAuth407(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "plain-ok")
	}))
	defer upstream.Close()
	p := startProxy(t, Rules{AllowedDomains: []string{"*"}},
		strings.TrimPrefix(upstream.URL, "http://"), false)

	// Go's transport injects Proxy-Authorization from the proxy URL's
	// userinfo, so each case dials through a differently-credentialed URL.
	get := func(user *url.Userinfo) int {
		tr := &http.Transport{Proxy: http.ProxyURL(&url.URL{
			Scheme: "http", Host: p.Addr(), User: user})}
		req, _ := http.NewRequest("GET", "http://anything.test/", nil)
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.ReadAll(resp.Body)
		return resp.StatusCode
	}
	if code := get(nil); code != http.StatusProxyAuthRequired {
		t.Errorf("missing auth should 407, got %d", code)
	}
	if code := get(url.UserPassword("srt", "wrong")); code != http.StatusProxyAuthRequired {
		t.Errorf("bad password should 407, got %d", code)
	}
	if code := get(url.UserPassword("srt", p.AuthToken)); code != http.StatusOK {
		t.Errorf("good auth should 200, got %d", code)
	}
	// CONNECT needs auth too
	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT anything.test:443 HTTP/1.1\r\nHost: anything.test:443\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Errorf("unauthenticated CONNECT should 407, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Proxy-Authenticate") != `Basic realm="srt"` {
		t.Errorf("Proxy-Authenticate = %q", resp.Header.Get("Proxy-Authenticate"))
	}
}

// ---------- post-dial remote IP re-verification ----------

type fakeRemoteConn struct {
	net.Conn
	remote net.Addr
}

func (c fakeRemoteConn) RemoteAddr() net.Addr { return c.remote }

func TestPostDialReverify(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()
	upAddr := strings.TrimPrefix(upstream.URL, "https://")

	p := New(testCA(t))
	p.SetRules(Rules{
		AllowedDomains: []string{"ok.test"},
		DeniedDomains:  []string{"192.168.0.0/16"},
	})
	// decide() resolves nothing (no resolver hits a CIDR rule by host) but
	// the socket lands on a denied IP — the rebinding window.
	p.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		c, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", upAddr)
		if err != nil {
			return nil, err
		}
		return fakeRemoteConn{c, &net.TCPAddr{IP: net.ParseIP("192.168.9.9"), Port: 443}}, nil
	}
	p.UpstreamTLS = &tls.Config{InsecureSkipVerify: true}
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT ok.test:443 HTTP/1.1\r\nHost: ok.test:443\r\nProxy-Authorization: %s\r\n\r\n", proxyAuthHeader(p))
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("post-dial denied IP should 403, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "deny list") {
		t.Errorf("body should carry the reason: %q", body)
	}
}

// ---------- h2 passthrough inside MITM ----------

// tlsEcho starts a TLS listener advertising the given ALPN protos that
// echoes payload bytes back.
func tlsEcho(t *testing.T, ca *CA, host string, protos []string) string {
	t.Helper()
	cert, err := ca.Mint(host)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{*cert},
		NextProtos:   protos,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(c)
		}
	}()
	return ln.Addr().String()
}

// When the client and upstream both speak h2, the MITM splices decrypted
// bytes verbatim instead of parsing frames.
func TestMITMH2Splice(t *testing.T) {
	ca := testCA(t)
	upAddr := tlsEcho(t, ca, "up.test", []string{"h2", "http/1.1"})

	p := startProxy(t, Rules{AllowedDomains: []string{"up.test"}}, upAddr, true)

	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT up.test:443 HTTP/1.1\r\nHost: up.test:443\r\nProxy-Authorization: %s\r\n\r\n", proxyAuthHeader(p))
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("CONNECT = %d", resp.StatusCode)
	}
	resp.Body.Close()

	tc := tls.Client(conn, &tls.Config{
		RootCAs:    p.CA.Pool(),
		ServerName: "up.test",
		NextProtos: []string{"h2"},
	})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	if got := tc.ConnectionState().NegotiatedProtocol; got != "h2" {
		t.Fatalf("client negotiated %q, want h2", got)
	}
	payload := []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\nfake-h2-frames")
	if _, err := tc.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	tc.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(tc, got); err != nil {
		t.Fatalf("echo read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("spliced bytes differ: %q", got)
	}
}

// When the upstream can't do h2 the client is offered http/1.1 only and the
// MITM inspects as before (end-to-end http/1.1 fallback).
func TestMITMH2UpstreamFallback(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "h1-body")
	}))
	defer upstream.Close()
	p := startProxy(t, Rules{AllowedDomains: []string{"*.example.test"}},
		strings.TrimPrefix(upstream.URL, "https://"), true)

	// client offers h2+h1; upstream is h1-only → MITM http/1.1 path
	client := proxyClient(p, &tls.Config{RootCAs: p.CA.Pool()})
	resp, err := client.Get("https://app.example.test/")
	if err != nil {
		t.Fatalf("GET through MITM proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "h1-body" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
}

// ---------- SOCKS5 ----------

// echoTCP starts a plain-TCP echo server; returns its addr.
func echoTCP(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(c)
		}
	}()
	return ln.Addr().String()
}

// socksGreeting writes VER/NMETHODS/METHODS and checks the server's choice.
func socksGreeting(t *testing.T, c net.Conn, methods []byte, wantMethod byte) {
	t.Helper()
	msg := append([]byte{5, byte(len(methods))}, methods...)
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(c, resp); err != nil {
		t.Fatalf("greeting response: %v", err)
	}
	if resp[0] != 5 || resp[1] != wantMethod {
		t.Fatalf("greeting = %v, want method %d", resp, wantMethod)
	}
}

// socksUserPass writes the RFC1929 credentials and checks the status byte.
func socksUserPass(t *testing.T, c net.Conn, user, pass string, wantOK bool) {
	t.Helper()
	msg := append([]byte{1, byte(len(user))}, []byte(user)...)
	msg = append(msg, byte(len(pass)))
	msg = append(msg, []byte(pass)...)
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(c, resp); err != nil {
		t.Fatalf("auth response: %v", err)
	}
	want := byte(0)
	if !wantOK {
		want = 1
	}
	if resp[1] != want {
		t.Fatalf("auth status = %d, want %d", resp[1], want)
	}
}

// socksConnect writes a CONNECT request for a domain host and returns the
// reply code byte.
func socksConnect(t *testing.T, c net.Conn, host string, port int) byte {
	t.Helper()
	msg := []byte{5, 1, 0, 3, byte(len(host))}
	msg = append(msg, []byte(host)...)
	var pb [2]byte
	binary.BigEndian.PutUint16(pb[:], uint16(port))
	msg = append(msg, pb[:]...)
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 10) // VER REP RSV ATYP(1) 4B IP 2B port
	if _, err := io.ReadFull(c, resp); err != nil {
		t.Fatalf("connect reply: %v", err)
	}
	if resp[0] != 5 {
		t.Fatalf("bad reply ver %d", resp[0])
	}
	return resp[1]
}

func TestSocksConnectAllowDeny(t *testing.T) {
	upAddr := echoTCP(t)
	ca := testCA(t)
	p := New(ca)
	p.AuthToken = "" // exercise the no-auth method (must be set before Start)
	p.SetRules(Rules{AllowedDomains: []string{"ok.test"}})
	p.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", upAddr)
	}
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })

	// allowed host → granted + spliced echo
	c, err := net.Dial("tcp", p.socksLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	socksGreeting(t, c, []byte{0}, 0)
	if rep := socksConnect(t, c, "ok.test", 80); rep != 0 {
		t.Fatalf("allowed CONNECT reply = %d", rep)
	}
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("spliced echo: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("echo = %q", got)
	}
	c.Close()

	// denied host → connection not allowed (2)
	c2, err := net.Dial("tcp", p.socksLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	socksGreeting(t, c2, []byte{0}, 0)
	if rep := socksConnect(t, c2, "evil.test", 80); rep != 2 {
		t.Fatalf("denied CONNECT reply = %d, want 2", rep)
	}
}

func TestSocksUserPassAuth(t *testing.T) {
	p := startProxy(t, Rules{AllowedDomains: []string{"ok.test"}}, echoTCP(t), false)

	c, err := net.Dial("tcp", p.socksLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	socksGreeting(t, c, []byte{0, 2}, 2) // server must pick user/pass
	socksUserPass(t, c, "srt."+commandTag("curl x"), p.AuthToken, true)
	if rep := socksConnect(t, c, "ok.test", 443); rep != 0 {
		t.Fatalf("authenticated CONNECT reply = %d", rep)
	}

	// wrong password → [1,1] + close
	c2, err := net.Dial("tcp", p.socksLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	socksGreeting(t, c2, []byte{2}, 2)
	socksUserPass(t, c2, "srt", "nope", false)
}

// Real's EM path: a client that can't do user/pass auth (nc -X 5) gets a
// refusal — an SSH banner for port 22 so git+ssh shows the reason.
func TestSocksUnauthenticatedRefusal(t *testing.T) {
	p := startProxy(t, Rules{AllowedDomains: []string{"*"}}, "", false)

	c, err := net.Dial("tcp", p.socksLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	socksGreeting(t, c, []byte{0}, 0) // EM accepts no-auth, then refuses post-request
	if rep := socksConnect(t, c, "git.example.test", 22); rep != 0 {
		t.Fatalf("port-22 refusal path should reply success first, got %d", rep)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("reading SSH banner: %v", err)
	}
	if !strings.Contains(line, "SSH-2.0-policy_refusal") {
		t.Fatalf("banner = %q", line)
	}

	// non-22 unauthenticated → hard refusal (2)
	c2, err := net.Dial("tcp", p.socksLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	socksGreeting(t, c2, []byte{0}, 0)
	if rep := socksConnect(t, c2, "ok.test", 443); rep != 2 {
		t.Fatalf("unauthenticated non-22 reply = %d, want 2", rep)
	}
}
