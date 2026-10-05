package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
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
)

func envTruthy(name string) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	return v != "" && v != "0" && v != "false" && v != "no"
}

func envOr(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// DetectProvider reads the CLAUDE_CODE_USE_* env selectors, in the same
// precedence order as the real client (mod_0068.js He()).
func DetectProvider() Provider {
	switch {
	case envTruthy("CLAUDE_CODE_USE_GATEWAY"):
		return ProviderGateway
	case envTruthy("CLAUDE_CODE_USE_BEDROCK"):
		return ProviderBedrock
	case envTruthy("CLAUDE_CODE_USE_FOUNDRY"):
		return ProviderFoundry
	case envTruthy("CLAUDE_CODE_USE_VERTEX"):
		return ProviderVertex
	default:
		return ProviderFirstParty
	}
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
	default: // firstParty + gateway both honor ANTHROPIC_BASE_URL
		if b := os.Getenv("ANTHROPIC_BASE_URL"); b != "" {
			return strings.TrimRight(b, "/")
		}
		return "https://api.anthropic.com"
	}
}

// isMessagesPath reports whether path is one the provider SDKs rewrite
// (mod_0899.js: "/v1/complete", "/v1/messages", "/v1/messages?beta=true").
func isMessagesPath(path string) bool {
	p := strings.SplitN(path, "?", 2)[0]
	return p == "/v1/messages" || p == "/v1/complete"
}

// rewriteRequest applies the provider backend middleware: path rewrite +
// body mutation. Returns the full request URL and (possibly mutated) body.
func (c *Client) rewriteRequest(path string, raw []byte) (string, []byte, error) {
	if len(raw) == 0 || !isMessagesPath(path) {
		return c.BaseURL + path, raw, nil
	}
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
		o["anthropic_beta"] = splitBetas(c.betaHeaderValue())
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
	full := c.baseSchemeHost() + prefixPath(c.BaseURL) + "/model/" + url.PathEscape(model) + suffix
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
	project := envOr("ANTHROPIC_VERTEX_PROJECT_ID", "GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT")
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
	default: // firstParty, gateway — claude.ai creds
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
