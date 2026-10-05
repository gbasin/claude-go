package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"claude-go/internal/auth"
)

const Version = "2.1.289"

// betaFlags in real's exact wire order (captured from claude 2.1.289
// default-model request through the mitm relay).
const betaFlags = "claude-code-20250219," +
	"oauth-2025-04-20," +
	"interleaved-thinking-2025-05-14," +
	"thinking-token-count-2026-05-13," +
	"context-management-2025-06-27," +
	"prompt-caching-scope-2026-01-05," +
	"mid-conversation-system-2026-04-07," +
	"per-turn-control-2026-07-01," +
	"mid-conversation-tool-changes-2026-07-01," +
	"advisor-tool-2026-03-01," +
	"advanced-tool-use-2025-11-20," +
	"mid-conversation-system-clear-at-2026-08-21," +
	"effort-2025-11-24," +
	"dangerous-tool-use-2026-09-03," +
	"thinking-binding-controls-2026-08-01," +
	"afk-mode-2026-01-31," +
	"extended-cache-ttl-2025-04-11," +
	"cache-diagnosis-2026-04-07"

// streamTimeout is the per-request deadline for streaming calls, matching
// the SDK's default timeout (X-Stainless-Timeout: 600). Applied to the
// request context, never to http.Client.Timeout — that would kill streams.
const streamTimeout = 600 * time.Second

// retryBackoff is the SDK backoff table (maxRetries=2 → 3 total attempts).
var retryBackoff = []time.Duration{1 * time.Second, 3 * time.Second, 9 * time.Second}

// Entrypoint mirrors cc_entrypoint in the billing header.
type Entrypoint string

const (
	EntryCLI    Entrypoint = "cli"
	EntrySDKCLI Entrypoint = "sdk-cli"
)

type Client struct {
	BaseURL    string
	HTTP       *http.Client
	Creds      *auth.Credentials // nil for bedrock/vertex/foundry
	SessionID  string
	Entrypoint Entrypoint
	Provider   Provider
	Model      string // drives model-conditional betas (1m family)
	Wants1m    bool   // [1m] suffix on configured model → context-1m beta
	RemoteEnv  bool   // eval hasRemoteEnvironment attr
	evalFailed bool   // eval returned empty features (fallback state)
	RetryCount int    // exposed for x-stainless-retry-count
	UnixSocket string // ANTHROPIC_UNIX_SOCKET path, when set

	mu              sync.Mutex
	RateLimit       *RateLimit // headers from the most recent response
	vertexToken     string
	vertexTokenExp  time.Time
	promptID        string
	bootstrap       map[string]any // /api/claude_cli/bootstrap payload
	accountSettings map[string]any // /api/oauth/account/settings payload
}

func NewClient(creds *auth.Credentials, sessionID string, ep Entrypoint) *Client {
	p := DetectProvider()
	tr := &http.Transport{
		MaxIdleConns:        8,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		DisableCompression:  false,
	}
	// fingerprint fidelity: first-party HTTPS calls go over utls + a raw
	// HTTP/1.1 writer so TLS ClientHello (BoringSSL-shaped) and header order
	// match real Bun byte-for-byte. CLAUDE_GO_RAW_HTTP=0 falls back.
	var rt http.RoundTripper = tr
	if p == ProviderFirstParty && os.Getenv("CLAUDE_GO_RAW_HTTP") != "0" {
		rt = routingTransport{raw: rawHTTP11RoundTripper{}, std: tr}
	}
	c := &Client{
		Provider:   p,
		BaseURL:    resolveBaseURL(p),
		Creds:      creds,
		SessionID:  sessionID,
		Entrypoint: ep,
		HTTP: &http.Client{
			Timeout:   0, // streaming: no overall timeout (deadline is per-request ctx)
			Transport: rt,
		},
	}
	// ANTHROPIC_UNIX_SOCKET: route every request over a local unix socket
	// (claude ssh remote / local forwarder). The URL host is retained so the
	// forwarder still sees the right Host header.
	if sock := os.Getenv("ANTHROPIC_UNIX_SOCKET"); sock != "" {
		d := &net.Dialer{}
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "unix", sock)
		}
		c.UnixSocket = sock
	}
	return c
}

// PromptID is the current turn's prompt uuid — same value real puts in
// the x-claude-code-prompt-id header and the billing sys[0] cc_prompt_id.
func (c *Client) PromptID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.promptID == "" {
		c.promptID = newUUID()
	}
	return c.promptID
}

// betaHeaderValue is the full anthropic-beta header/body list for this
// client. The 1m-context family (fable/sonnet-5/opus-5 — same predicate as
// the context window) adds context-1m at index 2 and per-turn-control at
// index 8; verified against live captures of both model classes.
func (c *Client) betaHeaderValue() string {
	betas := betaFlags
	c.mu.Lock()
	evalFailed := c.evalFailed
	c.mu.Unlock()
	if evalFailed {
		// eval-unreachable fallback (captured): the four default-on betas
		// appear, context-1m is absent (needs a positive flag)
		return "claude-code-20250219,oauth-2025-04-20," +
			"interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13," +
			"context-management-2025-06-27,prompt-caching-scope-2026-01-05," +
			"mid-conversation-system-2026-04-07,per-turn-control-2026-07-01," +
			"mid-conversation-tool-changes-2026-07-01,advisor-tool-2026-03-01," +
			"advanced-tool-use-2025-11-20,mid-conversation-system-clear-at-2026-08-21," +
			"effort-2025-11-24,dangerous-tool-use-2026-09-03," +
			"thinking-binding-controls-2026-08-01,afk-mode-2026-01-31," +
			"extended-cache-ttl-2025-04-11,cache-diagnosis-2026-04-07"
	}
	// context-1m needs a positive server flag — the captured wire shows it
	// absent even with a [1m]-configured model and eval unreachable.
	// TODO: emit when the bootstrap/eval flag confirms 1m availability.
	if c.Entrypoint == EntryCLI {
		betas += ",thinking-display-updates-2026-08-18"
	}
	// haiku-class requests carry the message-threads beta + thread field
	// (captured: haiku -p turn had both; fable/opus did not)
	if strings.Contains(c.Model, "haiku") {
		betas += ",message-threads-2026-08-12"
	}
	return betas
}

func is1mModel(m string) bool {
	return strings.Contains(m, "fable") || strings.Contains(m, "sonnet-5") ||
		strings.Contains(m, "opus-5")
}

func (c *Client) headers(req *http.Request) {
	h := req.Header
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	h.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	h.Set("Connection", "keep-alive")
	h.Set("User-Agent", "claude-cli/"+Version+" (external, "+string(c.Entrypoint)+")")
	h.Set("X-Claude-Code-Session-Id", c.SessionID)
	h.Set("X-Stainless-Arch", runtime.GOARCH)
	h.Set("X-Stainless-Lang", "js")
	switch runtime.GOOS {
	case "darwin":
		h.Set("X-Stainless-OS", "MacOS")
	case "linux":
		h.Set("X-Stainless-OS", "Linux")
	case "windows":
		h.Set("X-Stainless-OS", "Windows")
	default:
		h.Set("X-Stainless-OS", runtime.GOOS)
	}
	h.Set("X-Stainless-Package-Version", "0.128.0")
	h.Set("X-Stainless-Retry-Count", fmt.Sprint(c.RetryCount))
	h.Set("X-Stainless-Runtime", "node")
	h.Set("X-Stainless-Runtime-Version", "v26.3.0")
	h.Set("X-Stainless-Timeout", "600")
	// On Bedrock the beta list travels in the request body's anthropic_beta
	// field, not the header (mod_0899.js backendMiddleware).
	if c.Provider != ProviderBedrock {
		h.Set("anthropic-beta", c.betaHeaderValue())
	}
	h.Set("anthropic-dangerous-direct-browser-access", "true")
	h.Set("anthropic-version", "2023-06-01")
	h.Set("x-app", "cli")
	// provider auth: bedrock → SigV4 (in the caller — needs the body);
	// vertex → ADC bearer; foundry → api-key or Entra token
	switch c.Provider {
	case ProviderVertex:
		if h.Get("Authorization") == "" {
			if t := gcpAccessToken(); t != "" {
				h.Set("Authorization", "Bearer "+t)
			}
		}
	case ProviderFoundry:
		if k := os.Getenv("ANTHROPIC_FOUNDRY_API_KEY"); k != "" {
			h.Set("x-api-key", k)
		} else if t := azureAccessToken(); t != "" {
			h.Set("Authorization", "Bearer "+t)
		}
	}
	// Bedrock service tier passthrough (mod_0278.js).
	if c.Provider == ProviderBedrock {
		if st := os.Getenv("ANTHROPIC_BEDROCK_SERVICE_TIER"); st != "" {
			h.Set("X-Amzn-Bedrock-Service-Tier", st)
		}
	}
}

// applyCustomHeaders merges ANTHROPIC_CUSTOM_HEADERS (newline-separated
// "Name: value" pairs) onto the request. Applied last so it can override
// anything the client set.
func applyCustomHeaders(req *http.Request) {
	if ch := os.Getenv("ANTHROPIC_CUSTOM_HEADERS"); ch != "" {
		for _, line := range strings.FieldsFunc(ch, func(r rune) bool { return r == 10 || r == 13 }) {
			if k, v, ok := strings.Cut(line, ":"); ok {
				req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
			}
		}
	}
}

// retryAfterDelay parses retry-after-ms then Retry-After (seconds or HTTP
// date), matching WSe in mod_0386.js. ok=false means "fall back to backoff".
func retryAfterDelay(h http.Header, now time.Time) (time.Duration, bool) {
	if v := h.Get("retry-after-ms"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f != 0 {
			return time.Duration(f * float64(time.Millisecond)), true
		}
	}
	if s := h.Get("Retry-After"); s != "" {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return time.Duration(f * float64(time.Second)), true
		}
		if t, err := http.ParseTime(s); err == nil {
			return t.Sub(now), true
		}
	}
	return 0, false
}

// retryDelay returns the wait before the next attempt: a valid Retry-After
// value (clamped to int32-ms like the SDK) wins over the backoff table.
func retryDelay(h http.Header, attempt int) time.Duration {
	wait := retryBackoff[min(attempt, len(retryBackoff)-1)]
	if d, ok := retryAfterDelay(h, time.Now()); ok &&
		d > 0 && d <= time.Duration(math.MaxInt32)*time.Millisecond {
		return d
	}
	return wait
}

func (c *Client) setRateLimit(resp *http.Response) {
	rl := parseRateLimitHeaders(resp.Header)
	c.mu.Lock()
	c.RateLimit = rl
	c.mu.Unlock()
}

// LastRateLimit returns the unified rate-limit state from the most recent
// response, or nil if none was received.
func (c *Client) LastRateLimit() *RateLimit {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.RateLimit
}

// Do executes a request, retrying on 429/5xx with backoff like the SDK
// (maxRetries=2, i.e. up to 3 attempts; backoff 1s/3s/9s, overridden by a
// valid Retry-After / Retry-After-Ms response header).
func (c *Client) Do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if os.Getenv("CLAUDE_GO_DEBUG_REQ") != "" {
			if f, ferr := os.OpenFile("/tmp/reqs.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); ferr == nil {
				f.WriteString("=== " + path + "\n" + string(raw) + "\n")
				f.Close()
			}
		}
		if err != nil {
			return nil, err
		}
	}
	url, raw, err := c.rewriteRequest(path, raw)
	if err != nil {
		return nil, err
	}
	var lastErr error
	maxRetries := 2 // SDK default
	if v := os.Getenv("CLAUDE_CODE_MAX_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			maxRetries = n
		}
	}
	for attempt := 0; attempt <= maxRetries; attempt++ {
		c.RetryCount = attempt
		var rdr io.Reader
		if raw != nil {
			rdr = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rdr)
		if err != nil {
			return nil, err
		}
		c.headers(req)
		if err := c.applyAuth(req); err != nil {
			return nil, err
		}
		if c.Provider == ProviderBedrock {
			if err := sigv4Sign(req, raw); err != nil {
				return nil, err
			}
		}
		applyCustomHeaders(req)
		// real sets these on every main request (verified in wire
		// captures): dispatch-id is a constant, prompt-id is stable across
		// a turn's retries/subrequests, client-request-id is fresh per
		// request attempt.
		req.Header.Set("anthropic-dispatch-id", "v2d")
		class_ := "main"
		if r, ok := body.(*Request); ok && r.Class != "" {
			class_ = r.Class
		}
		req.Header.Set("x-claude-code-request-class", class_)
		req.Header.Set("x-claude-code-prompt-id", c.PromptID())
		req.Header.Set("x-client-request-id", newUUID())
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
		} else {
			c.setRateLimit(resp)
			// x-should-retry: server-side override suppresses/forces retries
			if sr := resp.Header.Get("x-should-retry"); sr == "false" {
				return nil, decodeErr(resp) // surface the real error body
			}
			if sr := resp.Header.Get("x-should-retry"); sr == "true" ||
				resp.StatusCode == 408 || resp.StatusCode == 409 ||
				resp.StatusCode == 429 || resp.StatusCode >= 500 {
				wait := retryDelay(resp.Header, attempt)
				resp.Body.Close()
				lastErr = &ErrAPI{
					E:          &APIError{Type: errorTypeForStatus(resp.StatusCode), Message: resp.Status},
					StatusCode: resp.StatusCode,
					Headers:    resp.Header,
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(wait):
				}
				continue
			}
			return resp, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryBackoff[min(attempt, len(retryBackoff)-1)]):
		}
	}
	return nil, lastErr
}

// withStreamDeadline applies the 600s request deadline unless the caller's
// context already carries one.
func withStreamDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, streamTimeout)
}

// decodeErr parses an error response body into ErrAPI.
func decodeErr(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if os.Getenv("CLAUDE_GO_DEBUG_HTTP") != "" {
		fmt.Fprintf(os.Stderr, "[debug] %d body: %.800s\n", resp.StatusCode, string(b))
	}
	var e struct {
		Error APIError `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && (e.Error.Message != "" || e.Error.Type != "") {
		if e.Error.Type == "" {
			e.Error.Type = errorTypeForStatus(resp.StatusCode)
		}
		return &ErrAPI{E: &e.Error, StatusCode: resp.StatusCode, Headers: resp.Header}
	}
	return &ErrAPI{
		E:          &APIError{Type: errorTypeForStatus(resp.StatusCode), Message: string(b)},
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
	}
}

// StreamMessage POSTs /v1/messages?beta=true with stream=true and invokes
// onEvent per decoded SSE event. Returns the assembled final Response.
// The request carries a 600s deadline unless ctx already has one.
func (c *Client) StreamMessage(ctx context.Context, req *Request, onEvent func(*StreamEvent) error) (*Response, error) {
	req.Stream = true
	ctx, cancel := withStreamDeadline(ctx)
	defer cancel()
	resp, err := c.Do(ctx, "POST", "/v1/messages?beta=true", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, decodeErr(resp)
	}

	// Assemble streamed content blocks.
	var out Response
	var curBlocks []ContentBlock
	var jsonBuf = map[int]*bytes.Buffer{}
	err = StreamEvents(resp.Body, func(ev *StreamEvent) error {
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				out = *ev.Message
				curBlocks = make([]ContentBlock, 0)
			}
		case "content_block_start":
			if ev.ContentBlock != nil {
				curBlocks = append(curBlocks, *ev.ContentBlock)
				if ev.ContentBlock.Type == "tool_use" {
					jsonBuf[ev.Index] = &bytes.Buffer{}
				}
			}
		case "content_block_delta":
			if ev.Delta == nil || ev.Index >= len(curBlocks) {
				break
			}
			b := &curBlocks[ev.Index]
			switch ev.Delta.Type {
			case "text_delta":
				b.Text += ev.Delta.Text
			case "thinking_delta":
				b.Thinking += ev.Delta.Thinking
			case "signature_delta":
				b.Signature += ev.Delta.Signature
			case "input_json_delta":
				if buf, ok := jsonBuf[ev.Index]; ok {
					buf.WriteString(ev.Delta.PartialJSON)
				}
			}
		case "content_block_stop":
			if buf, ok := jsonBuf[ev.Index]; ok && ev.Index < len(curBlocks) {
				curBlocks[ev.Index].Input = append([]byte(nil), buf.Bytes()...)
			}
		case "message_delta":
			if ev.Delta != nil {
				out.StopReason = ev.Delta.StopReason
				out.StopSequence = ev.Delta.StopSequence
			}
			if ev.Usage != nil {
				out.Usage.OutputTokens = ev.Usage.OutputTokens
				if ev.Usage.InputTokens > 0 {
					out.Usage.InputTokens = ev.Usage.InputTokens
					out.Usage.CacheCreationInputTokens = ev.Usage.CacheCreationInputTokens
					out.Usage.CacheReadInputTokens = ev.Usage.CacheReadInputTokens
				}
			}
		}
		return onEvent(ev)
	})
	out.Content = curBlocks
	return &out, err
}

// SendMessage posts a non-streamed request (used for probes/validation).
func (c *Client) SendMessage(ctx context.Context, req *Request) (*Response, error) {
	req.Stream = false
	ctx, cancel := withStreamDeadline(ctx)
	defer cancel()
	resp, err := c.Do(ctx, "POST", "/v1/messages?beta=true", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, decodeErr(resp)
	}
	var out Response
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<22)).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// WireTransport is the first-party transport: https goes over utls + raw
// HTTP/1.1 (real's ClientHello + header order); http stays stdlib. Used by
// the connector/proxy MCP transports too so their fingerprint also matches.
func WireTransport() http.RoundTripper {
	return routingTransport{raw: rawHTTP11RoundTripper{}, std: http.DefaultTransport}
}

// routingTransport picks the utls+raw path for https requests and the std
// transport otherwise (http capture servers, unix sockets, non-443 ports).
type routingTransport struct {
	raw http.RoundTripper
	std http.RoundTripper
}

func (r routingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "https" {
		return r.raw.RoundTrip(req)
	}
	return r.std.RoundTrip(req)
}
