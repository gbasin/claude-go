package bg

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
)

// SpawnOpts seeds a detached bg worker — /background (Resume: current
// session keeps running headless) and --bg (fresh session id, optional
// first prompt).
type SpawnOpts struct {
	SessionID  string
	CWD        string
	ResumeFrom string
	Prompt     string
	Name       string
}

// Spawn launches `claude-go bg-host` detached (own session, stdio to
// /dev/null) and returns the job record the worker will publish. The
// registry entry lands when the host writes job.json — call WaitJob.
func Spawn(o SpawnOpts) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	args := []string{"bg-host", "--session", o.SessionID, "--cwd", o.CWD}
	if o.ResumeFrom != "" {
		args = append(args, "--resume-from", o.ResumeFrom)
	}
	if o.Prompt != "" {
		args = append(args, "--prompt", o.Prompt)
	}
	if o.Name != "" {
		args = append(args, "--name", o.Name)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = os.Environ()
	cmd.SysProcAttr = detachAttr()
	// per-worker attach auth — real's CLAUDE_BG_PTY_AUTH: without it any
	// local process that finds the rendezvous socket could drive the pty
	var tok [16]byte
	if _, err := rand.Read(tok[:]); err == nil {
		cmd.Env = append(cmd.Env, "CLAUDE_BG_PTY_AUTH="+hex.EncodeToString(tok[:]))
	}
	cmd.Stdin = nil
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer devnull.Close()
	cmd.Stdout, cmd.Stderr = devnull, devnull
	if dbg := os.Getenv("CLAUDE_GO_BG_DEBUG"); dbg != "" {
		if f, err := os.OpenFile(dbg, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			defer f.Close()
			cmd.Stderr = f
		}
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn bg-host: %w", err)
	}
	go cmd.Wait() // reap — the worker outlives us
	// real's daemon is the service arm of bg sessions — spawned on-demand
	// ("transient spawn") when a session backgrounds. It respawns crashed
	// workers; no-op when already alive or disabled (tests set
	// CLAUDE_GO_BG_NODAEMON so the supervisor doesn't outlive a run).
	if os.Getenv("CLAUDE_GO_BG_NODAEMON") == "" && !DaemonAlive() {
		_, _ = StartDaemon()
	}
	return cmd, nil
}

// StartDaemon launches `claude daemon run` detached — same shape as a
// worker spawn. No-op-ish if a daemon is already alive (callers check).
func StartDaemon() (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, "daemon", "run")
	cmd.Env = os.Environ()
	cmd.SysProcAttr = detachAttr()
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer devnull.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn daemon: %w", err)
	}
	go cmd.Wait()
	return cmd, nil
}
