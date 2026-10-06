package bg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"claude-go/internal/config"
)

// Job is the registry record for one background session — real's jobDir
// record flattened to what our transient-spawn spine needs. One dir per
// session under <config>/bg/jobs/<sid8>/ holding job.json + rv.sock.
type Job struct {
	SessionID string    `json:"sessionId"`
	Short     string    `json:"short"` // sid8 — attach/agents key
	CWD       string    `json:"cwd"`
	PID       int       `json:"pid"` // bg-host worker pid
	ChildPID  int       `json:"childPid"`
	StartedAt time.Time `json:"startedAt"`
	Heartbeat time.Time `json:"heartbeat"`
	Name      string    `json:"name,omitempty"`   // optional session name
	Prompt    string    `json:"prompt,omitempty"` // first prompt (agents listing)
}

const heartbeatStale = 15 * time.Second // real heartbeat is 5s; triple it

func jobsRoot() string {
	return filepath.Join(config.ClaudeDir(), "bg", "jobs")
}

func (j *Job) dir() string { return filepath.Join(jobsRoot(), j.Short) }

// SocketPath is the rendezvous UDS for attach clients. Sockets live under
// $TMPDIR — job dirs under a deeply-nested config home can blow past the
// ~104-byte sun_path limit (e2e tempdirs do).
func (j *Job) SocketPath() string {
	return filepath.Join(rvDir(), j.Short+".sock")
}

func rvDir() string { return filepath.Join(os.TempDir(), "claude-go-rv") }

// Write persists job.json into the job's dir.
func (j *Job) Write() error {
	if err := os.MkdirAll(j.dir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(j.dir(), "job.json"), b, 0o644)
}

// Beat refreshes the heartbeat timestamp in place.
func (j *Job) Beat() {
	j.Heartbeat = time.Now()
	_ = j.Write()
}

// Remove tears the job dir down (session exited).
func (j *Job) Remove() { _ = os.RemoveAll(j.dir()) }

// Alive reports whether the worker process and heartbeat are both healthy.
func (j *Job) Alive() bool {
	if !pidAlive(j.PID) {
		return false
	}
	return time.Since(j.Heartbeat) < heartbeatStale
}

func loadJob(dir string) (*Job, error) {
	b, err := os.ReadFile(filepath.Join(dir, "job.json"))
	if err != nil {
		return nil, err
	}
	var j Job
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// List returns live registered jobs, newest first. Dead workers are
// filtered but NOT reaped — a crashed host's job dir must survive for the
// daemon's respawn sweep (its pid dir IS the session's liveness record).
// Removal happens on clean exit (host), `claude stop`, or daemon respawn.
func List() []*Job {
	ents, err := os.ReadDir(jobsRoot())
	if err != nil {
		return nil
	}
	var out []*Job
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		j, err := loadJob(filepath.Join(jobsRoot(), e.Name()))
		if err != nil {
			continue
		}
		if !j.Alive() {
			continue
		}
		out = append(out, j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].StartedAt.After(out[k].StartedAt) })
	return out
}

// Lookup resolves a session id, sid prefix, or name to a live job.
func Lookup(prefix string) (*Job, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil, nil
	}
	var match *Job
	for _, j := range List() {
		if j.SessionID == prefix || j.Short == prefix || j.Name == prefix ||
			strings.HasPrefix(j.SessionID, prefix) {
			if match != nil {
				return nil, errAmbiguous
			}
			match = j
		}
	}
	return match, nil
}

// Stop asks a live worker to end its session — real's `claude stop`-era
// path: the host SIGTERM handler TERM's the child pgrp and removes the job.
func Stop(prefix string) error {
	j, err := Lookup(prefix)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("no running background session %q", prefix)
	}
	signalHost(j.PID)
	for i := 0; i < 40 && pidAlive(j.PID); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if pidAlive(j.PID) {
		return fmt.Errorf("worker %s did not stop", j.Short)
	}
	j.Remove()
	os.Remove(j.SocketPath())
	return nil
}
