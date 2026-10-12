//go:build e2e

// tools_more_e2e_test.go — e2e coverage for the remaining dispatchable
// tools: Skill expansion, Glob/Grep, ListAgents, NotebookEdit, worktree
// enter/exit, EndConversation, cron list, and the advisor subcall.
package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	s.env = append(s.env, "ENABLE_TOOL_SEARCH=1")
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
	s.env = append(s.env, "ENABLE_TOOL_SEARCH=1")
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
	s.env = append(s.env, "ENABLE_TOOL_SEARCH=1")
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
	s.env = append(s.env, "ENABLE_TOOL_SEARCH=1")
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
	s.env = append(s.env, "ENABLE_TOOL_SEARCH=1")
	p := startREPL(t, s, "--permission-mode", "bypassPermissions")
	// deferred — ToolSearch loads the schema first
	p.sendUntilEchoed(t,
		`calls:[{"name":"ToolSearch","input":{"query":"select:EndConversation"}},`+
			`{"name":"EndConversation","input":{}}]`+"\r",
		`❯ calls:[{"name":"ToolSearch"`, 8)
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
	s.env = append(s.env, "ENABLE_TOOL_SEARCH=1")
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
	s.env = append(s.env, "ENABLE_TOOL_SEARCH=1")
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
	s.env = append(s.env, "ENABLE_TOOL_SEARCH=1")
	p := startREPL(t, s, "--permission-mode", "bypassPermissions")

	p.sendUntilEchoed(t,
		`calls:[{"name":"ToolSearch","input":{"query":"select:EnterPlanMode"}},`+
			`{"name":"EnterPlanMode","input":{}}]`+"\r",
		`❯ calls:[{"name":"ToolSearch"`, 8)
	p.waitFor("plan mode", 30*time.Second)
	// the turn isn't done when "plan mode" prints — sending the Write
	// directive mid-turn queues it as model text. Wait for the turn's
	// end line first (✻ <verb> for Ns).
	p.waitFor("· done", 30*time.Second)

	target := filepath.Join(s.cwd, "blocked.txt")
	p.sendUntilEchoed(t,
		`tool:Write {"file_path":"`+target+`","content":"nope"}`+"\r",
		"❯ tool:Write", 8)
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
	p.sendUntilEchoed(t, "!echo bang-marker-42\r", "❯ !echo", 8)
	p.waitFor("bang-marker-42", 15*time.Second)
	p.exit()
}

// TestCtrlCDoubleExit: ctrl+c on an empty buffer warns once; a second
// press exits.
func TestCtrlCDoubleExit(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)
	p.send("\x03")
	p.waitFor("Press Ctrl-C again to exit", 15*time.Second)
	p.send("\x03")
	if err := p.waitExit(15 * time.Second); err != nil {
		t.Fatalf("double ctrl-C should exit: %v", err)
	}
}

// TestContinueFlag: `-c` resumes the cwd's latest session — the next
// request carries the previous exchange's history.
func TestContinueFlag(t *testing.T) {
	s := newSession(t)
	r1 := s.run(t, "--mock", "-p", "remember kw-marker-A")
	s.mustOK(t, r1)

	rec := filepath.Join(t.TempDir(), "reqs")
	r2 := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, 60*time.Second,
		"--mock", "-c", "-p", "hi")
	s.mustOK(t, r2)

	ents, err := os.ReadDir(rec)
	if err != nil || len(ents) == 0 {
		t.Fatalf("no recorded requests from the resumed run: %v", err)
	}
	var carried bool
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(rec, e.Name()))
		if bytes.Contains(b, []byte("kw-marker-A")) {
			carried = true
		}
	}
	if !carried {
		t.Fatal("-c did not carry the prior session's messages into the request")
	}
}

// TestOutputFormatJSON: --output-format json emits the single result blob.
func TestOutputFormatJSON(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p", "hi", "--output-format", "json")
	s.mustOK(t, r)
	var out struct {
		Type      string `json:"type"`
		Result    string `json:"result"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatalf("json output didn't parse: %v\n%s", err, truncStr(r.stdout, 500))
	}
	if strings.TrimSpace(out.Result) != "Mock response to: hi" || out.SessionID == "" {
		t.Fatalf("unexpected json result: %+v", out)
	}
}

// TestPipeStdin: `claude < file` — each stdin line is a prompt answered in
// order (runPipe path, no PTY).
func TestPipeStdin(t *testing.T) {
	s := newSession(t)
	r := s.runStdin(t, "pipe line one\npipe line two\n", "--mock")
	s.mustOK(t, r)
	for _, want := range []string{"pipe line one", "pipe line two"} {
		if !strings.Contains(r.stdout, want) {
			t.Fatalf("pipe reply missing %q: %s", want, truncStr(r.stdout, 1000))
		}
	}
}

// TestInputFormatStreamJSON: --input-format stream-json consumes JSONL
// user messages from stdin in -p mode.
func TestInputFormatStreamJSON(t *testing.T) {
	s := newSession(t)
	r := s.runStdin(t,
		`{"type":"user","message":{"role":"user","content":"stream-in marker"}}`+"\n",
		"--mock", "-p", "--input-format", "stream-json", "--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "Mock response to: stream-in marker") {
		t.Fatalf("stream-json input not answered: %s", truncStr(r.stdout, 1500))
	}
}

// TestAtMention: @file expands into the prompt — the file content lands on
// the wire (recorded request body).
func TestAtMention(t *testing.T) {
	s := newSession(t)
	s.writeFile("men.txt", "MENTION-CONTENTS-77")
	rec := filepath.Join(t.TempDir(), "reqs")

	p := s.runPTY(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, "--mock")
	p.waitFor("❯", 30*time.Second)
	p.sendUntilEchoed(t, "read @men.txt\r", "❯ read @men.txt", 8)
	p.waitFor("Mock response to:", 30*time.Second)
	p.exit()

	ents, _ := os.ReadDir(rec)
	var found bool
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(rec, e.Name()))
		if bytes.Contains(b, []byte("MENTION-CONTENTS-77")) {
			found = true
		}
	}
	if !found {
		t.Fatal("@mention content did not reach the wire")
	}
}

// TestMonitorArm: the deferred Monitor tool arms a background task whose
// stdout becomes task-notifications.
func TestMonitorArm(t *testing.T) {
	s := newSession(t)
	s.env = append(s.env, "ENABLE_TOOL_SEARCH=1")
	r := s.run(t, "--mock", "-p",
		`calls:[{"name":"ToolSearch","input":{"query":"select:Monitor"}},`+
			`{"name":"Monitor","input":{"command":"echo mon-e2e","timeout_ms":5000}}]`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "armed") {
		t.Fatalf("monitor should report armed: %s", truncStr(r.stdout, 1500))
	}
}

// TestPushNotification: the tool dispatches (osascript path on darwin —
// sandbox may deny, either result is a clean surface).
func TestPushNotification(t *testing.T) {
	s := newSession(t)
	s.env = append(s.env, "ENABLE_TOOL_SEARCH=1")
	r := s.run(t, "--mock", "-p",
		`calls:[{"name":"ToolSearch","input":{"query":"select:PushNotification"}},`+
			`{"name":"PushNotification","input":{"message":"e2e ping"}}]`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "Notification sent") &&
		!strings.Contains(r.stdout, "notification failed") &&
		!strings.Contains(r.stdout, "actively at the terminal") {
		t.Fatalf("unexpected notification result: %s", truncStr(r.stdout, 1500))
	}
}

// TestScheduleWakeupNoop: the noop path returns without arming.
func TestScheduleWakeupNoop(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p",
		`tool:ScheduleWakeup {"noop":true}`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "No wakeup scheduled") {
		t.Fatalf("unexpected wakeup result: %s", truncStr(r.stdout, 1500))
	}
}

// TestReportFindings: writes a findings artifact and reports the path.
func TestReportFindings(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p",
		`tool:ReportFindings {"level":"info","findings":[{"title":"e2e","severity":"low"}]}`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "Findings recorded to") {
		t.Fatalf("unexpected findings result: %s", truncStr(r.stdout, 1500))
	}
}

// TestModelPicker: bare /model lists the catalog; typing the index picks
// the model — the next request goes out on the picked model.
func TestModelPicker(t *testing.T) {
	s := newSession(t)
	rec := filepath.Join(t.TempDir(), "reqs")
	p := s.runPTY(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, "--mock")
	p.waitFor("❯", 30*time.Second)
	p.sendUntilEchoed(t, "/model\r", "❯ /model", 8)
	p.waitFor("✔", 15*time.Second)
	// cursor starts on the active model (opus); ↓ reaches Sonnet
	p.send("\x1b[B\r")
	p.waitFor("model → claude-sonnet-5-5", 15*time.Second)

	p.sendUntilEchoed(t, "hi\r", "❯ hi", 8)
	p.waitFor("Mock response to: hi", 30*time.Second)
	p.exit()

	ents, _ := os.ReadDir(rec)
	var found bool
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(rec, e.Name()))
		var req struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(b, &req) == nil && req.Model == "claude-sonnet-5-5" {
			found = true
		}
	}
	if !found {
		t.Fatal("picked model did not reach the wire")
	}
}

// TestRemainingToolsDispatch: every offered tool dispatches to a clean
// result — never "Unknown tool" — even when the backend (cloud, LSP
// server, design service) is absent.
func TestRemainingToolsDispatch(t *testing.T) {
	s := newSession(t)
	s.env = append(s.env, "ENABLE_TOOL_SEARCH=1")
	// deferred tools need select first; harmless to select all
	r := s.run(t, "--mock", "-p",
		`calls:[{"name":"ToolSearch","input":{"query":"select:RemoteTrigger,LSP,DesignSync,ArtifactComments,ArtifactData,SendFeedback,SendFeedback,ReadNotifications,ScheduleWakeup"}},`+
			`{"name":"RemoteTrigger","input":{"action":"list"}},`+
			`{"name":"LSP","input":{"operation":"start","serverName":"tsserver"}},`+
			`{"name":"DesignSync","input":{}},`+
			`{"name":"ArtifactComments","input":{}},`+
			`{"name":"ArtifactData","input":{}},`+
			`{"name":"SendFeedback","input":{"feedback":"e2e"}}]`,
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json")
	s.mustOK(t, r)
	if strings.Contains(r.stdout, "Unknown tool") {
		// surface which one
		for _, line := range strings.Split(r.stdout, "\n") {
			if strings.Contains(line, "Unknown tool") {
				t.Fatalf("unregistered tool on the wire surface: %s", truncStr(line, 300))
			}
		}
	}
}

// TestQuickMemory: `# note` appends to the project MEMORY.md — the memory
// is written to the project-scoped memory dir.
func TestQuickMemory(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)
	p.sendUntilEchoed(t, "# mem-fact-99\r", "❯ # mem-fact-99", 8)
	// real’s ack rotates (Got it./Good to know./Noted.) — poll the file
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(filepath.Join(s.home, ".claude", "projects", "*", "memory", "MEMORY.md"))
		for _, m := range matches {
			if b, _ := os.ReadFile(m); strings.Contains(string(b), "mem-fact-99") {
				p.exit()
				return
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	p.exit()

	// memory lives under ~/.claude/projects/<slug>/memory/MEMORY.md
	matches, _ := filepath.Glob(filepath.Join(s.home, ".claude", "projects", "*", "memory", "MEMORY.md"))
	var found bool
	for _, m := range matches {
		b, _ := os.ReadFile(m)
		if strings.Contains(string(b), "mem-fact-99") {
			found = true
		}
	}
	if !found {
		t.Fatalf("quick memory not persisted — matches: %v", matches)
	}
}

// TestClaudeMDOnWire: a project CLAUDE.md rides the first request as a
// system-reminder context block (verified in the recorded request).
func TestClaudeMDOnWire(t *testing.T) {
	s := newSession(t)
	s.writeFile("CLAUDE.md", "PROJECT-RULES-E2E: be terse")
	rec := filepath.Join(t.TempDir(), "reqs")

	r := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, 60*time.Second,
		"--mock", "-p", "hi")
	s.mustOK(t, r)

	ents, _ := os.ReadDir(rec)
	var found bool
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(rec, e.Name()))
		if bytes.Contains(b, []byte("PROJECT-RULES-E2E")) {
			found = true
		}
	}
	if !found {
		t.Fatal("CLAUDE.md content did not reach the wire")
	}
}

// TestSearchTools: Glob/Grep/LS dispatch end-to-end — pattern match,
// content search, directory listing all return real results.
func TestSearchTools(t *testing.T) {
	s := newSession(t)
	s.env = append(s.env, "ENABLE_TOOL_SEARCH=1")
	s.writeFile("alpha-unique.go", "package x\n// MARKER-GREP-E2E\n")
	s.writeFile("beta-unique.go", "package x\n")

	p := startREPL(t, s, "--permission-mode", "bypassPermissions")

	// Glob
	p.sendUntilEchoed(t,
		`calls:[{"name":"ToolSearch","input":{"query":"select:Glob"}},`+
			`{"name":"Glob","input":{"pattern":"*-unique.go"}}]`+"\r",
		"❯ calls:", 8)
	p.waitFor("alpha-unique.go", 30*time.Second)
	p.waitFor("beta-unique.go", 5*time.Second)

	// Grep content mode
	p.sendUntilEchoed(t,
		`calls:[{"name":"ToolSearch","input":{"query":"select:Grep"}},`+
			`{"name":"Grep","input":{"pattern":"MARKER-GREP-E2E","output_mode":"content"}}]`+"\r",
		"❯ calls:", 8)
	p.waitFor("MARKER-GREP-E2E", 30*time.Second)

	// LS
	p.sendUntilEchoed(t,
		`calls:[{"name":"ToolSearch","input":{"query":"select:LS"}},`+
			`{"name":"LS","input":{"path":"`+s.cwd+`"}}]`+"\r",
		"❯ calls:", 8)
	p.waitFor("alpha-unique.go", 30*time.Second)
	p.exit()
}

// TestMultiEdit: MultiEdit applies ordered find-replace pairs in one call.
func TestMultiEdit(t *testing.T) {
	s := newSession(t)
	s.env = append(s.env, "ENABLE_TOOL_SEARCH=1")
	f := s.writeFile("editme.txt", "hello one\nhello two\n")
	// Read first — Write-family tools require it

	p := startREPL(t, s, "--permission-mode", "bypassPermissions")
	p.sendUntilEchoed(t,
		`calls:[{"name":"ToolSearch","input":{"query":"select:MultiEdit"}},`+
			`{"name":"Read","input":{"file_path":"`+f+`"}},`+
			`{"name":"MultiEdit","input":{"file_path":"`+f+`","edits":[`+
			`{"old_string":"hello one","new_string":"bye one"},`+
			`{"old_string":"hello two","new_string":"bye two"}]}}]`+"\r",
		"❯ calls:", 8)
	p.waitFor("Tool results received", 30*time.Second)
	b, _ := os.ReadFile(f)
	if string(b) != "bye one\nbye two\n" {
		t.Fatalf("MultiEdit didn't apply both edits: %q", b)
	}
	p.exit()
}

// TestNotebookRead: NotebookRead returns cell content for an .ipynb file.
func TestNotebookRead(t *testing.T) {
	s := newSession(t)
	s.env = append(s.env, "ENABLE_TOOL_SEARCH=1")
	nb := s.writeFile("book.ipynb",
		`{"cells":[{"cell_type":"code","execution_count":1,`+
			`"source":["print(1)"],"outputs":[],"metadata":{}}],`+
			`"nbformat":4,"nbformat_minor":5,"metadata":{}}`)
	p := startREPL(t, s, "--permission-mode", "bypassPermissions")
	p.sendUntilEchoed(t,
		`calls:[{"name":"ToolSearch","input":{"query":"select:NotebookRead"}},`+
			`{"name":"NotebookRead","input":{"notebook_path":"`+nb+`"}}]`+"\r",
		"❯ calls:", 8)
	p.waitFor("print(1)", 30*time.Second)
	p.exit()
}

// TestTaskTools: TaskCreate → TaskList → TaskGet → TaskUpdate lifecycle
// through the deferred surface.
func TestTaskTools(t *testing.T) {
	s := newSession(t)
	s.env = append(s.env, "ENABLE_TOOL_SEARCH=1")
	// task tools are gated behind CLAUDE_CODE_TASK_TOOLS / tasks.enabled
	p := s.runPTY(t, []string{"CLAUDE_CODE_TASK_TOOLS=1"},
		"--mock", "--permission-mode", "bypassPermissions")
	p.waitFor("❯", 30*time.Second)

	p.sendUntilEchoed(t,
		`calls:[{"name":"ToolSearch","input":{"query":"select:TaskCreate,TaskList,TaskGet,TaskUpdate"}},`+
			`{"name":"TaskCreate","input":{"subject":"e2e task","description":"do the thing"}},`+
			`{"name":"TaskList","input":{}}]`+"\r",
		"❯ calls:", 8)
	p.waitFor("Task #", 30*time.Second)
	p.waitFor("e2e task", 10*time.Second)

	p.sendUntilEchoed(t,
		`calls:[{"name":"TaskGet","input":{"taskId":"1"}},`+
			`{"name":"TaskUpdate","input":{"taskId":"1","status":"completed"}},`+
			`{"name":"TaskList","input":{}}]`+"\r",
		"❯ calls:", 8)
	p.waitFor("completed", 30*time.Second)
	p.exit()
}

// TestTodoWrite: TodoWrite manages the checklist — /todos shows it.
func TestTodoWrite(t *testing.T) {
	s := newSession(t)
	s.env = append(s.env, "ENABLE_TOOL_SEARCH=1")
	p := startREPL(t, s, "--permission-mode", "bypassPermissions")
	p.sendUntilEchoed(t,
		`calls:[{"name":"ToolSearch","input":{"query":"select:TodoWrite"}},`+
			`{"name":"TodoWrite","input":{"todos":[{"content":"write the tests","status":"in_progress","activeForm":"writing tests"},{"content":"ship it","status":"pending","activeForm":"shipping"}]}}]`+"\r",
		"❯ calls:", 8)
	p.waitFor("Tool results received", 30*time.Second)
	p.sendUntilEchoed(t, "/todos\r", "❯ /todos", 8)
	p.waitFor("write the tests", 15*time.Second)
	p.waitFor("ship it", 5*time.Second)
	p.exit()
}

// TestTaskToolsGated: without the tasks.enabled gate the Task* tools are
// absent from the wire surface (registration and surface stay in sync —
// a surfaced-but-unregistered tool fails with "Unknown tool").
func TestTaskToolsGated(t *testing.T) {
	s := newSession(t)
	r, bodies := recordRequests(t, s, "-p", "hi")
	s.mustOK(t, r)
	for _, b := range bodies {
		if bytes.Contains(b, []byte(`"TaskCreate"`)) ||
			bytes.Contains(b, []byte(`"TaskList"`)) {
			t.Fatal("Task* tools on the wire without tasks.enabled")
		}
	}
}

// TestBashTimeout: a Bash that exceeds its timeout errors with
// "Command timed out" — the tool_use_error reaches the model.
func TestBashTimeout(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s, "--permission-mode", "bypassPermissions")
	p.sendUntilEchoed(t,
		`tool:Bash {"command":"python3 -c \"import time;time.sleep(20)\"","timeout":1500}`+"\r",
		"❯ tool:Bash", 8)
	p.waitFor("timed out after 1.5s", 30*time.Second)
	p.exit()
}

// TestBashAutoBackground: CLAUDE_CODE_AUTO_BACKGROUND_TIMEOUT_MS=N flips
// a long foreground command into a background task at N ms.
func TestBashAutoBackground(t *testing.T) {
	s := newSession(t)
	p := s.runPTY(t, []string{"CLAUDE_CODE_AUTO_BACKGROUND_TIMEOUT_MS=2500"},
		"--mock", "--permission-mode", "bypassPermissions")
	p.waitFor("❯", 30*time.Second)
	p.sendUntilEchoed(t,
		`tool:Bash {"command":"python3 -c \"import time;time.sleep(15)\""}`+"\r",
		"❯ tool:Bash", 8)
	p.waitFor("moved to the background", 30*time.Second)
	p.exit()
}

// TestTaskToolsSettingsGate: tasks.enabled in project settings enables
// Task* tools — resolved against the project dir, not launch PWD.
func TestTaskToolsSettingsGate(t *testing.T) {
	s := newSession(t)
	s.writeSettings(`{"tasks":{"enabled":true}}`)
	rec := t.TempDir() + "/reqs"
	r := s.runEx(t, []string{"CLAUDE_GO_MOCK_RECORD=" + rec}, 60*time.Second,
		"--mock", "-p", "hi", "--output-format", "stream-json")
	s.mustOK(t, r)

	ents, _ := os.ReadDir(rec)
	var wire []byte
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(rec, e.Name()))
		if bytes.Contains(b, []byte(`"tools"`)) || bytes.Contains(b, []byte("TaskCreate")) {
			wire = b
		}
	}
	if !bytes.Contains(wire, []byte("TaskCreate")) {
		t.Fatal("tasks.enabled=true but TaskCreate absent from wire")
	}

	// dispatch path
	r = s.run(t, "--mock", "-p",
		`calls:[{"name":"TaskCreate","input":{"subject":"s","description":"d"}},`+
			`{"name":"TaskList","input":{}}]`,
		"--permission-mode", "bypassPermissions", "--output-format", "stream-json")
	s.mustOK(t, r)
	if !strings.Contains(r.stdout, "Task") {
		t.Fatalf("TaskList didn't dispatch under settings gate: %s", truncStr(r.stdout, 1500))
	}
}

// TestSandboxDenyWrite: sandbox.filesystem.denyWrite re-denies a path
// inside the workspace — the write is refused before touching the FS.
func TestSandboxDenyWrite(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("seatbelt profile is macOS-only")
	}
	s := newSession(t)
	os.MkdirAll(filepath.Join(s.cwd, "protected"), 0o755)
	s.writeSettings(`{"sandbox":{"enabled":true,"filesystem":{"denyWrite":["protected/**"]}}}`)
	p := s.runPTY(t, nil, "--mock", "--permission-mode", "bypassPermissions")
	p.waitFor("❯", 30*time.Second)
	p.sendUntilEchoed(t,
		`tool:Bash {"command":"touch protected/x && echo SHOULD-NOT-PRINT"}`+"\r",
		"❯ tool:Bash", 8)
	p.waitFor("Tool results received", 30*time.Second)
	if _, err := os.Stat(filepath.Join(s.cwd, "protected", "x")); err == nil {
		t.Fatal("denyWrite path was written — sandbox didn't block it")
	}
	// counter-case: a sibling path stays writable under the sandbox
	p.sendUntilEchoed(t,
		`tool:Bash {"command":"touch allowed-x"}`+"\r", "❯ tool:Bash", 8)
	p.waitFor("Tool results received", 30*time.Second)
	p.exit()
	if _, err := os.Stat(filepath.Join(s.cwd, "allowed-x")); err != nil {
		t.Fatal("sandbox blocked a write outside denyWrite")
	}
}

// TestSandboxDenyNetwork: sandbox.network.deniedDomains:["*"] blocks
// outbound — a curl to example.com fails inside seatbelt.
func TestSandboxDenyNetwork(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("seatbelt profile is macOS-only")
	}
	s := newSession(t)
	s.writeSettings(`{"sandbox":{"enabled":true,"network":{"deniedDomains":["*"]}}}`)
	p := s.runPTY(t, nil, "--mock", "--permission-mode", "bypassPermissions")
	p.waitFor("❯", 30*time.Second)
	p.sendUntilEchoed(t,
		`tool:Bash {"command":"curl -s -m 5 -o /dev/null -w \"%{http_code}\" https://example.com || echo NET-DENIED-E2E"}`+"\r",
		"❯ tool:Bash", 8)
	out := p.waitFor("Tool results received", 30*time.Second)
	p.exit()
	if !strings.Contains(out, "000") && !strings.Contains(out, "NET-DENIED") {
		t.Fatalf("network deny not enforced:\n%s", truncStr(out, 800))
	}
}

// TestSandboxDenyRead: filesystem.denyRead blocks reads inside the
// sandbox even though the file exists and is world-readable.
func TestSandboxDenyRead(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("seatbelt profile is macOS-only")
	}
	s := newSession(t)
	s.writeFile("protected/secret.txt", "SECRET-CONTENTS\n")
	s.writeSettings(`{"sandbox":{"enabled":true,"filesystem":{"denyRead":["protected/**"]}}}`)
	p := s.runPTY(t, nil, "--mock", "--permission-mode", "bypassPermissions")
	p.waitFor("❯", 30*time.Second)
	p.sendUntilEchoed(t,
		`tool:Bash {"command":"cat protected/secret.txt 2>&1 || echo READ-DENIED"}`+"\r",
		"❯ tool:Bash", 8)
	out := p.waitFor("Tool results received", 30*time.Second)
	p.exit()
	if strings.Contains(out, "SECRET-CONTENTS") {
		t.Fatal("denyRead path was readable under sandbox")
	}
}

// TestSandboxDangerouslyDisable: dangerouslyDisableSandbox:true bypasses
// the profile — a denied-write path becomes writable.
func TestSandboxDangerouslyDisable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("seatbelt profile is macOS-only")
	}
	s := newSession(t)
	s.writeSettings(`{"sandbox":{"enabled":true,"filesystem":{"denyWrite":["protected/**"]},"allowUnsandboxedCommands":true}}`)
	os.MkdirAll(filepath.Join(s.cwd, "protected"), 0o755)
	p := s.runPTY(t, nil, "--mock", "--permission-mode", "bypassPermissions")
	p.waitFor("❯", 30*time.Second)
	p.sendUntilEchoed(t,
		`tool:Bash {"command":"touch protected/bypassed","dangerouslyDisableSandbox":true}`+"\r",
		"❯ tool:Bash", 8)
	p.waitFor("Tool results received", 30*time.Second)
	p.exit()
	if !fileExists(filepath.Join(s.cwd, "protected", "bypassed")) {
		t.Fatal("dangerouslyDisableSandbox didn't bypass the profile")
	}
}
