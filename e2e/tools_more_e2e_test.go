//go:build e2e

// tools_more_e2e_test.go — e2e coverage for the remaining dispatchable
// tools: Skill expansion, Glob/Grep, ListAgents, NotebookEdit, worktree
// enter/exit, EndConversation, cron list, and the advisor subcall.
package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSkillTool: a project skill's SKILL.md expands $ARGUMENTS and the
// skill-dir variable when invoked via the Skill tool.
func TestSkillTool(t *testing.T) {
	s := newSession(t)
	s.writeFile(".claude/skills/e2e-skill/SKILL.md",
		"---\ndescription: e2e test skill\n---\nBody says: $ARGUMENTS\nDir: ${CLAUDE_SKILL_DIR}\n")

	r := s.run(t, "--mock", "-p",
		`tool:Skill {"skill":"e2e-skill","args":"e2e-args-marker"}`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	for _, want := range []string{"e2e-args-marker", "e2e-skill"} {
		if !strings.Contains(r.stdout, want) {
			t.Fatalf("skill result missing %q: %s", want, truncStr(r.stdout, 2000))
		}
	}
}

// TestEditTool: Edit applies old_string→new_string through the tool
// surface (Read first — unread-file writes refuse, real behavior).
func TestEditTool(t *testing.T) {
	s := newSession(t)
	f := s.writeFile("editme.txt", "alpha BETA-OLD gamma")

	r := s.run(t, "--mock", "-p",
		`calls:[{"name":"Read","input":{"file_path":"`+f+`"}},`+
			`{"name":"Edit","input":{"file_path":"`+f+`","old_string":"BETA-OLD","new_string":"BETA-NEW"}}]`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	b, _ := os.ReadFile(f)
	if !strings.Contains(string(b), "BETA-NEW") {
		t.Fatalf("edit did not land: %s", b)
	}
}

// TestListAgents: ListAgents enumerates a just-launched background agent
// (the record registers synchronously before RunAgent returns).
func TestListAgents(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p",
		`calls:[{"name":"Agent","input":{"description":"e2e-agent-1","prompt":"linger"}},`+
			`{"name":"ListAgents","input":{}}]`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "e2e-agent-1") {
		t.Fatalf("ListAgents should show the running agent: %s", truncStr(r.stdout, 2000))
	}
}

// TestSendMessage: SendMessage (deferred) delivers into a live in-process
// agent's inbox.
func TestSendMessage(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p",
		`calls:[{"name":"ToolSearch","input":{"query":"select:SendMessage"}},`+
			`{"name":"Agent","input":{"description":"e2e-mailbox","prompt":"linger"}},`+
			`{"name":"SendMessage","input":{"to":"e2e-mailbox","message":"hello agent"}}]`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "in-process agent") {
		t.Fatalf("SendMessage should report in-process delivery: %s", truncStr(r.stdout, 2000))
	}
}

// TestCronList: CronList (deferred) reports the empty schedule.
func TestCronList(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p",
		`calls:[{"name":"ToolSearch","input":{"query":"select:CronList"}},`+
			`{"name":"CronList","input":{}}]`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "No scheduled jobs") {
		t.Fatalf("CronList empty result expected: %s", truncStr(r.stdout, 2000))
	}
}

// TestNotebookEdit: deferred tool — ToolSearch loads the schema, Read marks
// the notebook read, then replace updates the cell source on disk.
func TestNotebookEdit(t *testing.T) {
	s := newSession(t)
	nb := s.writeFile("nb.ipynb",
		`{"cells":[{"id":"c1","cell_type":"code","source":"print(1)","metadata":{},"outputs":[],"execution_count":null}],"metadata":{},"nbformat":4,"nbformat_minor":5}`)

	r := s.run(t, "--mock", "-p",
		`calls:[{"name":"ToolSearch","input":{"query":"select:NotebookEdit"}},`+
			`{"name":"Read","input":{"file_path":"`+nb+`"}},`+
			`{"name":"NotebookEdit","input":{"notebook_path":"`+nb+`","cell_id":"c1","new_source":"print(42)"}}]`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)

	b, _ := os.ReadFile(nb)
	if !strings.Contains(string(b), "print(42)") {
		t.Fatalf("notebook cell not updated: %s", b)
	}
}

// TestWorktreeRoundTrip: EnterWorktree creates .claude/worktrees/<name>
// inside the repo; ExitWorktree keep returns to the original cwd.
func TestWorktreeRoundTrip(t *testing.T) {
	s := newSession(t)
	// tool needs a git repo
	git := func(args ...string) {
		out, err := exec.Command("git", append([]string{"-C", s.cwd}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "e2e@test")
	git("config", "user.name", "e2e")
	s.writeFile("seed.txt", "x")
	git("add", "seed.txt")
	git("commit", "-qm", "init")

	r := s.run(t, "--mock", "-p",
		`calls:[{"name":"ToolSearch","input":{"query":"select:EnterWorktree,ExitWorktree"}},`+
			`{"name":"EnterWorktree","input":{"name":"e2ewt"}},`+
			`{"name":"ExitWorktree","input":{"action":"keep"}}]`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "worktree") {
		t.Fatalf("worktree results missing: %s", truncStr(r.stdout, 2000))
	}
	wt := filepath.Join(s.cwd, ".claude", "worktrees", "e2ewt")
	if _, err := os.Stat(filepath.Join(wt, "seed.txt")); err != nil {
		t.Fatalf("worktree not created at %s: %v", wt, err)
	}
}

// TestEndConversation: the tool ends the interactive session — process
// exits after the turn.
func TestEndConversation(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s, "--permission-mode", "bypassPermissions")
	// deferred — ToolSearch loads the schema first
	p.sendUntilEchoed(t,
		`calls:[{"name":"ToolSearch","input":{"query":"select:EndConversation"}},`+
			`{"name":"EndConversation","input":{}}]`+"\r",
		`> calls:[{"name":"ToolSearch"`, 8)
	if err := p.waitExit(30 * time.Second); err != nil {
		t.Fatalf("EndConversation should end the session: %v", err)
	}
}

// TestAdvisorTool: the advisor forwards the conversation to a second model
// call — against the mock it returns the mock's reply as advice.
func TestAdvisorTool(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p",
		`calls:[{"name":"Bash","input":{"command":"echo advisor-setup"}},`+
			`{"name":"advisor","input":{}}]`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "tool_result") {
		t.Fatalf("advisor should produce a tool_result: %s", truncStr(r.stdout, 2000))
	}
}

// TestCronCreateList: CronCreate (deferred) schedules a session job and
// CronList enumerates it.
func TestCronCreateList(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p",
		`calls:[{"name":"ToolSearch","input":{"query":"select:CronCreate,CronList"}},`+
			`{"name":"CronCreate","input":{"cron":"0 9 * * *","prompt":"morning check"}},`+
			`{"name":"CronList","input":{}}]`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "morning check") {
		t.Fatalf("CronList should show the created job: %s", truncStr(r.stdout, 2000))
	}
}

// TestTaskStopLive: a background Bash task (b1 — the first bg slot) is
// stopped by TaskStop (deferred tool).
func TestTaskStopLive(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p",
		`calls:[{"name":"ToolSearch","input":{"query":"select:TaskStop"}},`+
			`{"name":"Bash","input":{"command":"sleep 60","run_in_background":true}},`+
			`{"name":"TaskStop","input":{"task_id":"b1"}}]`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "Task b1 stopped") {
		t.Fatalf("expected the task-stop result: %s", truncStr(r.stdout, 2000))
	}
}

// TestReadNotifications: the tool dispatches and reports the empty queue.
func TestReadNotifications(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p",
		`tool:ReadNotifications {}`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "No pending notifications") {
		t.Fatalf("expected empty-notification result: %s", truncStr(r.stdout, 2000))
	}
}

// TestPlanModeBlocksWrite: after EnterPlanMode, a Write call is denied
// read-only-mode style and the file is never created.
func TestPlanModeBlocksWrite(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s, "--permission-mode", "bypassPermissions")

	p.sendUntilEchoed(t,
		`calls:[{"name":"ToolSearch","input":{"query":"select:EnterPlanMode"}},`+
			`{"name":"EnterPlanMode","input":{}}]`+"\r",
		`> calls:[{"name":"ToolSearch"`, 8)
	p.waitFor("plan mode", 30*time.Second)

	target := filepath.Join(s.cwd, "blocked.txt")
	p.sendUntilEchoed(t,
		`tool:Write {"file_path":"`+target+`","content":"nope"}`+"\r",
		"> tool:Write", 8)
	p.waitFor("Tool results received", 30*time.Second)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("plan mode must refuse writes — file exists: %s", target)
	}
	p.exit()
}

// TestBangCommand: `!cmd` runs through Bash locally and the output is
// pushed as a bash-input message (visible to the model next turn).
func TestBangCommand(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s, "--permission-mode", "bypassPermissions")
	p.sendUntilEchoed(t, "!echo bang-marker-42\r", "> !echo", 8)
	p.waitFor("bang-marker-42", 15*time.Second)
	p.exit()
}

// TestCtrlCDoubleExit: ctrl+c on an empty buffer warns once; a second
// press exits.
func TestCtrlCDoubleExit(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)
	p.send("\x03")
	p.waitFor("ctrl-C again to exit", 15*time.Second)
	p.send("\x03")
	if err := p.waitExit(15 * time.Second); err != nil {
		t.Fatalf("double ctrl-C should exit: %v", err)
	}
}
