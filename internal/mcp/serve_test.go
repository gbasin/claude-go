package mcp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"claude-go/internal/tools"
)

func TestServeHandshake(t *testing.T) {
	var in bytes.Buffer
	var out bytes.Buffer
	in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}` + "\n")
	in.WriteString(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	in.WriteString(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n")
	in.WriteString(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"Glob","arguments":{"pattern":"*.go"}}}` + "\n")
	if err := Serve(&tools.Context{CWD: t.TempDir()}, &in, &out); err != nil {
		t.Fatal(err)
	}
	var sawInit, sawList, sawCall bool
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var d struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal([]byte(line), &d) != nil {
			t.Fatalf("bad frame: %s", line)
		}
		switch d.ID {
		case 1:
			sawInit = strings.Contains(string(d.Result), "claude-go")
		case 2:
			sawList = strings.Contains(string(d.Result), `"inputSchema"`)
		case 3:
			sawCall = strings.Contains(string(d.Result), `"content"`)
		}
	}
	if !sawInit || !sawList || !sawCall {
		t.Fatalf("init=%v list=%v call=%v", sawInit, sawList, sawCall)
	}
}

// tools.34 regression: a tool whose InputSchema isn't a JSON object
// must emit a permissive {}-schema — real's clients reject null.
func TestServeToolSchemasAreObjects(t *testing.T) {
	for _, td := range serveToolDefs(&tools.Context{}) {
		s, ok := td["inputSchema"].(map[string]any)
		if !ok || s["type"] != "object" {
			t.Fatalf("%s: inputSchema not an object schema", td["name"])
		}
	}
}
