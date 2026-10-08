package mcp

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// sseTransport implements the legacy MCP "SSE" transport (a.k.a. http+sse,
// protocol revision 2024-11-05): the client opens a long-lived GET
// text/event-stream on the server URL; the server emits an `endpoint`
// event naming the POST channel; JSON-RPC requests are POSTed there and
// their responses arrive on the GET stream.
type sseTransport struct {
	url      string
	endpoint string // resolved POST URL
	headers  map[string]string
	client   *http.Client
	streamHC *http.Client // no total timeout: the GET is long-lived
	c        *Client
	auth     *serverAuth

	mu     sync.Mutex
	ready  chan struct{} // closed once the endpoint event arrives
	endErr error
	done   chan struct{}
	onced  sync.Once
	respB  *http.Response
}

func connectSSE(name string, cfg ServerConfig) (*Client, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("mcp %s: sse transport requires url", name)
	}
	c := &Client{Name: name, pending: map[string]chan rpcResp{}}
	t := &sseTransport{
		url:      cfg.URL,
		headers:  cfg.Headers,
		client:   &http.Client{Timeout: 60 * time.Second},
		streamHC: &http.Client{},
		c:        c,
		auth:     loadServerAuth(name, cfg),
		ready:    make(chan struct{}),
		done:     make(chan struct{}),
	}
	c.tr = t
	if err := t.openStream(); err != nil {
		return nil, err
	}
	return c, nil
}

func (t *sseTransport) applyHeaders(req *http.Request) {
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Authorization") == "" {
		if tok := t.auth.accessToken(); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
}

// openStream issues the GET and waits for the endpoint event.
func (t *sseTransport) openStream() error {
	return t.openStreamOnce(true)
}

func (t *sseTransport) openStreamOnce(retryAuth bool) error {
	// The GET stream outlives this function, so the request uses a
	// background context; the timeout only bounds the endpoint wait below.
	req, err := http.NewRequest("GET", t.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	t.applyHeaders(req)
	resp, err := t.streamHC.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		wwwAuth := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		if retryAuth && t.auth != nil && t.auth.refresh(context.Background()) == nil {
			return t.openStreamOnce(false)
		}
		ae := &AuthError{Server: t.c.Name, URL: t.url, WWWAuthenticate: wwwAuth}
		ae.AuthURL, _ = DiscoverAuthURL(context.Background(), t.url, wwwAuth)
		t.c.Status = "needs-auth"
		t.c.AuthURL = ae.AuthURL
		return ae
	}
	if resp.StatusCode != http.StatusOK ||
		!strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		ct := resp.Header.Get("Content-Type")
		resp.Body.Close()
		return fmt.Errorf("mcp %s: sse stream: http %d (%s)", t.c.Name, resp.StatusCode, ct)
	}
	t.respB = resp
	go t.readEvents(resp.Body)

	// Wait for the endpoint event (or failure) before returning.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer waitCancel()
	select {
	case <-t.ready:
		t.mu.Lock()
		err := t.endErr
		t.mu.Unlock()
		return err
	case <-waitCtx.Done():
		resp.Body.Close()
		return fmt.Errorf("mcp %s: sse stream: no endpoint event", t.c.Name)
	}
}

// readEvents pumps SSE frames off the GET stream until close.
func (t *sseTransport) readEvents(body io.ReadCloser) {
	defer body.Close()
	br := bufio.NewReaderSize(body, 1<<16)
	for {
		select {
		case <-t.done:
			return
		default:
		}
		ev, err := nextSSEEvent(br)
		if ev.Event == "endpoint" && ev.Data != "" {
			t.setEndpoint(ev.Data)
		} else if ev.Data != "" {
			// "message" events (and unlabeled frames) carry JSON-RPC
			t.c.dispatch([]byte(ev.Data))
		}
		if err != nil {
			// stream ended: unblock waiters if endpoint never arrived
			t.setEndpointErr(fmt.Errorf("mcp %s: sse stream closed: %w", t.c.Name, err))
			return
		}
	}
}

func (t *sseTransport) setEndpoint(raw string) {
	base, err := url.Parse(t.url)
	if err != nil {
		t.setEndpointErr(err)
		return
	}
	ref, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		t.setEndpointErr(err)
		return
	}
	t.mu.Lock()
	t.endpoint = base.ResolveReference(ref).String()
	t.mu.Unlock()
	t.onced.Do(func() { close(t.ready) })
}

func (t *sseTransport) setEndpointErr(err error) {
	t.mu.Lock()
	if t.endErr == nil {
		t.endErr = err
	}
	t.mu.Unlock()
	t.onced.Do(func() { close(t.ready) })
}

func (t *sseTransport) send(ctx context.Context, msg []byte) error {
	t.mu.Lock()
	ep := t.endpoint
	t.mu.Unlock()
	if ep == "" {
		return fmt.Errorf("mcp %s: sse endpoint not established", t.c.Name)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", ep, bytes.NewReader(msg))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	t.applyHeaders(req)
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &AuthError{Server: t.c.Name, URL: ep,
			WWWAuthenticate: resp.Header.Get("WWW-Authenticate")}
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("mcp %s: sse post: http %d: %s", t.c.Name,
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// 200/202 normally carry no payload (response arrives on the stream),
	// but some servers echo the JSON-RPC response inline — dispatch it.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) > 0 {
		t.c.dispatch(body)
	}
	return nil
}

func (t *sseTransport) close() error {
	select {
	case <-t.done:
	default:
		close(t.done)
	}
	if t.respB != nil {
		t.respB.Body.Close()
	}
	return nil
}
