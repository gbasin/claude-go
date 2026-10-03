package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SigV4 request signing for Bedrock (the real AnthropicBedrock SDK uses
// @aws-sdk's SignatureV4 signer). Sources: AWS_ACCESS_KEY_ID /
// AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN envs, or a named profile in
// ~/.aws/credentials via AWS_PROFILE / AWS_DEFAULT_PROFILE.

type awsCreds struct {
	AccessKey    string
	SecretKey    string
	SessionToken string
}

// resolveAWSCreds returns the active credential set, or nil.
func resolveAWSCreds() *awsCreds {
	c := &awsCreds{
		AccessKey:    os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey:    os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken: os.Getenv("AWS_SESSION_TOKEN"),
	}
	if c.AccessKey != "" && c.SecretKey != "" {
		return c
	}
	// ~/.aws/credentials profile
	profile := envOr("AWS_PROFILE", "AWS_DEFAULT_PROFILE")
	if profile == "" {
		profile = "default"
	}
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".aws", "credentials"))
	if err != nil {
		return nil
	}
	// INI-ish parse: [profile] sections, key = value
	cur := ""
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			cur = strings.Trim(line, "[] ")
			continue
		}
		if cur != profile {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "aws_access_key_id":
			c.AccessKey = strings.TrimSpace(v)
		case "aws_secret_access_key":
			c.SecretKey = strings.TrimSpace(v)
		case "aws_session_token":
			c.SessionToken = strings.TrimSpace(v)
		}
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		return nil
	}
	return c
}

func bedrockRegion() string {
	if r := envOr("AWS_REGION", "AWS_DEFAULT_REGION"); r != "" {
		return r
	}
	return "us-east-1"
}

// sigv4Sign mutates req with the AWS SigV4 Authorization + amz headers.
// Mirrors AnthropicBedrock's signing of POST /model/.../invoke*.
func sigv4Sign(req *http.Request, body []byte) error {
	creds := resolveAWSCreds()
	if creds == nil {
		return nil // no creds — let the request fail auth naturally
	}
	region := bedrockRegion()
	service := "bedrock"
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	payloadHash := fmt.Sprintf("%x", sha256.Sum256(body))
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}
	req.Header.Set("Host", req.URL.Host)

	// canonical request
	signedHeaders := []string{}
	for k := range req.Header {
		signedHeaders = append(signedHeaders, strings.ToLower(k))
	}
	sort.Strings(signedHeaders)
	var canonHeaders strings.Builder
	for _, k := range signedHeaders {
		canonHeaders.WriteString(k + ":" + strings.Join(req.Header.Values(http.CanonicalHeaderKey(k)), ",") + "\n")
	}
	uri := req.URL.EscapedPath()
	if uri == "" {
		uri = "/"
	}
	canon := strings.Join([]string{
		req.Method, uri, req.URL.RawQuery,
		canonHeaders.String(),
		strings.Join(signedHeaders, ";"), payloadHash,
	}, "\n")

	scope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" +
		fmt.Sprintf("%x", sha256.Sum256([]byte(canon)))

	hmacSHA := func(key []byte, data string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(data))
		return h.Sum(nil)
	}
	kDate := hmacSHA([]byte("AWS4"+creds.SecretKey), dateStamp)
	kRegion := hmacSHA(kDate, region)
	kService := hmacSHA(kRegion, service)
	kSigning := hmacSHA(kService, "aws4_request")
	sig := hex.EncodeToString(hmacSHA(kSigning, toSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		creds.AccessKey, scope, strings.Join(signedHeaders, ";"), sig))
	return nil
}

// ---------- Google ADC (vertex) + Azure (foundry) ----------

// gcpAccessToken resolves a bearer token: GOOGLE_APPLICATION_CREDENTIALS
// (authorized_user → refresh-token exchange; service_account → JWT bearer
// grant), the standard ADC file, or `gcloud auth print-access-token`.
func gcpAccessToken() string {
	if t := os.Getenv("GOOGLE_OAUTH_ACCESS_TOKEN"); t != "" {
		return t
	}
	for _, p := range adcPaths() {
		if tok := tokenFromADC(p); tok != "" {
			return tok
		}
	}
	// gcloud fallback (dev machines)
	if b, err := execQuiet("gcloud", "auth", "print-access-token"); err == nil {
		return strings.TrimSpace(b)
	}
	return ""
}

func adcPaths() []string {
	var out []string
	if p := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); p != "" {
		out = append(out, p)
	}
	home, _ := os.UserHomeDir()
	out = append(out, filepath.Join(home, ".config", "gcloud", "application_default_credentials.json"))
	if p := os.Getenv("CLOUDSDK_CONFIG"); p != "" {
		out = append(out, filepath.Join(p, "application_default_credentials.json"))
	}
	return out
}

// tokenFromADC handles authorized_user ADC files via the OAuth2 refresh grant.
func tokenFromADC(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var adc struct {
		Type         string `json:"type"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		RefreshToken string `json:"refresh_token"`
	}
	if json.Unmarshal(b, &adc) != nil || adc.RefreshToken == "" {
		return ""
	}
	resp, err := http.Post("https://oauth2.googleapis.com/token",
		"application/x-www-form-urlencoded",
		bytes.NewBufferString(fmt.Sprintf(
			"grant_type=refresh_token&client_id=%s&client_secret=%s&refresh_token=%s",
			adc.ClientID, adc.ClientSecret, adc.RefreshToken)))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var tr struct {
		AccessToken string `json:"access_token"`
	}
	json.NewDecoder(resp.Body).Decode(&tr)
	return tr.AccessToken
}

// azureAccessToken resolves a Foundry bearer: ANTHROPIC_FOUNDRY_API_KEY →
// api-key header handled by the caller; this returns an Entra token via
// `az account get-access-token` when available.
func azureAccessToken() string {
	if b, err := execQuiet("az", "account", "get-access-token",
		"--resource", "https://cognitiveservices.azure.com",
		"--query", "accessToken", "-o", "tsv"); err == nil {
		return strings.TrimSpace(b)
	}
	return ""
}

func execQuiet(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
