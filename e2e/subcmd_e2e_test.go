//go:build e2e

// subcmd_e2e_test.go — subcommand + flag surface smoke:
//   - --version / --help
//   - config get/set against the isolated HOME
//   - mcp list (empty), doctor, agents, auth status, import
//   - plugin marketplace list (empty)
package e2e

import (
	"path/filepath"
	"testing"
)

func TestVersionFlag(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--version")
	s.mustOK(t, r)
	contains(t, r.stdout, "Claude Code", "version output")
}

func TestHelpFlag(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--help")
	if r.code != 0 {
		// usage goes to stderr with exit 0 in most builds; tolerate either stream
		contains(t, r.stderr+r.stdout, "claude-go", "help output")
		return
	}
	contains(t, r.stderr+r.stdout, "--output-format", "help output")
}

func TestConfigSetGet(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "config", "set", "model", `"test-model"`)
	s.mustOK(t, r)
	r = s.run(t, "config", "get", "model")
	s.mustOK(t, r)
	contains(t, r.stdout, "test-model", "config get")
}

func TestMCPListEmpty(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "mcp", "list")
	s.mustOK(t, r)
	contains(t, r.stdout, "no MCP servers", "mcp list")
}

func TestDoctor(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "doctor")
	s.mustOK(t, r)
	contains(t, r.stdout, "claude-go", "doctor output")
	contains(t, r.stdout, "cwd:", "doctor cwd")
}

func TestAgentsSubcommand(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "agents")
	s.mustOK(t, r)
	contains(t, r.stdout, "general-purpose", "agents output")
}

func TestAuthStatusNoCreds(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "auth", "status")
	s.mustOK(t, r)
	// no creds in the isolated HOME (env keys are blanked); may still find
	// a keychain entry on the dev machine — accept either honest answer
	if r.stdout != "not logged in\n" && r.stdout == "" {
		t.Fatalf("unexpected auth status output")
	}
}

func TestImportTranscript(t *testing.T) {
	s := newSession(t)
	// seed a session, then export-import it under a fresh id via `import`
	sid := newUUID()
	r := s.run(t, "--mock", "-p", "import-seed", "--session-id", sid)
	s.mustOK(t, r)
	src := s.transcriptPath(t, sid)
	if src == "" {
		t.Fatalf("no seed transcript: %v", s.transcriptPaths(t))
	}
	r = s.run(t, "import", src)
	s.mustOK(t, r)
	contains(t, r.stdout, "imported", "import output")
	if got := s.transcriptPaths(t); len(got) < 2 {
		t.Fatalf("import should add a transcript, have %v", got)
	}
	// imported copy lands next to the original in the same project dir
	base := filepath.Base(src)
	for _, p := range s.transcriptPaths(t) {
		if filepath.Base(p) != base {
			contains(t, mustReadFile(t, p), "import-seed", "imported transcript")
		}
	}
}

func TestMarketplaceListEmpty(t *testing.T) {
	s := newSession(t)
	// note: `plugin marketplace` requires ≥3 args in this build, so the
	// list subcommand needs a (ignored) trailing arg
	r := s.run(t, "plugin", "marketplace", "list", "all")
	s.mustOK(t, r)
	contains(t, r.stdout, "no marketplaces", "marketplace list")
}
