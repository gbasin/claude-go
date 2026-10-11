//go:build e2e && !windows

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestBgSessionLifecycle exercises the --bg → agents → attach → ^Z-detach
// spine (real's mod_0488): the session runs under a detached bg-pty-host
// worker, the registry lists it, attach streams the live session, Ctrl+Z
// detaches without killing it, and session exit reaps the registry.
func TestBgSessionLifecycle(t *testing.T) {
	s := newSession(t)
	mockEnv := []string{"CLAUDE_GO_MOCK=1", // propagates to the bg child
		"CLAUDE_GO_BG_DEBUG=/tmp/e2e-bghost.log",
		"CLAUDE_GO_BG_NODAEMON=1"} // lifecycle test manages the worker directly

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
	for i := 0; i < 200 && !ok; i++ {
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
	poll(t, 15*time.Second, "session still running after detach", func() bool {
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

// TestBgStop covers `claude stop <id>` — the worker is SIGTERMed, the job
// is reaped from `agents`, and the transcript survives on disk.
func TestBgStop(t *testing.T) {
	s := newSession(t)
	mockEnv := []string{"CLAUDE_GO_MOCK=1", "CLAUDE_GO_BG_NODAEMON=1"}
	r := s.runEx(t, mockEnv, 30*time.Second, "--bg", "count to three")
	s.mustOK(t, r)
	m := regexp.MustCompile(`claude attach ([0-9a-f]{8})`).FindStringSubmatch(r.stdout)
	if len(m) < 2 {
		t.Fatalf("--bg output lacks attach hint: %q", r.stdout)
	}
	short := m[1]
	poll(t, 30*time.Second, "worker in agents", func() bool {
		return strings.Contains(s.run(t, "agents").stdout, short)
	})
	poll(t, 15*time.Second, "first turn logged", func() bool {
		trans, _ := filepath.Glob(s.home + "/.claude/projects/*/*.jsonl")
		return len(trans) > 0
	})

	r = s.run(t, "stop", short)
	s.mustOK(t, r)
	contains(t, r.stdout, "stopped", "stop output")
	poll(t, 15*time.Second, "worker reaped after stop", func() bool {
		return !strings.Contains(s.run(t, "agents").stdout, short)
	})
	if trans, _ := filepath.Glob(s.home + "/.claude/projects/*/*.jsonl"); len(trans) == 0 {
		t.Fatal("stop removed the transcript — real keeps it")
	}
}

// TestDaemonRespawn covers the supervisor sweep: a killed bg worker is
// respawned — the session resumes its own transcript under a new host and
// attach keeps working.
func TestDaemonRespawn(t *testing.T) {
	s := newSession(t)
	mockEnv := []string{"CLAUDE_GO_MOCK=1", "CLAUDE_GO_BG_NODAEMON=1"}
	r := s.runEx(t, mockEnv, 30*time.Second, "--bg", "count to three")
	s.mustOK(t, r)
	m := regexp.MustCompile(`claude attach ([0-9a-f]{8})`).FindStringSubmatch(r.stdout)
	if len(m) < 2 {
		t.Fatalf("--bg output lacks attach hint: %q", r.stdout)
	}
	short := m[1]
	poll(t, 30*time.Second, "worker in agents", func() bool {
		return strings.Contains(s.run(t, "agents").stdout, short)
	})

	// kill the host — the job dir survives (no clean exit), the daemon
	// respawns it on its next sweep
	jobFile := filepath.Join(s.home, ".claude", "jobs", short, "job.json")
	var j struct {
		PID int `json:"pid"`
	}
	b, err := os.ReadFile(jobFile)
	if err != nil || json.Unmarshal(b, &j) != nil || j.PID == 0 {
		t.Fatalf("job record unreadable: %v", err)
	}
	syscall.Kill(j.PID, syscall.SIGKILL)

	dcmd := exec.Command(binPath, "daemon", "run")
	dcmd.Dir = s.cwd
	dcmd.Env = append(append([]string{}, s.env...), "CLAUDE_GO_MOCK=1", "CLAUDE_GO_BG_SWEEP_MS=150")
	if err := dcmd.Start(); err != nil {
		t.Fatalf("daemon run: %v", err)
	}
	t.Cleanup(func() { dcmd.Process.Kill() })

	poll(t, 30*time.Second, "worker respawned", func() bool {
		return strings.Contains(s.run(t, "agents").stdout, short)
	})

	// the respawned session is attachable and answers — the transcript
	// continues under the same id
	r = s.runStdin(t, "still-there?\r\x1a", "attach", short)
	if !strings.Contains(r.stdout, "keeps running") {
		t.Fatalf("attach to respawned session failed: %s", r.stderr)
	}
	poll(t, 10*time.Second, "respawned turn logged", func() bool {
		trans, _ := filepath.Glob(s.home + "/.claude/projects/*/*.jsonl")
		for _, tp := range trans {
			b, _ := os.ReadFile(tp)
			if strings.Contains(string(b), "still-there?") {
				return true
			}
		}
		return false
	})

	// cleanup: /exit ends the respawned session; stop the daemon
	s.runStdin(t, "/exit\r", "attach", short)
	s.run(t, "daemon", "stop")
}

// TestBackgroundSlashDetach covers /background inside a live REPL: the
// interactive process exits, the session continues under a bg worker, and
// attach reaches it.
func TestBackgroundSlashDetach(t *testing.T) {
	s := newSession(t)
	p := s.runPTY(t, []string{"CLAUDE_GO_MOCK=1", "CLAUDE_GO_BG_NODAEMON=1"}, "--mock")
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
	if !strings.Contains(r.stdout, "detached — session "+short+" keeps running") {
		t.Fatalf("attach failed — stderr: %s", r.stderr)
	}

	// clean up the worker so it doesn't outlive the test session
	s.runStdin(t, "/exit\r", "attach", short)
}

// TestResumeLiveBgRefused: real refuses to resume a session that's live
// in a bg worker — "Can't open — this session is running in another
// terminal". --fork-session bypasses; direct -r exits nonzero.
func TestResumeLiveBgRefused(t *testing.T) {
	s := newSession(t)
	sid := newUUID()

	r := s.run(t, "--mock", "--bg", "-p", "ping", "--session-id", sid)
	s.mustOK(t, r)
	short := sid[:8]
	registered := false
	for i := 0; i < 200 && !registered; i++ {
		time.Sleep(150 * time.Millisecond)
		registered = strings.Contains(s.run(t, "agents").stdout, short)
	}
	if !registered {
		jd := filepath.Join(s.home, ".claude", "jobs")
		ents, _ := os.ReadDir(jd)
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		xnote, _ := os.ReadFile(filepath.Join(jd, "last-exit.log"))
		t.Fatalf("worker never registered (jobdirs=%v agents=%q exitnote=%s)",
			names, s.run(t, "agents").stdout, xnote)
	}

	// direct -r on the live session → refused
	r = s.run(t, "--mock", "-r", sid, "-p", "clobber")
	if r.code == 0 {
		t.Fatalf("resume of a live bg session should refuse: %s", r.stdout)
	}
	contains(t, r.stderr, "running in another terminal", "refusal text")

	// also refuses by short id prefix (registry lookup)
	r = s.run(t, "--mock", "-r", short, "-p", "clobber")
	if r.code == 0 || !strings.Contains(r.stderr, "running in another terminal") {
		jobDir := filepath.Join(s.home, ".claude", "jobs")
		ents, _ := os.ReadDir(jobDir)
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		xnote, _ := os.ReadFile(filepath.Join(jobDir, "last-exit.log"))
		t.Fatalf("prefix resume of a live bg session should refuse (code=%d stderr=%q stdout=%q jobdirs=%v agents=%q\nexitnote=%s)",
			r.code, r.stderr, r.stdout, names, s.run(t, "agents").stdout, xnote)
	}

	// --fork-session is the sanctioned bypass — forks the transcript
	r = s.run(t, "--mock", "-r", sid, "--fork-session", "-p", "branch off")
	s.mustOK(t, r)

	// cleanup — keep poking /exit; a host that's slow to bring its
	// socket up fails the attach fast so we can retry
	for i := 0; i < 15; i++ {
		s.runStdin(t, "/exit\r", "attach", sid)
		out := s.run(t, "agents").stdout
		if !strings.Contains(out, short) {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	poll(t, 30*time.Second, "worker exit", func() bool {
		out := s.run(t, "agents").stdout
		i := strings.Index(out, "background sessions:")
		return i < 0 || strings.Contains(out[i:], "none")
	})
}

// TestDaemonRespawnsVersionStale: the daemon recycles workers built on
// an older CLI once they sit idle — real's respawnIfIdleStale (upgrade
// sweep). Attached or busy workers are left alone.
func TestDaemonRespawnsVersionStale(t *testing.T) {
	s := newSession(t)
	sid := newUUID()
	// worker stamped with an old version — simulates a session started
	// before a CLI upgrade
	r := s.runEx(t, []string{"CLAUDE_GO_MOCK=1", "CLAUDE_GO_BG_NODAEMON=1",
		"CLAUDE_BG_CLI_VERSION=0.0.0-old"}, 30*time.Second,
		"--bg", "-p", "ping", "--session-id", sid)
	s.mustOK(t, r)
	short := sid[:8]
	poll(t, 30*time.Second, "worker in agents", func() bool {
		return strings.Contains(s.run(t, "agents").stdout, short)
	})
	jobFile := filepath.Join(s.home, ".claude", "jobs", short, "job.json")
	var j0 struct{ PID int }
	b, _ := os.ReadFile(jobFile)
	_ = json.Unmarshal(b, &j0)
	if j0.PID == 0 {
		t.Fatal("job record unreadable")
	}

	dbg := filepath.Join(s.home, "daemon.log")
	dcmd := exec.Command(binPath, "daemon", "run")
	dcmd.Dir = s.cwd
	dcmd.Env = append(append([]string{}, s.env...), "CLAUDE_GO_MOCK=1",
		"CLAUDE_GO_BG_SWEEP_MS=150", "CLAUDE_GO_BG_SKEW_IDLE_MS=200",
		"CLAUDE_GO_BG_DEBUG="+dbg)
	if err := dcmd.Start(); err != nil {
		t.Fatalf("daemon run: %v", err)
	}
	t.Cleanup(func() { dcmd.Process.Kill() })
	defer s.run(t, "daemon", "stop")

	// daemon terms + respawns the stale worker — same session, new pid
	fail := func() {
		dl, _ := os.ReadFile(dbg)
		var dump []string
		jd := filepath.Join(s.home, ".claude", "jobs")
		filepath.Walk(jd, func(p string, i os.FileInfo, e error) error {
			if i != nil && !i.IsDir() {
				b, _ := os.ReadFile(p)
				dump = append(dump, p[len(jd):]+": "+string(b[:min(300, len(b))]))
			}
			return nil
		})
		t.Fatalf("worker not respawned\ndaemon.log:\n%s\njobfiles:\n%s", dl, strings.Join(dump, "\n"))
	}
	deadline := time.Now().Add(30 * time.Second)
	var okSkew bool
	for !okSkew && time.Now().Before(deadline) {
		b, err := os.ReadFile(jobFile)
		if err != nil {
			continue
		}
		var j struct {
			PID     int    `json:"pid"`
			Version string `json:"version"`
		}
		if json.Unmarshal(b, &j) != nil {
			continue
		}
		okSkew = j.PID != 0 && j.PID != j0.PID && j.Version != "0.0.0-old"
		if !okSkew {
			time.Sleep(150 * time.Millisecond)
		}
	}
	if !okSkew {
		fail()
	}
	// keep poking /exit — the respawned host's rendezvous socket may not
	// be up yet when we first attach (a dead socket fails fast)
	for i := 0; i < 15; i++ {
		r := s.runStdin(t, "/exit\r", "attach", sid)
		out := s.run(t, "agents").stdout
		if !strings.Contains(out, short) {
			break
		}
		_ = r
		time.Sleep(400 * time.Millisecond)
	}
	poll(t, 30*time.Second, "worker exit", func() bool {
		out := s.run(t, "agents").stdout
		return !strings.Contains(out, short)
	})
}
