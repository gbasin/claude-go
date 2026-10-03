// Package mcp implements a minimal MCP (Model Context Protocol) stdio client.
// Servers are configured in ~/.claude.json ("mcpServers") or .mcp.json.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type ServerConfig struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"` // http/sse servers unsupported for now
	Headers map[string]string `json:"headers"`
}

// LoadConfigs merges mcpServers from ~/.claude.json and <cwd>/.mcp.json.
func LoadConfigs(cwd string) map[string]ServerConfig {
	out := map[string]ServerConfig{}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(home, ".claude.json"),
		filepath.Join(cwd, ".mcp.json"),
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var top map[string]json.RawMessage
		if json.Unmarshal(b, &top) != nil {
			continue
		}
		for _, key := range []string{"mcpServers"} {
			raw, ok := top[key]
			if !ok {
				continue
			}
			var servers map[string]ServerConfig
			if json.Unmarshal(raw, &servers) == nil {
				for n, c := range servers {
					out[n] = c
				}
			}
		}
	}
	return out
}

type rpcReq struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Client is one stdio MCP server connection.
type Client struct {
	Name    string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	seq     atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan rpcResp
	Tools   []Tool
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Connect spawns the server and performs the MCP handshake.
func Connect(name string, cfg ServerConfig) (*Client, error) {
	if cfg.Type != "" && cfg.Type != "stdio" {
		return nil, fmt.Errorf("mcp transport %q unsupported", cfg.Type)
	}
	cmd := exec.Command(cfg.Command, cfg.Args...)
	env := os.Environ()
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &Client{Name: name, cmd: cmd, stdin: stdin, pending: map[int64]chan rpcResp{}}
	go c.readLoop(stdout)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var initRes struct {
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	err = c.call(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "claude-go", "version": "2.1.288"},
	}, &initRes)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s initialize: %w", name, err)
	}
	c.notify("notifications/initialized", nil)

	var listRes struct {
		Tools []Tool `json:"tools"`
	}
	if err := c.call(ctx, "tools/list", nil, &listRes); err != nil {
		// server may have no tools; that's ok
	} else {
		c.Tools = listRes.Tools
	}
	return c, nil
}

func (c *Client) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var resp rpcResp
		if json.Unmarshal(line, &resp) != nil {
			continue
		}
		c.mu.Lock()
		if ch, ok := c.pending[resp.ID]; ok {
			delete(c.pending, resp.ID)
			ch <- resp
		}
		c.mu.Unlock()
	}
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	id := c.seq.Add(1)
	ch := make(chan rpcResp, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	b, _ := json.Marshal(rpcReq{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if _, err := c.stdin.Write(append(b, '\n')); err != nil {
		return err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return fmt.Errorf("mcp error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		if out != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) notify(method string, params any) {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": method, "params": params,
	})
	c.stdin.Write(append(b, '\n'))
}

type callResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// CallTool invokes a tool and returns concatenated text content.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
	var params map[string]any
	if len(args) > 0 {
		json.Unmarshal(args, &params)
	}
	var res callResult
	ctx2, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	err := c.call(ctx2, "tools/call", map[string]any{
		"name": name, "arguments": params,
	}, &res)
	if err != nil {
		return "", true, err
	}
	var out string
	for _, ct := range res.Content {
		out += ct.Text
	}
	return out, res.IsError, nil
}

func (c *Client) Close() {
	if c.cmd != nil && c.cmd.Process != nil {
		c.cmd.Process.Kill()
		c.cmd.Wait()
	}
}

// ListResources calls resources/list and returns a text listing.
func (c *Client) ListResources(ctx context.Context) (string, error) {
	var res struct {
		Resources []struct {
			URI         string `json:"uri"`
			Name        string `json:"name"`
			Description string `json:"description"`
			MimeType    string `json:"mimeType"`
		} `json:"resources"`
	}
	if err := c.call(ctx, "resources/list", nil, &res); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, r := range res.Resources {
		fmt.Fprintf(&b, "%s  %s  %s\n", r.URI, r.Name, r.Description)
	}
	return b.String(), nil
}

// ReadResource calls resources/read.
func (c *Client) ReadResource(ctx context.Context, uri string) (string, error) {
	var res struct {
		Contents []struct {
			URI      string `json:"uri"`
			MimeType string `json:"mimeType"`
			Text     string `json:"text"`
			Blob     string `json:"blob"`
		} `json:"contents"`
	}
	if err := c.call(ctx, "resources/read", map[string]any{"uri": uri}, &res); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, ct := range res.Contents {
		if ct.Text != "" {
			b.WriteString(ct.Text)
		} else if ct.Blob != "" {
			b.WriteString("[base64 blob " + ct.MimeType + "]")
		}
	}
	return b.String(), nil
}

// HTTPClient is an MCP client using the streamable-http transport.
type HTTPClient struct {
	URL          string
	Headers      map[string]string
	Name         string
	Tools        []Tool
	Instructions string
	client       *http.Client
	seq          atomic.Int64
	sessID       string
}

// ConnectHTTP initializes an HTTP-transport MCP server.
func ConnectHTTP(name, url string, headers map[string]string) (*HTTPClient, error) {
	c := &HTTPClient{URL: url, Headers: headers, Name: name,
		client: &http.Client{Timeout: 30 * time.Second}}
	params, _ := json.Marshal(map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "claude-go", "version": "2.1.288"},
	})
	var res struct {
		ServerInfo struct {
			Name, Version string
		} `json:"serverInfo"`
		Instructions string `json:"instructions"`
	}
	if err := c.rpc(context.Background(), "initialize", params, &res); err != nil {
		return nil, err
	}
	c.Instructions = res.Instructions
	c.notifyInit()
	var toolsRes struct {
		Tools []Tool `json:"tools"`
	}
	if err := c.rpc(context.Background(), "tools/list", nil, &toolsRes); err == nil {
		c.Tools = toolsRes.Tools
	}
	return c, nil
}

func (c *HTTPClient) rpc(ctx context.Context, method string, params json.RawMessage, out any) error {
	id := c.seq.Add(1)
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": json.RawMessage(params),
	})
	req, err := http.NewRequestWithContext(ctx, "POST", c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	if c.sessID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessID)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sessID = sid
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	// streamable-http may return SSE or plain JSON
	var m map[string]json.RawMessage
	if bytes.Contains(data, []byte("data:")) {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "data:") {
				json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &m)
			}
		}
	} else {
		json.Unmarshal(data, &m)
	}
	if e, ok := m["error"]; ok && len(e) > 0 {
		return fmt.Errorf("mcp error: %s", e)
	}
	if out != nil {
		return json.Unmarshal(m["result"], out)
	}
	return nil
}

func (c *HTTPClient) notifyInit() {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": "notifications/initialized",
	})
	req, _ := http.NewRequest("POST", c.URL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	if c.sessID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessID)
	}
	if r, err := c.client.Do(req); err == nil {
		r.Body.Close()
	}
}

func (c *HTTPClient) call(ctx context.Context, method string, params any, out any) error {
	b, _ := json.Marshal(params)
	return c.rpc(ctx, method, b, out)
}

// CallTool implements the MCPCaller interface for http servers.
func (c *HTTPClient) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	var res struct {
		Content []struct {
			Type, Text string
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	err := c.call(ctx, "tools/call", map[string]any{
		"name": name, "arguments": json.RawMessage(args),
	}, &res)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, ct := range res.Content {
		if ct.Text != "" {
			b.WriteString(ct.Text)
		}
	}
	return b.String(), nil
}

// ListResources for HTTP transport.
func (c *HTTPClient) ListResources(ctx context.Context) (string, error) {
	var res struct {
		Resources []struct {
			URI, Name, Description string
		} `json:"resources"`
	}
	if err := c.call(ctx, "resources/list", nil, &res); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, r := range res.Resources {
		fmt.Fprintf(&b, "%s  %s  %s\n", r.URI, r.Name, r.Description)
	}
	return b.String(), nil
}

// ReadResource for HTTP transport.
func (c *HTTPClient) ReadResource(ctx context.Context, uri string) (string, error) {
	var res struct {
		Contents []struct{ URI, MimeType, Text, Blob string } `json:"contents"`
	}
	if err := c.call(ctx, "resources/read", map[string]any{"uri": uri}, &res); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, ct := range res.Contents {
		b.WriteString(ct.Text)
	}
	return b.String(), nil
}// Instructions captured from initialize (injected into system prompt).
var Instructions = map[string]string{}


