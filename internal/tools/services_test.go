package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCronMatch(t *testing.T) {
	at := time.Date(2026, 10, 2, 14, 30, 0, 0, time.Local) // Friday
	cases := []struct {
		expr string
		want bool
	}{
		{"* * * * *", true},
		{"30 14 * * *", true},
		{"31 14 * * *", false},
		{"*/15 * * * *", true},
		{"0-30 * * * *", true},
		{"45-59 * * * *", false},
		{"* 14 * * 5", true},
		{"* 14 * * 1", false},
		{"bad", false},
	}
	for _, c := range cases {
		if got := matchCron(c.expr, at); got != c.want {
			t.Errorf("matchCron(%q) = %v want %v", c.expr, got, c.want)
		}
	}
}

func TestCronLifecycle(t *testing.T) {
	c := &Context{Ctx: context.Background(), CWD: t.TempDir()}

	create, _ := json.Marshal(map[string]any{"cron": "*/5 * * * *", "prompt": "hi", "recurring": true})
	r := execCronCreate(c, create)
	if r.IsError {
		t.Fatalf("create: %s", r.Content)
	}
	r = execCronList(c, nil)
	if r.IsError || r.Content == "" {
		t.Fatal("list empty after create")
	}
	cronMu.Lock()
	jobs := sessJobs
	cronMu.Unlock()
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d", len(jobs))
	}
	del, _ := json.Marshal(map[string]string{"id": jobs[0].ID})
	r = execCronDelete(c, del)
	if r.IsError {
		t.Fatalf("delete: %s", r.Content)
	}
	cronMu.Lock()
	defer cronMu.Unlock()
	if len(sessJobs) != 0 {
		t.Error("job not deleted")
	}
}

func TestSendMessageListAgents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := &Context{Ctx: context.Background(), CWD: home}
	sm, _ := json.Marshal(map[string]any{"to": "main-agent", "message": "status?"})
	r := execSendMessage(c, sm)
	if r.IsError {
		t.Fatalf("send: %s", r.Content)
	}
	r = execListAgents(c, nil)
	if r.IsError || r.Content == "" {
		t.Fatal("expected message in list")
	}
}

func TestAllToolsDispatch(t *testing.T) {
	c := &Context{Ctx: context.Background(), CWD: t.TempDir()}
	for _, tool := range All() {
		// every tool should respond (even stubs) without panic
		r := Dispatch(c, tool.Def.Name, json.RawMessage(`{}`))
		if r.Content == "" {
			t.Errorf("%s returned empty", tool.Def.Name)
		}
	}
}

func TestBashPersistentCWD(t *testing.T) {
	c := &Context{Ctx: context.Background(), CWD: t.TempDir()}
	real, _ := filepath.EvalSymlinks(c.CWD)
	c.CWD = real
	sub := filepath.Join(c.CWD, "sub")
	os.Mkdir(sub, 0o755)
	in, _ := json.Marshal(map[string]any{"command": "cd sub"})
	r := execBash(c, in)
	if r.IsError {
		t.Fatalf("cd: %s", r.Content)
	}
	if c.CWD != sub {
		t.Errorf("cwd = %s, want %s", c.CWD, sub)
	}
	in2, _ := json.Marshal(map[string]any{"command": "pwd"})
	r = execBash(c, in2)
	if r.Content != sub {
		t.Errorf("pwd = %q, want %q", r.Content, sub)
	}
}

func TestBashSleepBlocked(t *testing.T) {
	c := &Context{Ctx: context.Background(), CWD: t.TempDir()}
	in, _ := json.Marshal(map[string]any{"command": "sleep 5"})
	r := execBash(c, in)
	if !r.IsError {
		t.Error("foreground sleep should be blocked")
	}
}

func TestBgTaskKill(t *testing.T) {
	c := &Context{Ctx: context.Background(), CWD: t.TempDir()}
	in, _ := json.Marshal(map[string]any{
		"command": "sleep 60", "run_in_background": true,
	})
	r := execBash(c, in)
	if r.IsError {
		t.Fatalf("bg: %s", r.Content)
	}
	id := "b1"
	stop, _ := json.Marshal(map[string]string{"task_id": id})
	r = execTaskStop(c, stop)
	if r.IsError {
		t.Fatalf("stop: %s", r.Content)
	}
	select {
	case <-c.BgTasks.Get(id).Done:
	case <-time.After(15 * time.Second): // slow under -race on CI
		t.Error("task not stopped")
	}
}

// sessions/<pid>.json gains bridgeSessionId when Remote Control adopts a
// session (mod_2173.js teleport gate reads this field), and clears cleanly.
func TestBridgeSessionIDField(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := RegisterSession("sess-local", t.TempDir(), "n"); err != nil {
		t.Fatal(err)
	}
	defer UnregisterSession("sess-local")
	read := func() map[string]any {
		b, err := os.ReadFile(sessionFile(os.Getpid()))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if json.Unmarshal(b, &m) != nil {
			t.Fatal("bad session file json")
		}
		return m
	}
	if _, ok := read()["bridgeSessionId"]; ok {
		t.Fatal("bridgeSessionId present before connect")
	}
	SetBridgeSessionID("cse_remote1")
	if got := read()["bridgeSessionId"]; got != "cse_remote1" {
		t.Fatalf("bridgeSessionId=%v", got)
	}
	SetBridgeSessionID("")
	if _, ok := read()["bridgeSessionId"]; ok {
		t.Fatal("bridgeSessionId not cleared")
	}
}
