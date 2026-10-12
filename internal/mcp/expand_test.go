package mcp

import (
	"os"
	"testing"
)

func TestExpandServerEnvPluginRoot(t *testing.T) {
	sc := &ServerConfig{
		Command: "/bin/sh",
		Args:    []string{"${CLAUDE_PLUGIN_ROOT}/bin/hook", "mcp"},
		Env:     map[string]string{"X": "${MISSING_VAR:-fallback}"},
		URL:     "http://localhost/${PORT:-8080}/mcp",
	}
	expandServerEnv(sc, map[string]string{"CLAUDE_PLUGIN_ROOT": "/plugins/foo"})
	if sc.Args[0] != "/plugins/foo/bin/hook" {
		t.Fatalf("args[0] = %q", sc.Args[0])
	}
	if sc.Env["X"] != "fallback" {
		t.Fatalf("env = %q", sc.Env["X"])
	}
	if sc.URL != "http://localhost/8080/mcp" {
		t.Fatalf("url = %q", sc.URL)
	}
}

func TestExpandServerEnvProcessEnv(t *testing.T) {
	os.Setenv("TEST_MCP_EXP", "yes")
	defer os.Unsetenv("TEST_MCP_EXP")
	sc := &ServerConfig{Command: "${TEST_MCP_EXP}/bin/x"}
	expandServerEnv(sc, nil)
	if sc.Command != "yes/bin/x" {
		t.Fatalf("command = %q", sc.Command)
	}
}

// TestStdioDeadSpawnFailsPending: a server that exits before answering
// initialize must fail the handshake fast — not hang to the timeout.
func TestStdioDeadSpawnFailsPending(t *testing.T) {
	c, err := connectStdio("dead", ServerConfig{
		Type: "stdio", Command: "/bin/sh", Args: []string{"-c", "exit 127"},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = c.handshake()
	if err == nil {
		t.Fatal("expected handshake error")
	}
	if got := err.Error(); got == "" || (len(got) > 0 && contains(got, "deadline")) {
		t.Fatalf("handshake should fail fast, got %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
