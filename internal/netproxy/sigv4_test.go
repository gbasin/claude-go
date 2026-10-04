package netproxy

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The AWS SigV4 documentation test vector (iam.amazonaws.com ListUsers):
// signing with AKIDEXAMPLE + the example secret over this exact request
// must reproduce signature 5d672d79… — the reference check for the whole
// canonical-request → string-to-sign → key-derivation chain.
const (
	awsTestKey    = "AKIDEXAMPLE"
	awsTestSecret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	awsTestSig    = "5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"
)

func sigv4SentinelAuth(sentinel, signedHeaders, signature string) string {
	return fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/20150830/us-east-1/iam/aws4_request, SignedHeaders=%s, Signature=%s",
		sentinel, signedHeaders, signature)
}

func TestClassifySigV4(t *testing.T) {
	h := func(k, v string) http.Header { return http.Header{k: []string{v}} }
	cases := []struct {
		name   string
		header http.Header
		target string
		kind   string
		key    string
	}{
		{"header-sigv4", h("Authorization", "AWS4-HMAC-SHA256 Credential=SENT/2020/us-1/s3/aws4_request, SignedHeaders=host, Signature=abc"), "/", "header-sigv4", "SENT"},
		{"streaming", http.Header{
			"Authorization":        []string{"AWS4-HMAC-SHA256 Credential=SENT/2020/us-1/s3/aws4_request, SignedHeaders=host, Signature=abc"},
			"X-Amz-Content-Sha256": []string{"STREAMING-AWS4-HMAC-SHA256-PAYLOAD"},
		}, "/", "streaming", "SENT"},
		{"sigv4a", h("Authorization", "AWS4-ECDSA-P256-SHA256 Credential=SENT/2020/us-1/s3/aws4_request, SignedHeaders=host, Signature=abc"), "/", "sigv4a", "SENT"},
		{"presigned", http.Header{}, "/x?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=SENT%2F2020%2Fus-1%2Fs3%2Faws4_request&X-Amz-Signature=ff", "presigned", "SENT"},
		{"presigned-no-sig", http.Header{}, "/x?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=SENT%2F2020", "", ""},
		{"plain", h("Authorization", "Bearer tok"), "/x", "", ""},
		{"no-auth", http.Header{}, "/x", "", ""},
	}
	for _, c := range cases {
		got := classifySigV4(c.header, c.target)
		if c.kind == "" {
			if got != nil {
				t.Errorf("%s: expected nil, got %+v", c.name, got)
			}
			continue
		}
		if got == nil || got.kind != c.kind || got.accessKeyID != c.key {
			t.Errorf("%s: got %+v, want kind=%q key=%q", c.name, got, c.kind, c.key)
		}
	}
}

func TestParseSigV4Auth(t *testing.T) {
	a := parseSigV4Auth(sigv4SentinelAuth("SENTINEL", "host;x-amz-date", "ab12"))
	if a == nil {
		t.Fatal("nil parse")
	}
	if a.accessKeyID != "SENTINEL" || a.date != "20150830" || a.region != "us-east-1" || a.service != "iam" {
		t.Fatalf("scope = %+v", a)
	}
	if a.signedHeaders[0] != "host" || a.signedHeaders[1] != "x-amz-date" || a.signature != "ab12" {
		t.Fatalf("fields = %+v", a)
	}
	for _, bad := range []string{
		"AWS4-ECDSA-P256-SHA256 Credential=x/1/2/3/aws4_request, SignedHeaders=h, Signature=ff",
		"AWS4-HMAC-SHA256 Credential=x/1/2/3/4/5/aws4_request, SignedHeaders=h, Signature=ff", // 6-segment scope
		"AWS4-HMAC-SHA256 Credential=x/1/2/3/notaws4, SignedHeaders=h, Signature=ff",
		"AWS4-HMAC-SHA256 SignedHeaders=h, Signature=ff",
		"AWS4-HMAC-SHA256 Credential=/1/2/3/aws4_request, SignedHeaders=h, Signature=ff",
	} {
		if parseSigV4Auth(bad) != nil {
			t.Errorf("expected nil for %q", bad)
		}
	}
}

func newPairProxy(injectHosts []string) *Proxy {
	p := New(nil)
	p.AWSPairs.Register("fake_value_awskey", awsTestKey, awsTestSecret, "", injectHosts)
	return p
}

func awsVectorRequest() (*http.Request, string) {
	req := httptest.NewRequest(http.MethodGet,
		"https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set("X-Amz-Date", "20150830T123600Z")
	req.Header.Set("Authorization", sigv4SentinelAuth("fake_value_awskey",
		"content-type;host;x-amz-date", strings.Repeat("0", 64)))
	return req, "/?Action=ListUsers&Version=2010-05-08"
}

func TestSigV4ResignMatchesAWSVector(t *testing.T) {
	p := newPairProxy([]string{"iam.amazonaws.com"})
	req, target := awsVectorRequest()
	plan := p.planSigV4(req.Method, target, req.Header, "iam.amazonaws.com")
	if plan == nil || plan.deny != "" {
		t.Fatalf("plan = %+v", plan)
	}
	if reason := plan.resignTo(p, req, "iam.amazonaws.com"); reason != "" {
		t.Fatalf("resign denied: %s", reason)
	}
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, " +
		"SignedHeaders=content-type;host;x-amz-date, Signature=" + awsTestSig
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("re-signed Authorization mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestSigV4ResignWithSessionToken(t *testing.T) {
	p := New(nil)
	p.AWSPairs.Register("fake_value_k", "REALKEY", "realsecret", "realtoken", []string{"*"})
	req := httptest.NewRequest(http.MethodPost, "https://svc.example.com/put", strings.NewReader("body"))
	req.Header.Set("X-Amz-Date", "20200101T000000Z")
	req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=fake_value_k/20200101/us-west-2/svc/aws4_request, SignedHeaders=host;x-amz-date, Signature="+strings.Repeat("0", 64))
	plan := p.planSigV4(req.Method, "/put", req.Header, "svc.example.com")
	if plan == nil {
		t.Fatal("nil plan")
	}
	if reason := plan.resignTo(p, req, "svc.example.com"); reason != "" {
		t.Fatalf("resign denied: %s", reason)
	}
	if got := req.Header.Get("X-Amz-Security-Token"); got != "realtoken" {
		t.Fatalf("security token = %q", got)
	}
	// the pushed token header must be covered by the new signature
	if !strings.Contains(req.Header.Get("Authorization"), "x-amz-security-token") {
		t.Fatalf("SignedHeaders missing token: %q", req.Header.Get("Authorization"))
	}
	// UNSIGNED-PAYLOAD stays literal — no body buffering, no sha header rewrite
	if got := req.Header.Get("X-Amz-Content-Sha256"); got != "UNSIGNED-PAYLOAD" {
		t.Fatalf("content-sha256 = %q", got)
	}
}

func TestSigV4PlanDenies(t *testing.T) {
	p := newPairProxy([]string{"iam.amazonaws.com"})
	mk := func(mut func(h http.Header)) (*sigV4Plan, *http.Request) {
		req, target := awsVectorRequest()
		if mut != nil {
			mut(req.Header)
		}
		return p.planSigV4(req.Method, target, req.Header, "iam.amazonaws.com"), req
	}
	// malformed authorization (classifies via the sentinel credential but
	// the header doesn't parse — real's Jv null path)
	if plan, _ := mk(func(h http.Header) {
		h.Set("Authorization", "AWS4-HMAC-SHA256 Credential=fake_value_awskey/20150830/us-east-1/iam/aws4_request")
	}); plan == nil || !strings.Contains(plan.deny, "malformed") {
		t.Fatalf("malformed: %+v", plan)
	}
	// an Authorization that doesn't classify at all is forwarded untouched
	if plan, _ := mk(func(h http.Header) {
		h.Set("Authorization", "AWS4-HMAC-SHA256 garbage")
	}); plan != nil {
		t.Fatalf("unclassifiable auth should pass: %+v", plan)
	}
	// missing x-amz-date
	if plan, _ := mk(func(h http.Header) { h.Del("X-Amz-Date") }); plan == nil ||
		!strings.Contains(plan.deny, "x-amz-date") {
		t.Fatalf("no amz-date: %+v", plan)
	}
	// missing a signed header
	if plan, _ := mk(func(h http.Header) { h.Del("Content-Type") }); plan == nil ||
		!strings.Contains(plan.deny, `"content-type"`) {
		t.Fatalf("missing signed header: %+v", plan)
	}
	// streaming shape → policy deny
	if plan, _ := mk(func(h http.Header) {
		h.Set("X-Amz-Content-Sha256", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD")
	}); plan == nil || !strings.Contains(plan.deny, "streaming") ||
		!strings.Contains(plan.deny, "passthrough") {
		t.Fatalf("streaming: %+v", plan)
	}
	// streaming passthrough → nil plan, logged
	p.SigV4.Streaming = "passthrough"
	var logged string
	p.Logf = func(f string, a ...any) { logged = fmt.Sprintf(f, a...) }
	if plan, _ := mk(func(h http.Header) {
		h.Set("X-Amz-Content-Sha256", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD")
	}); plan != nil {
		t.Fatalf("passthrough should pass: %+v", plan)
	}
	if !strings.Contains(logged, "passed through un-resigned") {
		t.Fatalf("passthrough not logged: %q", logged)
	}
	p.SigV4.Streaming = ""
	// presigned URL → policy deny
	req := httptest.NewRequest("GET",
		"https://iam.amazonaws.com/o?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=fake_value_awskey%2F20150830%2Fus-east-1%2Fiam%2Faws4_request&X-Amz-Signature=ff", nil)
	if plan := p.planSigV4(req.Method,
		"/o?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=fake_value_awskey%2F20150830%2Fus-east-1%2Fiam%2Faws4_request&X-Amz-Signature=ff",
		req.Header, "iam.amazonaws.com"); plan == nil || !strings.Contains(plan.deny, "presigned") {
		t.Fatalf("presigned: %+v", plan)
	}
	// sentinel not registered → untouched
	p2 := New(nil)
	req2, target2 := awsVectorRequest()
	if plan := p2.planSigV4(req2.Method, target2, req2.Header, "iam.amazonaws.com"); plan != nil {
		t.Fatalf("unregistered sentinel should not plan: %+v", plan)
	}
	// host outside injectHosts → untouched
	p3 := newPairProxy([]string{"other.example.com"})
	if plan := p3.planSigV4(req2.Method, target2, req2.Header, "iam.amazonaws.com"); plan != nil {
		t.Fatalf("out-of-scope host should not plan: %+v", plan)
	}
}

func TestSigV4ResignBodyHash(t *testing.T) {
	// a signed literal body hash: the proxy buffers the (substituted) body,
	// hashes it, and covers the new hash in the signature.
	p := newPairProxy([]string{"svc.example.com"})
	req := httptest.NewRequest(http.MethodPost, "https://svc.example.com/do",
		strings.NewReader("hello body"))
	req.Header.Set("X-Amz-Date", "20200101T000000Z")
	req.Header.Set("X-Amz-Content-Sha256", strings.Repeat("a", 64)) // sentinel-side hash
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=fake_value_awskey/20200101/us-west-2/svc/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature="+strings.Repeat("0", 64))
	plan := p.planSigV4(req.Method, "/do", req.Header, "svc.example.com")
	if plan == nil || plan.unsigned {
		t.Fatalf("plan = %+v", plan)
	}
	if reason := plan.resignTo(p, req, "svc.example.com"); reason != "" {
		t.Fatalf("resign denied: %s", reason)
	}
	body, _ := io.ReadAll(req.Body)
	if string(body) != "hello body" || req.ContentLength != 10 {
		t.Fatalf("body = %q len %d", body, req.ContentLength)
	}
	// the content-sha256 header was replaced by the hash of the real body
	wantSHA := sigv4HashHex([]byte("hello body"))
	if got := req.Header.Get("X-Amz-Content-Sha256"); got != wantSHA {
		t.Fatalf("content-sha256 = %q, want %q", got, wantSHA)
	}
	// a signed header that is absent → deny (real's missing-header check)
	p2 := newPairProxy([]string{"svc.example.com"})
	reqM := httptest.NewRequest(http.MethodPost, "https://svc.example.com/do",
		strings.NewReader("hello body"))
	reqM.Header.Set("X-Amz-Date", "20200101T000000Z")
	reqM.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=fake_value_awskey/20200101/us-west-2/svc/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature="+strings.Repeat("0", 64))
	if plan := p2.planSigV4(reqM.Method, "/do", reqM.Header, "svc.example.com"); plan == nil ||
		!strings.Contains(plan.deny, "x-amz-content-sha256") {
		t.Fatalf("missing signed header should deny: %+v", plan)
	}
	// over-limit buffering → deny with real's message
	p.MaxSigV4ResignBodyBytes = 4
	req2 := httptest.NewRequest(http.MethodPost, "https://svc.example.com/do",
		strings.NewReader("hello body"))
	req2.Header.Set("X-Amz-Date", "20200101T000000Z")
	req2.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=fake_value_awskey/20200101/us-west-2/svc/aws4_request, SignedHeaders=host;x-amz-date, Signature="+strings.Repeat("0", 64))
	plan2 := p.planSigV4(req2.Method, "/do", req2.Header, "svc.example.com")
	if reason := plan2.resignTo(p, req2, "svc.example.com"); !strings.Contains(reason, "UNSIGNED-PAYLOAD") {
		t.Fatalf("expected buffer-limit deny, got %q", reason)
	}
}

// End-to-end: a sentinel-signed request through the MITM path reaches the
// upstream carrying a signature valid for the REAL key — checked against
// the AWS documentation vector byte-for-byte.
func TestSigV4ResignEndToEnd(t *testing.T) {
	var gotAuth, gotHost, gotSHA string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotHost = r.Host
		gotSHA = r.Header.Get("X-Amz-Content-Sha256")
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	p := startProxy(t, Rules{AllowedDomains: []string{"iam.amazonaws.com"}},
		strings.TrimPrefix(upstream.URL, "https://"), true)
	p.AWSPairs.Register("fake_value_awskey", awsTestKey, awsTestSecret, "", []string{"iam.amazonaws.com"})

	client := proxyClient(p, &tls.Config{RootCAs: p.CA.Pool()})
	req, _ := http.NewRequest(http.MethodGet,
		"https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set("X-Amz-Date", "20150830T123600Z")
	req.Header.Set("Authorization", sigv4SentinelAuth("fake_value_awskey",
		"content-type;host;x-amz-date", strings.Repeat("0", 64)))

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET through MITM proxy: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("upstream status %d — request should have been forwarded", resp.StatusCode)
	}
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, " +
		"SignedHeaders=content-type;host;x-amz-date, Signature=" + awsTestSig
	if gotAuth != want {
		t.Fatalf("upstream Authorization mismatch:\n got %q\nwant %q", gotAuth, want)
	}
	if gotHost != "iam.amazonaws.com" {
		t.Fatalf("upstream Host = %q", gotHost)
	}
	if gotSHA != "" {
		t.Fatalf("unexpected content-sha header: %q", gotSHA)
	}
}

// Streaming requests default to deny: the MITM path 403s with real's
// policy text instead of forwarding a doomed signature.
func TestSigV4StreamingDeniedEndToEnd(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("streaming request reached upstream")
	}))
	defer upstream.Close()
	p := startProxy(t, Rules{AllowedDomains: []string{"s3.amazonaws.com"}},
		strings.TrimPrefix(upstream.URL, "https://"), true)
	p.AWSPairs.Register("fake_value_awskey", awsTestKey, awsTestSecret, "", []string{"s3.amazonaws.com"})

	client := proxyClient(p, &tls.Config{RootCAs: p.CA.Pool()})
	req, _ := http.NewRequest(http.MethodPut, "https://s3.amazonaws.com/b/k", strings.NewReader("x"))
	req.Header.Set("X-Amz-Date", "20150830T123600Z")
	req.Header.Set("X-Amz-Content-Sha256", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD")
	req.Header.Set("Authorization", sigv4SentinelAuth("fake_value_awskey",
		"host;x-amz-date", strings.Repeat("0", 64)))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PUT through MITM proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d (%q)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "cannot be re-signed by the sandbox proxy") {
		t.Fatalf("deny body = %q", body)
	}
}
