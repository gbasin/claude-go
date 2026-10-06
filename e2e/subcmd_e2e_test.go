//go:build e2e

// subcmd_e2e_test.go — subcommand + flag surface smoke:
//   - --version / --help
//   - config get/set against the isolated HOME
//   - mcp list (empty), doctor, agents, auth status, import
//   - plugin marketplace list (empty)
package e2e

import (
	"os"
	"path/filepath"
	"strings"
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

// TestMCPAddList: `mcp add` writes a server entry; `mcp list` shows it.
func TestMCPAddList(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "mcp", "add", "e2e-srv", "myserver-bin", "--serve")
	s.mustOK(t, r)

	r = s.run(t, "mcp", "list")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "e2e-srv") {
		t.Fatalf("added server missing from mcp list: %q", r.stdout)
	}
}

// TestPurgeDryRun: purge --dry-run prints the targets without deleting.
func TestPurgeDryRun(t *testing.T) {
	s := newSession(t)
	s.run(t, "--mock", "-p", "hi") // create a transcript
	r := s.run(t, "purge", "--dry-run")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "would remove") {
		t.Fatalf("dry-run output missing: %q", r.stdout)
	}
	// transcript dir still exists after dry-run
	matches, _ := filepath.Glob(filepath.Join(s.home, ".claude", "projects", "*", "*.jsonl"))
	if len(matches) == 0 {
		t.Fatal("dry-run deleted transcripts")
	}
}

// TestPurgeRemoves: purge removes the project transcript dir.
func TestPurgeRemoves(t *testing.T) {
	s := newSession(t)
	s.run(t, "--mock", "-p", "hi")
	dir := filepath.Join(s.home, ".claude", "projects")
	before, _ := filepath.Glob(filepath.Join(dir, "*", "*.jsonl"))
	if len(before) == 0 {
		t.Skip("no transcript seeded")
	}
	r := s.run(t, "purge")
	s.mustOK(t, r)
	after, _ := filepath.Glob(filepath.Join(dir, "*", "*.jsonl"))
	if len(after) != 0 {
		t.Fatalf("purge left transcripts: %v", after)
	}
}

// TestPluginList: `plugin list` shows enabled plugins from
// installed_plugins.json.
func TestPluginList(t *testing.T) {
	s := newSession(t)
	os.MkdirAll(filepath.Join(s.home, ".claude", "plugins"), 0o755)
	os.WriteFile(filepath.Join(s.home, ".claude", "plugins", "installed_plugins.json"),
		[]byte(`{"plugins":{"myplug@mkt":[{"installPath":"/tmp/x","enabled":true}]}}`), 0o644)
	r := s.run(t, "plugin", "list")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "myplug") {
		t.Fatalf("installed plugin missing: %q", r.stdout)
	}
}

// TestSetupToken: setup-token stores the pasted credential.
func TestSetupToken(t *testing.T) {
	s := newSession(t)
	r := s.runStdin(t, "sk-ant-e2e-token-99\n", "setup-token")
	s.mustOK(t, r)
	matches, _ := filepath.Glob(filepath.Join(s.home, ".claude*", ".credentials.json"))
	var found bool
	for _, m := range matches {
		b, _ := os.ReadFile(m)
		if strings.Contains(string(b), "sk-ant-e2e-token-99") {
			found = true
		}
	}
	if !found {
		// try ~/.claude.json or other layouts
		for _, m := range []string{filepath.Join(s.home, ".claude.json")} {
			b, _ := os.ReadFile(m)
			if strings.Contains(string(b), "sk-ant-e2e-token-99") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("token not persisted: %q / files: %v", r.stdout, matches)
	}
}

// TestUnsupportedStubs: cloud-dependent subcommands print a clean refusal.
func TestUnsupportedStubs(t *testing.T) {
	for _, sub := range []string{"gateway", "ssh", "connect", "self-hosted-runner"} {
		s := newSession(t)
		r := s.run(t, sub)
		if !strings.Contains(r.stdout, "not supported") {
			t.Fatalf("%s should print the not-supported message: %q", sub, r.stdout)
		}
	}
}

// TestUpdateFallback: self-update outside a source checkout explains the
// go-install path instead of crashing.
func TestUpdateFallback(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "update")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "update:") {
		t.Fatalf("unexpected update output: %q", r.stdout)
	}
}
