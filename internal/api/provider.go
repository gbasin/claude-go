package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Provider identifies which backend the client talks to. Detection mirrors
// Claude Code's He() precedence (mod_0068.js): gateway, then bedrock, foundry,
// vertex, then first-party Anthropic.
type Provider string

const (
	ProviderFirstParty Provider = "firstParty"
	ProviderBedrock    Provider = "bedrock"
	ProviderVertex     Provider = "vertex"
	ProviderFoundry    Provider = "foundry"
	ProviderGateway    Provider = "gateway"
	// Claude Platform providers (mod_0046.js Pe()): anthropicAws is the
	// Anthropic-API-shape endpoint on AWS; anthropicGoogleCloud is the
	// claude.googleapis.com workspace endpoint; mantle is Bedrock Mantle.
	ProviderAnthropicAWS         Provider = "anthropicAws"
	ProviderAnthropicGoogleCloud Provider = "anthropicGoogleCloud"
	ProviderMantle               Provider = "mantle"
)

// envTruthy mirrors real's Le() (chunk-42f9cy1h / mod_0023.js): an env var
// is on iff its trimmed lowercase value is 1/true/yes/on. Anything else —
// including other non-empty strings — is off.
func envTruthy(name string) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func envOr(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// envTrim mirrors real's cy()/f() reader: trimmed, empty -> unset.
func envTrim(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

// DetectProvider reads the CLAUDE_CODE_USE_* env selectors, in the same
// precedence order as the real client (mod_0046.js Pe()).
func DetectProvider() Provider {
	switch {
	case envTruthy("CLAUDE_CODE_USE_GATEWAY"):
		return ProviderGateway
	case envTruthy("CLAUDE_CODE_USE_BEDROCK"):
		return ProviderBedrock
	case envTruthy("CLAUDE_CODE_USE_FOUNDRY"):
		return ProviderFoundry
	case envTruthy("CLAUDE_CODE_USE_ANTHROPIC_AWS"):
		return ProviderAnthropicAWS
	case envTruthy("CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD"):
		return ProviderAnthropicGoogleCloud
	case envTruthy("CLAUDE_CODE_USE_MANTLE"):
		return ProviderMantle
	case envTruthy("CLAUDE_CODE_USE_VERTEX"):
		return ProviderVertex
	default:
		return ProviderFirstParty
	}
}

// IsFirstPartyBaseURL mirrors real's pi() (mod_0046.js): true when the
// session should be treated as talking to api.anthropic.com — either the
// base URL is unset or points at api.anthropic.com, or the operator
// asserted first-party treatment via _CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL.
func IsFirstPartyBaseURL() bool {
	if envTruthy("_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL") {
		return true
	}
	b := os.Getenv("ANTHROPIC_BASE_URL")
	if b == "" {
		return true
	}
	u, err := url.Parse(b)
	if err != nil {
		return false
	}
	return u.Host == "api.anthropic.com"
}

// Bedrock/Vertex body constants from the JS SDKs (mod_0899.js, mod_0896.js).
const (
	bedrockAnthropicVersion = "bedrock-2023-05-31"
	vertexAnthropicVersion  = "vertex-2023-10-16"
)

// resolveBaseURL computes the endpoint for the active provider.
func resolveBaseURL(p Provider) string {
	switch p {
	case ProviderBedrock:
		region := envOr("AWS_REGION", "AWS_DEFAULT_REGION")
		if region == "" {
			region = "us-east-1" // SDK default (mod_0899.js)
		}
		if b := os.Getenv("ANTHROPIC_BEDROCK_BASE_URL"); b != "" {
			return strings.TrimRight(b, "/")
		}
		fips := ""
		if strings.HasPrefix(region, "us-gov-") {
			fips = "-fips"
		}
		return fmt.Sprintf("https://bedrock-runtime%s.%s.amazonaws.com", fips, region)
	case ProviderVertex:
		if b := os.Getenv("ANTHROPIC_VERTEX_BASE_URL"); b != "" {
			return strings.TrimRight(b, "/")
		}
		// Region may be unset here; vertex requests error lazily if so.
		switch r := os.Getenv("CLOUD_ML_REGION"); r {
		case "global":
			return "https://aiplatform.googleapis.com/v1"
		case "us":
			return "https://aiplatform.us.rep.googleapis.com/v1"
		case "eu":
			return "https://aiplatform.eu.rep.googleapis.com/v1"
		case "":
			return "https://aiplatform.googleapis.com/v1"
		default:
			return fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1", r)
		}
	case ProviderFoundry:
		if b := os.Getenv("ANTHROPIC_FOUNDRY_BASE_URL"); b != "" {
			return strings.TrimRight(b, "/")
		}
		if res := os.Getenv("ANTHROPIC_FOUNDRY_RESOURCE"); res != "" {
			// AnthropicFoundry SDK: https://{resource}.services.ai.azure.com/anthropic/
			return fmt.Sprintf("https://%s.services.ai.azure.com/anthropic", res)
		}
		return "https://api.anthropic.com" // will fail validation at request time
	case ProviderAnthropicAWS:
		// AnthropicAws (mod_0946.js): ANTHROPIC_AWS_BASE_URL or the
		// regional Claude-Platform-on-AWS host. No region and no base URL
		// fails lazily at request time.
		if b := os.Getenv("ANTHROPIC_AWS_BASE_URL"); b != "" {
			return strings.TrimRight(b, "/")
		}
		if r := awsRegionAny(); r != "" {
			return fmt.Sprintf("https://aws-external-anthropic.%s.api.aws", r)
		}
		return ""
	case ProviderAnthropicGoogleCloud:
		// AnthropicGoogleCloud (mod_0947.js): ANTHROPIC_GOOGLE_CLOUD_BASE_URL
		// or the per-workspace invoke URL on claude.googleapis.com.
		if b := os.Getenv("ANTHROPIC_GOOGLE_CLOUD_BASE_URL"); b != "" {
			return strings.TrimRight(b, "/")
		}
		if p := gcpCloudProject(); p != "" {
			if ws := envTrim("ANTHROPIC_GOOGLE_CLOUD_WORKSPACE_ID"); ws != "" {
				loc := envTrim("ANTHROPIC_GOOGLE_CLOUD_LOCATION")
				if loc == "" {
					loc = "global"
				}
				return fmt.Sprintf("https://claude.googleapis.com/v1alpha/projects/%s/locations/%s/workspaces/%s/invoke",
					p, loc, ws)
			}
		}
		return "https://claude.googleapis.com"
	case ProviderMantle:
		// AnthropicBedrockMantle (mod_0950.js): ANTHROPIC_BEDROCK_MANTLE_BASE_URL
		// or the regional bedrock-mantle host (mod_0304.js X9 — CC passes
		// baseURL explicitly, so the SDK's /anthropic suffix doesn't apply).
		if b := os.Getenv("ANTHROPIC_BEDROCK_MANTLE_BASE_URL"); b != "" {
			return strings.TrimRight(b, "/")
		}
		if r := awsRegionAny(); r != "" {
			return fmt.Sprintf("https://bedrock-mantle.%s.api.aws", r)
		}
		return ""
	default: // firstParty + gateway — ANTHROPIC_BASE_URL wins (mod_0304 Zd())
		if b := os.Getenv("ANTHROPIC_BASE_URL"); b != "" {
			return strings.TrimRight(b, "/")
		}
		if b := os.Getenv("CLAUDE_CODE_API_BASE_URL"); b != "" {
			return strings.TrimRight(b, "/")
		}
		return "https://api.anthropic.com"
	}
}

// awsRegionAny resolves the AWS region: AWS_REGION / AWS_DEFAULT_REGION,
// then ~/.aws/config for the AWS_PROFILE (or default) profile — the SDK's
// region fallback chain (mod_0946.js _resolveRegionFromConfig).
func awsRegionAny() string {
	if r := envOr("AWS_REGION", "AWS_DEFAULT_REGION"); r != "" {
		return r
	}
	return awsConfigRegion()
}

func awsConfigRegion() string {
	cfg := envOr("AWS_CONFIG_FILE")
	if cfg == "" {
		home, _ := os.UserHomeDir()
		cfg = filepath.Join(home, ".aws", "config")
	}
	b, err := os.ReadFile(cfg)
	if err != nil {
		return ""
	}
	profile := envOr("AWS_PROFILE", "AWS_DEFAULT_PROFILE")
	if profile == "" {
		profile = "default"
	}
	// ~/.aws/config names non-default sections "[profile NAME]"
	want := "default"
	if profile != "default" {
		want = "profile " + profile
	}
	cur := ""
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			cur = strings.TrimSpace(strings.Trim(line, "[]"))
			continue
		}
		if cur != want {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == "region" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// gcpCloudProject is AnthropicGoogleCloud's project resolution
// (mod_0947.js): ANTHROPIC_GOOGLE_CLOUD_PROJECT ?? GOOGLE_CLOUD_PROJECT.
func gcpCloudProject() string {
	return envOr("ANTHROPIC_GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_PROJECT")
}

// gcpPathEnvGate mirrors real's Ws() (mod_0304.js): the GCP path-segment
// envs are interpolated into the claude.googleapis.com request path, so
// URL metacharacters in them would rewrite what the request targets.
var gcpPathSegmentRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func gcpPathEnvGate() error {
	for _, name := range []string{
		"ANTHROPIC_GOOGLE_CLOUD_PROJECT", "ANTHROPIC_GOOGLE_CLOUD_LOCATION",
		"ANTHROPIC_GOOGLE_CLOUD_WORKSPACE_ID", "GCLOUD_PROJECT",
		"GOOGLE_CLOUD_PROJECT", "gcloud_project", "google_cloud_project",
	} {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			continue
		}
		if !gcpPathSegmentRe.MatchString(v) {
			return fmt.Errorf("%s may only contain letters, digits, hyphens, and underscores — "+
				"it is interpolated into the claude.googleapis.com request path, so URL "+
				"metacharacters in its value would rewrite the project/workspace the request "+
				"targets. Unset it or correct its value", name)
		}
	}
	return nil
}

// isMessagesPath reports whether path is one the provider SDKs rewrite
// (mod_0899.js: "/v1/complete", "/v1/messages", "/v1/messages?beta=true").
func isMessagesPath(path string) bool {
	p := strings.SplitN(path, "?", 2)[0]
	return p == "/v1/messages" || p == "/v1/complete"
}

// mergeExtraBody folds CLAUDE_CODE_EXTRA_BODY into the request body
// (mod_0416.js cY()): a JSON object shallow-merged over the generated
// fields. "betas"/"anthropic_beta" string values are comma-split, trimmed,
// and afk-mode entries dropped; a JSON-encoded metadata.user_id loses its
// "tk" key before it goes on the wire.
func mergeExtraBody(raw []byte) []byte {
	v := os.Getenv("CLAUDE_CODE_EXTRA_BODY")
	if v == "" {
		return raw
	}
	var extra map[string]any
	if err := json.Unmarshal([]byte(v), &extra); err != nil || extra == nil {
		return raw
	}
	for _, k := range []string{"betas", "anthropic_beta"} {
		b, ok := extra[k]
		if !ok {
			continue
		}
		if sv, ok := b.(string); ok {
			var parts []string
			for _, p := range strings.Split(sv, ",") {
				if p = strings.TrimSpace(p); p != "" && !strings.Contains(p, "afk-mode") {
					parts = append(parts, p)
				}
			}
			if len(parts) > 0 {
				extra[k] = parts
			} else {
				delete(extra, k)
			}
		}
	}
	if m, ok := extra["metadata"].(map[string]any); ok {
		if uid, ok := m["user_id"].(string); ok {
			var u map[string]any
			if json.Unmarshal([]byte(uid), &u) == nil {
				if _, has := u["tk"]; has {
					delete(u, "tk")
					if nb, err := json.Marshal(u); err == nil {
						m["user_id"] = string(nb)
					}
				}
			}
		}
	}
	var base map[string]any
	if err := json.Unmarshal(raw, &base); err != nil || base == nil {
		return raw
	}
	for k, v := range extra {
		base[k] = v
	}
	out, err := json.Marshal(base)
	if err != nil {
		return raw
	}
	return out
}

// rewriteRequest applies the provider backend middleware: path rewrite +
// body mutation. Returns the full request URL and (possibly mutated) body.
func (c *Client) rewriteRequest(path string, raw []byte) (string, []byte, error) {
	if len(raw) == 0 || !isMessagesPath(path) {
		return c.BaseURL + path, raw, nil
	}
	raw = mergeExtraBody(raw)
	switch c.Provider {
	case ProviderBedrock:
		return c.rewriteBedrock(raw)
	case ProviderVertex:
		return c.rewriteVertex(raw)
	default:
		return c.BaseURL + path, raw, nil
	}
}

// rewriteBedrock mirrors AnthropicBedrock's backendMiddleware (mod_0899.js):
// body gains anthropic_version + anthropic_beta, model/stream move into the
// path /model/{model}/invoke[-with-response-stream], ?beta is dropped.
func (c *Client) rewriteBedrock(raw []byte) (string, []byte, error) {
	var o map[string]any
	if err := json.Unmarshal(raw, &o); err != nil || o == nil {
		return "", raw, nil
	}
	if _, ok := o["anthropic_version"]; !ok {
		o["anthropic_version"] = bedrockAnthropicVersion
	}
	if _, ok := o["anthropic_beta"]; !ok {
		// The effective anthropic-beta header — including --betas /
		// ANTHROPIC_BETAS, which ride ANTHROPIC_CUSTOM_HEADERS — becomes the
		// body's anthropic_beta list (mod_0899.js: s.get("anthropic-beta")).
		beta := splitBetas(c.betaHeaderValue())
		for _, line := range strings.Split(os.Getenv("ANTHROPIC_CUSTOM_HEADERS"), "\n") {
			name, val, ok := strings.Cut(line, ":")
			if ok && strings.EqualFold(strings.TrimSpace(name), "anthropic-beta") {
				beta = splitBetas(strings.TrimSpace(val))
			}
		}
		o["anthropic_beta"] = beta
	}
	model, _ := o["model"].(string)
	if model == "" {
		return "", nil, errors.New("bedrock: request body missing model")
	}
	delete(o, "model")
	stream, _ := o["stream"].(bool)
	delete(o, "stream")
	suffix := "/invoke"
	if stream {
		suffix = "/invoke-with-response-stream"
	}
	out, err := json.Marshal(o)
	if err != nil {
		return "", nil, err
	}
	base := c.BaseURL
	// Xh(model, region) (mod_0304.js): when the request targets the
	// small/fast model and no explicit ANTHROPIC_BEDROCK_BASE_URL is set,
	// ANTHROPIC_SMALL_FAST_MODEL_AWS_REGION overrides the regional host.
	if sfr := envTrim("ANTHROPIC_SMALL_FAST_MODEL_AWS_REGION"); sfr != "" &&
		os.Getenv("ANTHROPIC_BEDROCK_BASE_URL") == "" {
		sfm := envOr("ANTHROPIC_SMALL_FAST_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL")
		if sfm == "" {
			sfm = "claude-haiku-4-5-20251001"
		}
		if model == sfm {
			base = "https://bedrock-runtime." + sfr + ".amazonaws.com"
		}
	}
	full := strings.TrimRight(base, "/") + "/model/" + url.PathEscape(model) + suffix
	return full, out, nil
}

// rewriteVertex mirrors AnthropicVertex's backendMiddleware (mod_0896.js):
// body gains anthropic_version, model/stream move into
// /projects/{p}/locations/{r}/publishers/anthropic/models/{m}:{rawPredict|streamRawPredict}.
func (c *Client) rewriteVertex(raw []byte) (string, []byte, error) {
	var o map[string]any
	if err := json.Unmarshal(raw, &o); err != nil || o == nil {
		return "", raw, nil
	}
	project := envOr("GCLOUD_PROJECT", "GOOGLE_CLOUD_PROJECT", "gcloud_project",
		"google_cloud_project", "ANTHROPIC_VERTEX_PROJECT_ID")
	region := os.Getenv("CLOUD_ML_REGION")
	if project == "" {
		return "", nil, errors.New("vertex: ANTHROPIC_VERTEX_PROJECT_ID is not set")
	}
	if region == "" {
		return "", nil, errors.New("vertex: CLOUD_ML_REGION is not set")
	}
	if _, ok := o["anthropic_version"]; !ok {
		o["anthropic_version"] = vertexAnthropicVersion
	}
	model, _ := o["model"].(string)
	if model == "" {
		return "", nil, errors.New("vertex: request body missing model")
	}
	delete(o, "model")
	stream, _ := o["stream"].(bool)
	delete(o, "stream")
	op := "rawPredict"
	if stream {
		op = "streamRawPredict"
	}
	newPath := fmt.Sprintf("%s/projects/%s/locations/%s/publishers/anthropic/models/%s:%s",
		prefixPath(c.BaseURL), project, region, model, op)
	out, err := json.Marshal(o)
	if err != nil {
		return "", nil, err
	}
	return c.baseSchemeHost() + newPath, out, nil
}

// prefixPath returns BaseURL's path component ("" if none), so provider
// rewrites preserve any path prefix in a custom base URL.
func prefixPath(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return strings.TrimRight(u.Path, "/")
}

// baseSchemeHost returns scheme://host of BaseURL for rewritten paths.
func (c *Client) baseSchemeHost() string {
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme == "" {
		return c.BaseURL
	}
	return u.Scheme + "://" + u.Host
}

// splitBetas splits a comma-separated anthropic-beta header value.
func splitBetas(s string) []string {
	var out []string
	for _, b := range strings.Split(s, ",") {
		if b = strings.TrimSpace(b); b != "" {
			out = append(out, b)
		}
	}
	return out
}

// applyAuth sets the Authorization / x-api-key headers for the provider and
// performs provider-specific header fixups. Called after generic headers and
// before ANTHROPIC_CUSTOM_HEADERS (so user headers can still override).
func (c *Client) applyAuth(req *http.Request) error {
	switch c.Provider {
	case ProviderBedrock:
		if envTruthy("CLAUDE_CODE_SKIP_BEDROCK_AUTH") {
			return nil
		}
		if t := strings.TrimSpace(os.Getenv("AWS_BEARER_TOKEN_BEDROCK")); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
			// SDK removes X-Api-Key unless the user supplied one via
			// default headers; we set none, so just drop it.
			req.Header.Del("X-Api-Key")
			return nil
		}
		// SigV4 (env keys or ~/.aws/credentials profile) signs in Do()
		// after headers are complete — nothing to set here when creds
		// resolve; without them the request would 401 opaquely.
		if resolveAWSCreds() == nil {
			return errors.New("bedrock: no AWS credentials; " +
				"set AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, AWS_BEARER_TOKEN_BEDROCK, " +
				"or CLAUDE_CODE_SKIP_BEDROCK_AUTH=1")
		}
		return nil
	case ProviderVertex:
		if envTruthy("CLAUDE_CODE_SKIP_VERTEX_AUTH") {
			return nil
		}
		tok, err := c.vertexBearerToken(req.Context())
		if err != nil {
			return err
		}
		if tok == "" {
			return errors.New("vertex: no Google credentials; " +
				"set GOOGLE_OAUTH_ACCESS_TOKEN, configure gcloud ADC, or set CLAUDE_CODE_SKIP_VERTEX_AUTH=1")
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		return nil
	case ProviderFoundry:
		if t := os.Getenv("ANTHROPIC_FOUNDRY_AUTH_TOKEN"); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
			return nil
		}
		if k := os.Getenv("ANTHROPIC_FOUNDRY_API_KEY"); k != "" {
			req.Header.Set("x-api-key", k)
			return nil
		}
		if envTruthy("CLAUDE_CODE_SKIP_FOUNDRY_AUTH") {
			return nil
		}
		return errors.New("foundry: set ANTHROPIC_FOUNDRY_API_KEY or ANTHROPIC_FOUNDRY_AUTH_TOKEN " +
			"(Azure AD token provider is not implemented)")
	case ProviderAnthropicAWS:
		// AnthropicAws (mod_0946.js): workspace-id header is mandatory
		// unless auth is skipped; api key -> x-api-key, else SigV4 with
		// service aws-external-anthropic.
		skip := envTruthy("CLAUDE_CODE_SKIP_ANTHROPIC_AWS_AUTH")
		if ws := envTrim("ANTHROPIC_AWS_WORKSPACE_ID"); ws != "" {
			req.Header.Set("anthropic-workspace-id", ws)
		} else if !skip {
			return errors.New("anthropic-aws: no workspace ID found. " +
				"Set the ANTHROPIC_AWS_WORKSPACE_ID environment variable " +
				"or CLAUDE_CODE_SKIP_ANTHROPIC_AWS_AUTH=1")
		}
		if skip {
			return nil
		}
		if k := envTrim("ANTHROPIC_AWS_API_KEY"); k != "" {
			req.Header.Set("x-api-key", k)
			return nil
		}
		if resolveAWSCreds() == nil {
			return errors.New("anthropic-aws: no AWS credentials; set ANTHROPIC_AWS_API_KEY, " +
				"configure AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, or set CLAUDE_CODE_SKIP_ANTHROPIC_AWS_AUTH=1")
		}
		return sigv4ProviderSign(req, "aws-external-anthropic", awsRegionAny())
	case ProviderAnthropicGoogleCloud:
		// AnthropicGoogleCloud (mod_0947.js): Google bearer token; the
		// path-segment envs are charset-gated because they land in the
		// request URL (mod_0304.js Ws()).
		if err := gcpPathEnvGate(); err != nil {
			return err
		}
		skip := envTruthy("CLAUDE_CODE_SKIP_ANTHROPIC_GOOGLE_CLOUD_AUTH")
		if !skip {
			if gcpCloudProject() == "" && os.Getenv("ANTHROPIC_GOOGLE_CLOUD_BASE_URL") == "" {
				return errors.New("anthropic-google-cloud: no project was given. " +
					"Set the ANTHROPIC_GOOGLE_CLOUD_PROJECT environment variable " +
					"(or provide ANTHROPIC_GOOGLE_CLOUD_BASE_URL)")
			}
			if envTrim("ANTHROPIC_GOOGLE_CLOUD_WORKSPACE_ID") == "" &&
				os.Getenv("ANTHROPIC_GOOGLE_CLOUD_BASE_URL") == "" {
				return errors.New("anthropic-google-cloud: no workspace ID found. " +
					"Set the ANTHROPIC_GOOGLE_CLOUD_WORKSPACE_ID environment variable")
			}
			tok, err := c.vertexBearerToken(req.Context())
			if err != nil {
				return err
			}
			if tok == "" {
				return errors.New("anthropic-google-cloud: no Google credentials; " +
					"set GOOGLE_OAUTH_ACCESS_TOKEN, configure gcloud ADC, or set " +
					"CLAUDE_CODE_SKIP_ANTHROPIC_GOOGLE_CLOUD_AUTH=1")
			}
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		return nil
	case ProviderMantle:
		// AnthropicBedrockMantle (mod_0950.js): AWS_BEARER_TOKEN_BEDROCK ->
		// Authorization Bearer (x-api-key is dropped unless user-supplied),
		// else SigV4 with service bedrock-mantle, else skipAuth.
		if envTruthy("CLAUDE_CODE_SKIP_MANTLE_AUTH") {
			return nil
		}
		if t := strings.TrimSpace(os.Getenv("AWS_BEARER_TOKEN_BEDROCK")); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
			req.Header.Del("X-Api-Key")
			return nil
		}
		if resolveAWSCreds() == nil {
			return errors.New("mantle: no AWS credentials; set AWS_BEARER_TOKEN_BEDROCK, " +
				"configure AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, or set CLAUDE_CODE_SKIP_MANTLE_AUTH=1")
		}
		return sigv4ProviderSign(req, "bedrock-mantle", awsRegionAny())
	case ProviderGateway:
		// The gateway JWT may arrive on an inherited fd
		// (CLAUDE_CODE_GATEWAY_TOKEN_FILE_DESCRIPTOR, mod_0072.js cred list);
		// otherwise the session's own creds apply.
		if t := readFDToken("CLAUDE_CODE_GATEWAY_TOKEN_FILE_DESCRIPTOR"); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
			return nil
		}
		return c.applyClaudeCreds(req)
	default: // firstParty — api key > workload identity > claude.ai creds
		if c.Creds != nil && c.Creds.IsAPIKey {
			tok, err := c.Creds.Token()
			if err != nil {
				return err
			}
			req.Header.Set("x-api-key", tok)
			return nil
		}
		// Workload identity federation (mod_0282.js / mod_0304.js): when no
		// API key is configured, an oidc_federation / user_oauth profile —
		// from the env-quad (ANTHROPIC_FEDERATION_RULE_ID +
		// ANTHROPIC_ORGANIZATION_ID) or <configDir>/configs/<profile>.json —
		// supplies the bearer token before claude.ai creds are consulted.
		ok, err := c.applyIdentityAuth(req)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		return c.applyClaudeCreds(req)
	}
}

// applyClaudeCreds sets the claude.ai credential (x-api-key or Bearer).
func (c *Client) applyClaudeCreds(req *http.Request) error {
	if c.Creds == nil {
		return errors.New("api: no credentials")
	}
	tok, err := c.Creds.Token()
	if err != nil {
		return err
	}
	if c.Creds.IsAPIKey {
		req.Header.Set("x-api-key", tok)
	} else {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return nil
}

// readFDToken reads a credential from an inherited file descriptor —
// real's fd creds pattern (CLAUDE_CODE_*_FILE_DESCRIPTOR, mod_0072.js).
func readFDToken(env string) string {
	fds := strings.TrimSpace(os.Getenv(env))
	if fds == "" {
		return ""
	}
	b, err := os.ReadFile("/dev/fd/" + fds)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// requestBody returns the request body bytes via GetBody (client.go builds
// requests from bytes.Reader, so GetBody is always populated).
func requestBody(req *http.Request) []byte {
	if req.GetBody == nil {
		return nil
	}
	rc, err := req.GetBody()
	if err != nil {
		return nil
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return b
}

// sigv4ProviderSign is sigv4Sign (awssign.go) parameterized by service and
// region — Claude Platform on AWS signs with service "aws-external-anthropic"
// and Bedrock Mantle with "bedrock-mantle" (mod_0946.js / mod_0950.js).
func sigv4ProviderSign(req *http.Request, service, region string) error {
	creds := resolveAWSCreds()
	if creds == nil {
		return errors.New("no AWS credentials to sign with")
	}
	if region == "" {
		return errors.New("no AWS region found. Set AWS_REGION / AWS_DEFAULT_REGION " +
			"or configure a region for your profile in ~/.aws/config")
	}
	body := requestBody(req)
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

// vertexBearerToken resolves a Google OAuth access token, best-effort:
// env vars first, then gcloud ADC / user tokens via the gcloud CLI.
// The real client uses GoogleAuth (full ADC: service-account keys, metadata
// server, etc.) — that is out of scope here.
func (c *Client) vertexBearerToken(ctx context.Context) (string, error) {
	if t := envOr("GOOGLE_OAUTH_ACCESS_TOKEN", "ANTHROPIC_VERTEX_AUTH_TOKEN"); t != "" {
		return t, nil
	}
	c.mu.Lock()
	if c.vertexToken != "" && time.Now().Before(c.vertexTokenExp) {
		t := c.vertexToken
		c.mu.Unlock()
		return t, nil
	}
	c.mu.Unlock()
	for _, args := range [][]string{
		{"auth", "application-default", "print-access-token"},
		{"auth", "print-access-token"},
	} {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		out, err := exec.CommandContext(cctx, "gcloud", args...).Output()
		cancel()
		if err == nil {
			if t := strings.TrimSpace(string(out)); t != "" {
				c.mu.Lock()
				c.vertexToken = t
				// gcloud tokens live ~1h; refresh early.
				c.vertexTokenExp = time.Now().Add(50 * time.Minute)
				c.mu.Unlock()
				return t, nil
			}
		}
	}
	return "", nil
}

// ---------- Anthropic workload identity (mod_0282.js / mod_0115.js) ----------
//
// When no API key is configured, the real client resolves credentials from
// the Anthropic config store: an env-quad profile
// (ANTHROPIC_FEDERATION_RULE_ID + ANTHROPIC_ORGANIZATION_ID, fed by
// ANTHROPIC_IDENTITY_TOKEN{_FILE}), or a profile config file at
// <ANTHROPIC_CONFIG_DIR|XDG_CONFIG_HOME/anthropic|~/.config/anthropic>/
// configs/<ANTHROPIC_PROFILE|active_config|default>.json. The identity token
// is exchanged for a scoped access token at <base>/v1/oauth/token with the
// oauth-2025-04-20 + oidc-federation-2026-04-01 betas.

type identityConfig struct {
	OrganizationID string `json:"organization_id"`
	WorkspaceID    string `json:"workspace_id"`
	BaseURL        string `json:"base_url"`
	Authentication struct {
		Type             string         `json:"type"` // "oidc_federation" | "user_oauth"
		FederationRuleID string         `json:"federation_rule_id"`
		ServiceAccountID string         `json:"service_account_id"`
		IdentityToken    *idTokenSource `json:"identity_token"`
		Scope            string         `json:"scope"`
		CredentialsPath  string         `json:"credentials_path"`
		ClientID         string         `json:"client_id"`
	} `json:"authentication"`
}

type idTokenSource struct {
	Source string `json:"source"` // only "file" is supported by this version
	Path   string `json:"path"`
}

// anthropicConfigDir is CF() (mod_0115.js): ANTHROPIC_CONFIG_DIR or the
// platform config dir. Trimmed; empty -> no identity store.
func anthropicConfigDir() string {
	if d := envTrim("ANTHROPIC_CONFIG_DIR"); d != "" {
		return d
	}
	if runtime.GOOS == "windows" {
		if a := envTrim("APPDATA"); a != "" {
			return filepath.Join(a, "Anthropic")
		}
		if u := envTrim("USERPROFILE"); u != "" {
			return filepath.Join(u, "AppData", "Roaming", "Anthropic")
		}
		return ""
	}
	if x := envTrim("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "anthropic")
	}
	if h := envTrim("HOME"); h != "" {
		return filepath.Join(h, ".config", "anthropic")
	}
	return ""
}

// anthropicProfile is A() (mod_0115.js): ANTHROPIC_PROFILE, then the
// <configDir>/active_config pointer file, then "default".
func anthropicProfile(dir string) string {
	if p := envTrim("ANTHROPIC_PROFILE"); p != "" {
		return p
	}
	if b, err := os.ReadFile(filepath.Join(dir, "active_config")); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	return "default"
}

// validProfile is H() (mod_0282.js): profile names are filesystem path
// segments — reject separators, dot-dirs, and non-portable characters.
var profileNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func validProfile(p string) error {
	if p == "" {
		return errors.New("profile name is empty")
	}
	if p == "." || p == ".." {
		return fmt.Errorf("profile name %q is not allowed", p)
	}
	if strings.ContainsAny(p, `/\`) {
		return fmt.Errorf("profile name %q must not contain path separators", p)
	}
	if !profileNameRe.MatchString(p) {
		return fmt.Errorf("profile name %q contains disallowed characters "+
			"(allowed: letters, digits, '_', '.', '-')", p)
	}
	return nil
}

// envQuadConfig synthesizes the env-quad identity config (mod_0115.js ke()).
func envQuadConfig() *identityConfig {
	rule, org := envTrim("ANTHROPIC_FEDERATION_RULE_ID"), envTrim("ANTHROPIC_ORGANIZATION_ID")
	if rule == "" || org == "" {
		return nil
	}
	c := &identityConfig{
		OrganizationID: org,
		WorkspaceID:    envTrim("ANTHROPIC_WORKSPACE_ID"),
		BaseURL:        envTrim("ANTHROPIC_BASE_URL"),
	}
	c.Authentication.Type = "oidc_federation"
	c.Authentication.FederationRuleID = rule
	c.Authentication.ServiceAccountID = envTrim("ANTHROPIC_SERVICE_ACCOUNT_ID")
	if f := envTrim("ANTHROPIC_IDENTITY_TOKEN_FILE"); f != "" {
		c.Authentication.IdentityToken = &idTokenSource{Source: "file", Path: f}
	}
	c.Authentication.Scope = envTrim("ANTHROPIC_SCOPE")
	return c
}

func readProfileAuthType(dir, profile string) string {
	b, err := os.ReadFile(filepath.Join(dir, "configs", profile+".json"))
	if err != nil {
		return ""
	}
	var c identityConfig
	if json.Unmarshal(b, &c) != nil {
		return ""
	}
	return c.Authentication.Type
}

// identitySource mirrors Et() (mod_0115.js): an explicit ANTHROPIC_PROFILE
// pins file-based resolution (env-quad ignored); otherwise the env-quad wins;
// otherwise a resolvable profile file is implicit.
func identitySource() string {
	dir := anthropicConfigDir()
	if p := envTrim("ANTHROPIC_PROFILE"); p != "" {
		if dir == "" || validProfile(p) != nil {
			return ""
		}
		if t := readProfileAuthType(dir, p); t == "oidc_federation" || t == "user_oauth" {
			return "profile-explicit"
		}
		return ""
	}
	if envQuadConfig() != nil {
		return "env-quad"
	}
	if dir != "" {
		if t := readProfileAuthType(dir, anthropicProfile(dir)); t == "oidc_federation" ||
			t == "user_oauth" {
			return "profile-implicit"
		}
	}
	return ""
}

// loadIdentityConfig mirrors ke() + O() (mod_0115.js / mod_0282.js):
// resolves the effective identity config per the source precedence, with
// env backfill of missing file fields.
func loadIdentityConfig() (*identityConfig, bool, error) {
	switch identitySource() {
	case "env-quad":
		return envQuadConfig(), false, nil
	case "profile-explicit", "profile-implicit":
	default:
		return nil, false, nil
	}
	dir := anthropicConfigDir()
	profile := anthropicProfile(dir)
	if err := validProfile(profile); err != nil {
		return nil, false, err
	}
	path := filepath.Join(dir, "configs", profile+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("failed to read config file %s: %v", path, err)
	}
	var c identityConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, false, fmt.Errorf("failed to parse config file %s: %v", path, err)
	}
	// Env backfill — each supplies only when the file field is empty
	// (config takes precedence).
	if c.OrganizationID == "" {
		c.OrganizationID = envTrim("ANTHROPIC_ORGANIZATION_ID")
	}
	if c.WorkspaceID == "" {
		c.WorkspaceID = envTrim("ANTHROPIC_WORKSPACE_ID")
	}
	if c.BaseURL == "" {
		c.BaseURL = envTrim("ANTHROPIC_BASE_URL")
	}
	a := &c.Authentication
	if a.Scope == "" {
		a.Scope = envTrim("ANTHROPIC_SCOPE")
	}
	if a.Type == "oidc_federation" {
		if a.IdentityToken == nil {
			if f := envTrim("ANTHROPIC_IDENTITY_TOKEN_FILE"); f != "" {
				a.IdentityToken = &idTokenSource{Source: "file", Path: f}
			}
		}
		if a.FederationRuleID == "" {
			a.FederationRuleID = envTrim("ANTHROPIC_FEDERATION_RULE_ID")
		}
		if a.ServiceAccountID == "" {
			a.ServiceAccountID = envTrim("ANTHROPIC_SERVICE_ACCOUNT_ID")
		}
	}
	if a.CredentialsPath == "" {
		a.CredentialsPath = filepath.Join(dir, "credentials", profile+".json")
	}
	switch a.Type {
	case "oidc_federation", "user_oauth":
	default:
		return nil, false, fmt.Errorf("authentication.type %q is not a known authentication type",
			a.Type)
	}
	return &c, true, nil
}

// identityToken reads the assertion for oidc_federation (mod_0282.js oe()):
// config identity_token{source:"file"} > ANTHROPIC_IDENTITY_TOKEN_FILE >
// ANTHROPIC_IDENTITY_TOKEN (inline).
func (c *identityConfig) identityToken() (string, error) {
	a := &c.Authentication
	if a.IdentityToken != nil {
		if a.IdentityToken.Source != "file" {
			return "", fmt.Errorf("identity_token.source %q is not supported by this "+
				"version (only \"file\")", a.IdentityToken.Source)
		}
		if a.IdentityToken.Path == "" {
			return "", errors.New(`identity_token.source "file" requires a non-empty path`)
		}
		tok, err := os.ReadFile(a.IdentityToken.Path)
		if err != nil {
			return "", fmt.Errorf("failed to read identity token at %s: %v", a.IdentityToken.Path, err)
		}
		if t := strings.TrimSpace(string(tok)); t != "" {
			return t, nil
		}
		return "", fmt.Errorf("identity token file at %s is empty", a.IdentityToken.Path)
	}
	if f := envTrim("ANTHROPIC_IDENTITY_TOKEN_FILE"); f != "" {
		tok, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("failed to read identity token at %s: %v", f, err)
		}
		if t := strings.TrimSpace(string(tok)); t != "" {
			return t, nil
		}
		return "", fmt.Errorf("identity token file at %s is empty", f)
	}
	if t := envTrim("ANTHROPIC_IDENTITY_TOKEN"); t != "" {
		return t, nil
	}
	return "", errors.New("oidc_federation config requires an identity token " +
		"(set authentication.identity_token, ANTHROPIC_IDENTITY_TOKEN_FILE, or " +
		"ANTHROPIC_IDENTITY_TOKEN)")
}

// ---------- token exchange (mod_0115.js X() / K()) ----------

var identityHTTPClient = &http.Client{Timeout: 10 * time.Second}

// identityBaseURL mirrors uAt's baseURL: ANTHROPIC_BASE_URL ||
// config.base_url || https://api.anthropic.com, https-only unless localhost.
func (c *identityConfig) identityBaseURL() (string, error) {
	u := envTrim("ANTHROPIC_BASE_URL")
	if u == "" {
		u = c.BaseURL
	}
	if u == "" {
		u = "https://api.anthropic.com"
	}
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return "", fmt.Errorf("identity base URL %q is not a valid URL", u)
	}
	if p.Scheme != "https" &&
		!(p.Hostname() == "localhost" || p.Hostname() == "127.0.0.1") {
		return "", fmt.Errorf("identity base URL %q must use https:// (http:// is only "+
			"allowed for localhost); set ANTHROPIC_BASE_URL or base_url to an https:// endpoint", u)
	}
	return strings.TrimRight(u, "/"), nil
}

type identityTokenResult struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expiresAt"` // unix seconds; 0 = no expiry
}

// exchangeOIDCToken is X() (mod_0115.js): JWT-bearer grant at
// /v1/oauth/token.
func (c *identityConfig) exchangeOIDCToken(ctx context.Context) (*identityTokenResult, error) {
	a := &c.Authentication
	assertion, err := c.identityToken()
	if err != nil {
		return nil, err
	}
	if a.FederationRuleID == "" {
		return nil, errors.New("oidc_federation config requires 'federation_rule_id'. " +
			"Set it in authentication.federation_rule_id in your profile, or via " +
			"ANTHROPIC_FEDERATION_RULE_ID (profile takes precedence).")
	}
	if c.OrganizationID == "" {
		return nil, errors.New("oidc_federation config requires organization_id " +
			"(set ANTHROPIC_ORGANIZATION_ID or config.organization_id)")
	}
	if len(assertion) > 16384 {
		return nil, fmt.Errorf("identity token is %d KiB, exceeds the 16 KiB "+
			"assertion limit", len(assertion)/1024)
	}
	base, err := c.identityBaseURL()
	if err != nil {
		return nil, err
	}
	payload := map[string]string{
		"grant_type":         "urn:ietf:params:oauth:grant-type:jwt-bearer",
		"assertion":          assertion,
		"federation_rule_id": a.FederationRuleID,
		"organization_id":    c.OrganizationID,
	}
	if a.ServiceAccountID != "" {
		payload["service_account_id"] = a.ServiceAccountID
	}
	if c.WorkspaceID != "" {
		payload["workspace_id"] = c.WorkspaceID
	}
	return identityTokenPost(ctx, base, payload,
		"oauth-2025-04-20,oidc-federation-2026-04-01")
}

func identityTokenPost(ctx context.Context, base string, payload map[string]string, beta string) (*identityTokenResult, error) {
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/v1/oauth/token", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", beta)
	req.Header.Set("User-Agent", "anthropic-sdk-typescript/0.128.0 oidcFederationProvider")
	res, err := identityHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		return nil, fmt.Errorf("token exchange failed with status %d: %s",
			res.StatusCode, body)
	}
	var b map[string]any
	if err := json.NewDecoder(res.Body).Decode(&b); err != nil {
		return nil, err
	}
	tok, _ := b["access_token"].(string)
	if tok == "" {
		return nil, errors.New("token exchange response is missing access_token")
	}
	var exp int64
	if e, ok := b["expires_in"].(float64); ok && e > 0 {
		exp = time.Now().Unix() + int64(e)
	}
	return &identityTokenResult{Token: tok, ExpiresAt: exp}, nil
}

// ---------- token caches (mod_0115.js se() / dAt) ----------

// federationCacheDir is pKe() (mod_0115.js): CLAUDE_CODE_FEDERATION_CACHE_DIR
// or <configDir>/credentials/federation.
func federationCacheDir() string {
	if d := envTrim("CLAUDE_CODE_FEDERATION_CACHE_DIR"); d != "" {
		return d
	}
	if d := anthropicConfigDir(); d != "" {
		return filepath.Join(d, "credentials", "federation")
	}
	return ""
}

type credentialsFile struct {
	AccessToken  string `json:"access_token"`
	ExpiresAt    int64  `json:"expires_at"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

// federationCacheKey mirrors se()'s cache filename — a hash over the config
// tuple + the identity token itself, so rotating assertions get fresh slots.
func federationCacheKey(c *identityConfig, assertion, base string) string {
	k, _ := json.Marshal([]string{
		c.Authentication.FederationRuleID, c.OrganizationID, c.WorkspaceID,
		c.Authentication.ServiceAccountID, c.Authentication.Scope, base,
		fmt.Sprintf("%x", sha256.Sum256([]byte(assertion))),
	})
	return fmt.Sprintf("%x", sha256.Sum256(k))
}

var (
	identMu    sync.Mutex
	identCache = map[string]*identityTokenResult{}
)

// oidcToken resolves (with caching) the bearer token for oidc_federation.
// Cache reuse window mirrors Nit (mod_0115.js): refresh when within 30s of
// expiry.
func (c *identityConfig) oidcToken(ctx context.Context) (string, error) {
	tok, err := c.identityToken()
	if err != nil {
		return "", err
	}
	base, err := c.identityBaseURL()
	if err != nil {
		return "", err
	}
	key := federationCacheKey(c, tok, base)
	fresh := func(t *identityTokenResult) bool {
		return t != nil && t.Token != "" &&
			(t.ExpiresAt == 0 || time.Now().Unix() < t.ExpiresAt-30)
	}
	identMu.Lock()
	if t := identCache[key]; fresh(t) {
		identMu.Unlock()
		return t.Token, nil
	}
	identMu.Unlock()
	if dir := federationCacheDir(); dir != "" {
		if b, err := os.ReadFile(filepath.Join(dir, key+".json")); err == nil {
			var f credentialsFile
			if json.Unmarshal(b, &f) == nil &&
				fresh(&identityTokenResult{Token: f.AccessToken, ExpiresAt: f.ExpiresAt}) {
				identMu.Lock()
				identCache[key] = &identityTokenResult{Token: f.AccessToken, ExpiresAt: f.ExpiresAt}
				identMu.Unlock()
				return f.AccessToken, nil
			}
		}
	}
	res, err := c.exchangeOIDCToken(ctx)
	if err != nil {
		return "", err
	}
	identMu.Lock()
	identCache[key] = res
	identMu.Unlock()
	if dir := federationCacheDir(); dir != "" {
		_ = os.MkdirAll(dir, 0o700)
		raw, _ := json.Marshal(credentialsFile{AccessToken: res.Token, ExpiresAt: res.ExpiresAt})
		_ = os.WriteFile(filepath.Join(dir, key+".json"), raw, 0o600)
	}
	return res.Token, nil
}

// userOAuthToken reads/refreshes the user_oauth credentials file
// (mod_0115.js K()).
func (c *identityConfig) userOAuthToken(ctx context.Context) (string, error) {
	a := &c.Authentication
	if a.CredentialsPath == "" {
		return "", errors.New("user_oauth config requires " +
			"authentication.credentials_path (or load via a profile so it " +
			"defaults to <config_dir>/credentials/<profile>.json)")
	}
	b, err := os.ReadFile(a.CredentialsPath)
	if err != nil {
		return "", fmt.Errorf("failed to read credentials at %s: %v", a.CredentialsPath, err)
	}
	var f credentialsFile
	if err := json.Unmarshal(b, &f); err != nil {
		return "", fmt.Errorf("failed to parse credentials at %s: %v", a.CredentialsPath, err)
	}
	if f.AccessToken != "" &&
		(f.ExpiresAt == 0 || time.Now().Unix() < f.ExpiresAt-30) {
		return f.AccessToken, nil
	}
	if f.RefreshToken == "" {
		return "", fmt.Errorf("access token at %s has expired and no refresh is available "+
			"(re-authenticate that profile)", a.CredentialsPath)
	}
	base, err := c.identityBaseURL()
	if err != nil {
		return "", err
	}
	payload := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": f.RefreshToken,
	}
	if a.ClientID != "" {
		payload["client_id"] = a.ClientID
	}
	res, err := identityTokenPost(ctx, base, payload, "oauth-2025-04-20")
	if err != nil {
		return "", err
	}
	// Persist the rotated credentials (mod_0115.js writes back on refresh).
	f.AccessToken = res.Token
	f.ExpiresAt = res.ExpiresAt
	if raw, err := json.Marshal(f); err == nil {
		_ = os.WriteFile(a.CredentialsPath, raw, 0o600)
	}
	return res.Token, nil
}

// applyIdentityAuth runs the workload-identity auth path when configured.
// Returns true when an Authorization header was set. Mirroring real's call
// site (mod_0304.js): only for the firstParty provider, and only when no
// API key is configured.
func (c *Client) applyIdentityAuth(req *http.Request) (bool, error) {
	if c.Provider != ProviderFirstParty {
		return false, nil
	}
	cfg, fromFile, err := loadIdentityConfig()
	if err != nil {
		return false, err
	}
	if cfg == nil {
		return false, nil
	}
	var tok string
	switch cfg.Authentication.Type {
	case "oidc_federation":
		tok, err = cfg.oidcToken(req.Context())
	case "user_oauth":
		if !fromFile {
			return false, errors.New("user_oauth config requires " +
				"authentication.credentials_path (or load via a profile so it " +
				"defaults to <config_dir>/credentials/<profile>.json)")
		}
		tok, err = cfg.userOAuthToken(req.Context())
	}
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	// real's extraHeaders: workspace id travels as a header for user_oauth
	// (for oidc_federation it goes into the token-exchange body).
	if cfg.Authentication.Type == "user_oauth" && cfg.WorkspaceID != "" {
		req.Header.Set("anthropic-workspace-id", cfg.WorkspaceID)
	}
	// The profile's base_url overrides the request destination when
	// ANTHROPIC_BASE_URL didn't pin it (mod_0304.js Wt).
	if cfg.BaseURL != "" && envTrim("ANTHROPIC_BASE_URL") == "" {
		if u, err := url.Parse(cfg.BaseURL); err == nil && u.Host != "" {
			req.URL.Scheme = u.Scheme
			req.URL.Host = u.Host
		}
	}
	return true, nil
}
