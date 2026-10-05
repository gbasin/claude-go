package api

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// rawHTTP11 sends requests over a raw HTTP/1.1 conn with real's exact
// header order (captured from Bun/undici in claude 2.1.289):
//
//	Accept, Authorization, Content-Type, User-Agent,
//	X-Claude-Code-Session-Id, X-Stainless-Arch, X-Stainless-Lang,
//	X-Stainless-OS, X-Stainless-Package-Version, X-Stainless-Retry-Count,
//	X-Stainless-Runtime, X-Stainless-Runtime-Version, X-Stainless-Timeout,
//	anthropic-beta, anthropic-dangerous-direct-browser-access,
//	anthropic-version, x-app, Connection, Host, Accept-Encoding,
//	Content-Length
//
// node fetch appends the transport fields (Connection, Host,
// Accept-Encoding, Content-Length) after the caller's headers — we emit
// them in the same position.
var rawHeaderOrder = []string{
	"Accept", "Authorization", "Content-Type", "User-Agent",
	"X-Claude-Code-Session-Id", "X-Stainless-Arch", "X-Stainless-Lang",
	"X-Stainless-OS", "X-Stainless-Package-Version",
	"X-Stainless-Retry-Count", "X-Stainless-Runtime",
	"X-Stainless-Runtime-Version", "X-Stainless-Timeout",
	"anthropic-beta", "anthropic-dangerous-direct-browser-access",
	"anthropic-version", "x-app",
}

// rawConn is a keep-alive HTTP/1.1 connection over utls.
type rawConn struct {
	conn   net.Conn
	br     *bufio.Reader
	closed bool
}

var rawPool = struct {
	mu    sync.Mutex
	conns map[string]*rawConn // host → idle conn
}{conns: map[string]*rawConn{}}

var rawRootsOnce = struct {
	once sync.Once
	pool *x509.CertPool
	err  error
}{}

func rawRoots() (*x509.CertPool, error) {
	rawRootsOnce.once.Do(func() {
		p, err := x509.SystemCertPool()
		if p == nil {
			p = x509.NewCertPool()
		}
		// real honors NODE_EXTRA_CA_CERTS (Bun/node root store)
		for _, env := range []string{"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE"} {
			if pem, e := os.ReadFile(os.Getenv(env)); e == nil {
				p.AppendCertsFromPEM(pem)
			}
		}
		rawRootsOnce.pool, rawRootsOnce.err = p, err
	})
	return rawRootsOnce.pool, rawRootsOnce.err
}

// rawHTTP11SendRoundTripper implements http.RoundTripper over utls + raw
// HTTP/1.1 serialization. Used for first-party Anthropic requests so the
// wire shape matches real's Bun stack byte-for-byte.
type rawHTTP11RoundTripper struct{}

func (rawHTTP11RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	if !strings.Contains(host, ":") {
		host += ":443"
	}
	serverName := req.URL.Hostname()

	rc, err := getRawConn(req.Context(), host, serverName)
	if err != nil {
		return nil, err
	}
	if err := writeRawRequest(rc, req); err != nil {
		putRawConn(host, rc, true)
		return nil, err
	}
	resp, err := readRawResponse(rc, req)
	if err != nil {
		putRawConn(host, rc, true)
		return nil, err
	}
	if req.Close {
		// axios-lane aux requests (Connection: close) never re-enter the
		// keep-alive pool — the server closes its side after the body.
		rc.closed = true
	}
	if os.Getenv("CLAUDE_GO_DEBUG_HTTP") != "" {
		fmt.Fprintf(os.Stderr, "[debug] %s %s -> %d\n", req.Method, req.URL.String(), resp.StatusCode)
	}
	// return conn to pool when the body is fully consumed or closed;
	// a cancelled request ctx must also kill the conn or Read blocks
	// forever (esc-interrupt during a long stream relies on this)
	body := &rawBody{rc: rc, host: host, r: resp.Body}
	if done := req.Context().Done(); done != nil {
		stop := context.AfterFunc(req.Context(), func() {
			rc.closed = true
			rc.conn.Close()
		})
		body.stopCancel = func() { stop() }
	}
	resp.Body = body
	return resp, nil
}

// rawProxyURL mirrors node fetch's proxy env handling: HTTPS_PROXY /
// ALL_PROXY honored, NO_PROXY CIDR/domain list bypasses. Real's proxy
// chain is undici EnvHttpProxyAgent — same env vars.
func rawProxyURL(serverName string) *url.URL {
	host := strings.ToLower(serverName)
	if noProxy := os.Getenv("NO_PROXY"); noProxy != "" {
		if noProxy == "*" {
			return nil
		}
		for _, e := range strings.Split(noProxy, ",") {
			e = strings.ToLower(strings.TrimSpace(e))
			if e == "" {
				continue
			}
			e = strings.TrimPrefix(e, "*.")
			if host == e || strings.HasSuffix(host, "."+e) {
				return nil
			}
		}
	}
	for _, env := range []string{"HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		if v := os.Getenv(env); v != "" {
			if u, err := url.Parse(v); err == nil {
				return u
			}
		}
	}
	return nil
}

func getRawConn(ctx context.Context, host, serverName string) (*rawConn, error) {
	rawPool.mu.Lock()
	if rc := rawPool.conns[host]; rc != nil && !rc.closed {
		delete(rawPool.conns, host)
		rawPool.mu.Unlock()
		// stale-conn probe: nonblocking read
		rc.conn.SetReadDeadline(time.Now())
		var b [1]byte
		_, err := rc.br.Read(b[:])
		rc.conn.SetReadDeadline(time.Time{})
		if err == nil || !isTimeoutErr(err) {
			rc.conn.Close()
		} else {
			return rc, nil
		}
	}
	rawPool.mu.Unlock()

	roots, err := rawRoots()
	if err != nil {
		return nil, err
	}
	conn, err := rawDial(ctx, host, serverName, roots)
	if err != nil {
		return nil, err
	}
	return &rawConn{conn: conn, br: bufio.NewReaderSize(conn, 64*1024)}, nil
}

func putRawConn(host string, rc *rawConn, discard bool) {
	if discard || rc.closed {
		rc.closed = true
		rc.conn.Close()
		return
	}
	rawPool.mu.Lock()
	rawPool.conns[host] = rc
	rawPool.mu.Unlock()
}

func isTimeoutErr(err error) bool {
	if ne, ok := err.(net.Error); ok {
		return ne.Timeout()
	}
	return false
}

// writeRawRequest serializes the request in real's exact order: caller
// headers in rawHeaderOrder, then Connection, Host, Accept-Encoding,
// Content-Length (node fetch's trailer block).
func writeRawRequest(rc *rawConn, req *http.Request) error {
	var b strings.Builder
	path := req.URL.RequestURI()
	if path == "" {
		path = "/"
	}
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\n", req.Method, path)

	hasBody := req.Body != nil && req.Body != http.NoBody
	seen := map[string]bool{}
	for _, name := range rawHeaderOrder {
		vals := req.Header.Values(name)
		if len(vals) == 0 {
			// defaults real sets: Accept, Content-Type. Aux-lane GETs
			// (axios) send no Content-Type on bodyless requests.
			switch name {
			case "Accept":
				vals = []string{"application/json"}
			case "Content-Type":
				if hasBody || req.Method == "POST" {
					vals = []string{"application/json"}
				}
			}
		}
		for _, v := range vals {
			seen[http.CanonicalHeaderKey(name)] = true
			fmt.Fprintf(&b, "%s: %s\r\n", name, v)
		}
	}
	// any remaining caller headers (x-client-request-id on retries etc.)
	var extra []string
	for k := range req.Header {
		if !seen[http.CanonicalHeaderKey(k)] && !hopHeader(k) {
			extra = append(extra, k)
		}
	}
	sortStrings(extra)
	for _, k := range extra {
		for _, v := range req.Header.Values(k) {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}

	// node fetch trailer block. Aux-lane requests (req.Close) ride axios,
	// whose block differs: Accept-Encoding advertises compress, the conn
	// is Connection: close, and field order is Accept-Encoding/Host/
	// Connection (captured 2.1.289).
	if req.Close {
		b.WriteString("Accept-Encoding: gzip, compress, deflate, br\r\n")
		fmt.Fprintf(&b, "Host: %s\r\n", req.URL.Host)
		b.WriteString("Connection: close\r\n")
	} else {
		b.WriteString("Connection: keep-alive\r\n")
		fmt.Fprintf(&b, "Host: %s\r\n", req.URL.Host)
		b.WriteString("Accept-Encoding: gzip, deflate, br, zstd\r\n")
	}
	var body []byte
	if hasBody {
		var err error
		body, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return err
		}
		req.Body = io.NopCloser(strings.NewReader(string(body)))
	}
	// axios GETs carry no Content-Length; POSTs always do.
	if hasBody || req.Method == "POST" {
		fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
	}
	b.WriteString("\r\n")
	if _, err := rc.conn.Write([]byte(b.String())); err != nil {
		return err
	}
	if len(body) > 0 {
		if _, err := rc.conn.Write(body); err != nil {
			return err
		}
	}
	return nil
}

func hopHeader(k string) bool {
	switch http.CanonicalHeaderKey(k) {
	case "Host", "Connection", "Content-Length", "Accept-Encoding", "Keep-Alive",
		"Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer",
		"Transfer-Encoding", "Upgrade":
		return true
	}
	return false
}

// rawBody releases the conn to the keep-alive pool at EOF/Close.
type rawBody struct {
	rc         *rawConn
	host       string
	r          io.ReadCloser
	done       bool
	stopCancel func() // detaches the ctx-kill watcher on clean close
}

func (b *rawBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == io.EOF && !b.done {
		b.done = true
		if b.stopCancel != nil {
			b.stopCancel() // pooled conn must outlive the request ctx
		}
		putRawConn(b.host, b.rc, false)
	}
	return n, err
}
func (b *rawBody) Close() error {
	if b.stopCancel != nil {
		b.stopCancel()
	}
	if !b.done {
		b.done = true
		putRawConn(b.host, b.rc, true) // body not drained — conn unusable
	}
	return b.r.Close()
}

// readRawResponse parses status line + headers + body (identity or
// chunked), with Content-Encoding decoding for gzip/deflate/br/zstd.
func readRawResponse(rc *rawConn, req *http.Request) (*http.Response, error) {
	rc.conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	defer rc.conn.SetReadDeadline(time.Time{})
	statusLine, err := rc.br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(strings.TrimSpace(statusLine), " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/") {
		return nil, fmt.Errorf("malformed status line: %q", statusLine)
	}
	code, _ := strconv.Atoi(parts[1])
	reason := ""
	if len(parts) == 3 {
		reason = parts[2]
	}
	hdr := http.Header{}
	for {
		line, err := rc.br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if i := strings.Index(line, ":"); i > 0 {
			hdr.Add(line[:i], strings.TrimSpace(line[i+1:]))
		}
	}
	var body io.ReadCloser
	switch {
	case strings.Contains(strings.ToLower(hdr.Get("Transfer-Encoding")), "chunked"):
		body = io.NopCloser(&chunkReader{br: rc.br})
	case hdr.Get("Content-Length") != "":
		n, _ := strconv.ParseInt(hdr.Get("Content-Length"), 10, 64)
		body = io.NopCloser(io.LimitReader(rc.br, n))
	default:
		body = io.NopCloser(rc.br) // read until close
		rc.closed = true
	}
	// content-encoding decode (we advertise the full set like real)
	switch strings.ToLower(hdr.Get("Content-Encoding")) {
	case "gzip":
		zr, err := gzip.NewReader(body)
		if err != nil {
			return nil, err
		}
		body = zr
	case "deflate":
		body = struct{ io.ReadCloser }{flate.NewReader(body)}
	case "br":
		body = io.NopCloser(brotli.NewReader(body))
	case "zstd":
		zr, err := zstd.NewReader(body)
		if err != nil {
			return nil, err
		}
		body = &zstdReadCloser{zr}
	}
	resp := &http.Response{
		Status:     parts[0] + " " + parts[1] + " " + reason,
		StatusCode: code,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1, ProtoMinor: 1,
		Header:  hdr,
		Body:    body,
		Request: req,
	}
	if cl, err := strconv.ParseInt(hdr.Get("Content-Length"), 10, 64); err == nil {
		resp.ContentLength = cl
	}
	return resp, nil
}

// chunkReader decodes HTTP/1.1 chunked transfer encoding.
type chunkReader struct {
	br   *bufio.Reader
	left int64
	done bool
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.done {
		return 0, io.EOF
	}
	if c.left == 0 {
		line, err := c.br.ReadString('\n')
		if err != nil {
			return 0, err
		}
		line = strings.TrimSpace(line)
		if i := strings.Index(line, ";"); i >= 0 {
			line = line[:i]
		}
		n, err := strconv.ParseInt(line, 16, 64)
		if err != nil {
			return 0, fmt.Errorf("bad chunk size %q", line)
		}
		if n == 0 {
			// trailers
			for {
				l, err := c.br.ReadString('\n')
				if err != nil {
					return 0, err
				}
				if l == "\r\n" || l == "\n" {
					break
				}
			}
			c.done = true
			return 0, io.EOF
		}
		c.left = n
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.br.Read(p)
	c.left -= int64(n)
	if c.left == 0 {
		c.br.ReadString('\n') // trailing CRLF
	}
	return n, err
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

type zstdReadCloser struct{ z *zstd.Decoder }

func (z *zstdReadCloser) Read(p []byte) (int, error) { return z.z.Read(p) }
func (z *zstdReadCloser) Close() error {
	z.z.Close()
	return nil
}

// rawDial dials the target directly or tunnels through the configured
// proxy (CONNECT for http(s) proxies, SOCKS5 for socks://), then runs the
// utls handshake end-to-end inside the tunnel.
func rawDial(ctx context.Context, host, serverName string, roots *x509.CertPool) (net.Conn, error) {
	px := rawProxyURL(serverName)
	if px == nil {
		return claudeTLSDial(ctx, "tcp", host, serverName, roots)
	}
	paddr := px.Host
	if !strings.Contains(paddr, ":") {
		paddr += ":8080"
	}
	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", paddr)
	if err != nil {
		return nil, err
	}
	switch px.Scheme {
	case "http", "https":
		conn := raw
		if px.Scheme == "https" {
			tc, err := claudeTLSHandshake(ctx, conn, px.Hostname(), roots)
			if err != nil {
				raw.Close()
				return nil, err
			}
			conn = tc
		}
		auth := ""
		if px.User != nil {
			pw, _ := px.User.Password()
			auth = "Basic " + base64.StdEncoding.EncodeToString(
				[]byte(px.User.Username()+":"+pw))
		}
		req := "CONNECT " + host + " HTTP/1.1\r\nHost: " + host + "\r\n"
		if auth != "" {
			req += "Proxy-Authorization: " + auth + "\r\n"
		}
		req += "\r\n"
		if _, err := conn.Write([]byte(req)); err != nil {
			conn.Close()
			return nil, err
		}
		br := bufio.NewReader(conn)
		line, err := br.ReadString(0x0a)
		if err != nil {
			conn.Close()
			return nil, err
		}
		if !strings.Contains(line, " 200") {
			conn.Close()
			return nil, fmt.Errorf("proxy CONNECT failed: %s", strings.TrimSpace(line))
		}
		for { // drain proxy response headers
			l, err := br.ReadString(0x0a)
			if err != nil {
				conn.Close()
				return nil, err
			}
			if l == "\r\n" || l == "\n" {
				break
			}
		}
		return claudeTLSHandshakeBufio(ctx, conn, br, serverName, roots)
	case "socks5", "socks5h", "socks":
		if err := socks5Handshake(ctx, raw, px, host); err != nil {
			raw.Close()
			return nil, err
		}
		return claudeTLSHandshakeBufio(ctx, raw, bufio.NewReader(raw), serverName, roots)
	}
	raw.Close()
	return nil, fmt.Errorf("unsupported proxy scheme %q", px.Scheme)
}

// socks5Handshake does RFC1928 CONNECT (+RFC1929 auth) through a raw conn.
func socks5Handshake(ctx context.Context, c net.Conn, px *url.URL, target string) error {
	methods := []byte{0}
	user, pw := "", ""
	if px.User != nil {
		user = px.User.Username()
		pw, _ = px.User.Password()
		methods = append(methods, 2)
	}
	greet := append([]byte{5, byte(len(methods))}, methods...)
	if _, err := c.Write(greet); err != nil {
		return err
	}
	var resp [2]byte
	if _, err := io.ReadFull(c, resp[:]); err != nil {
		return err
	}
	if resp[1] == 0xff {
		return fmt.Errorf("socks5: no acceptable auth methods")
	}
	if resp[1] == 2 {
		auth := append([]byte{1, byte(len(user))}, user...)
		auth = append(auth, byte(len(pw)))
		auth = append(auth, pw...)
		if _, err := c.Write(auth); err != nil {
			return err
		}
		if _, err := io.ReadFull(c, resp[:]); err != nil {
			return err
		}
		if resp[1] != 0 {
			return fmt.Errorf("socks5: auth rejected")
		}
	}
	host, portStr, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portStr)
	if port == 0 {
		port = 443
	}
	req := []byte{5, 1, 0}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			req = append(req, 1, v4[0], v4[1], v4[2], v4[3])
		} else {
			v6 := ip.To16()
			req = append(req, 4)
			req = append(req, v6...)
		}
	} else {
		req = append(req, 3, byte(len(host)))
		req = append(req, host...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		return err
	}
	var head [4]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return err
	}
	if head[1] != 0 {
		return fmt.Errorf("socks5: connect failed, rep=%d", head[1])
	}
	var skip int
	switch head[3] {
	case 1:
		skip = 4 + 2
	case 4:
		skip = 16 + 2
	case 3:
		var lb [1]byte
		if _, err := io.ReadFull(c, lb[:]); err != nil {
			return err
		}
		skip = int(lb[0]) + 2
	}
	if skip > 0 {
		io.ReadFull(c, make([]byte, skip))
	}
	return nil
}
