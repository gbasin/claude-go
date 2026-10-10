package bg

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"claude-go/internal/config"
)

// Job is the registry record for one background session — real's jobDir
// record flattened to what our transient-spawn spine needs. One dir per
// session under <config>/jobs/<sid8>/ holding job.json + rv.sock —
// real's job root is ~/.claude/jobs (mod_0240, mod_0533 error strings).
type Job struct {
	SessionID string    `json:"sessionId"`
	Short     string    `json:"short"` // sid8 — attach/agents key
	CWD       string    `json:"cwd"`
	PID       int       `json:"pid"` // bg-host worker pid
	ChildPID  int       `json:"childPid"`
	StartedAt time.Time `json:"startedAt"`
	Heartbeat time.Time `json:"heartbeat"`
	Name      string    `json:"name,omitempty"`       // optional session name
	Prompt    string    `json:"prompt,omitempty"`     // first prompt (agents listing)
	Auth      string    `json:"auth,omitempty"`       // CLAUDE_BG_PTY_AUTH — attach handshake token
	Version   string    `json:"version,omitempty"`    // cli build that spawned the worker
	Attached  int       `json:"attached,omitempty"`   // live attach clients — real won't respawn
	Activity  time.Time `json:"activityAt,omitempty"` // last pty output — idle-stale guard
}

const heartbeatStale = 15 * time.Second // real heartbeat is 5s; triple it

func jobsRoot() string {
	// real's root: ~/.claude/jobs — migrate any old bg/jobs entries
	dir := filepath.Join(config.ClaudeDir(), "jobs")
	old := filepath.Join(config.ClaudeDir(), "bg", "jobs")
	if ents, err := os.ReadDir(old); err == nil && len(ents) > 0 {
		if _, err2 := os.Stat(dir); os.IsNotExist(err2) {
			if err3 := os.Rename(old, dir); err3 == nil {
				os.Remove(filepath.Join(config.ClaudeDir(), "bg"))
			}
		}
	}
	return dir
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
	// tmp+rename: Beat() rewrites this file every 5s — a plain WriteFile
	// truncates in place and a concurrent Lookup reads torn JSON (the
	// resume-live guard missed the job mid-beat under load)
	tmp := filepath.Join(j.dir(), ".job.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil { // holds the attach token
		return err
	}
	return os.Rename(tmp, filepath.Join(j.dir(), "job.json"))
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

// LookupAny resolves a prefix against every job dir — including dead or
// settled workers (List filters those). `claude rm`/`respawn` need them.
func LookupAny(prefix string) (*Job, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil, nil
	}
	var match *Job
	for _, j := range allJobs() {
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

// Logs fetches the worker's rolling pty tail — `claude logs <id>` prints
// the session's recent terminal output without attaching. A live worker
// answers over the rendezvous socket (hello{mode:"logs"}); a dead one
// falls back to the last-exit breadcrumb.
func Logs(prefix string, stdout io.Writer) error {
	j, err := LookupAny(prefix)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("no background session %q", prefix)
	}
	if j.Alive() {
		conn, err := net.Dial("unix", j.SocketPath())
		if err == nil {
			defer conn.Close()
			hello, _ := json.Marshal(map[string]any{"auth": j.Auth, "mode": "logs"})
			if err := writeFrame(conn, opHello, hello); err != nil {
				return err
			}
			op, payload, err := readFrame(conn)
			if err == nil && op == opReady {
				for {
					op, payload, err = readFrame(conn)
					if err != nil || op == opExit {
						return nil
					}
					if op == opData {
						stdout.Write(payload)
					}
				}
			}
			if err == nil && op == opError {
				return fmt.Errorf("logs %s: %s", j.Short, payload)
			}
		}
	}
	// dead/missing socket — replay the exit breadcrumb if one exists
	if b, err := os.ReadFile(filepath.Join(jobsRoot(), "last-exit.log")); err == nil {
		stdout.Write(b)
		return nil
	}
	return fmt.Errorf("session %s has no output (worker not running)", j.Short)
}

// Respawn restarts a session's worker — `claude respawn <id>`: TERM the
// live host if wedged, then spawn a fresh bg-host that resumes the same
// session transcript (same flow as the daemon's dead-worker sweep).
func Respawn(prefix string) error {
	j, err := LookupAny(prefix)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("no background session %q", prefix)
	}
	if pidAlive(j.PID) {
		signalHost(j.PID)
		for i := 0; i < 40 && pidAlive(j.PID); i++ {
			time.Sleep(50 * time.Millisecond)
		}
		if pidAlive(j.PID) {
			killHost(j.PID)
		}
	}
	var tok [16]byte
	newAuth := ""
	if _, err := rand.Read(tok[:]); err == nil {
		newAuth = hex.EncodeToString(tok[:])
	}
	cmd, err := Spawn(SpawnOpts{
		SessionID:  j.SessionID,
		ResumeFrom: j.SessionID,
		Auth:       newAuth,
		CWD:        j.CWD,
		Name:       j.Name,
		// Prompt intentionally absent — respawn resumes the transcript,
		// it doesn't re-type the original first prompt (daemon parity)
	})
	if err != nil {
		return err
	}
	j.PID, j.ChildPID, j.Heartbeat = cmd.Process.Pid, 0, time.Now()
	j.Attached = 0
	return j.Write()
}

// RespawnAll respawns every registered job (real's `claude respawn --all`).
func RespawnAll() error {
	jobs := allJobs()
	if len(jobs) == 0 {
		return fmt.Errorf("no background sessions")
	}
	var firstErr error
	for _, j := range jobs {
		if err := Respawn(j.Short); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// RemoveJob deletes a session's registry record (and stale socket) —
// `claude rm <id>`. A live worker must be stopped first; the transcript
// under ~/.claude/projects is left alone (real's rm clears job records).
func RemoveJob(prefix string) error {
	j, err := LookupAny(prefix)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("no background session %q", prefix)
	}
	if j.Alive() {
		return fmt.Errorf("session %s is still running — `claude stop %s` first", j.Short, j.Short)
	}
	j.Remove()
	os.Remove(j.SocketPath())
	return nil
}
