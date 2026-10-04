package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------- durable persistence round-trip ----------

func TestScheduledTasksRoundTrip(t *testing.T) {
	t.Setenv("CLAUDE_GO_CRON_DURABLE", "1")
	dir := t.TempDir()
	c := &Context{Ctx: context.Background(), CWD: dir, Session: "sess-1"}

	in, _ := json.Marshal(map[string]any{
		"cron": "*/5 * * * *", "prompt": "hi", "durable": true})
	r := execCronCreate(c, in)
	if r.IsError {
		t.Fatalf("create: %s", r.Content)
	}
	if !strings.Contains(r.Content, "Persisted to .claude/scheduled_tasks.json") {
		t.Fatalf("expected durable note, got %q", r.Content)
	}

	// real file shape: {"tasks":[{id,cron,prompt,createdAt,recurring,...}]}
	b, err := os.ReadFile(filepath.Join(dir, ".claude", "scheduled_tasks.json"))
	if err != nil {
		t.Fatal("scheduled_tasks.json not written")
	}
	var f struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if json.Unmarshal(b, &f) != nil || len(f.Tasks) != 1 {
		t.Fatalf("bad file: %s", b)
	}
	task := f.Tasks[0]
	if task["cron"] != "*/5 * * * *" || task["prompt"] != "hi" {
		t.Fatalf("task fields: %v", task)
	}
	if _, ok := task["createdAt"].(float64); !ok {
		t.Fatal("createdAt missing/not number")
	}
	if task["recurring"] != true {
		t.Fatal("recurring default should be true")
	}
	if task["createdBySessionId"] != "sess-1" {
		t.Fatal("createdBySessionId missing")
	}
	if _, ok := task["createdByPid"]; !ok {
		t.Fatal("createdByPid missing")
	}
	if task["createdInProject"] != dir {
		t.Fatal("createdInProject missing")
	}

	// list merges durable + session rows
	r = execCronList(c, nil)
	if r.IsError || !strings.Contains(r.Content, "(recurring)") ||
		strings.Contains(r.Content, "[session-only]") {
		t.Fatalf("list: %q", r.Content)
	}

	// a "next session" reader sees the job (re-advertised from disk)
	tasks := loadScheduledTasks(dir)
	if len(tasks) != 1 {
		t.Fatal("reload lost the durable task")
	}

	// delete removes from the file
	del, _ := json.Marshal(map[string]string{"id": tasks[0].ID})
	r = execCronDelete(c, del)
	if r.IsError || r.Content != "Cancelled job "+tasks[0].ID+"." {
		t.Fatalf("delete: %q", r.Content)
	}
	if got := loadScheduledTasks(dir); len(got) != 0 {
		t.Fatal("durable task not removed")
	}
}

func TestDurableGateOffIsSessionOnly(t *testing.T) {
	dir := t.TempDir()
	c := &Context{Ctx: context.Background(), CWD: dir, Session: "s"}
	in, _ := json.Marshal(map[string]any{
		"cron": "*/5 * * * *", "prompt": "hi", "durable": true})
	r := execCronCreate(c, in)
	if r.IsError {
		t.Fatalf("create: %s", r.Content)
	}
	if !strings.Contains(r.Content, "Session-only") {
		t.Fatalf("gate off should stay session-only: %q", r.Content)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude", "scheduled_tasks.json")); !os.IsNotExist(err) {
		t.Fatal("file written while gate off")
	}
	cronMu.Lock()
	sessJobs = nil
	cronMu.Unlock()
}

// ---------- scheduler lock (mod_1918.js oe/U) ----------

func TestSchedLockAcquireRelease(t *testing.T) {
	dir := t.TempDir()
	if !acquireSchedLock(dir, "sess-A") {
		t.Fatal("first acquire failed")
	}
	if readSchedLock(dir) == nil {
		t.Fatal("lock file not written")
	}
	// same session re-acquires fine
	if !acquireSchedLock(dir, "sess-A") {
		t.Fatal("same-session reacquire failed")
	}
	releaseSchedLock(dir, "sess-A")
	if readSchedLock(dir) != nil {
		t.Fatal("lock not released")
	}
}

func TestSchedLockLiveHolderBlocks(t *testing.T) {
	dir := t.TempDir()
	// foreign session holding the lock with a live pid (ours)
	writeSchedLock(dir, schedLockRec{
		SessionID: "other", Pid: os.Getpid(), AcquiredAt: time.Now().UnixMilli()})
	if acquireSchedLock(dir, "sess-B") {
		t.Fatal("should be blocked by live holder")
	}
	releaseSchedLock(dir, "sess-B")
	// foreign lock survives our release attempt
	if readSchedLock(dir) == nil {
		t.Fatal("foreign lock wrongly released")
	}
}

func TestSchedLockStaleRecovery(t *testing.T) {
	dir := t.TempDir()
	writeSchedLock(dir, schedLockRec{
		SessionID: "dead", Pid: 99999999, ProcStart: "bogus",
		AcquiredAt: time.Now().UnixMilli() - 60000})
	if !acquireSchedLock(dir, "sess-C") {
		t.Fatal("stale lock should be recovered")
	}
	cur := readSchedLock(dir)
	if cur == nil || cur.SessionID != "sess-C" {
		t.Fatal("recovered lock not ours")
	}
}

// ---------- missed one-shot surfacing ----------

func TestMissedOneShotSurfaced(t *testing.T) {
	dir := t.TempDir()
	// a one-shot whose first fire (every-minute cron created 5min ago) is past
	past := time.Now().Add(-5 * time.Minute)
	spec := scheduledTask{
		ID: "missed1", Cron: "* * * * *", Prompt: "check the deploy",
		CreatedAt: past.UnixMilli(), CreatedInProject: dir,
	}
	rec := scheduledTask{
		ID: "keep1", Cron: "30 9 * * *", Prompt: "daily thing",
		CreatedAt: past.UnixMilli(), Recurring: true, CreatedInProject: dir,
	}
	if err := saveScheduledTasks(dir, []scheduledTask{spec, rec}); err != nil {
		t.Fatal(err)
	}
	// drain queue
	for len(SessionQueue) > 0 {
		<-SessionQueue
	}
	sched.mu.Lock()
	sched.dir, sched.sessionID = dir, "sess-x"
	sched.mu.Unlock()
	surfaceMissedOneShots(dir)
	sched.mu.Lock()
	sched.dir, sched.sessionID = "", ""
	sched.mu.Unlock()

	var got string
	select {
	case got = <-SessionQueue:
	case <-time.After(2 * time.Second):
		t.Fatal("no missed-task prompt queued")
	}
	if !strings.Contains(got, "one-shot scheduled task was missed") ||
		!strings.Contains(got, "AskUserQuestion") ||
		!strings.Contains(got, "check the deploy") {
		t.Fatalf("bad missed prompt: %q", got)
	}
	rest := loadScheduledTasks(dir)
	if len(rest) != 1 || rest[0].ID != "keep1" {
		t.Fatalf("missed task should be removed, recurring kept: %+v", rest)
	}
}

// ---------- cron engine ----------

func TestParseCronStrict(t *testing.T) {
	cases := []struct {
		expr  string
		valid bool
	}{
		{"*/5 * * * *", true}, {"30 14 * * *", true}, {"0 9 * * 1-5", true},
		{"0 0 * * 7", true}, // dow 7 = 0
		{"0,30 9 * * *", true}, {"0 9-17/2 * * *", true},
		{"61 * * * *", false}, {"0 9", false}, {"a b c d e", false},
		{"0 9 * * 8", false},
	}
	for _, c := range cases {
		if got := parseCron(c.expr) != nil; got != c.valid {
			t.Errorf("parseCron(%q) valid=%v want %v", c.expr, got, c.valid)
		}
	}
}

func TestNextCronTime(t *testing.T) {
	s := parseCron("30 14 * * *")
	base := time.Date(2026, 10, 2, 14, 31, 0, 0, time.Local)
	nf := nextCronTime(s, base)
	if nf == nil {
		t.Fatal("no next fire")
	}
	want := time.Date(2026, 10, 3, 14, 30, 0, 0, time.Local)
	if !nf.Equal(want) {
		t.Fatalf("next = %v want %v", nf, want)
	}
	// strictly after: same-minute base rolls to next day
	nf2 := nextCronTime(s, time.Date(2026, 10, 2, 14, 30, 30, 0, time.Local))
	if !nf2.Equal(want) {
		t.Fatalf("next2 = %v want %v", nf2, want)
	}
}

func TestHumanSchedule(t *testing.T) {
	cases := map[string]string{
		"* * * * *":   "Every minute",
		"*/5 * * * *": "Every 5 minutes",
		"0 * * * *":   "Every hour",
		"7 * * * *":   "Every hour at :07",
		"0 */2 * * *": "Every 2 hours",
		"30 14 * * *": "Every day at 2:30 PM",
		"30 14 * * 1": "Every Monday at 2:30 PM",
		"0 9 * * 1-5": "Weekdays at 9:00 AM",
		"0 8 1 * *":   "0 8 1 * *", // unmatched → raw expr
	}
	for expr, want := range cases {
		if got := humanSchedule(expr); got != want {
			t.Errorf("humanSchedule(%q) = %q want %q", expr, got, want)
		}
	}
}

// ---------- body/result shapes ----------

func TestCronCreateResultShape(t *testing.T) {
	c := &Context{Ctx: context.Background(), CWD: t.TempDir()}
	in, _ := json.Marshal(map[string]any{"cron": "*/5 * * * *", "prompt": "hi"})
	r := execCronCreate(c, in)
	if r.IsError {
		t.Fatalf("create: %s", r.Content)
	}
	if !strings.Contains(r.Content, "Scheduled recurring job") ||
		!strings.Contains(r.Content, "Every 5 minutes") ||
		!strings.Contains(r.Content, "Auto-expires after 7 days") {
		t.Fatalf("bad result text: %q", r.Content)
	}
	// one-shot
	in, _ = json.Marshal(map[string]any{
		"cron": "30 14 * * *", "prompt": "once", "recurring": false})
	r = execCronCreate(c, in)
	if r.IsError || !strings.Contains(r.Content, "Scheduled one-shot task") ||
		!strings.Contains(r.Content, "fire once then auto-delete") {
		t.Fatalf("bad one-shot text: %q", r.Content)
	}
	// invalid cron
	in, _ = json.Marshal(map[string]any{"cron": "bad expr", "prompt": "x"})
	r = execCronCreate(c, in)
	if !r.IsError || !strings.Contains(r.Content, "Expected 5 fields") {
		t.Fatalf("bad expr not rejected: %q", r.Content)
	}
	cronMu.Lock()
	sessJobs = nil
	cronMu.Unlock()
}
