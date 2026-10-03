package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"time"

	"claude-go/internal/auth"
)

const Version = "2.1.288"

const betaFlags = "claude-code-20250219," +
	"oauth-2025-04-20," +
	"context-1m-2025-08-07," +
	"interleaved-thinking-2025-05-14," +
	"thinking-token-count-2026-05-13," +
	"context-management-2025-06-27," +
	"prompt-caching-scope-2026-01-05," +
	"mid-conversation-system-2026-04-07," +
	"per-turn-control-2026-07-01," +
	"mid-conversation-tool-changes-2026-07-01," +
	"advisor-tool-2026-03-01," +
	"effort-2025-11-24," +
	"dangerous-tool-use-2026-09-03," +
	"afk-mode-2026-01-31," +
	"extended-cache-ttl-2025-04-11"

// Entrypoint mirrors cc_entrypoint in the billing header.
type Entrypoint string

const (
	EntryCLI    Entrypoint = "cli"
	EntrySDKCLI Entrypoint = "sdk-cli"
)

type Client struct {
	BaseURL    string
	HTTP       *http.Client
	Creds      *auth.Credentials
	SessionID  string
	Entrypoint Entrypoint
	RetryCount int // exposed for x-stainless-retry-count
}

func NewClient(creds *auth.Credentials, sessionID string, ep Entrypoint) *Client {
	base := os.Getenv("ANTHROPIC_BASE_URL")
	if base == "" {
		base = "https://api.anthropic.com"
	}
	return &Client{
		BaseURL:    base,
		Creds:      creds,
		SessionID:  sessionID,
		Entrypoint: ep,
		HTTP: &http.Client{
			Timeout: 0, // streaming: no overall timeout
			Transport: &http.Transport{
				MaxIdleConns:        8,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
				DisableCompression:  false,
			},
		},
	}
}

func (c *Client) headers(req *http.Request, tok string) {
	h := req.Header
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	if c.Creds.IsAPIKey {
		h.Set("x-api-key", tok)
	} else {
		h.Set("Authorization", "Bearer "+tok)
	}
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
	betas := betaFlags
	if c.Entrypoint == EntryCLI {
		betas += ",thinking-display-updates-2026-08-18"
	}
	h.Set("anthropic-beta", betas)
	h.Set("anthropic-dangerous-direct-browser-access", "true")
	h.Set("anthropic-version", "2023-06-01")
	h.Set("x-app", "cli")
}

// Do executes a request, retrying on 429/5xx with backoff like the SDK.
func (c *Client) Do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	tok, err := c.Creds.Token()
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt <= 3; attempt++ {
		c.RetryCount = attempt
		var rdr io.Reader
		if raw != nil {
			rdr = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
		if err != nil {
			return nil, err
		}
		c.headers(req, tok)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
		} else if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("api %s", resp.Status)
			resp.Body.Close()
		} else {
			return resp, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(1<<attempt) * 500 * time.Millisecond):
		}
	}
	return nil, lastErr
}

// StreamMessage POSTs /v1/messages?beta=true with stream=true and invokes
// onEvent per decoded SSE event. Returns the assembled final Response.
func (c *Client) StreamMessage(ctx context.Context, req *Request, onEvent func(*StreamEvent) error) (*Response, error) {
	req.Stream = true
	resp, err := c.Do(ctx, "POST", "/v1/messages?beta=true", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var e struct {
			Error APIError `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
			return nil, &ErrAPI{&e.Error}
		}
		return nil, fmt.Errorf("api %s: %s", resp.Status, string(b))
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
