// AWS SigV4 re-signing for masked credential pairs — the Go port of
// real's Xv/Jv/A_ pipeline plus its $3/W3/H3/e_ signer (mod_0222.js).
//
// sandbox.credentials.awsPairs (plus the conventional AWS_ACCESS_KEY_ID /
// AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN auto-pair) groups whole-value
// masked env vars into a credential set. Inside the sandbox the SDK signs
// with the sentinel access key and the sentinel secret — a signature that
// would fail upstream. On the TLS-terminated path the proxy detects a
// request whose Authorization (or presigned X-Amz-Credential) carries a
// registered sentinel access key, swaps in the real access key id, and
// RE-COMPUTES the signature over the original scope, signed-header set
// and (substituted) body using the real secret.
//
// Request shapes the proxy cannot re-sign (streaming bodies, presigned
// URLs, sigv4a) fall under sandbox.credentials.sigv4 policy: "deny"
// (default) or "passthrough".
package netproxy

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	sigv4Alg        = "AWS4-HMAC-SHA256"
	sigv4aAlg       = "AWS4-ECDSA-P256-SHA256"
	unsignedPayload = "UNSIGNED-PAYLOAD"
	streamingPrefix = "STREAMING-"
	sigv4Terminator = "aws4_request"

	// defMaxSigV4ResignBodyBytes is real's n_ (64 MiB): a request that
	// signs a literal body hash must be buffered to re-sign.
	defMaxSigV4ResignBodyBytes = 67108864
)

// awsPair is one registered credential set — real's register() payload:
// the sentinel access key id seen inside the sandbox, the real key/secret
// swapped in on egress, an optional real session token, and the inject
// scope (defaults to the effective allowedDomains host patterns).
type awsPair struct {
	accessKeyIDSentinel string
	realAccessKeyID     string
	realSecretAccessKey string
	realSessionToken    string // "" when no usable sessionTokenVar
	injectHosts         []string
}

func (p *awsPair) injectable(host string) bool {
	for _, pat := range p.injectHosts {
		if matchDomain(host, pat) {
			return true
		}
	}
	return false
}

// AWSPairStore is real's class H0 (field Cp): sentinel access-key-id →
// pair. Registration happens at wrap time; the proxy only reads.
type AWSPairStore struct {
	bySentinel map[string]*awsPair
}

func NewAWSPairStore() *AWSPairStore {
	return &AWSPairStore{bySentinel: map[string]*awsPair{}}
}

// Register records a pair — real's h.register({accessKeyIdSentinel,
// realAccessKeyId, realSecretAccessKey, realSessionToken?, injectHosts}).
// sessionToken == "" means the pair re-signs without x-amz-security-token.
func (s *AWSPairStore) Register(sentinel, realKey, realSecret, sessionToken string, injectHosts []string) {
	s.bySentinel[sentinel] = &awsPair{
		accessKeyIDSentinel: sentinel,
		realAccessKeyID:     realKey,
		realSecretAccessKey: realSecret,
		realSessionToken:    sessionToken,
		injectHosts:         injectHosts,
	}
}

// Lookup returns the pair registered under sentinel — real's lookup().
func (s *AWSPairStore) Lookup(sentinel string) *awsPair {
	if s == nil {
		return nil
	}
	return s.bySentinel[sentinel]
}

func (s *AWSPairStore) Size() int {
	if s == nil {
		return 0
	}
	return len(s.bySentinel)
}

func (s *AWSPairStore) Clear() {
	s.bySentinel = map[string]*awsPair{}
}

// SigV4Policy mirrors sandbox.credentials.sigv4 (real's FM): the action
// for each request shape the proxy cannot re-sign — "deny" (default) or
// "passthrough".
type SigV4Policy struct {
	Streaming string
	Presigned string
	SigV4a    string
}

func (p SigV4Policy) forKind(kind string) string {
	switch kind {
	case "streaming":
		if p.Streaming != "" {
			return p.Streaming
		}
	case "presigned":
		if p.Presigned != "" {
			return p.Presigned
		}
	case "sigv4a":
		if p.SigV4a != "" {
			return p.SigV4a
		}
	}
	return "deny"
}

// ---------- request classification (real's Xv / Kv / Jv) ----------

var (
	// real's Kv: the access-key-id inside any Credential= scope.
	sigv4CredIDRe = regexp.MustCompile(`Credential=([^/,\s]+)/`)
	// real's Jv field extractors.
	sigv4CredRe   = regexp.MustCompile(`Credential=([^,\s]+)`)
	sigv4SignedRe = regexp.MustCompile(`SignedHeaders=([^,\s]+)`)
	sigv4SigRe    = regexp.MustCompile(`Signature=([0-9a-fA-F]+)`)
)

// sigV4Shape is real's Xv result: the request shape and the access key id
// (the sentinel, when masked) it claims.
type sigV4Shape struct {
	kind        string // "sigv4a" | "streaming" | "header-sigv4" | "presigned"
	accessKeyID string
}

// classifySigV4 is real's Xv: Authorization-header SigV4a → SigV4
// (streaming when x-amz-content-sha256 starts with STREAMING-) → else a
// presigned URL carrying X-Amz-Algorithm + X-Amz-Signature in the query.
func classifySigV4(h http.Header, requestTarget string) *sigV4Shape {
	if auth := h.Get("Authorization"); auth != "" {
		if strings.HasPrefix(auth, sigv4aAlg) {
			if id := sigv4CredentialID(auth); id != "" {
				return &sigV4Shape{kind: "sigv4a", accessKeyID: id}
			}
		} else if strings.HasPrefix(auth, sigv4Alg) {
			if id := sigv4CredentialID(auth); id != "" {
				kind := "header-sigv4"
				if strings.HasPrefix(h.Get("X-Amz-Content-Sha256"), streamingPrefix) {
					kind = "streaming"
				}
				return &sigV4Shape{kind: kind, accessKeyID: id}
			}
		}
	}
	qi := strings.Index(requestTarget, "?")
	if qi < 0 {
		return nil
	}
	q, _ := url.ParseQuery(requestTarget[qi+1:])
	if q.Get("X-Amz-Algorithm") != sigv4Alg || !q.Has("X-Amz-Signature") {
		return nil
	}
	cred := q.Get("X-Amz-Credential")
	if cred == "" {
		return nil
	}
	id, _, _ := strings.Cut(cred, "/")
	if id == "" {
		return nil
	}
	return &sigV4Shape{kind: "presigned", accessKeyID: id}
}

func sigv4CredentialID(auth string) string {
	if m := sigv4CredIDRe.FindStringSubmatch(auth); m != nil {
		return m[1]
	}
	return ""
}

// sigV4Auth is real's Jv parse of the client's Authorization header: the
// original credential scope and signed-header list the re-sign reuses.
type sigV4Auth struct {
	accessKeyID   string
	date          string
	region        string
	service       string
	signedHeaders []string // lowercased, order as sent
	signature     string
}

// parseSigV4Auth is real's Jv: null unless the header is well-formed
// AWS4-HMAC-SHA256 with a five-segment aws4_request credential scope.
func parseSigV4Auth(auth string) *sigV4Auth {
	if !strings.HasPrefix(auth, sigv4Alg) {
		return nil
	}
	cred := sigv4CredRe.FindStringSubmatch(auth)
	sh := sigv4SignedRe.FindStringSubmatch(auth)
	sig := sigv4SigRe.FindStringSubmatch(auth)
	if cred == nil || sh == nil || sig == nil {
		return nil
	}
	parts := strings.Split(cred[1], "/")
	if len(parts) != 5 || parts[4] != sigv4Terminator {
		return nil
	}
	if parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return nil
	}
	hdrs := strings.Split(sh[1], ";")
	for i := range hdrs {
		hdrs[i] = strings.ToLower(hdrs[i])
	}
	return &sigV4Auth{
		accessKeyID:   parts[0],
		date:          parts[1],
		region:        parts[2],
		service:       parts[3],
		signedHeaders: hdrs,
		signature:     sig[1],
	}
}

// ---------- the plan (real's A_) ----------

// sigV4Plan is the outcome of planSigV4: either a deny reason, or a
// re-sign closure's captured inputs.
type sigV4Plan struct {
	deny    string // non-empty → deny the request with this reason
	pair    *awsPair
	auth    *sigV4Auth
	amzDate string
	// signedHeaders is the client's list, lowercased, plus
	// x-amz-security-token when the pair has a real token and the list
	// lacks it (real pushes it onto N).
	signedHeaders []string
	unsigned      bool   // x-amz-content-sha256 was UNSIGNED-PAYLOAD
	hadContentSHA bool   // request carried x-amz-content-sha256 at all
	method        string // captured for the canonical request
	target        string // raw origin-form request target
}

// planSigV4 is real's A_(Cp, credentials.sigv4, hBe): nil when the
// request isn't a masked-credential SigV4 request in scope; a deny plan
// for non-resignable shapes or malformed input; a re-sign plan otherwise.
//
// It must run BEFORE sentinel substitution: detection keys on the
// sentinel access key in the request.
func (p *Proxy) planSigV4(method, requestTarget string, h http.Header, host string) *sigV4Plan {
	reg := p.AWSPairs
	if reg == nil || reg.Size() == 0 {
		return nil
	}
	shape := classifySigV4(h, requestTarget)
	if shape == nil {
		return nil
	}
	pair := reg.Lookup(shape.accessKeyID)
	if pair == nil {
		return nil
	}
	if !pair.injectable(host) {
		return nil
	}
	if shape.kind != "header-sigv4" {
		if p.SigV4.forKind(shape.kind) == "passthrough" {
			p.logf("[credential-aws] %s request with masked credential passed through un-resigned per credentials.sigv4.%s (its signature covers the placeholder secret and will fail upstream)",
				shape.kind, shape.kind)
			return nil
		}
		return &sigV4Plan{deny: fmt.Sprintf(
			`AWS SigV4 %s request uses a masked credential but cannot be re-signed by the sandbox proxy; denied by policy. Set credentials.sigv4.%s to "passthrough" to forward it unmodified (it will fail upstream: the signature covers the masked placeholder).`,
			shape.kind, shape.kind)}
	}
	auth := parseSigV4Auth(h.Get("Authorization"))
	if auth == nil {
		return &sigV4Plan{deny: "AWS SigV4 request uses a masked credential but its Authorization header is malformed; the proxy cannot re-sign it."}
	}
	amzDate := h.Get("X-Amz-Date")
	if amzDate == "" {
		return &sigV4Plan{deny: "AWS SigV4 request uses a masked credential but carries no x-amz-date header; the proxy cannot re-sign it."}
	}
	signedHeaders := append([]string(nil), auth.signedHeaders...)
	for _, n := range signedHeaders {
		if n == "host" {
			continue // always supplied from the upstream authority
		}
		if n == "x-amz-security-token" && pair.realSessionToken != "" {
			continue // the apply injects the real token
		}
		if !hasLowerHeader(h, n) {
			return &sigV4Plan{deny: fmt.Sprintf(
				`AWS SigV4 request uses a masked credential but signed header "%s" is missing; the proxy cannot re-sign it.`, n)}
		}
	}
	if pair.realSessionToken != "" && !containsStr(signedHeaders, "x-amz-security-token") {
		signedHeaders = append(signedHeaders, "x-amz-security-token")
	}
	sha := h.Get("X-Amz-Content-Sha256")
	return &sigV4Plan{
		pair:          pair,
		auth:          auth,
		amzDate:       amzDate,
		signedHeaders: signedHeaders,
		unsigned:      sha == unsignedPayload,
		hadContentSHA: sha != "" || hasLowerHeader(h, "x-amz-content-sha256"),
		method:        method,
		target:        requestTarget,
	}
}

func hasLowerHeader(h http.Header, lower string) bool {
	for k := range h {
		if strings.ToLower(k) == lower {
			return true
		}
	}
	return false
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

var errBodyOverLimit = fmt.Errorf("request body exceeds the re-sign buffering limit")

// resignTo performs real's resign path on req: when the signature covers
// a literal body hash the (already sentinel-substituted) body is buffered
// up to the limit and hashed; then apply rewrites the amz headers and
// Authorization. Returns a user-facing deny reason — the caller answers
// 403 and drops the request.
func (pl *sigV4Plan) resignTo(p *Proxy, req *http.Request, hostHeader string) string {
	if pl.pair == nil || pl.auth == nil {
		return pl.deny // defensive: a deny plan is not resignable
	}
	payloadHash := unsignedPayload
	if !pl.unsigned {
		body, err := readAllLimit(req.Body, p.maxResignBodyBytes())
		if err != nil {
			if err == errBodyOverLimit {
				return fmt.Sprintf(
					"AWS SigV4 request uses a masked credential and signs a literal body hash, so the proxy must buffer the body to re-sign it, but the body exceeds the %d-byte buffering limit; denied. Sign the payload as UNSIGNED-PAYLOAD to stream it without buffering, or use an unmasked credential to have the request forwarded untouched.",
					p.maxResignBodyBytes())
			}
			return "AWS SigV4 re-signing failed: " + err.Error()
		}
		req.Body.Close()
		payloadHash = sigv4HashHex(body)
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	pl.apply(req, hostHeader, payloadHash)
	return ""
}

// apply is real's B.apply(D, M, X): inject the real session token, fix
// the content-sha256 header when it signed a literal hash, and recompute
// Authorization over the original scope + signed-header set.
func (pl *sigV4Plan) apply(req *http.Request, hostHeader, payloadHash string) {
	if pl.pair.realSessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", pl.pair.realSessionToken)
	}
	if pl.hadContentSHA && !pl.unsigned {
		req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	}
	req.Header.Set("Authorization", sigV4Authorization(
		pl.method, pl.target, req.Header, hostHeader,
		pl.signedHeaders, payloadHash, pl.amzDate,
		pl.auth, pl.pair))
}

func (p *Proxy) maxResignBodyBytes() int64 {
	if p.MaxSigV4ResignBodyBytes > 0 {
		return p.MaxSigV4ResignBodyBytes
	}
	return defMaxSigV4ResignBodyBytes
}

func readAllLimit(r io.Reader, limit int64) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	buf, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > limit {
		return nil, errBodyOverLimit
	}
	return buf, nil
}

// ---------- the signer (real's O0/V3/G3/Yv/$3/W3/H3/e_) ----------

func sigv4HashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sigv4HMAC(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// sigV4DeriveKey is real's H3: AWS4+secret → date → region → service →
// aws4_request.
func sigV4DeriveKey(secret, date, region, service string) []byte {
	k := sigv4HMAC([]byte("AWS4"+secret), date)
	k = sigv4HMAC(k, region)
	k = sigv4HMAC(k, service)
	return sigv4HMAC(k, sigv4Terminator)
}

// sigV4URIEncode is real's O0: encodeURIComponent with [!'()*] also
// percent-encoded — i.e., the RFC3986 unreserved set is kept verbatim.
func sigV4URIEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// sigV4Decode is real's jv: URI-decode, keeping the raw text on failure.
// PathUnescape (not QueryUnescape): "+" is a literal, like
// decodeURIComponent.
func sigV4Decode(s string) string {
	if d, err := url.PathUnescape(s); err == nil {
		return d
	}
	return s
}

// sigV4CanonicalURI is real's V3: empty → "/"; s3 keeps the raw path;
// other services get dot-segment normalization plus full (double-)
// encoding with %2F restored to "/".
func sigV4CanonicalURI(path, service string) string {
	if path == "" {
		return "/"
	}
	if service == "s3" {
		return path
	}
	var segs []string
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg == "." {
			continue
		}
		if seg == ".." {
			if len(segs) > 0 {
				segs = segs[:len(segs)-1]
			}
			continue
		}
		segs = append(segs, seg)
	}
	trailing := ""
	if len(segs) > 0 && strings.HasSuffix(path, "/") {
		trailing = "/"
	}
	uri := "/" + strings.Join(segs, "/") + trailing
	return strings.ReplaceAll(sigV4URIEncode(uri), "%2F", "/")
}

// sigV4CanonicalQuery is real's G3: decode then re-encode each name and
// value, sort by (name, value), rejoin.
func sigV4CanonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	var pairs [][2]string
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		pairs = append(pairs, [2]string{sigV4URIEncode(sigV4Decode(k)), sigV4URIEncode(sigV4Decode(v))})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][0] != pairs[j][0] {
			return pairs[i][0] < pairs[j][0]
		}
		return pairs[i][1] < pairs[j][1]
	})
	var b strings.Builder
	for i, pr := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(pr[0] + "=" + pr[1])
	}
	return b.String()
}

// sigV4NormValue is real's Yv: trim then collapse runs of space/tab.
func sigV4NormValue(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\t", " ")), " ")
}

// sigV4HeaderValue joins a header's values the way real's canonical
// request does: each value normalized, then comma-joined.
func sigV4HeaderValue(h http.Header, lower string) (string, bool) {
	var vals []string
	for k, vs := range h {
		if strings.ToLower(k) != lower {
			continue
		}
		for _, v := range vs {
			vals = append(vals, sigV4NormValue(v))
		}
	}
	if len(vals) == 0 {
		return "", false
	}
	return strings.Join(vals, ","), true
}

// sigV4Authorization is real's $3+W3+H3+e_: build the canonical request
// over the original signed-header set (host comes from the upstream
// authority), the string-to-sign over the original scope and amz-date,
// and the Authorization header carrying the real access key id.
func sigV4Authorization(method, requestTarget string, h http.Header,
	hostHeader string, signedHeaders []string, payloadHash, amzDate string,
	auth *sigV4Auth, pair *awsPair) string {

	sorted := append([]string(nil), signedHeaders...)
	for i := range sorted {
		sorted[i] = strings.ToLower(sorted[i])
	}
	sort.Strings(sorted)

	var canonHeaders strings.Builder
	for _, name := range sorted {
		var v string
		var ok bool
		if name == "host" {
			v, ok = hostHeader, true
		} else {
			v, ok = sigV4HeaderValue(h, name)
		}
		if !ok {
			// real throws inside apply(); the plan already verified every
			// signed header is present, so this is unreachable in practice.
			continue
		}
		canonHeaders.WriteString(name + ":" + v + "\n")
	}

	path, query, _ := strings.Cut(requestTarget, "?")
	canon := strings.Join([]string{
		strings.ToUpper(method),
		sigV4CanonicalURI(path, auth.service),
		sigV4CanonicalQuery(query),
		canonHeaders.String(),
		strings.Join(sorted, ";"),
		payloadHash,
	}, "\n")

	scope := auth.date + "/" + auth.region + "/" + auth.service + "/" + sigv4Terminator
	toSign := sigv4Alg + "\n" + amzDate + "\n" + scope + "\n" + sigv4HashHex([]byte(canon))
	sig := hex.EncodeToString(sigv4HMAC(sigV4DeriveKey(
		pair.realSecretAccessKey, auth.date, auth.region, auth.service), toSign))

	return fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		sigv4Alg, pair.realAccessKeyID, scope, strings.Join(sorted, ";"), sig)
}
