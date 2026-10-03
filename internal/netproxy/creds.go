// Masked-credential sentinels — the Go port of real's SRT credential
// registry (mod_0222.js class M0, kM, ip, lp, cp, D3).
//
// Model: sandbox.credentials.{envVars,files} entries with mode "mask" have
// their real values replaced, inside the sandboxed process, by per-value
// "fake_value_..." sentinel strings. The child never sees the secret. On
// egress the proxy scans outbound requests for sentinels injectable at the
// request's host (injectHosts, defaulting to network.allowedDomains) and
// substitutes the real value — headers always on the TLS-terminated path,
// plus plaintext requests when sandbox.credentials.allowPlaintextInject is
// set, and bodies the same way.
package netproxy

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

// sentinelPrefix is real's Hu — every generated sentinel starts with it so
// substitution can skip strings that contain none.
const sentinelPrefix = "fake_value_"

// sentinelPadAlphabet is real's y_ — padding chars used to lengthen a
// sentinel to the real value's byte length.
const sentinelPadAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789_-"

// fakeJWTSig is real's E_: the fixed signature segment of fake JWTs
// ("srt-fake" base64url).
const fakeJWTSig = "c3J0LWZha2U"

// fakeJWTExp is real's DM — the exp claim stamped on whole-token fakes.
const fakeJWTExp = 9999999999

// credEntry is one registered real→sentinel pair.
type credEntry struct {
	name        string
	sentinel    string
	real        string
	injectHosts []string // domain patterns (hBe semantics, no ports)
}

// CredStore is the sentinel registry — real's class M0 (field pc). Not
// safe for Register concurrent with itself across goroutines unless callers
// hold the proxy lock; proxy request paths only read via the methods below.
type CredStore struct {
	mu          sync.RWMutex
	byName      map[string]*credEntry
	bySentinel  map[string]*credEntry
	allPrefixed bool // every registered sentinel still has sentinelPrefix
}

// NewCredStore allocates an empty registry.
func NewCredStore() *CredStore {
	return &CredStore{
		byName:      map[string]*credEntry{},
		bySentinel:  map[string]*credEntry{},
		allPrefixed: true,
	}
}

// randUUID returns a random UUIDv4 string (real uses randomUUID()).
func randUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// genSentinel is real's kM: "fake_value_<uuid>" padded with alphabet chars
// to the real value's byte length when the real value is longer, so an
// equal-length substitution keeps Content-Length valid.
func genSentinel(real string) string {
	s := sentinelPrefix + randUUID()
	if n := len(real) - len(s); n > 0 {
		var pad [64]byte
		if _, err := rand.Read(pad[:]); err != nil {
			panic(err)
		}
		out := make([]byte, 0, len(s)+n)
		out = append(out, s...)
		for i := 0; i < n; i++ {
			out = append(out, sentinelPadAlphabet[int(pad[i%len(pad)])%len(sentinelPadAlphabet)])
		}
		s = string(out)
	}
	return s
}

// Register mints a fresh sentinel for (name, real, injectHosts) — real's
// register(). Returns the sentinel.
func (s *CredStore) Register(name, real string, injectHosts []string) string {
	sentinel := genSentinel(real)
	s.RegisterWithSentinel(name, sentinel, real, injectHosts)
	return sentinel
}

// RegisterWithSentinel is real's registerWithSentinel: records an explicit
// (non-generated) sentinel — used for JWT fake tokens and extract captures.
func (s *CredStore) RegisterWithSentinel(name, sentinel, real string, injectHosts []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !bytes.HasPrefix([]byte(sentinel), []byte(sentinelPrefix)) {
		s.allPrefixed = false
	}
	e := &credEntry{name: name, sentinel: sentinel, real: real, injectHosts: injectHosts}
	s.byName[name] = e
	s.bySentinel[sentinel] = e
}

// LookupReal returns the real value behind a sentinel (real's lookupReal).
func (s *CredStore) LookupReal(sentinel string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e, ok := s.bySentinel[sentinel]; ok {
		return e.real, true
	}
	return "", false
}

// Size is the number of registered sentinels.
func (s *CredStore) Size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.bySentinel)
}

// injectable reports whether host matches any of the entry's injectHosts
// patterns — real's `injectHosts.some(h => hBe(host, h))`.
func (e *credEntry) injectable(host string) bool {
	for _, pat := range e.injectHosts {
		if matchDomain(host, pat) {
			return true
		}
	}
	return false
}

// SubstituteString swaps every sentinel injectable at host for its real
// value — real's substituteInString.
func (s *CredStore) SubstituteString(v, host string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.bySentinel) == 0 {
		return v
	}
	if s.allPrefixed && !bytes.Contains([]byte(v), []byte(sentinelPrefix)) {
		return v
	}
	for _, e := range s.bySentinel {
		if !strings.Contains(v, e.sentinel) {
			continue
		}
		if !e.injectable(host) {
			continue
		}
		v = strings.ReplaceAll(v, e.sentinel, e.real)
	}
	return v
}

// SubstituteHeaders applies SubstituteString to every header value — real's
// substituteInHeaders, wired as the proxy's mutateHeaders callback.
func (s *CredStore) SubstituteHeaders(h http.Header, host string) {
	if s == nil || s.Size() == 0 {
		return
	}
	for _, vs := range h {
		for i, v := range vs {
			vs[i] = s.SubstituteString(v, host)
		}
	}
}

// subPair is a sentinel→real byte pair for body substitution (real's
// sentinelsForHost output).
type subPair struct {
	sentinel []byte
	real     []byte
}

// SentinelsForHost returns the substitution pairs injectable at host —
// real's sentinelsForHost(e, hBe).
func (s *CredStore) SentinelsForHost(host string) []subPair {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []subPair
	for _, e := range s.bySentinel {
		if e.injectable(host) {
			out = append(out, subPair{[]byte(e.sentinel), []byte(e.real)})
		}
	}
	return out
}

// BodySubstituter prepares a request-body substitution for host, mirroring
// real's Qf: GET/HEAD/OPTIONS without a body are skipped; a request with
// Content-Encoding is skipped (the sentinel would reach upstream as the
// fake value); when any sentinel differs in length from its real value the
// request's Content-Length must be dropped so the transport re-chunks.
// Returns the substituting reader, whether the transform applies, and
// whether content-length must be cleared.
func bodySubstitution(r io.Reader, subs []subPair) (io.Reader, bool) {
	if len(subs) == 0 {
		return r, false
	}
	return &substReader{r: r, subs: subs, maxLen: maxSentinelLen(subs)}, true
}

func maxSentinelLen(subs []subPair) int {
	m := 0
	for _, p := range subs {
		if len(p.sentinel) > m {
			m = len(p.sentinel)
		}
	}
	return m
}

func allSameLen(subs []subPair) bool {
	for _, p := range subs {
		if len(p.sentinel) != len(p.real) {
			return false
		}
	}
	return true
}

// substReader is the Go port of real's D3/k3 streaming substitution: it
// replaces sentinel bytes with real bytes while holding back a
// (maxSentinel-1)-byte tail so sentinels split across reads still match.
type substReader struct {
	r      io.Reader
	subs   []subPair
	maxLen int
	pend   []byte // unprocessed input tail
	out    []byte // processed output awaiting Read
	eof    bool
	err    error
}

func (s *substReader) Read(b []byte) (int, error) {
	for len(s.out) == 0 {
		if s.eof {
			if len(s.pend) > 0 {
				s.out = s.substitute(s.pend, true)
				s.pend = nil
				continue
			}
			if s.err != nil && s.err != io.EOF {
				return 0, s.err
			}
			return 0, io.EOF
		}
		chunk := make([]byte, 32*1024)
		n, err := s.r.Read(chunk)
		if n > 0 {
			s.pend = append(s.pend, chunk[:n]...)
			s.out = s.substitute(s.pend, false)
		}
		if err != nil {
			s.eof = true
			s.err = err
		}
	}
	cp := copy(b, s.out)
	s.out = s.out[cp:]
	return cp, nil
}

// substitute runs the D3 algorithm over data, emitting substituted output
// and stashing the unprocessed tail in s.pend.
func (s *substReader) substitute(data []byte, final bool) []byte {
	var out []byte
	h := 0
	for {
		best := -1
		var pair *subPair
		for i := range s.subs {
			idx := bytes.Index(data[h:], s.subs[i].sentinel)
			if idx >= 0 && (best == -1 || h+idx < best) {
				best = h + idx
				pair = &s.subs[i]
			}
		}
		if pair == nil {
			break
		}
		out = append(out, data[h:best]...)
		out = append(out, pair.real...)
		h = best + len(pair.sentinel)
	}
	limit := len(data)
	if !final {
		limit = len(data) - (s.maxLen - 1)
		if limit < h {
			limit = h
		}
	}
	out = append(out, data[h:limit]...)
	s.pend = append(s.pend[:0], data[limit:]...)
	return out
}

// ---------- extract / JWT masking helpers (real's ip, ap, cp, lp) ----------

// b64url encodes JSON as base64url (real's G0).
func b64urlJSON(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func b64urlDecode(s string) ([]byte, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	return b, err == nil
}

// IsJWT is real's ap: three dot-separated parts, header decodes to a JSON
// object containing "alg", payload decodes to JSON.
func IsJWT(s string) bool {
	var header, payload string
	var ok bool
	if header, payload, ok = splitJWT(s); !ok {
		return false
	}
	hb, ok := b64urlDecode(header)
	if !ok {
		return false
	}
	var hmap map[string]any
	if err := json.Unmarshal(hb, &hmap); err != nil || hmap == nil {
		return false
	}
	if _, has := hmap["alg"]; !has {
		return false
	}
	pb, ok := b64urlDecode(payload)
	if !ok {
		return false
	}
	var pmap map[string]any
	return json.Unmarshal(pb, &pmap) == nil && pmap != nil
}

func splitJWT(s string) (header, payload string, ok bool) {
	i := bytes.IndexByte([]byte(s), '.')
	if i < 0 {
		return "", "", false
	}
	j := bytes.IndexByte([]byte(s[i+1:]), '.')
	if j < 0 {
		return "", "", false
	}
	j += i + 1
	if strings.IndexByte(s[j+1:], '.') >= 0 {
		// JWT has exactly 3 segments; trailing content isn't one — real's
		// split(".") check requires exactly 3 parts.
		return "", "", false
	}
	// the signature segment is dropped: fakes always carry fakeJWTSig
	return s[:i], s[i+1 : j], true
}

// FakeJWT is real's cp: a structurally valid fake JWT whose sub claim holds
// a sentinel-style random value.
func FakeJWT() string {
	hdr := b64urlJSON(map[string]any{"alg": "HS256", "typ": "JWT"})
	pl := b64urlJSON(map[string]any{"sub": sentinelPrefix + randUUID(), "exp": fakeJWTExp})
	return hdr + "." + pl + "." + fakeJWTSig
}

// MaskJWTClaims is real's lp: replaces each named claim that exists as a
// string with a per-claim sentinel (produced by reg), returning the fake
// token and the claim→sentinel map. ok=false when no named claim is a
// present string.
func MaskJWTClaims(tok string, claims []string, reg func(claim, val string) string) (string, map[string]string, bool) {
	header, payload, ok := splitJWT(tok)
	if !ok {
		return "", nil, false
	}
	pb, ok := b64urlDecode(payload)
	if !ok {
		return "", nil, false
	}
	var obj map[string]any
	if err := json.Unmarshal(pb, &obj); err != nil || obj == nil {
		return "", nil, false
	}
	sentinels := map[string]string{}
	for _, c := range claims {
		if v, is := obj[c].(string); is {
			sentinels[c] = reg(c, v)
			obj[c] = sentinels[c]
		}
	}
	if len(sentinels) == 0 {
		return "", nil, false
	}
	return header + "." + b64urlJSON(obj) + "." + fakeJWTSig, sentinels, true
}

// maskExtract applies the user's capture-group regex globally, replacing
// group 1 of each match with a per-occurrence sentinel from reg — real's
// ip(). Returns masked value and number of captures; ok=false when the
// pattern matched nothing (caller then applies onExtractNoMatch).
//
// maskDuplicates additionally replaces remaining literal occurrences of a
// captured value with that value's first sentinel (real's maskDuplicates
// pass, longest-first, skipping spans already replaced).
func maskExtract(s string, re *regexp.Regexp, reg func(capture string, idx int) string, maskDuplicates bool) (string, int, error) {
	matches := re.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s, 0, nil
	}
	type span struct{ start, end int }
	type repl struct {
		span
		sentinel string
	}
	var repls []repl
	byCapture := map[string]int{}      // capture -> dedup index
	sentinelFor := map[string]string{} // capture -> first sentinel (maskDuplicates)
	count := 0
	for _, m := range matches {
		if m[2] < 0 {
			return "", 0, fmt.Errorf("extract pattern /%s/ matched at offset %d but capture group 1 is undefined — group 1 must capture the credential value on every match", re.String(), m[0])
		}
		capStart, capEnd := m[2], m[3]
		if capEnd == capStart {
			continue
		}
		capture := s[capStart:capEnd]
		idx, seen := byCapture[capture]
		if !seen {
			idx = len(byCapture)
			byCapture[capture] = idx
		}
		sent := reg(capture, idx)
		if _, has := sentinelFor[capture]; !has {
			sentinelFor[capture] = sent
		}
		repls = append(repls, repl{span{capStart, capEnd}, sent})
		count++
	}
	if maskDuplicates {
		// longest capture first; skip occurrences overlapping emitted spans
		type kv struct {
			capture, sentinel string
		}
		var caps []kv
		for c, sent := range sentinelFor {
			if c != sent {
				caps = append(caps, kv{c, sent})
			}
		}
		for i := 0; i < len(caps); i++ {
			for j := i + 1; j < len(caps); j++ {
				if len(caps[j].capture) > len(caps[i].capture) {
					caps[i], caps[j] = caps[j], caps[i]
				}
			}
		}
		overlaps := func(st, en int) bool {
			for _, r := range repls {
				if st < r.end && r.start < en {
					return true
				}
			}
			return false
		}
		for _, c := range caps {
			for off := 0; ; {
				i := bytes.Index([]byte(s[off:]), []byte(c.capture))
				if i < 0 {
					break
				}
				st := off + i
				en := st + len(c.capture)
				if !overlaps(st, en) {
					repls = append(repls, repl{span{st, en}, c.sentinel})
				}
				off = st + 1
			}
		}
	}
	// sort by start, then build the masked value
	for i := 0; i < len(repls); i++ {
		for j := i + 1; j < len(repls); j++ {
			if repls[j].start < repls[i].start {
				repls[i], repls[j] = repls[j], repls[i]
			}
		}
	}
	var out bytes.Buffer
	pos := 0
	for _, r := range repls {
		out.WriteString(s[pos:r.start])
		out.WriteString(r.sentinel)
		pos = r.end
	}
	out.WriteString(s[pos:])
	return out.String(), count, nil
}

// jwtPattern is real's b_ — the built-in JWT candidate regex used for
// files with decode "jwt" and no explicit extract.
var jwtPattern = regexp.MustCompile(`(eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)`)

// MaskSpec drives MaskedValue — the shared core of real's env-var masking
// (v_) and file masking (x_).
type MaskSpec struct {
	Value            string
	Extract          *regexp.Regexp                        // nil + !DecodeJWT → whole-value mask
	DecodeJWT        bool                                  // decode:"jwt"
	MaskClaims       []string                              // JWT claims masked individually
	MaskDuplicates   bool                                  // re-mask literal repeats of a capture
	WholeName        string                                // registry name for the whole-value case
	NameFn           func(idx int) string                  // per-capture registry name
	ClaimFn          func(occIdx int, claim string) string // per-JWT-claim name
	Register         func(name, real string) string
	RegisterExplicit func(name, sentinel, real string)
}

// MaskedValue computes the sentinel-masked form of spec.Value. ok=false
// means nothing was masked; the caller then applies onExtractNoMatch.
//
// Per real: with no extract and no decode the whole value becomes one
// sentinel. With extract (or the built-in JWT candidate regex under
// decode:"jwt"), each capture group 1 occurrence gets its own sentinel;
// under decode the capture must additionally verify as a JWT, and
// maskClaims masks named string claims into a structurally valid fake
// token instead of replacing the whole value.
func MaskedValue(spec MaskSpec) (masked string, ok bool, err error) {
	if spec.Extract == nil && !spec.DecodeJWT {
		return spec.Register(spec.WholeName, spec.Value), true, nil
	}
	pattern := spec.Extract
	if pattern == nil {
		pattern = jwtPattern
	}
	count := 0
	masked, _, err = maskExtract(spec.Value, pattern, func(capture string, idx int) string {
		if spec.DecodeJWT && !IsJWT(capture) {
			return capture // not a verified JWT — leave the candidate alone
		}
		name := spec.NameFn(idx)
		if spec.DecodeJWT && len(spec.MaskClaims) > 0 {
			fake, _, did := MaskJWTClaims(capture, spec.MaskClaims, func(claim, val string) string {
				return spec.Register(spec.ClaimFn(idx, claim), val)
			})
			if !did {
				return capture
			}
			count++
			spec.RegisterExplicit(name, fake, capture)
			return fake
		}
		count++
		if spec.DecodeJWT {
			fake := FakeJWT()
			spec.RegisterExplicit(name, fake, capture)
			return fake
		}
		return spec.Register(name, capture)
	}, spec.MaskDuplicates)
	if err != nil {
		return "", false, err
	}
	if count == 0 {
		return spec.Value, false, nil
	}
	return masked, true, nil
}
