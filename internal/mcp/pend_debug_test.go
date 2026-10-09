package mcp

import (
	"os"
	"testing"
)

func TestDebugPending(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(dir+"/.mcp.json", []byte(`{"mcpServers":{"srv":{"command":"true"}}}`), 0o644)
	t.Log("pending:", PendingProjectServers(dir))
}
