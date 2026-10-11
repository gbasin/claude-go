package bg

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/gbasin/claude-go/internal/api"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

var (
	dbgOnce sync.Once
	dbgLog  *os.File
)

// Host is real's bg-pty-host (mod_0488): a detached per-session worker that
// owns a pty, runs the REPL inside it, and bridges attach clients over the
// rendezvous socket. The daemon-supervisor tier (respawn sweeps, auth
// rekey, service install) is real's hardening — the transient-spawn shape
// here is real's no-daemon fallback path.
type HostOpts struct {
	SessionID  string // the bg session's own id (attach/registry key)
	CWD        string
	ResumeFrom string // -r <sid> — carry an existing session's history
	Prompt     string // typed into the pty once the session boots
	Name       string
	Cols       int
	Rows       int
}

type attacher struct {
	conn net.Conn
	out  chan []byte // framed, written by pump() — broadcast never blocks
	done chan struct{}
}

// pump is the client's writer goroutine — the only writer on the conn.
// A client that stops reading fills `out`; the next send drops it, never
// stalls the pty bridge (real kills stalled attachers the same way).
func (a *attacher) pump() {
	for framed := range a.out {
		if _, err := a.conn.Write(framed); err != nil {
			break
		}
	}
	close(a.done)
}

// send enqueues a frame; a full outbox means the client is dead — drop it.
func (a *attacher) send(op byte, p []byte) bool {
	var hdr [5]byte
	hdr[0] = op
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(p)))
	framed := append(hdr[:], p...)
	select {
	case a.out <- framed:
		return true
	default:
		return false
	}
}

// drop disconnects the client (kicked or gone).
func (a *attacher) drop() { _ = a.conn.Close() }

// RunHost is the bg-host worker entrypoint; it never returns until the
// session process exits (or it's asked to die).
func RunHost(o HostOpts) int {
	if o.Cols == 0 {
		o.Cols, o.Rows = 120, 40
	}
	short := o.SessionID
	if len(short) > 8 {
		short = short[:8]
	}
	job := &Job{
		SessionID: o.SessionID,
		Short:     short,
		CWD:       o.CWD,
		PID:       os.Getpid(),
		StartedAt: time.Now(),
		Name:      o.Name,
		Prompt:    o.Prompt,
		Auth:      os.Getenv("CLAUDE_BG_PTY_AUTH"), // attach handshake token
		Version:   cliVersion(),
		Activity:  time.Now(),
	}
	if err := os.MkdirAll(job.dir(), 0o755); err != nil {
		return fatalHost("job dir", err)
	}
	job.Heartbeat = time.Now()
	_ = job.Write()

	master, slave, err := openPTY(o.Cols, o.Rows)
	if err != nil {
		return fatalHost("pty", err)
	}

	exe, err := os.Executable()
	if err != nil {
		return fatalHost("exe", err)
	}
	args := []string{exe}
	if os.Getenv("CLAUDE_GO_MOCK") != "" {
		args = append(args, "--mock")
	}
	if o.ResumeFrom != "" {
		// -r carries the source session's history; --session-id pins the
		// NEW session's id so registry/attach/picker all agree on it
		args = append(args, "-r", o.ResumeFrom)
	}
	args = append(args, "--session-id", o.SessionID)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = o.CWD
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.Env = append(os.Environ(),
		"CLAUDE_CODE_SESSION_KIND=bg",
		"CLAUDE_BG_BACKEND=daemon",
		"CLAUDE_JOB_DIR="+job.dir(),
		"CLAUDE_BG_RENDEZVOUS_SOCK="+job.SocketPath(),
	)
	// own session + the pty as controlling terminal — the inner REPL gets
	// real job control (its ^Z suspends the session inside the bg pty)
	cmd.SysProcAttr = childAttr()
	if err := cmd.Start(); err != nil {
		return fatalHost("spawn", err)
	}
	dbgf("child spawned pid=%d args=%v", cmd.Process.Pid, args)
	slave.Close()
	job.ChildPID = cmd.Process.Pid
	_ = job.Write()

	sock := job.SocketPath()
	_ = os.Remove(sock)
	if err := os.MkdirAll(rvDir(), 0o700); err != nil {
		cmd.Process.Kill()
		return fatalHost("rv dir", err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		cmd.Process.Kill()
		return fatalHost("listen", err)
	}
	os.Chmod(sock, 0o600)

	var (
		mu      sync.Mutex
		clients = map[*attacher]bool{}
		tail    []byte     // rolling pty-output tail replayed to new attachers
		ptyMu   sync.Mutex // serializes attach-input writes to the pty
	)

	// broadcast fans pty output to attachers. Frame encoding happens once
	// here; a client whose outbox is full gets dropped rather than stalling
	// the bridge — the pty pump must never block on a dead attach.
	broadcast := func(b []byte) {
		var hdr [5]byte
		hdr[0] = opData
		binary.BigEndian.PutUint32(hdr[1:], uint32(len(b)))
		framed := append(hdr[:], b...)
		mu.Lock()
		tail = append(tail, b...)
		if len(tail) > tailCap {
			tail = tail[len(tail)-tailCap:]
		}
		var dead []*attacher
		for c := range clients {
			select {
			case c.out <- framed:
			default:
				dead = append(dead, c)
			}
		}
		for _, c := range dead {
			delete(clients, c)
		}
		mu.Unlock()
		for _, c := range dead {
			c.drop()
		}
	}

	// pty → clients + tail (+ debug mirror)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				if dbgLog != nil {
					dbgLog.Write(buf[:n])
				}
				job.Activity = time.Now() // real's lastInputAt — version-skew respawn skips busy workers
				broadcast(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	// heartbeat — real's .fleetview-heartbeat (5s)
	go func() {
		for range time.Tick(5 * time.Second) {
			job.Beat()
		}
	}()

	// host lifecycle: child exit or SIGTERM ends the session
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		dbgf("child exited err=%v", waitErr)
		close(done)
	}()
	sigc := make(chan os.Signal, 2)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGINT)
	signalled := false
	go func() {
		select {
		case <-sigc:
			// signal death keeps the job dir — the daemon may respawn
			// the session; only a clean child exit or `claude stop` is
			// a real "session over"
			signalled = true
			killChildPgrp(cmd.Process.Pid, syscall.SIGTERM)
			<-done
		case <-done:
		}
	}()

	// optional first prompt — written into the pty after boot like a user
	// typing it (real seeds the worker with the /background arg)
	if o.Prompt != "" {
		go func() {
			time.Sleep(1500 * time.Millisecond)
			io.WriteString(master, o.Prompt+"\r")
		}()
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveAttach(conn, master, job, &mu, clients, &tail, &ptyMu)
		}
	}()

	<-done
	mu.Lock()
	for c := range clients {
		c.send(opExit, nil)
		c.drop()
	}
	mu.Unlock()
	ln.Close()
	master.Close()
	// last-exit breadcrumb survives the job dir removal — invaluable when
	// a worker dies for no visible reason
	mu.Lock()
	outTail := append([]byte(nil), tail...)
	mu.Unlock()
	if n := len(outTail); n > 600 {
		outTail = outTail[n-600:]
	}
	_ = os.WriteFile(filepath.Join(jobsRoot(), "last-exit.log"),
		[]byte(fmt.Sprintf("%s %s exited: child=%v\n%s", time.Now().Format(time.RFC3339), job.SessionID, waitErr, outTail)), 0o644)
	if !signalled {
		job.Remove()
	}
	return 0
}

const tailCap = 64 * 1024

// serveAttach pumps one attach client: input/resize → pty, pty → client
// via the outbox pump (never back-pressured into the bridge).
func serveAttach(conn net.Conn, master *os.File, job *Job,
	mu *sync.Mutex, clients map[*attacher]bool, tail *[]byte,
	ptyMu *sync.Mutex) {
	defer conn.Close()

	op, payload, err := readFrame(conn)
	if err != nil || op != opHello {
		return
	}
	var hello struct {
		Cols, Rows int
		Auth       string `json:"auth"`
		Mode       string `json:"mode"` // "logs" = tail dump, no attach (claude logs)
	}
	_ = json.Unmarshal(payload, &hello)
	if job.Auth != "" && hello.Auth != job.Auth {
		// wrong/absent token — a stray process found the socket; refuse
		_ = writeFrame(conn, opError, []byte("attach rejected: bad pty auth token"))
		return
	}
	if hello.Mode == "logs" {
		mu.Lock()
		snap := append([]byte(nil), (*tail)...)
		mu.Unlock()
		_ = writeFrame(conn, opReady, mustJSON(map[string]any{
			"session": job.SessionID, "mode": "logs"}))
		if len(snap) > 0 {
			_ = writeFrame(conn, opData, snap)
		}
		_ = writeFrame(conn, opExit, nil)
		return
	}
	if hello.Cols > 0 {
		setWinSize(master, hello.Cols, hello.Rows)
		if job.ChildPID > 0 {
			killChildPgrp(job.ChildPID, sigWinch)
		}
	}

	a := &attacher{conn: conn, out: make(chan []byte, 64), done: make(chan struct{})}
	go a.pump()
	// opReady + snapshot must leave BEFORE the attacher joins clients —
	// a concurrent broadcast queued between registration and opReady
	// makes the client see opData first ("unexpected handshake op 3").
	mu.Lock()
	snap := append([]byte(nil), (*tail)...)
	mu.Unlock()
	a.send(opReady, mustJSON(map[string]any{
		"session": job.SessionID, "cols": hello.Cols, "rows": hello.Rows}))
	if len(snap) > 0 {
		var hdr [5]byte
		hdr[0] = opData
		binary.BigEndian.PutUint32(hdr[1:], uint32(len(snap)))
		select {
		case a.out <- append(hdr[:], snap...):
		default:
			return // client already not reading — let the disconnect path reap
		}
	}
	mu.Lock()
	clients[a] = true
	job.Attached++
	_ = job.Write() // daemon reads attached state for skew respawn
	mu.Unlock()
	defer func() {
		mu.Lock()
		delete(clients, a)
		job.Attached--
		_ = job.Write()
		mu.Unlock()
		close(a.out)
	}()

	for {
		op, payload, err := readFrame(conn)
		if err != nil {
			return
		}
		switch op {
		case opData:
			dbgf("input %q", payload)
			ptyMu.Lock()
			n, werr := master.Write(payload)
			dbgf("wrote %d/%d err=%v", n, len(payload), werr)
			ptyMu.Unlock()
		case opResize:
			var r struct {
				Cols, Rows int
			}
			if json.Unmarshal(payload, &r) == nil && r.Cols > 0 {
				setWinSize(master, r.Cols, r.Rows)
				if job.ChildPID > 0 {
					killChildPgrp(job.ChildPID, sigWinch)
				}
			}
		case opDetach:
			return
		}
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func fatalHost(what string, err error) int {
	os.Stderr.WriteString("bg-host: " + what + ": " + err.Error() + "\n")
	return 1
}

// dbgf writes host-side trace lines when CLAUDE_GO_BG_DEBUG is set —
// the attach path otherwise runs blind (stdio is /dev/null).
func dbgf(format string, args ...any) {
	dbgOnce.Do(func() {
		if p := os.Getenv("CLAUDE_GO_BG_DEBUG"); p != "" {
			dbgLog, _ = os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		}
	})
	if dbgLog != nil {
		fmt.Fprintf(dbgLog, "bg-host[%d] "+format+"\n", append([]any{os.Getpid()}, args...)...)
	}
}

// cliVersion is the binary version stamped into job.json — a worker
// carries the version of the CLI that spawned it (env override simulates
// an older-binary worker for tests).
func cliVersion() string {
	if v := os.Getenv("CLAUDE_BG_CLI_VERSION"); v != "" {
		return v
	}
	return api.Version
}
