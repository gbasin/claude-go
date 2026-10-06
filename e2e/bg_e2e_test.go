//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// poll re-runs cond until it holds or the deadline passes.
func poll(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestBgSessionLifecycle exercises the --bg → agents → attach → ^Z-detach
// spine (real's mod_0488): the session runs under a detached bg-pty-host
// worker, the registry lists it, attach streams the live session, Ctrl+Z
// detaches without killing it, and session exit reaps the registry.
func TestBgSessionLifecycle(t *testing.T) {
	s := newSession(t)
	mockEnv := []string{"CLAUDE_GO_MOCK=1", // propagates to the bg child
		"CLAUDE_GO_BG_DEBUG=/tmp/e2e-bghost.log"}

	// --bg: spawn the session, print the id + management commands
	r := s.runEx(t, mockEnv, 30*time.Second, "--bg", "count to three")
	s.mustOK(t, r)
	contains(t, r.stdout, "background session", "--bg output")
	m := regexp.MustCompile(`claude attach ([0-9a-f]{8})`).FindStringSubmatch(r.stdout)
	if len(m) < 2 {
		t.Fatalf("--bg output lacks attach hint: %q", r.stdout)
	}
	short := m[1]

	// claude agents lists the live worker (registry write can lag a beat)
	ok := false
	for i := 0; i < 60 && !ok; i++ {
		time.Sleep(150 * time.Millisecond)
		ok = strings.Contains(s.run(t, "agents").stdout, short)
	}
	if !ok {
		log, _ := os.ReadFile(s.home + "/bghost.log")
		trans, _ := filepath.Glob(s.home + "/.claude/projects/*/*.jsonl")
		t.Fatalf("agents never listed %s\nbghost log:\n%s\ntranscripts: %v", short, log, trans)
	}

	// attach: submit a prompt to the live session, then ^Z detaches and the
	// session keeps running
	r = s.runStdin(t, "ping-from-attach\r\x1a", "attach", short)
	contains(t, r.stdout, "detached — session "+short+" keeps running", "detach message")
	poll(t, 5*time.Second, "session still running after detach", func() bool {
		return strings.Contains(s.run(t, "agents").stdout, short)
	})

	// /exit through an attach ends the session: opExit notice + registry reap
	r = s.runStdin(t, "/exit\r", "attach", short)
	if !strings.Contains(r.stdout, "session "+short+" ended") {
		trans, _ := filepath.Glob(s.home + "/.claude/projects/*/*.jsonl")
		var tr string
		for _, tp := range trans {
			b, _ := os.ReadFile(tp)
			tr += "\n== " + tp + "\n" + string(b[len(b)-min(3000, len(b)):])
		}
		log, _ := os.ReadFile("/tmp/e2e-bghost.log")
		ps, _ := exec.Command("ps", "-axo", "pid,stat,command").Output()
		t.Fatalf("session-end missing\ntranscripts:%s\nlog:\n%s\nps:\n%s", tr, log, ps)
	}
	poll(t, 10*time.Second, "registry reaped after session exit", func() bool {
		return !strings.Contains(s.run(t, "agents").stdout, short)
	})
}

// TestBackgroundSlashDetach covers /background inside a live REPL: the
// interactive process exits, the session continues under a bg worker, and
// attach reaches it.
func TestBackgroundSlashDetach(t *testing.T) {
	s := newSession(t)
	p := s.runPTY(t, []string{"CLAUDE_GO_MOCK=1"}, "--mock")
	p.waitFor("❯", 20*time.Second)

	// give the session a transcript first — the worker resumes from it
	p.send("say hi\r")
	p.waitFor("Mock", 30*time.Second)
	p.waitFor("❯", 20*time.Second)

	p.send("/background\r")
	p.waitFor("session detached", 15*time.Second)
	if err := p.waitExit(30 * time.Second); err != nil {
		t.Fatalf("REPL still running after /background: %v", err)
	}

	// the bg worker shows up in claude agents
	short := ""
	poll(t, 15*time.Second, "worker in agents", func() bool {
		out := s.run(t, "agents").stdout
		i := strings.Index(out, "background sessions:")
		if i < 0 || strings.Contains(out[i:], "none") {
			return false
		}
		line := strings.TrimSpace(out[i+strings.Index(out[i:], "\n"):])
		if f := strings.Fields(line); len(f) > 0 {
			short = f[0]
		}
		return short != ""
	})

	// attach reaches the resumed session; ^Z detaches
	r := s.runStdin(t, "\x1a", "attach", short)
	contains(t, r.stdout, "detached — session "+short+" keeps running", "detach message")

	// clean up the worker so it doesn't outlive the test session
	s.runStdin(t, "/exit\r", "attach", short)
}
