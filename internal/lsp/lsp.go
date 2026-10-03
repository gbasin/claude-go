// Package lsp implements a minimal LSP client over stdio (Content-Length framed).
package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Client struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	seq     atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan json.RawMessage
	root    string
	open    map[string]bool
}

var servers = map[string][]string{
	"go":   {"gopls", "serve"},
	"ts":   {"typescript-language-server", "--stdio"},
	"js":   {"typescript-language-server", "--stdio"},
	"tsx":  {"typescript-language-server", "--stdio"},
	"jsx":  {"typescript-language-server", "--stdio"},
	"py":   {"pylsp"},
	"rs":   {"rust-analyzer"},
	"c":    {"clangd"},
	"cpp":  {"clangd"},
	"rb":   {"solargraph", "stdio"},
	"java": {"jdtls"},
	"lua":  {"lua-language-server"},
	"zig":  {"zls"},
}

var (
	activeMu sync.Mutex
	active   = map[string]*Client{}
)

func extLang(path string) string {
	ext := strings.TrimPrefix(filepath.Ext(path), ".")
	if ext == "" {
		return ""
	}
	return ext
}

// For returns a running client for the file's language, starting the
// server if needed. Returns nil when no server is available.
func For(filePath, root string) *Client {
	lang := extLang(filePath)
	args, ok := servers[lang]
	if !ok {
		return nil
	}
	activeMu.Lock()
	defer activeMu.Unlock()
	key := lang + ":" + root
	if c, ok := active[key]; ok && c.alive() {
		return c
	}
	c, err := start(args, root)
	if err != nil {
		return nil
	}
	active[key] = c
	return c
}

func (c *Client) alive() bool {
	return c.cmd != nil && c.cmd.Process != nil && c.cmd.ProcessState == nil
}

func start(argv []string, root string) (*Client, error) {
	if _, err := exec.LookPath(argv[0]); err != nil {
		return nil, fmt.Errorf("lsp server %q not installed", argv[0])
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = root
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
	c := &Client{cmd: cmd, stdin: stdin, pending: map[int64]chan json.RawMessage{},
		root: root, open: map[string]bool{}}
	go c.readLoop(stdout)

	var initRes json.RawMessage
	if err := c.call("initialize", map[string]any{
		"processId": os.Getpid(),
		"rootUri":   uri(root),
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"hover":              map[string]any{},
				"definition":         map[string]any{},
				"references":         map[string]any{},
				"documentSymbol":     map[string]any{},
				"publishDiagnostics": map[string]any{},
			},
			"workspace": map[string]any{"symbol": map[string]any{}},
		},
	}, &initRes); err != nil {
		c.Kill()
		return nil, err
	}
	c.notify("initialized", map[string]any{})
	return c, nil
}

func uri(path string) string {
	abs, _ := filepath.Abs(path)
	return (&url.URL{Scheme: "file", Path: abs}).String()
}

func (c *Client) readLoop(r io.Reader) {
	br := bufio.NewReader(r)
	for {
		hdr, err := br.ReadString('\n')
		if err != nil {
			return
		}
		if !strings.HasPrefix(hdr, "Content-Length:") {
			continue
		}
		n, _ := strconv.Atoi(strings.TrimSpace(hdr[15:]))
		if n <= 0 {
			continue
		}
		br.ReadString('\n') // blank line
		body := make([]byte, n)
		if _, err := io.ReadFull(br, body); err != nil {
			return
		}
		var msg struct {
			ID     int64           `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(body, &msg) != nil || msg.ID == 0 {
			continue
		}
		c.mu.Lock()
		if ch, ok := c.pending[msg.ID]; ok {
			delete(c.pending, msg.ID)
			ch <- msg.Result
		}
		c.mu.Unlock()
	}
}

func (c *Client) send(v any) error {
	b, _ := json.Marshal(v)
	_, err := fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n%s", len(b), b)
	return err
}

func (c *Client) call(method string, params any, out any) error {
	id := c.seq.Add(1)
	ch := make(chan json.RawMessage, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case res := <-ch:
		if out != nil && len(res) > 0 {
			return json.Unmarshal(res, out)
		}
		return nil
	case <-time.After(30 * time.Second):
		return fmt.Errorf("lsp %s timed out", method)
	}
}

func (c *Client) notify(method string, params any) {
	c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *Client) didOpen(path string) error {
	if c.open[path] {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	c.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri": uri(path), "languageId": langID(extLang(path)),
			"version": 1, "text": string(b),
		},
	})
	c.open[path] = true
	return nil
}

func langID(ext string) string {
	m := map[string]string{
		"go": "go", "ts": "typescript", "js": "javascript",
		"tsx": "typescriptreact", "jsx": "javascriptreact",
		"py": "python", "rs": "rust", "c": "c", "cpp": "cpp",
		"rb": "ruby", "java": "java", "lua": "lua", "zig": "zig",
	}
	if l, ok := m[ext]; ok {
		return l
	}
	return ext
}

type Position struct{ Line, Character int }

type Location struct {
	URI   string `json:"uri"`
	Range struct {
		Start struct{ Line, Character int } `json:"start"`
		End   struct{ Line, Character int } `json:"end"`
	} `json:"range"`
}

func (c *Client) pos(file string, line, char int) map[string]any {
	return map[string]any{
		"textDocument": map[string]any{"uri": uri(file)},
		"position":     map[string]int{"line": line - 1, "character": char - 1},
	}
}

// Hover returns hover text at a position.
func (c *Client) Hover(file string, line, char int) (string, error) {
	if err := c.didOpen(file); err != nil {
		return "", err
	}
	var res struct {
		Contents any `json:"contents"`
	}
	if err := c.call("textDocument/hover", c.pos(file, line, char), &res); err != nil {
		return "", err
	}
	return flattenMarked(res.Contents), nil
}

func flattenMarked(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case map[string]any:
		if s, ok := c["value"].(string); ok {
			return s
		}
		if s, ok := c["kind"].(string); ok {
			_ = s
		}
	case []any:
		var b strings.Builder
		for _, x := range c {
			b.WriteString(flattenMarked(x))
			b.WriteString("\n")
		}
		return b.String()
	}
	return ""
}

// Locations runs a position query returning Location lists.
func (c *Client) Locations(method, file string, line, char int) ([]Location, error) {
	if err := c.didOpen(file); err != nil {
		return nil, err
	}
	params := c.pos(file, line, char)
	if method == "textDocument/references" {
		params["context"] = map[string]bool{"includeDeclaration": true}
	}
	var res []Location
	if err := c.call(method, params, &res); err != nil {
		// some servers return single location
		var one Location
		if err2 := c.call(method, params, &one); err2 != nil {
			return nil, err
		}
		return []Location{one}, nil
	}
	return res, nil
}

// Symbols returns document symbols.
func (c *Client) Symbols(file string) (json.RawMessage, error) {
	if err := c.didOpen(file); err != nil {
		return nil, err
	}
	var res json.RawMessage
	err := c.call("textDocument/documentSymbol",
		map[string]any{"textDocument": map[string]any{"uri": uri(file)}}, &res)
	return res, err
}

// WorkspaceSymbols searches workspace symbols.
func (c *Client) WorkspaceSymbols(query string) (json.RawMessage, error) {
	var res json.RawMessage
	err := c.call("workspace/symbol", map[string]any{"query": query}, &res)
	return res, err
}

// Diagnostics pulls textDocument/diagnostic if supported.
func (c *Client) Diagnostics(file string) (json.RawMessage, error) {
	if err := c.didOpen(file); err != nil {
		return nil, err
	}
	var res json.RawMessage
	err := c.call("textDocument/diagnostic",
		map[string]any{"textDocument": map[string]any{"uri": uri(file)}}, &res)
	return res, err
}

func (c *Client) Kill() {
	if c.cmd != nil && c.cmd.Process != nil {
		c.cmd.Process.Kill()
	}
}
