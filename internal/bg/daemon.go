package bg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Daemon is the background-session supervisor — real's claude-daemon. It
// sweeps the job registry: a worker whose host died unexpectedly (kill -9,
// crash) is respawned — the session resumes its own transcript and a new
// bg-pty-host takes over. Cleanly ended sessions remove their job dir
// before exit, so the sweeper never sees them.

const (
	// defaultSweep is the real-ish cadence for the worker respawn sweep.
	defaultSweep = 5 * time.Second
	// daemonStale is how fresh the daemon heartbeat must be for
	// status/`ensure` to consider the supervisor alive.
	daemonStale = 30 * time.Second
)

type daemonState struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"startedAt"`
	Heartbeat time.Time `json:"heartbeat"`
	Version   string    `json:"version"`
}

func daemonFile() string {
	return filepath.Join(configDir(), "bg", "daemon.json")
}

func configDir() string { return filepath.Dir(filepath.Dir(jobsRoot())) }

// DaemonAlive reports whether a supervisor process is running and healthy.
func DaemonAlive() bool {
	b, err := os.ReadFile(daemonFile())
	if err != nil {
		return false
	}
	var st daemonState
	if json.Unmarshal(b, &st) != nil {
		return false
	}
	return pidAlive(st.PID) && time.Since(st.Heartbeat) < daemonStale
}

// DaemonPID returns the supervisor's pid (0 when none/unknowable).
func DaemonPID() int {
	b, err := os.ReadFile(daemonFile())
	if err != nil {
		return 0
	}
	var st daemonState
	if json.Unmarshal(b, &st) != nil || !pidAlive(st.PID) {
		return 0
	}
	return st.PID
}

// RunDaemon runs the sweeper in the foreground until SIGTERM/SIGINT.
func RunDaemon(version string, sweep time.Duration) int {
	if sweep <= 0 {
		sweep = defaultSweep
	}
	st := daemonState{PID: os.Getpid(), StartedAt: time.Now(), Version: version}
	writeDaemon(st)
	defer os.Remove(daemonFile())

	respawn := func() {
		for _, j := range allJobs() {
			if pidAlive(j.PID) {
				continue // live worker
			}
			// dead host: respawn the session — the new worker resumes the
			// transcript in place (same session id, chained leaf)
			if _, err := Spawn(SpawnOpts{
				SessionID:  j.SessionID,
				ResumeFrom: j.SessionID,
				CWD:        j.CWD,
				Name:       j.Name,
			}); err == nil {
				j.Remove() // the new worker rewrites its own job dir
				os.Remove(j.SocketPath())
			}
		}
	}

	for {
		respawn()
		st.Heartbeat = time.Now()
		writeDaemon(st)
		time.Sleep(sweep)
	}
}

// allJobs lists every job dir WITHOUT the liveness filter — the sweeper
// needs to see dead workers to respawn them (unlike List, which filters).
func allJobs() []*Job {
	ents, err := os.ReadDir(jobsRoot())
	if err != nil {
		return nil
	}
	var out []*Job
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if j, err := loadJob(filepath.Join(jobsRoot(), e.Name())); err == nil {
			out = append(out, j)
		}
	}
	return out
}

func writeDaemon(st daemonState) {
	if err := os.MkdirAll(filepath.Dir(daemonFile()), 0o755); err != nil {
		return
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	_ = os.WriteFile(daemonFile(), b, 0o644)
}

// StopDaemon SIGTERMs a running supervisor.
func StopDaemon() error {
	pid := DaemonPID()
	if pid == 0 {
		return fmt.Errorf("no daemon running")
	}
	signalHost(pid)
	for i := 0; i < 40 && pidAlive(pid); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if pidAlive(pid) {
		return fmt.Errorf("daemon pid %d did not stop", pid)
	}
	return nil
}
