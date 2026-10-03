// SOCKS5 listener — the Go port of real's SRT socks server (mod_0222.js
// pM/mM/p_). RFC1928 CONNECT-only over TCP, ATYP IPv4/domain/IPv6. When an
// AuthToken is configured the listener requires RFC1929 username/password
// (user "srt" or "srt.<tag>", password = session token); clients that don't
// offer method 0x02 get real's EM fallback — a no-auth accept followed by a
// refusal, which for port 22 is delivered as an SSH-2.0 banner so
// `nc -X 5`-proxied git+ssh surfaces a readable reason instead of a dead
// socket.
package netproxy

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"syscall"
	"time"
)

// RFC1928 reply codes (real's U0).
const (
	socksGranted          = 0
	socksGeneralFailure   = 1
	socksNotAllowed       = 2
	socksNetUnreachable   = 3
	socksHostUnreachable  = 4
	socksConnRefused      = 5
	socksCmdNotSupported  = 7
	socksAtypNotSupported = 8
)

const (
	socksAuthNone     = 0
	socksAuthUserPass = 2
	socksAuthNoneOK   = 0xff
)

func (p *Proxy) serveSocks(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		if tc, ok := c.(*net.TCPConn); ok {
			tc.SetNoDelay(true)
		}
		go p.handleSocksConn(c)
	}
}

// socksReply writes a RFC1928 reply with a zero BOUND address (0.0.0.0:0),
// matching real's F0.
func socksReply(c net.Conn, rep byte) {
	c.Write([]byte{5, rep, 0, 1, 0, 0, 0, 0, 0, 0})
}

func (p *Proxy) handleSocksConn(c net.Conn) {
	// handed lets the splice goroutines own the conn on success; every
	// earlier return still closes it.
	handed := false
	defer func() {
		if !handed {
			c.Close()
		}
	}()
	c.SetDeadline(time.Now().Add(60 * time.Second))
	br := bufio.NewReader(c)
	tag := ""
	if p.AuthToken != "" {
		// Peek at the method list without committing: clients that offer
		// user/pass auth take the RFC1929 path; the rest get real's EM
		// unauthenticated refusal.
		hdr, err := br.Peek(2)
		if err != nil || hdr[0] != 5 {
			return
		}
		n := int(hdr[1])
		methods, err := br.Peek(2 + n)
		if err != nil {
			return
		}
		if !containsByte(methods[2:], socksAuthUserPass) {
			// EM path: greeting was only peeked — consume it so the request
			// read lands on the right bytes.
			br.Discard(2 + n)
			p.socksRefuseUnauthenticated(c, br)
			return
		}
		br.Discard(2 + n)
		c.Write([]byte{5, socksAuthUserPass})
		user, ok := p.socksUserPassAuth(br)
		if !ok {
			c.Write([]byte{1, 1})
			return
		}
		c.Write([]byte{1, 0})
		tag = decodeUserTag(user)
	} else {
		// no auth configured: require the no-auth method to be offered
		ver, err := br.ReadByte()
		if err != nil || ver != 5 {
			return
		}
		n, err := br.ReadByte()
		if err != nil || n == 0 {
			return
		}
		methods := make([]byte, int(n))
		if _, err := io.ReadFull(br, methods); err != nil {
			return
		}
		if !containsByte(methods, socksAuthNone) {
			c.Write([]byte{5, socksAuthNoneOK})
			return
		}
		c.Write([]byte{5, socksAuthNone})
	}

	// request: VER CMD RSV ATYP DST.ADDR DST.PORT — CONNECT only.
	if v, err := br.ReadByte(); err != nil || v != 5 {
		return
	}
	cmd, err := br.ReadByte()
	if err != nil {
		return
	}
	if _, err := br.ReadByte(); err != nil { // RSV
		return
	}
	host, ok := p.socksReadAddr(br)
	if !ok {
		return
	}
	var pb [2]byte
	if _, err := io.ReadFull(br, pb[:]); err != nil {
		return
	}
	port := int(binary.BigEndian.Uint16(pb[:]))
	if cmd != 1 {
		socksReply(c, socksCmdNotSupported)
		return
	}
	if host == "" || port < 1 {
		socksReply(c, socksAtypNotSupported)
		return
	}
	if ok, _ := p.gate(context.Background(), host, port, tag); !ok {
		socksReply(c, socksNotAllowed)
		return
	}
	up, err := p.dialChecked(context.Background(), "tcp",
		net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		reply := byte(socksHostUnreachable)
		var pe *policyError
		if errors.As(err, &pe) {
			reply = socksNotAllowed
		} else if errors.Is(err, syscall.ECONNREFUSED) {
			reply = socksConnRefused
		}
		socksReply(c, reply)
		return
	}
	socksReply(c, socksGranted)
	handed = true
	c.SetDeadline(time.Time{})
	// any request bytes the buffered reader already pulled are client payload
	go func() {
		io.Copy(up, io.MultiReader(br, c))
		up.Close()
	}()
	go func() {
		io.Copy(c, up)
		c.Close()
	}()
}

// socksReadAddr consumes ATYP + DST.ADDR. Returns the host (domain or IP
// literal string) and whether the encoding was understood.
func (p *Proxy) socksReadAddr(br *bufio.Reader) (string, bool) {
	atyp, err := br.ReadByte()
	if err != nil {
		return "", false
	}
	switch atyp {
	case 1: // IPv4
		b := make([]byte, 4)
		if _, err := io.ReadFull(br, b); err != nil {
			return "", false
		}
		return net.IP(b).String(), true
	case 3: // domain
		n, err := br.ReadByte()
		if err != nil {
			return "", false
		}
		b := make([]byte, int(n))
		if _, err := io.ReadFull(br, b); err != nil {
			return "", false
		}
		return string(b), true
	case 4: // IPv6
		b := make([]byte, 16)
		if _, err := io.ReadFull(br, b); err != nil {
			return "", false
		}
		return net.IP(b).String(), true
	}
	return "", false
}

// socksUserPassAuth runs RFC1929 sub-negotiation: ver(1) ulen uname plen
// passwd. Validates the srt-shaped user and the session token.
func (p *Proxy) socksUserPassAuth(br *bufio.Reader) (string, bool) {
	if v, err := br.ReadByte(); err != nil || v != 1 {
		return "", false
	}
	ulen, err := br.ReadByte()
	if err != nil {
		return "", false
	}
	ub := make([]byte, int(ulen))
	if _, err := io.ReadFull(br, ub); err != nil {
		return "", false
	}
	plen, err := br.ReadByte()
	if err != nil {
		return "", false
	}
	pb := make([]byte, int(plen))
	if _, err := io.ReadFull(br, pb); err != nil {
		return "", false
	}
	user := string(ub)
	if !validAuthUser(user) || string(pb) != p.AuthToken {
		return user, false
	}
	return user, true
}

// socksRefuseUnauthenticated is real's EM: the client offered no auth method
// we accept, so we answer as if no-auth were granted, read the CONNECT
// request, then refuse. For port 22 the refusal is an SSH-2.0 banner — git's
// `nc -X 5` ProxyCommand can't authenticate, so the banner is the only way
// the reason reaches the user.
func (p *Proxy) socksRefuseUnauthenticated(c net.Conn, br *bufio.Reader) {
	c.Write([]byte{5, socksAuthNone})
	host, port, ok := p.socksReadRequest(br)
	if !ok || host == "" {
		socksReply(c, socksNotAllowed)
		return
	}
	// decide (not gate): the conn is refused either way, so the interactive
	// prompt should not fire here.
	_, reason := p.decide(context.Background(), host, port)
	if port != 22 {
		p.logf("socks: unauthenticated client refused for %s:%d", host, port)
		socksReply(c, socksNotAllowed)
		return
	}
	if reason == "" {
		reason = "This proxy requires authentication, and this client did not offer an authentication method, so the connection was refused."
	}
	socksReply(c, socksGranted)
	fmt.Fprintf(c, "SSH-2.0-policy_refusal\r\n%s\r\n", sanitizeBanner(reason))
}

// socksReadRequest consumes a full CONNECT request for the refusal path.
func (p *Proxy) socksReadRequest(br *bufio.Reader) (string, int, bool) {
	if v, err := br.ReadByte(); err != nil || v != 5 {
		return "", 0, false
	}
	cmd, err := br.ReadByte()
	if err != nil || cmd != 1 {
		return "", 0, false
	}
	if _, err := br.ReadByte(); err != nil {
		return "", 0, false
	}
	host, ok := p.socksReadAddr(br)
	if !ok {
		return "", 0, false
	}
	var pb [2]byte
	if _, err := io.ReadFull(br, pb[:]); err != nil {
		return "", 0, false
	}
	return host, int(binary.BigEndian.Uint16(pb[:])), true
}

func containsByte(b []byte, v byte) bool {
	for _, x := range b {
		if x == v {
			return true
		}
	}
	return false
}

// sanitizeBanner strips control characters for embedding in the SSH banner
// line (real's xM does the same, capped at 1000 bytes).
func sanitizeBanner(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s) && len(out) < 1000; i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f || (c >= 0x80 && c <= 0x9f) {
			c = ' '
		}
		out = append(out, c)
	}
	return string(out)
}
