package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// sseEvent is one parsed text/event-stream frame.
type sseEvent struct {
	Event string
	Data  string
}

// nextSSEEvent reads one frame from an SSE stream. Blank lines dispatch;
// ':' lines are comments; fields without a colon are treated as the field
// name with an empty value (per spec).
func nextSSEEvent(r *bufio.Reader) (sseEvent, error) {
	var ev sseEvent
	var data strings.Builder
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
		}
		if err != nil {
			if err == io.EOF && (data.Len() > 0 || ev.Event != "") {
				ev.Data = data.String()
				return ev, err
			}
			return sseEvent{}, err
		}
		if line == "" {
			if data.Len() > 0 || ev.Event != "" {
				ev.Data = data.String()
				return ev, nil
			}
			continue
		}
		if line[0] == ':' {
			continue // comment / keepalive
		}
		field, val, _ := strings.Cut(line, ":")
		val = strings.TrimPrefix(val, " ")
		switch field {
		case "event":
			ev.Event = val
		case "data":
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(val)
		}
	}
}

// dispatchSSEBody parses every SSE frame in r and dispatches its data as a
// JSON-RPC message.
func dispatchSSEBody(c *Client, r io.Reader) {
	br := bufio.NewReaderSize(r, 1<<16)
	for {
		ev, err := nextSSEEvent(br)
		if ev.Data != "" {
			c.dispatch([]byte(ev.Data))
		}
		if err != nil {
			return
		}
	}
}

// httpTransport implements the MCP "streamable HTTP" transport: every
// message is a POST; the response body is either application/json or an
// SSE stream of JSON-RPC messages.
type httpTransport struct {
	url     string
	headers map[string]string
	client  *http.Client
	c       *Client
	auth    *serverAuth // stored OAuth tokens, if any

	mu     sync.Mutex
	sessID string
	getOK  atomic.Bool
	done   chan struct{}
}

func connectHTTP(name string, cfg ServerConfig) (*Client, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("mcp %s: http transport requires url", name)
	}
	c := &Client{Name: name, pending: map[string]chan rpcResp{}}
	t := &httpTransport{
		url:     cfg.URL,
		headers: cfg.Headers,
		client:  &http.Client{}, // ctx governs timeouts
		c:       c,
		auth:    loadServerAuth(name),
		done:    make(chan struct{}),
	}
	c.tr = t
	return c, nil
}

// applyHeaders sets config + auth + session headers on a request.
func (t *httpTransport) applyHeaders(req *http.Request) {
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	// stored OAuth bearer token, unless config already set Authorization
	if req.Header.Get("Authorization") == "" {
		if tok := t.auth.accessToken(); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	t.mu.Lock()
	sid := t.sessID
	t.mu.Unlock()
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
		req.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	}
}

func (t *httpTransport) send(ctx context.Context, msg []byte) error {
	return t.sendOnce(ctx, msg, true)
}

func (t *httpTransport) sendOnce(ctx context.Context, msg []byte, retryAuth bool) error {
	req, err := http.NewRequestWithContext(ctx, "POST", t.url, bytes.NewReader(msg))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	t.applyHeaders(req)
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.mu.Lock()
		t.sessID = sid
		t.mu.Unlock()
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		wwwAuth := resp.Header.Get("WWW-Authenticate")
		// refresh stored token and retry once
		if retryAuth && t.auth != nil && t.auth.refresh(ctx) == nil {
			return t.sendOnce(ctx, msg, false)
		}
		ae := &AuthError{Server: t.c.Name, URL: t.url, WWWAuthenticate: wwwAuth}
		ae.AuthURL, _ = DiscoverAuthURL(ctx, t.url, wwwAuth)
		t.c.Status = "needs-auth"
		t.c.AuthURL = ae.AuthURL
		return ae
	}
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		return nil // notification/response ack, no body
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("mcp %s: http %d: %s", t.c.Name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		dispatchSSEBody(t.c, resp.Body)
		return nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) > 0 {
		t.c.dispatch(data)
	}
	return nil
}

// openGetStream opens the optional standalone SSE stream that streamable-
// HTTP servers may provide for server-initiated messages. 405/404 = not
// supported, silently ignored.
func (t *httpTransport) openGetStream() {
	go func() {
		req, err := http.NewRequest("GET", t.url, nil)
		if err != nil {
			return
		}
		req.Header.Set("Accept", "text/event-stream")
		t.applyHeaders(req)
		resp, err := t.client.Do(req)
		if err != nil {
			return
		}
		if resp.StatusCode != http.StatusOK ||
			!strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
			resp.Body.Close()
			return
		}
		t.getOK.Store(true)
		done := t.done
		go func() {
			select {
			case <-done:
				resp.Body.Close()
			case <-req.Context().Done():
			}
		}()
		dispatchSSEBody(t.c, resp.Body)
	}()
}

func (t *httpTransport) close() error {
	select {
	case <-t.done:
	default:
		close(t.done)
	}
	return nil
}

// --- HTTPClient: backwards-compatible wrapper over *Client ---

// HTTPClient is an MCP client using the streamable-http transport.
// Deprecated: prefer Connect with ServerConfig{Type:"http"} which returns
// a full-featured *Client. Kept for API compatibility.
type HTTPClient struct {
	URL          string
	Headers      map[string]string
	Name         string
	Tools        []Tool
	Instructions string

	c *Client
}

// ConnectHTTP initializes an HTTP-transport MCP server.
func ConnectHTTP(name, url string, headers map[string]string) (*HTTPClient, error) {
	c, err := Connect(name, ServerConfig{Type: "http", URL: url, Headers: headers})
	if err != nil {
		return nil, err
	}
	return &HTTPClient{
		URL:          url,
		Headers:      headers,
		Name:         name,
		Tools:        c.Tools,
		Instructions: c.Instructions,
		c:            c,
	}, nil
}

// Client returns the underlying unified client.
func (c *HTTPClient) Client() *Client { return c.c }

// CallTool invokes a tool and returns concatenated text content.
// Signature kept for compatibility; prefer (*Client).CallTool.
func (c *HTTPClient) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	out, _, err := c.c.CallTool(ctx, name, args)
	return out, err
}

// ListResources calls resources/list.
func (c *HTTPClient) ListResources(ctx context.Context) (string, error) {
	return c.c.ListResources(ctx)
}

// ReadResource calls resources/read.
func (c *HTTPClient) ReadResource(ctx context.Context, uri string) (string, error) {
	return c.c.ReadResource(ctx, uri)
}

// ListPrompts calls prompts/list.
func (c *HTTPClient) ListPrompts(ctx context.Context, cursor string) ([]Prompt, string, error) {
	return c.c.ListPrompts(ctx, cursor)
}

// GetPrompt calls prompts/get.
func (c *HTTPClient) GetPrompt(ctx context.Context, name string, args map[string]string) (*GetPromptResult, error) {
	return c.c.GetPrompt(ctx, name, args)
}

// ListResourceTemplates calls resources/templates/list.
func (c *HTTPClient) ListResourceTemplates(ctx context.Context, cursor string) ([]ResourceTemplate, string, error) {
	return c.c.ListResourceTemplates(ctx, cursor)
}

// OnNotification delegates to the underlying client's hook.
func (c *HTTPClient) OnNotification(fn func(method string, params json.RawMessage)) {
	c.c.OnNotification = fn
}

func (c *HTTPClient) Close() { c.c.Close() }

// Call forwards a raw request to the underlying client.
func (c *HTTPClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return c.c.Call(ctx, method, params)
}

// ReadResourceDir forwards the directory read extension.
func (c *HTTPClient) ReadResourceDir(ctx context.Context, uri string) (string, error) {
	return c.c.ReadResourceDir(ctx, uri)
}

// ListResourcesJSON forwards the raw resource list.
func (c *HTTPClient) ListResourcesJSON(ctx context.Context) (json.RawMessage, error) {
	return c.c.ListResourcesJSON(ctx)
}
