package netproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Rules mirrors the sandbox.network settings block (mod_1164.js q). Entries
// in AllowedDomains/DeniedDomains follow real CC's Dyn() semantics: exact
// host, "*.suffix" (subdomains only, never the apex and never IP literals),
// "*" for all, and an optional ":port" qualifier. Entries that parse as an IP
// or CIDR are matched against the destination address instead (real's S6r).
// AllowAllLocal is a claude-go extension analogous to real's allowLocalBinding:
// it whitelists loopback/private/link-local destinations.
type Rules struct {
	AllowedDomains []string
	DeniedDomains  []string
	AllowedCidrs   []string
	AllowAllLocal  bool
}

type compiledRules struct {
	allow         []string
	deny          []string
	allowNet      []netip.Prefix
	denyNet       []netip.Prefix
	allowAllLocal bool
}

func compileRules(r Rules) compiledRules {
	var c compiledRules
	c.allowAllLocal = r.AllowAllLocal
	split := func(entries []string, hosts *[]string, nets *[]netip.Prefix) {
		for _, e := range entries {
			h, _ := parseRule(e)
			if p, err := netip.ParsePrefix(h); err == nil {
				*nets = append(*nets, p.Masked())
				continue
			}
			if ip, err := netip.ParseAddr(h); err == nil {
				*nets = append(*nets, netip.PrefixFrom(ip, ip.BitLen()))
				continue
			}
			*hosts = append(*hosts, e)
		}
	}
	split(r.AllowedDomains, &c.allow, &c.allowNet)
	split(r.DeniedDomains, &c.deny, &c.denyNet)
	split(r.AllowedCidrs, &c.allow, &c.allowNet)
	return c
}

// parseRule splits an optional ":port" qualifier off a rule, handling
// "host", "host:port", "[v6]", "[v6]:port" and bare IPv6 (mod_0215.js l8).
func parseRule(s string) (pattern string, port int) {
	if strings.HasPrefix(s, "[") {
		if i := strings.Index(s, "]"); i >= 0 {
			pattern = s[1:i]
			rest := s[i+1:]
			if strings.HasPrefix(rest, ":") {
				port, _ = strconv.Atoi(rest[1:])
			}
			return
		}
	}
	if i := strings.LastIndex(s, ":"); i > 0 && strings.Index(s, ":") == i {
		if n, err := strconv.Atoi(s[i+1:]); err == nil {
			return s[:i], n
		}
	}
	return s, 0
}

// matchDomain implements hBe: "*" matches all; "*.x" matches strict
// subdomains of x (not x itself) and never IP literals; otherwise exact,
// case-insensitive, trailing-dot-insensitive.
func matchDomain(host, pattern string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	pattern = strings.ToLower(strings.TrimSuffix(pattern, "."))
	if pattern == "*" {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		if _, err := netip.ParseAddr(host); err == nil {
			return false
		}
		return strings.HasSuffix(host, "."+pattern[2:])
	}
	return host == pattern
}

func matchRule(host string, port int, rule string) bool {
	pat, rp := parseRule(rule)
	if rp != 0 && rp != port {
		return false
	}
	return matchDomain(host, pat)
}

// normalizeHost mirrors $0: lowercase, strip brackets and trailing dot.
func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return strings.TrimSuffix(host, ".")
}

// splitAuthority splits a host[:port] authority, defaulting the port.
// Handles bare IPv6 in brackets.
func splitAuthority(authority string, defPort int) (string, int) {
	authority = strings.TrimSpace(authority)
	h, p, err := net.SplitHostPort(authority)
	if err == nil {
		port, err2 := strconv.Atoi(p)
		if err2 != nil || port < 1 || port > 65535 {
			port = defPort
		}
		return normalizeHost(h), port
	}
	return normalizeHost(authority), defPort
}

// hopHeaders is real's `he` set — hop-by-hop headers stripped before relay.
var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func stripHopHeaders(h http.Header) {
	// honor the Connection header's own token list first
	for _, v := range h.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			h.Del(strings.TrimSpace(tok))
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

// Proxy is the session egress proxy: an HTTP proxy on an ephemeral loopback
// port. Plain requests are forwarded after a host check; CONNECT tunnels are
// either MITM'd (Intercept + CA) or spliced through blindly.
type Proxy struct {
	CA *CA
	// Intercept controls CONNECT MITM. Defaults to true when a CA is present.
	// With interception off the proxy is still an allowlist gate on the
	// CONNECT authority, but cannot see per-request Host headers inside TLS.
	Intercept bool

	// Injectable hooks (tests; production uses real dialing):
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	Resolve     func(ctx context.Context, host string) ([]netip.Addr, error)
	// UpstreamTLS is the base tls.Config for upstream connections made during
	// MITM/plain forwarding (ServerName is filled per request). Tests use it
	// to trust httptest certs.
	UpstreamTLS *tls.Config
	// Transport overrides the upstream RoundTripper entirely when non-nil.
	Transport *http.Transport

	mu    sync.RWMutex
	rules compiledRules
	ln    net.Listener
	srv   *http.Server
	tr    *http.Transport
}

// New builds a proxy. ca may be nil: the proxy then runs in passthrough mode
// (CONNECT filtering only).
func New(ca *CA) *Proxy {
	p := &Proxy{CA: ca, Intercept: ca != nil}
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	return p
}

// SetRules swaps the allow/deny rules (call per command; settings can change
// between Bash invocations).
func (p *Proxy) SetRules(r Rules) {
	c := compileRules(r)
	p.mu.Lock()
	p.rules = c
	p.mu.Unlock()
}

func (p *Proxy) rulesSnapshot() compiledRules {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.rules
}

// Start binds 127.0.0.1:0 and serves in the background.
func (p *Proxy) Start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	p.ln = ln
	go p.srv.Serve(ln)
	return nil
}

func (p *Proxy) Port() int {
	if p.ln == nil {
		return 0
	}
	return p.ln.Addr().(*net.TCPAddr).Port
}

// Addr returns "127.0.0.1:port" once started.
func (p *Proxy) Addr() string {
	if p.ln == nil {
		return ""
	}
	return p.ln.Addr().String()
}

// Close shuts the proxy down.
func (p *Proxy) Close() error {
	if p.srv == nil {
		return nil
	}
	return p.srv.Close()
}

// Env returns the sandboxed-command environment — real's CBt() wiring:
// SANDBOX_RUNTIME, HTTP(S)_PROXY (both cases), ALL_PROXY/GRPC_PROXY pointing
// at this proxy, NO_PROXY covering local ranges, and every CA-bundle env var
// a common toolchain honors pointed at the CA cert.
func (p *Proxy) Env() []string {
	caPath := ""
	if p.CA != nil {
		caPath = p.CA.CertPath
	}
	return ProxyEnv(p.Port(), caPath)
}

// caBundleEnvKeys is real's `oe` list — every CA-file env var exported into
// sandboxed commands so toolchains trust the minted leaf certificates.
var caBundleEnvKeys = []string{
	"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "CURL_CA_BUNDLE",
	"REQUESTS_CA_BUNDLE", "PIP_CERT", "GIT_SSL_CAINFO", "AWS_CA_BUNDLE",
	"CARGO_HTTP_CAINFO", "DENO_CERT", "CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE",
	"NIX_SSL_CERT_FILE",
}

// ProxyEnv builds the env list for a proxy on the given port.
func ProxyEnv(port int, caCertPath string) []string {
	u := fmt.Sprintf("http://localhost:%d", port)
	env := []string{"SANDBOX_RUNTIME=1"}
	np := "localhost,127.0.0.1,::1,169.254.0.0/16,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"
	env = append(env,
		"NO_PROXY="+np, "no_proxy="+np,
		"HTTP_PROXY="+u, "HTTPS_PROXY="+u,
		"http_proxy="+u, "https_proxy="+u,
		"ALL_PROXY="+u, "all_proxy="+u,
		"GRPC_PROXY="+u, "grpc_proxy="+u,
	)
	if caCertPath != "" {
		for _, k := range caBundleEnvKeys {
			env = append(env, k+"="+caCertPath)
		}
	}
	return env
}

// ---------- allow/deny decision ----------

func (p *Proxy) resolve(ctx context.Context, host string) []netip.Addr {
	if p.Resolve != nil {
		ips, err := p.Resolve(ctx, host)
		if err == nil {
			return ips
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if ip, ok := netip.AddrFromSlice(a); ok {
			out = append(out, ip.Unmap())
		}
	}
	return out
}

func anyInNets(ips []netip.Addr, nets []netip.Prefix) bool {
	for _, ip := range ips {
		for _, n := range nets {
			if n.Contains(ip) {
				return true
			}
		}
	}
	return false
}

func isLocalAddr(ip netip.Addr) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}

// decide applies the real SRT order (mod_0222.js): denied host rules, denied
// addresses (literal + resolved), allowed host rules, allowed addresses,
// allowAllLocal — unmatched is denied (our proxy has no interactive prompt,
// so it behaves like real's strictAllowlist).
func (p *Proxy) decide(ctx context.Context, host string, port int) (bool, string) {
	host = normalizeHost(host)
	if host == "" || len(host) > 255 || strings.Contains(host, "%") {
		return false, "malformed host"
	}
	r := p.rulesSnapshot()
	for _, d := range r.deny {
		if matchRule(host, port, d) {
			return false, "host is on the deny list"
		}
	}
	// resolve when CIDR/local checks could apply
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip.Unmap()}
	} else if len(r.allowNet)+len(r.denyNet) > 0 || r.allowAllLocal {
		ips = p.resolve(ctx, host)
	}
	if len(r.denyNet) > 0 && anyInNets(ips, r.denyNet) {
		return false, "address is on the deny list"
	}
	for _, a := range r.allow {
		if matchRule(host, port, a) {
			return true, ""
		}
	}
	if len(r.allowNet) > 0 && anyInNets(ips, r.allowNet) {
		return true, ""
	}
	if r.allowAllLocal {
		if host == "localhost" || strings.HasSuffix(host, ".localhost") {
			return true, ""
		}
		if len(ips) > 0 {
			all := true
			for _, ip := range ips {
				if !isLocalAddr(ip) {
					all = false
					break
				}
			}
			if all {
				return true, ""
			}
		}
	}
	return false, "host is not on the allow list"
}

// ---------- http server ----------

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleHTTP(w, r)
}

func denyBody(host, reason string) string {
	return fmt.Sprintf("Blocked by claude-go sandbox: %s: %s\n", host, reason)
}

func denyHTTP(w http.ResponseWriter, host, reason string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	io.WriteString(w, denyBody(host, reason))
}

// handleHTTP forwards a plain-HTTP proxy request (absolute-form URI).
func (p *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	authority := r.URL.Host
	if authority == "" {
		authority = r.Host
	}
	host, port := splitAuthority(authority, 80)
	if ok, reason := p.decide(r.Context(), host, port); !ok {
		denyHTTP(w, authority, reason)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	if out.URL.Scheme == "" {
		out.URL.Scheme = "http"
	}
	if out.URL.Host == "" {
		out.URL.Host = authority
	}
	stripHopHeaders(out.Header)
	resp, err := p.upstream().RoundTrip(out)
	if err != nil {
		http.Error(w, "Bad Gateway: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	stripHopHeaders(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// handleConnect implements CONNECT: deny → 403; passthrough → splice;
// intercept → 200, TLS with a minted cert, then per-request host checks.
func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, port := splitAuthority(r.Host, 443)
	if host == "" {
		http.Error(w, "Bad Request: missing CONNECT authority", http.StatusBadRequest)
		return
	}
	if port < 1 || port > 65535 {
		http.Error(w, "Bad Request: invalid CONNECT port", http.StatusBadRequest)
		return
	}
	if ok, reason := p.decide(r.Context(), host, port); !ok {
		denyHTTP(w, host, reason)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	if !p.Intercept || p.CA == nil {
		p.tunnel(w, hj, host, port)
		return
	}
	p.mitm(w, hj, host, port)
}

// readConn lets a hijacked connection serve its already-buffered bytes first.
type readConn struct {
	net.Conn
	r io.Reader
}

func (c *readConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// hijackSource finishes the hijack: writes the 200, returns the conn plus a
// reader that drains any buffered client bytes before live reads.
func hijackSource(hj http.Hijacker) (net.Conn, io.Reader, error) {
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	if _, err := rw.WriteString("HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, io.MultiReader(rw.Reader, conn), nil
}

// tunnel is plain CONNECT passthrough: dial upstream, splice both ways.
func (p *Proxy) tunnel(w http.ResponseWriter, hj http.Hijacker, host string, port int) {
	upstream, err := p.dial(context.Background(), "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		http.Error(w, "Bad Gateway: "+err.Error(), http.StatusBadGateway)
		return
	}
	client, src, err := hijackSource(hj)
	if err != nil {
		upstream.Close()
		return
	}
	go func() {
		io.Copy(upstream, src)
		upstream.Close()
	}()
	go func() {
		io.Copy(client, upstream)
		client.Close()
	}()
}

// mitm wraps the hijacked conn in server-side TLS presenting a leaf minted
// for the client's SNI (falling back to the CONNECT host), then serves HTTP
// inside, re-checking each request's Host header (real checks the CONNECT
// host AND the per-request host).
func (p *Proxy) mitm(w http.ResponseWriter, hj http.Hijacker, host string, port int) {
	client, src, err := hijackSource(hj)
	if err != nil {
		return
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := hello.ServerName
			if name == "" {
				name = host
			}
			return p.CA.Mint(name)
		},
	}
	tc := tls.Server(&readConn{Conn: client, r: src}, cfg)
	tc.SetDeadline(time.Now().Add(30 * time.Second))
	if err := tc.Handshake(); err != nil {
		tc.Close()
		return
	}
	tc.SetDeadline(time.Time{})
	p.serveTLSConn(tc)
}

// serveTLSConn reads inner requests off the MITM'd conn and relays them.
func (p *Proxy) serveTLSConn(tc *tls.Conn) {
	defer tc.Close()
	br := bufio.NewReader(tc)
	for {
		tc.SetReadDeadline(time.Now().Add(10 * time.Minute))
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		authority := req.URL.Host
		if authority == "" {
			authority = req.Host
		}
		host, port := splitAuthority(authority, 443)
		ok, reason := p.decide(req.Context(), host, port)
		if !ok {
			writeRawResponse(tc, http.StatusForbidden, denyBody(host, reason), req.Close)
			if req.Close {
				return
			}
			continue
		}
		req.URL.Scheme = "https"
		req.URL.Host = authority
		req.RequestURI = ""
		stripHopHeaders(req.Header)
		resp, err := p.upstream().RoundTrip(req)
		if err != nil {
			writeRawResponse(tc, http.StatusBadGateway, "Bad Gateway: "+err.Error()+"\n", req.Close)
			if req.Close {
				return
			}
			continue
		}
		err = resp.Write(tc)
		resp.Body.Close()
		if err != nil || req.Close || resp.Close {
			return
		}
	}
}

func writeRawResponse(w io.Writer, code int, body string, close bool) {
	conn := "keep-alive"
	if close {
		conn = "close"
	}
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: %s\r\n\r\n%s",
		code, http.StatusText(code), len(body), conn, body)
}

// ---------- upstream ----------

func (p *Proxy) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if p.DialContext != nil {
		return p.DialContext(ctx, network, addr)
	}
	d := &net.Dialer{Timeout: 30 * time.Second}
	return d.DialContext(ctx, network, addr)
}

// upstream returns the RoundTripper for upstream requests: the injected
// Transport if set, else a lazily-built default that uses p.dial and
// p.UpstreamTLS.
func (p *Proxy) upstream() *http.Transport {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Transport != nil {
		return p.Transport
	}
	if p.tr == nil {
		p.tr = &http.Transport{
			DialContext:           p.dial,
			TLSClientConfig:       p.UpstreamTLS,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			MaxIdleConns:          64,
			IdleConnTimeout:       90 * time.Second,
		}
	}
	return p.tr
}
