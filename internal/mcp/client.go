// Package mcp implements a minimal MCP (Model Context Protocol) stdio client.
// Servers are configured in ~/.claude.json ("mcpServers") or .mcp.json.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
