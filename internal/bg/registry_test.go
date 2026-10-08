package bg

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeJob plants a dead-worker job record (pid 0 / stale heartbeat).
func writeJob(t *testing.T, short string) *Job {
	t.Helper()
	j := &Job{
		SessionID: "session-" + short,
		Short:     short,
		CWD:       t.TempDir(),
		PID:       999999, // almost certainly dead
		Heartbeat: time.Now().Add(-time.Hour),
	}
	if err := j.Write(); err != nil {
		t.Fatal(err)
	}
	return j
}

func TestLookupAnySeesDeadJobs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	j := writeJob(t, "deadbeef")

	// List filters dead jobs; LookupAny must still resolve them
	for _, lj := range List() {
		if lj.Short == j.Short {
			t.Fatal("List included dead job")
		}
	}
	got, err := LookupAny("deadbeef")
	if err != nil || got == nil {
		t.Fatalf("LookupAny: %v %v", got, err)
	}
	if got.SessionID != j.SessionID {
		t.Fatalf("got %v", got.SessionID)
	}
	if got, err := LookupAny("session-dead"); err != nil || got == nil {
		t.Fatal("session-id prefix lookup failed")
	}
	if got, _ := LookupAny("zzzz"); got != nil {
		t.Fatal("unexpected match")
	}
}

func TestRemoveJob(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	j := writeJob(t, "rmme1234")
	if err := RemoveJob("rmme1234"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(j.dir()); !os.IsNotExist(err) {
		t.Fatal("job dir not removed")
	}
	if err := RemoveJob("rmme1234"); err == nil {
		t.Fatal("expected error for missing job")
	}
}

func TestLogsDeadJobFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	j := writeJob(t, "logdead1")
	if err := os.MkdirAll(jobsRoot(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobsRoot(), "last-exit.log"),
		[]byte("exit tail here"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	// no socket → falls back to the last-exit breadcrumb
	if err := Logs("logdead1", &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "exit tail here" {
		t.Fatalf("logs = %q", buf.String())
	}
	_ = j
}
