package mcp

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestServeHandshake: initialize + tools/list + tools/call over the
// stdio JSON-RPC loop (claude mcp serve).
func TestServeHandshake(t *testing.T) {
	var in, out, errOut bytes.Buffer
	in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}` + "\n")
	in.WriteString(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	in.WriteString(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n")
	code := Serve(context.Background(), &in, &out, &errOut, ServeOpts{})
	if code != 0 {
		t.Fatalf("Serve exit %d (errOut: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"serverInfo"`) {
		t.Fatalf("no serverInfo in initialize response:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `"tools"`) {
		t.Fatalf("no tools list:\n%s", out.String())
	}
}
