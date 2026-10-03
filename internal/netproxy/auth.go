// Per-session proxy auth — the Go counterpart of SRT's proxyAuthToken +
// tagged-username scheme (mod_0215.js ce/Iyn/Soe, mod_0222.js l_ auth check).
//
// The sandboxed command's env carries proxy URLs of the form
// http://<user>:<token>@localhost:<port>; <user> is "srt" or "srt.<b64>"
// where <b64> is the base64 of the command's first 100 bytes — that is how
// real tags egress with the originating command. The listener requires a
// matching password on every HTTP request/CONNECT (407 + realm "srt"
// otherwise) and the SOCKS5 listener requires RFC1929 user/pass auth with
// the same credentials.
package netproxy

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
)

const (
	// authUserPrefix mirrors real's aPe — every proxy auth username is
	// "srt" or "srt.<tag>".
	authUserPrefix = "srt"
	// maxTagCommand mirrors real's l6r: only the first 100 bytes of the
	// command are base64'd into the username tag.
	maxTagCommand = 100
	// maxUserLen mirrors real's Y — the full username is capped at 255
	// bytes ("srt." + tag).
	maxUserLen = 255
)

// NewAuthToken generates a per-session proxy password: 24 random bytes,
// base64url — same shape as real's token.
func NewAuthToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// commandTag is real's Soe: base64 of at most the first 100 bytes of the
// command. It rides inside the proxy username for per-command attribution.
func commandTag(command string) string {
	if len(command) > maxTagCommand {
		command = command[:maxTagCommand]
	}
	return base64.StdEncoding.EncodeToString([]byte(command))
}

// authUser is real's ce: "srt" with no tag, else "srt.<tag>" truncated to
// 255 bytes.
func authUser(tag string) string {
	if tag == "" {
		return authUserPrefix
	}
	u := authUserPrefix + "." + tag
	if len(u) > maxUserLen {
		u = u[:maxUserLen]
	}
	return u
}

// decodeUserTag is real's Iyn: strip the "srt." prefix, returning the
// embedded command tag (still base64), or "" for a bare/invalid user.
func decodeUserTag(user string) string {
	if !strings.HasPrefix(user, authUserPrefix+".") {
		return ""
	}
	tag := user[len(authUserPrefix)+1:]
	if tag == "" || len(tag) > maxUserLen-len(authUserPrefix)-1 {
		return ""
	}
	return tag
}

// decodeTagCommand is real's iPe for display: base64 → command text.
// Returns "" when the tag is not valid base64.
func decodeTagCommand(tag string) string {
	b, err := base64.StdEncoding.DecodeString(tag)
	if err != nil {
		return ""
	}
	return string(b)
}

// validAuthUser reports whether user is shaped like a real proxy username:
// exactly "srt" or "srt.<something>".
func validAuthUser(user string) bool {
	return user == authUserPrefix || strings.HasPrefix(user, authUserPrefix+".")
}

// checkAuth validates r's Proxy-Authorization header against the session
// token — real's `n()` inside l_: "Basic <b64>", user non-empty, password
// equal to the token. Returns ok plus the embedded command tag ("" when
// none / auth disabled). Constant-time on the password.
func (p *Proxy) checkAuth(r *http.Request) (bool, string) {
	if p.AuthToken == "" {
		return true, ""
	}
	u, pass, ok := parseBasicAuth(r.Header.Get("Proxy-Authorization"))
	if !ok || u == "" {
		return false, ""
	}
	if subtle.ConstantTimeCompare([]byte(pass), []byte(p.AuthToken)) != 1 {
		return false, ""
	}
	return true, decodeUserTag(u)
}

// parseBasicAuth decodes a "Basic <b64 user:pass>" header value.
func parseBasicAuth(h string) (user, pass string, ok bool) {
	const pre = "Basic "
	if len(h) <= len(pre) || !strings.EqualFold(h[:len(pre)], pre) {
		return "", "", false
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[len(pre):]))
	if err != nil {
		return "", "", false
	}
	u, p, found := strings.Cut(string(dec), ":")
	if !found {
		return "", "", false
	}
	return u, p, true
}

// encodeURIComponent matches JS encodeURIComponent: everything except
// A-Z a-z 0-9 - _ . ! ~ * ' ( ) gets %XX. Real encodes the tagged username
// this way before embedding it in the proxy URL.
func encodeURIComponent(s string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.!~*'()"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
		} else {
			const hex = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		}
	}
	return b.String()
}
