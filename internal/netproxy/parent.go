// Parent-proxy chaining — the Go port of real's g6r/me/Oyn/Hyn/uXn/h6r
// (mod_0215.js). When sandbox.network.parentProxy is set — or HTTP(S)_PROXY
// is already in the parent environment — the sandbox proxy tunnels
// non-bypassed traffic through that upstream proxy instead of connecting
// directly: CONNECT handshakes for tunnels, absolute-form requests for
// plain HTTP (Go's transport does both from the Proxy func).
package netproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ParentProxyConfig mirrors sandbox.network.parentProxy (mod_1164.js M).
// Each field falls back to the process environment when empty.
type ParentProxyConfig struct {
	HTTP    string `json:"http"`    // upstream proxy URL for plain HTTP traffic
	HTTPS   string `json:"https"`   // for HTTPS/CONNECT (falls back to http)
	NoProxy string `json:"noProxy"` // comma-separated suffixes and CIDRs
}

// ParentProxy is the resolved upstream proxy + bypass set.
type ParentProxy struct {
	HTTPURL  *url.URL
	HTTPSURL *url.URL
	noProxy  noProxySet
}

// noProxySet is real's `me` output: "*" → all; CIDRs; leading-dot and bare
// hostname suffixes.
type noProxySet struct {
	all      bool
	suffixes []string
	cidrs    []netip.Prefix
}

// resolveParentProxy is real's g6r: per-field env fallback
// (http ← HTTP_PROXY/http_proxy; https ← HTTPS_PROXY/https_proxy then http;
// noProxy ← NO_PROXY/no_proxy). Returns nil when neither URL resolves to an
// http/https proxy.
func resolveParentProxy(cfg *ParentProxyConfig) *ParentProxy {
	var httpRaw, httpsRaw, npRaw string
	if cfg != nil {
		httpRaw, httpsRaw, npRaw = cfg.HTTP, cfg.HTTPS, cfg.NoProxy
	}
	if httpRaw == "" {
		httpRaw = firstEnv("HTTP_PROXY", "http_proxy")
	}
	if httpsRaw == "" {
		httpsRaw = firstEnv("HTTPS_PROXY", "https_proxy")
	}
	if httpsRaw == "" {
		httpsRaw = httpRaw
	}
	if npRaw == "" {
		npRaw = firstEnv("NO_PROXY", "no_proxy")
	}
	if httpRaw == "" && httpsRaw == "" {
		return nil
	}
	parse := func(raw string) *url.URL {
		if raw == "" {
			return nil
		}
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			return nil
		}
		return u
	}
	pp := &ParentProxy{HTTPURL: parse(httpRaw), HTTPSURL: parse(httpsRaw)}
	if pp.HTTPURL == nil && pp.HTTPSURL == nil {
		return nil
	}
	pp.noProxy = parseNoProxy(npRaw)
	return pp
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// parseNoProxy is real's me: comma-separated list of "*", CIDRs, bracketed
// IPv6 (optional :port), "*."-prefixed suffixes (stored with the dot), and
// bare suffixes (trailing ":port" stripped).
func parseNoProxy(s string) noProxySet {
	var t noProxySet
	for _, e := range strings.Split(s, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if e == "*" {
			t.all = true
			continue
		}
		if strings.Contains(e, "/") {
			if p, err := netip.ParsePrefix(e); err == nil {
				t.cidrs = append(t.cidrs, p.Masked())
			}
			continue
		}
		n := strings.ToLower(e)
		if m := bracketedHost(n); m != "" {
			n = m
		}
		if strings.HasPrefix(n, "*.") {
			n = n[1:] // "*.x" → ".x" suffix entry
		}
		if _, err := netip.ParseAddr(strings.Split(n, "%")[0]); err != nil {
			// not an IP literal: drop a trailing :port
			if i := strings.LastIndex(n, ":"); i >= 0 {
				if _, err := strconv.Atoi(n[i+1:]); err == nil {
					n = n[:i]
				}
			}
		} else {
			// IP literal: goes on the CIDR list (host match)
			if ip, err := netip.ParseAddr(strings.Split(n, "%")[0]); err == nil {
				t.cidrs = append(t.cidrs, netip.PrefixFrom(ip.Unmap(), ip.Unmap().BitLen()))
				continue
			}
		}
		t.suffixes = append(t.suffixes, n)
	}
	return t
}

// bracketedHost strips "[v6]" or "[v6]:port" down to the address.
func bracketedHost(s string) string {
	if !strings.HasPrefix(s, "[") {
		return ""
	}
	if i := strings.IndexByte(s, ']'); i >= 0 {
		rest := s[i+1:]
		if rest == "" || strings.HasPrefix(rest, ":") {
			return s[1:i]
		}
	}
	return ""
}

// bypass is real's Oyn: localhost/loopback always direct, then the noProxy
// list (all, CIDR, ".suffix" = apex+subs, bare = exact-or-subdomain).
func (pp *ParentProxy) bypass(host string) bool {
	h := normalizeHost(host)
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	var ip netip.Addr
	if a, err := netip.ParseAddr(strings.Split(h, "%")[0]); err == nil {
		ip = a.Unmap()
		if ip.IsLoopback() {
			return true
		}
	}
	if pp.noProxy.all {
		return true
	}
	if ip.IsValid() {
		for _, c := range pp.noProxy.cidrs {
			if c.Contains(ip) {
				return true
			}
		}
	}
	for _, n := range pp.noProxy.suffixes {
		if strings.HasPrefix(n, ".") {
			if h == n[1:] || strings.HasSuffix(h, n) {
				return true
			}
		} else if h == n || strings.HasSuffix(h, "."+n) {
			return true
		}
	}
	return false
}

// urlFor is real's Hyn: https destinations ride httpsUrl (falling back to
// httpUrl); plain HTTP rides httpUrl.
func (pp *ParentProxy) urlFor(isHTTPS bool) *url.URL {
	if isHTTPS {
		if pp.HTTPSURL != nil {
			return pp.HTTPSURL
		}
		return pp.HTTPURL
	}
	return pp.HTTPURL
}

// parentAuthHeader is real's y6r: Basic auth from the URL userinfo.
func parentAuthHeader(u *url.URL) string {
	if u == nil || u.User == nil {
		return ""
	}
	user := u.User.Username()
	pass, _ := u.User.Password()
	if user == "" && pass == "" {
		return ""
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// dialParentConnect is real's uXn/h6r: dial the proxy (TLS when its scheme
// is https), send `CONNECT host:port`, require a 2xx, return the socket.
// The parent hop is dialed unverified — real skips resolved-address checks
// for parent-routed hops ("that hop resolves the name").
func (p *Proxy) dialParentConnect(ctx context.Context, u *url.URL, host string, port int) (net.Conn, error) {
	phost := u.Hostname()
	pport := u.Port()
	if pport == "" {
		if u.Scheme == "https" {
			pport = "443"
		} else {
			pport = "80"
		}
	}
	conn, err := p.dial(ctx, "tcp", net.JoinHostPort(phost, pport))
	if err != nil {
		return nil, err
	}
	if u.Scheme == "https" {
		cfg := &tls.Config{}
		if p.UpstreamTLS != nil {
			cfg = p.UpstreamTLS.Clone()
		}
		cfg.ServerName = phost
		cfg.NextProtos = nil // parent CONNECT hops are always http/1.1
		tc := tls.Client(conn, cfg)
		tc.SetDeadline(time.Now().Add(15 * time.Second))
		if err := tc.Handshake(); err != nil {
			conn.Close()
			return nil, fmt.Errorf("parent proxy TLS handshake: %w", err)
		}
		tc.SetDeadline(time.Time{})
		conn = tc
	}
	target := net.JoinHostPort(host, strconv.Itoa(port))
	var b strings.Builder
	fmt.Fprintf(&b, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if auth := parentAuthHeader(u); auth != "" {
		b.WriteString("Proxy-Authorization: " + auth + "\r\n")
	}
	b.WriteString("\r\n")
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := conn.Write([]byte(b.String())); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("parent proxy CONNECT: %w", err)
	}
	// a 2xx CONNECT response has no framed body — the remainder IS the
	// tunnel; don't drain it (that would block until the deadline).
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		conn.Close()
		return nil, fmt.Errorf("proxy refused CONNECT: %s", resp.Status)
	}
	conn.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		// bytes already read past the response headers are tunnel payload
		return &readConn{Conn: conn, r: io.MultiReader(br, conn)}, nil
	}
	return conn, nil
}

// dialTarget picks the upstream dial path for a CONNECT/SOCKS destination:
// through the parent proxy when configured and not bypassed, else a direct
// post-dial-verified dial. Used by tunnel(), probeUpstreamTLS() and SOCKS.
func (p *Proxy) dialTarget(ctx context.Context, host string, port int) (net.Conn, error) {
	r := p.rulesSnapshot()
	if r.parent != nil && !r.parent.bypass(host) {
		if u := r.parent.urlFor(true); u != nil {
			return p.dialParentConnect(ctx, u, host, port)
		}
	}
	return p.dialChecked(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
}

// transportProxyFunc is the http.Transport.Proxy hook: send the request to
// the parent proxy unless the destination bypasses it. Go's transport then
// emits absolute-form for http:// requests and CONNECT+TLS for https:// —
// matching real's J/uXn paths, including Proxy-Authorization from URL
// userinfo.
func (p *Proxy) transportProxyFunc() func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		r := p.rulesSnapshot()
		if r.parent == nil {
			return nil, nil
		}
		host := req.URL.Hostname()
		if r.parent.bypass(host) {
			return nil, nil
		}
		return r.parent.urlFor(req.URL.Scheme == "https"), nil
	}
}

// isParentDial reports whether addr is the parent proxy's own endpoint —
// post-dial CIDR re-verification does not apply to the parent hop (real's
// resolved-address rules are skipped for parent-routed connections).
func (p *Proxy) isParentDial(addr string) bool {
	r := p.rulesSnapshot()
	if r.parent == nil {
		return false
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	for _, u := range []*url.URL{r.parent.HTTPURL, r.parent.HTTPSURL} {
		if u == nil {
			continue
		}
		pp := u.Port()
		if pp == "" {
			if u.Scheme == "https" {
				pp = "443"
			} else {
				pp = "80"
			}
		}
		if strings.EqualFold(host, u.Hostname()) && port == pp {
			return true
		}
	}
	return false
}
