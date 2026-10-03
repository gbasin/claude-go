package netproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"runtime"
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

// Proxy is the session egress proxy: an HTTP proxy plus a SOCKS5 listener,
// each on an ephemeral loopback port (real runs both). Plain requests are
// forwarded after a host check; CONNECT tunnels are either MITM'd
// (Intercept + CA) or spliced through blindly.
type Proxy struct {
	CA *CA
	// Intercept controls CONNECT MITM. Defaults to true when a CA is present.
	// With interception off the proxy is still an allowlist gate on the
	// CONNECT authority, but cannot see per-request Host headers inside TLS.
	Intercept bool

	// AuthToken is the per-session proxy password (real's proxyAuthToken).
	// Generated in New; when empty the listeners skip auth. Env() embeds it
	// in the proxy URLs and every HTTP request/CONNECT must carry a matching
	// Proxy-Authorization: Basic credential (SOCKS5 uses RFC1929 user/pass).
	AuthToken string

	// promptAllow, when non-nil, upgrades a "not on the allow list" verdict
	// into an interactive decision: return true to add the host to the
	// session allowlist and let the connection through. Wired by the Bash
	// tool via SetPromptAllow when sandbox.network.interactiveHostApproval
	// is set and an AskStructured prompt is available.
	promptAllowFn func(host string, port int) bool

	// Logf receives deny/audit lines. When nil, lines go to stderr only
	// under SRT_DEBUG=1 (same gate as real's bt()).
	Logf func(format string, args ...any)

	// Injectable hooks (tests; production uses real dialing):
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	Resolve     func(ctx context.Context, host string) ([]netip.Addr, error)
	// UpstreamTLS is the base tls.Config for upstream connections made during
	// MITM/plain forwarding (ServerName is filled per request). Tests use it
	// to trust httptest certs.
	UpstreamTLS *tls.Config
	// Transport overrides the upstream RoundTripper entirely when non-nil.
	Transport *http.Transport

	mu           sync.RWMutex
	rules        compiledRules
	sessionAllow map[string]bool // hosts approved interactively / via /sandbox allow
	promptMu     sync.Mutex      // serializes interactive prompts

	ln      net.Listener
	socksLn net.Listener
	srv     *http.Server
	tr      *http.Transport
}

// New builds a proxy. ca may be nil: the proxy then runs in passthrough mode
// (CONNECT filtering only). A fresh session AuthToken is minted — like real,
// which always configures proxyAuthToken.
func New(ca *CA) *Proxy {
	p := &Proxy{
		CA:           ca,
		Intercept:    ca != nil,
		AuthToken:    NewAuthToken(),
		sessionAllow: map[string]bool{},
	}
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

// Start binds the HTTP and SOCKS5 listeners on 127.0.0.1:0 and serves both
// in the background (real's SRT proxy also runs the pair).
func (p *Proxy) Start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	p.ln = ln
	sln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		ln.Close()
		return err
	}
	p.socksLn = sln
	go p.srv.Serve(ln)
	go p.serveSocks(sln)
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

// SocksPort is the SOCKS5 listener's port (0 before Start).
func (p *Proxy) SocksPort() int {
	if p.socksLn == nil {
		return 0
	}
	return p.socksLn.Addr().(*net.TCPAddr).Port
}

// Close shuts both listeners down.
func (p *Proxy) Close() error {
	if p.socksLn != nil {
		p.socksLn.Close()
	}
	if p.srv == nil {
		return nil
	}
	return p.srv.Close()
}

// AllowHost adds host to the session allowlist — the backing store of
// `/sandbox allow <host>` and interactive host approval. Host entries grant
// every port, like an unqualified allowedDomains entry.
func (p *Proxy) AllowHost(host string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessionAllow[normalizeHost(host)] = true
}

// logf writes a debug/audit line — real's bt(): stderr only under
// SRT_DEBUG=1 unless a Logf sink is installed.
func (p *Proxy) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
		return
	}
	if os.Getenv("SRT_DEBUG") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "[SandboxDebug] "+format+"\n", args...)
}

// Env returns the sandboxed-command environment — real's CBt() wiring:
// SANDBOX_RUNTIME, HTTP(S)_PROXY (both cases), ALL_PROXY/GRPC_PROXY pointing
// at this proxy, NO_PROXY covering local ranges, every CA-bundle env var a
// common toolchain honors pointed at the CA cert, plus the SOCKS-side
// variables (GIT_SSH_COMMAND, FTP_PROXY, RSYNC_PROXY, DOCKER_*_PROXY,
// CLOUDSDK_*). command becomes the base64 tag inside the auth username so
// egress can be attributed to the command that made it.
func (p *Proxy) Env(command string) []string {
	caPath := ""
	if p.CA != nil {
		caPath = p.CA.CertPath
	}
	return ProxyEnv(p.Port(), p.SocksPort(), caPath, p.AuthToken, command)
}

// caBundleEnvKeys is real's `oe` list — every CA-file env var exported into
// sandboxed commands so toolchains trust the minted leaf certificates.
var caBundleEnvKeys = []string{
	"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "CURL_CA_BUNDLE",
	"REQUESTS_CA_BUNDLE", "PIP_CERT", "GIT_SSL_CAINFO", "AWS_CA_BUNDLE",
	"CARGO_HTTP_CAINFO", "DENO_CERT", "CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE",
	"NIX_SSL_CERT_FILE",
}

// ProxyEnv builds the env list — a faithful port of real's CBt(e,t,r,n,o,i):
// e=httpPort, t=socksPort, r=caCertPath, n=authToken, i=command (tagged via
// Soe). Real's TMPDIR push (o) is skipped: our seatbelt profile always
// grants /tmp and /var/folders, so no redirect is needed.
func ProxyEnv(httpPort, socksPort int, caCertPath, authToken, command string) []string {
	user := authUser(commandTag(command))
	// userinfo prefix "<user>:<token>@", present only when auth is on; the
	// username is percent-encoded, the token goes in raw (base64url-safe).
	auth := ""
	if authToken != "" {
		auth = encodeURIComponent(user) + ":" + authToken + "@"
	}
	env := []string{"SANDBOX_RUNTIME=1"}
	if caCertPath != "" {
		for _, k := range caBundleEnvKeys {
			env = append(env, k+"="+caCertPath)
		}
	}
	if httpPort == 0 && socksPort == 0 {
		return env // real returns early: no proxy vars at all
	}
	np := "localhost,127.0.0.1,::1,169.254.0.0/16,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"
	env = append(env, "NO_PROXY="+np, "no_proxy="+np)
	if httpPort > 0 {
		u := fmt.Sprintf("http://%slocalhost:%d", auth, httpPort)
		env = append(env,
			"HTTP_PROXY="+u, "HTTPS_PROXY="+u,
			"http_proxy="+u, "https_proxy="+u)
		if authToken != "" {
			// real adds the literal single quotes — git config values
			// are parsed, so the quotes are part of the syntax.
			env = append(env, "GIT_CONFIG_PARAMETERS='http.proxyAuthMethod=basic'")
		}
	}
	all := ""
	if httpPort > 0 {
		all = fmt.Sprintf("http://%slocalhost:%d", auth, httpPort)
	} else if socksPort > 0 {
		all = fmt.Sprintf("socks5h://%slocalhost:%d", auth, socksPort)
	}
	if all != "" {
		env = append(env,
			"ALL_PROXY="+all, "all_proxy="+all,
			"GRPC_PROXY="+all, "grpc_proxy="+all)
	}
	if socksPort > 0 {
		switch runtime.GOOS {
		case "darwin":
			env = append(env, fmt.Sprintf(
				"GIT_SSH_COMMAND=ssh -o ControlMaster=no -o ControlPath=none -o ProxyCommand='nc -X 5 -x localhost:%d %%h %%p'",
				socksPort))
		case "linux":
			if httpPort > 0 {
				pa := ""
				if authToken != "" {
					pa = fmt.Sprintf(",proxyauth=%s:%s", user, authToken)
				}
				env = append(env, fmt.Sprintf(
					"GIT_SSH_COMMAND=ssh -o ControlMaster=no -o ControlPath=none -o ProxyCommand='socat - PROXY:localhost:%%h:%%p,proxyport=%d%s'",
					httpPort, pa))
			}
		}
		env = append(env,
			fmt.Sprintf("FTP_PROXY=socks5h://%slocalhost:%d", auth, socksPort),
			fmt.Sprintf("ftp_proxy=socks5h://%slocalhost:%d", auth, socksPort),
			fmt.Sprintf("RSYNC_PROXY=localhost:%d", socksPort),
		)
		dockerPort := httpPort
		if dockerPort == 0 {
			dockerPort = socksPort
		}
		env = append(env,
			fmt.Sprintf("DOCKER_HTTP_PROXY=http://%slocalhost:%d", auth, dockerPort),
			fmt.Sprintf("DOCKER_HTTPS_PROXY=http://%slocalhost:%d", auth, dockerPort),
		)
		if httpPort > 0 {
			env = append(env,
				"CLOUDSDK_PROXY_TYPE=http",
				"CLOUDSDK_PROXY_ADDRESS=localhost",
				fmt.Sprintf("CLOUDSDK_PROXY_PORT=%d", httpPort),
			)
			if authToken != "" {
				env = append(env,
					"CLOUDSDK_PROXY_USERNAME="+user,
					"CLOUDSDK_PROXY_PASSWORD="+authToken,
				)
			}
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

// reasonNotAllowed is the unmatched-host verdict — the only deny class the
// interactive approval path may upgrade (real only asks for hosts that are
// absent from the allowlist, never for explicit denies).
const reasonNotAllowed = "host is not on the allow list"

// decide applies the real SRT order (mod_0222.js): denied host rules, denied
// addresses (literal + resolved), allowed host rules (+ the session
// allowlist), allowed addresses, allowAllLocal — unmatched is denied.
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
	p.mu.RLock()
	sessionOK := p.sessionAllow[host]
	p.mu.RUnlock()
	if sessionOK {
		return true, ""
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
	return false, reasonNotAllowed
}

// SetPromptAllow installs (or clears, nil) the interactive-approval
// callback — safe to call while the proxy serves.
func (p *Proxy) SetPromptAllow(f func(host string, port int) bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.promptAllowFn = f
}

// gate is the verdict used by every listener: decide, then — for the
// unmatched-host deny class only — the interactive-approval upgrade
// (real's non-strictAllowlist ask callback), then an audit line on denies
// carrying the auth tag so egress is attributable to a command.
func (p *Proxy) gate(ctx context.Context, host string, port int, tag string) (bool, string) {
	ok, reason := p.decide(ctx, host, port)
	p.mu.RLock()
	prompt := p.promptAllowFn
	p.mu.RUnlock()
	if !ok && reason == reasonNotAllowed && prompt != nil {
		if p.promptAllow(normalizeHost(host), port, prompt) {
			return true, ""
		}
	}
	if !ok {
		if cmd := decodeTagCommand(tag); cmd != "" {
			p.logf("denied %s:%d: %s (command: %q)", host, port, reason, cmd)
		} else {
			p.logf("denied %s:%d: %s", host, port, reason)
		}
	}
	return ok, reason
}

// promptAllow serializes interactive prompts and re-checks the verdict
// inside the lock — a concurrent prompt may already have allowed the host.
func (p *Proxy) promptAllow(host string, port int, prompt func(string, int) bool) bool {
	p.promptMu.Lock()
	defer p.promptMu.Unlock()
	if ok, _ := p.decide(context.Background(), host, port); ok {
		return true
	}
	if !prompt(host, port) {
		return false
	}
	p.AllowHost(host)
	return true
}

// ---------- http server ----------

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ok, tag := p.checkAuth(r)
	if !ok {
		// real's 407: Proxy-Authenticate: Basic realm="srt"
		w.Header().Set("Proxy-Authenticate", `Basic realm="srt"`)
		w.WriteHeader(http.StatusProxyAuthRequired)
		return
	}
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r, tag)
		return
	}
	p.handleHTTP(w, r, tag)
}

func denyBody(host, reason string) string {
	return fmt.Sprintf("Blocked by claude-go sandbox: %s: %s\n", host, reason)
}

func denyHTTP(w http.ResponseWriter, host, reason string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// real stamps every policy deny with this header
	w.Header().Set("X-Proxy-Error", "blocked-by-allowlist")
	w.WriteHeader(http.StatusForbidden)
	io.WriteString(w, denyBody(host, reason))
}

// handleHTTP forwards a plain-HTTP proxy request (absolute-form URI).
func (p *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request, tag string) {
	authority := r.URL.Host
	if authority == "" {
		authority = r.Host
	}
	host, port := splitAuthority(authority, 80)
	if ok, reason := p.gate(r.Context(), host, port, tag); !ok {
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
		var pe *policyError
		if errors.As(err, &pe) {
			denyHTTP(w, host, pe.reason)
			return
		}
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
func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request, tag string) {
	host, port := splitAuthority(r.Host, 443)
	if host == "" {
		http.Error(w, "Bad Request: missing CONNECT authority", http.StatusBadRequest)
		return
	}
	if port < 1 || port > 65535 {
		http.Error(w, "Bad Request: invalid CONNECT port", http.StatusBadRequest)
		return
	}
	if ok, reason := p.gate(r.Context(), host, port, tag); !ok {
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
	p.mitm(w, hj, host, port, tag)
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
	upstream, err := p.dialChecked(context.Background(), "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		var pe *policyError
		if errors.As(err, &pe) {
			denyHTTP(w, host, pe.reason)
			return
		}
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
//
// ALPN: we probe the upstream first. When it speaks h2 we offer
// ["h2","http/1.1"] to the client and, if the client picks h2, splice the
// decrypted byte streams verbatim — real doesn't decode h2 frames either,
// so inspection only happens on the http/1.1 path. An upstream without h2
// gets a plain http/1.1 offer end-to-end.
func (p *Proxy) mitm(w http.ResponseWriter, hj http.Hijacker, host string, port int, tag string) {
	up, upH2, perr := p.probeUpstreamTLS(host, port)
	if perr != nil {
		// post-dial verification refused the upstream before we hijack —
		// the CONNECT can still get a proper deny status.
		var pe *policyError
		if errors.As(perr, &pe) {
			denyHTTP(w, host, pe.reason)
			return
		}
	}
	client, src, err := hijackSource(hj)
	if err != nil {
		if up != nil {
			up.Close()
		}
		return
	}
	nextProtos := []string{"http/1.1"}
	if upH2 {
		nextProtos = []string{"h2", "http/1.1"}
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: nextProtos,
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
		if up != nil {
			up.Close()
		}
		return
	}
	tc.SetDeadline(time.Time{})
	if tc.ConnectionState().NegotiatedProtocol == "h2" && up != nil {
		// h2 passthrough: decrypted client frames are re-encrypted to the
		// upstream (and back) without interpretation.
		p.splice(tc, up)
		return
	}
	if up != nil {
		up.Close()
	}
	p.serveTLSConn(tc, tag)
}

// splice relays bytes both ways and closes both ends when either side
// finishes.
func (p *Proxy) splice(a, b net.Conn) {
	go func() {
		io.Copy(a, b)
		a.Close()
		b.Close()
	}()
	go func() {
		io.Copy(b, a)
		a.Close()
		b.Close()
	}()
}

// probeUpstreamTLS dials the CONNECT target and runs a TLS handshake
// advertising ["h2","http/1.1"] to learn the upstream's ALPN capability.
// The dialed conn (post-dial verified) is reused as the h2 splice target
// when it negotiates h2. A nil conn + nil error means the probe failed
// non-fatally — the MITM then offers http/1.1 only and upstream errors
// surface per-request as before.
func (p *Proxy) probeUpstreamTLS(host string, port int) (*tls.Conn, bool, error) {
	raw, err := p.dialChecked(context.Background(), "tcp",
		net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, false, err
	}
	cfg := &tls.Config{}
	if p.UpstreamTLS != nil {
		cfg = p.UpstreamTLS.Clone()
	}
	// crypto/tls omits IP literals from the SNI extension itself but still
	// uses ServerName for IP-SAN verification, so this is safe for both.
	cfg.ServerName = host
	cfg.NextProtos = []string{"h2", "http/1.1"}
	tc := tls.Client(raw, cfg)
	tc.SetDeadline(time.Now().Add(15 * time.Second))
	err = tc.Handshake()
	tc.SetDeadline(time.Time{})
	if err != nil {
		raw.Close()
		return nil, false, nil
	}
	return tc, tc.ConnectionState().NegotiatedProtocol == "h2", nil
}

// serveTLSConn reads inner requests off the MITM'd conn and relays them.
func (p *Proxy) serveTLSConn(tc *tls.Conn, tag string) {
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
		ok, reason := p.gate(req.Context(), host, port, tag)
		if !ok {
			// inner-tunnel denies carry real's other X-Proxy-Error value
			writeRawResponse(tc, http.StatusForbidden, denyBody(host, reason),
				"blocked-by-sandbox-runtime", req.Close)
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
			var pe *policyError
			if errors.As(err, &pe) {
				writeRawResponse(tc, http.StatusForbidden, denyBody(host, pe.reason),
					"blocked-by-sandbox-runtime", req.Close)
			} else {
				writeRawResponse(tc, http.StatusBadGateway, "Bad Gateway: "+err.Error()+"\n",
					"", req.Close)
			}
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

func writeRawResponse(w io.Writer, code int, body, proxyErr string, close bool) {
	conn := "keep-alive"
	if close {
		conn = "close"
	}
	xerr := ""
	if proxyErr != "" {
		xerr = "X-Proxy-Error: " + proxyErr + "\r\n"
	}
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\n%sContent-Length: %d\r\nConnection: %s\r\n\r\n%s",
		code, http.StatusText(code), xerr, len(body), conn, body)
}

// ---------- upstream ----------

// policyError is a post-dial policy refusal (real's N0): the connected
// socket's remote IP fell under the address rules. Handlers map it to
// 403 / CONNECTION_NOT_ALLOWED instead of a generic 502.
type policyError struct {
	reason string
}

func (e *policyError) Error() string { return e.reason }

func (p *Proxy) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if p.DialContext != nil {
		return p.DialContext(ctx, network, addr)
	}
	d := &net.Dialer{Timeout: 30 * time.Second}
	return d.DialContext(ctx, network, addr)
}

// dialChecked dials, then re-verifies the connected remote IP against the
// CIDR/local address rules — real closes the same DNS-rebinding window by
// filtering resolved addresses before dial; the post-dial check covers the
// TOCTOU gap between decide()'s resolution and the dialer's own.
func (p *Proxy) dialChecked(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := p.dial(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	if err := p.verifyRemote(c); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// verifyRemote applies the address rules to a connected socket's remote IP:
// deny CIDRs deny; an allow CIDR match passes; with allowAllLocal the remote
// must be loopback/private/link-local. Non-IP remote addresses can't be
// verified and pass.
func (p *Proxy) verifyRemote(c net.Conn) error {
	var ip netip.Addr
	if ta, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		ip, _ = netip.AddrFromSlice(ta.IP)
	} else if h, _, err := net.SplitHostPort(c.RemoteAddr().String()); err == nil {
		ip, _ = netip.ParseAddr(h)
	}
	if !ip.IsValid() {
		return nil
	}
	ip = ip.Unmap()
	r := p.rulesSnapshot()
	if len(r.denyNet) > 0 && anyInNets([]netip.Addr{ip}, r.denyNet) {
		return &policyError{"upstream address is on the deny list"}
	}
	if len(r.allowNet) > 0 && anyInNets([]netip.Addr{ip}, r.allowNet) {
		return nil
	}
	if r.allowAllLocal && !isLocalAddr(ip) {
		return &policyError{"upstream address is not a local address"}
	}
	return nil
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
			DialContext:           p.dialChecked,
			TLSClientConfig:       p.UpstreamTLS,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			MaxIdleConns:          64,
			IdleConnTimeout:       90 * time.Second,
		}
	}
	return p.tr
}
