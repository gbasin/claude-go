package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------- background task manager ----------

type BgTask struct {
	ID       string
	Command  string
	Started  time.Time
	Done     chan struct{}
	Out      bytes.Buffer
	Err      error
	ExitCode int
	TimedOut bool
	cancel   context.CancelFunc
}

type Background struct {
	mu     sync.Mutex
	seq    atomic.Int64
	tasks  map[string]*BgTask
	notif  []string
	OnExit func(t *BgTask) // called when a task finishes
}

func NewBackground() *Background {
	return &Background{tasks: map[string]*BgTask{}}
}

func (b *Background) List() []*BgTask {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*BgTask, 0, len(b.tasks))
	for _, t := range b.tasks {
		out = append(out, t)
	}
	return out
}

func (b *Background) Get(id string) *BgTask {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tasks[id]
}

var sleepRe = regexp.MustCompile(`^\s*sleep\b`)

// ---------- Bash ----------

func init() {
	Register(&Tool{Def: loadSchema("Bash"), Exec: execBash})
	Register(&Tool{Def: loadSchema("TaskStop"), Exec: execTaskStop})
}

const (
	fgDefaultTimeout = 600_000 // ms
	fgMaxTimeout     = 600_000
	bgDefaultTimeout = 1_800_000
	bgMaxTimeout     = 7_200_000
	maxOutputBytes   = 30000
)

type bashInput struct {
	Command                   string  `json:"command"`
	Timeout                   float64 `json:"timeout"`
	Description               string  `json:"description"`
	RunInBackground           bool    `json:"run_in_background"`
	DangerouslyDisableSandbox bool    `json:"dangerouslyDisableSandbox"`
}

func execBash(c *Context, raw json.RawMessage) Result {
	var in bashInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	if in.Command == "" {
		return Result{Content: "command is required", IsError: true}
	}
	if !in.RunInBackground && sleepRe.MatchString(in.Command) {
		return Result{Content: "Foreground `sleep` is blocked; use Monitor with an until-loop to wait on a condition.", IsError: true}
	}
	if c.BgTasks == nil {
		c.BgTasks = NewBackground()
	}

	timeout := fgDefaultTimeout
	if in.Timeout > 0 {
		timeout = int(in.Timeout)
	}
	if in.RunInBackground {
		if in.Timeout <= 0 {
			timeout = bgDefaultTimeout
		}
		if timeout > bgMaxTimeout {
			timeout = bgMaxTimeout
		}
	} else if timeout > fgMaxTimeout {
		timeout = fgMaxTimeout
	}

	if in.RunInBackground {
		return c.runBackground(in.Command, timeout)
	}
	return c.runForeground(in.Command, timeout)
}

// shellCmd builds a command that runs in the persisted cwd and reports the
// resulting cwd back via a sentinel so the session can track `cd`.
const cwdSentinel = "__CLAUDE_GO_CWD_SENTINEL__"

// sandboxProfile restricts writes to cwd+scratchpad and temp; network allowed.
const sandboxProfile = `(version 1)
(allow default)
(deny file-write* (regex #"^/(?!private/tmp|var/folders|Users/[^/]+/Code|Users/[^/]+/claude)"))
(allow file-write* (subpath "/private/tmp") (subpath "/tmp") (subpath "/var") (subpath (param "CWD")))`

// maybeSandbox wraps a command in macOS sandbox-exec when CLAUDE_GO_SANDBOX=1.
// Writes a seatbelt profile to the scratchpad and prefixes the command.
func (c *Context) maybeSandbox(command string) string {
	if os.Getenv("CLAUDE_GO_SANDBOX") != "1" || runtime.GOOS != "darwin" {
		return command
	}
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		return command
	}
	dir := filepath.Join(os.TempDir(), "claude-go-sandbox")
	os.MkdirAll(dir, 0o755)
	pf := filepath.Join(dir, "profile.sb")
	profile := fmt.Sprintf(`(version 1)
(allow default)
(deny file-write* (regex #"^/(?!private/tmp|var/folders|private/var)"))
(allow file-write* (subpath "/private/tmp") (subpath "/tmp") (subpath "/var/folders") (subpath %q))
`, c.CWD)
	if err := os.WriteFile(pf, []byte(profile), 0o644); err != nil {
		return command
	}
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/bash"
	}
	return fmt.Sprintf("sandbox-exec -f %q %s -c %q", pf, sh, command)
}

func (c *Context) shellCmd(ctx context.Context, command string) *exec.Cmd {
	command = c.maybeSandbox(command)
	wrapped := command + "\nprintf '" + cwdSentinel + "%s' \"$PWD\""
	shell := os.Getenv("SHELL")
	if shell == "" || !strings.HasSuffix(shell, "sh") {
		shell = "/bin/bash"
	}
	cmd := exec.CommandContext(ctx, shell, "-c", wrapped)
	cmd.Dir = c.CWD
	cmd.Env = os.Environ()
	return cmd
}

func (c *Context) runForeground(command string, timeoutMs int) Result {
	ctx, cancel := context.WithTimeout(c.Ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	cmd := c.shellCmd(ctx, command)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return c.finishBash(&out, command, err, ctx.Err() == context.DeadlineExceeded)
}

func (c *Context) finishBash(out *bytes.Buffer, command string, err error, timedOut bool) Result {
	raw := out.Bytes()
	newCWD := ""
	if i := bytes.LastIndex(raw, []byte(cwdSentinel)); i >= 0 {
		newCWD = strings.TrimSpace(string(raw[i+len(cwdSentinel):]))
		raw = raw[:i]
	}
	if newCWD != "" {
		if fi, err2 := os.Stat(newCWD); err2 == nil && fi.IsDir() {
			c.CWD = newCWD
		}
	}
	text := string(bytes.TrimRight(raw, "\n"))
	truncated := false
	if len(text) > maxOutputBytes {
		text = text[:maxOutputBytes]
		truncated = true
	}
	var b strings.Builder
	if timedOut {
		b.WriteString(fmt.Sprintf("Command timed out\n"))
	}
	if err != nil && !timedOut {
		if ee, ok := err.(*exec.ExitError); ok {
			b.WriteString(fmt.Sprintf("Exit code %d\n", ee.ExitCode()))
		} else {
			b.WriteString("Error: " + err.Error() + "\n")
		}
	}
	b.WriteString(text)
	if truncated {
		b.WriteString("\n... (output truncated)")
	}
	res := Result{Content: b.String()}
	if err != nil || timedOut {
		res.IsError = true
	}
	return res
}

func (c *Context) runBackground(command string, timeoutMs int) Result {
	id := fmt.Sprintf("b%d", c.BgTasks.seq.Add(1))
	t := &BgTask{ID: id, Command: command, Started: time.Now(), Done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	t.cancel = cancel
	cmd := c.shellCmd(ctx, command)
	cmd.Stdout = &t.Out
	cmd.Stderr = &t.Out
	if err := cmd.Start(); err != nil {
		cancel()
		return Result{Content: "failed to start: " + err.Error(), IsError: true}
	}
	c.BgTasks.mu.Lock()
	c.BgTasks.tasks[id] = t
	c.BgTasks.mu.Unlock()
	go func() {
		defer close(t.Done)
		defer cancel()
		err := cmd.Wait()
		t.Err = err
		if ctx.Err() == context.DeadlineExceeded {
			t.TimedOut = true
		}
		if ee, ok := err.(*exec.ExitError); ok {
			t.ExitCode = ee.ExitCode()
		}
		// capture cwd drift like foreground
		raw := t.Out.Bytes()
		if i := bytes.LastIndex(raw, []byte(cwdSentinel)); i >= 0 {
			newCWD := strings.TrimSpace(string(raw[i+len(cwdSentinel):]))
			t.Out.Truncate(i)
			if fi, err2 := os.Stat(newCWD); err2 == nil && fi.IsDir() {
				c.CWD = newCWD
			}
		}
		if cb := c.BgTasks.OnExit; cb != nil {
			cb(t)
		}
	}()
	return Result{Content: fmt.Sprintf("Command running in background with ID %s", id)}
}

// ---------- TaskStop ----------

func execTaskStop(c *Context, raw json.RawMessage) Result {
	var in struct {
		TaskID  string `json:"task_id"`
		ShellID string `json:"shell_id"`
	}
	json.Unmarshal(raw, &in)
	id := in.TaskID
	if id == "" {
		id = in.ShellID
	}
	if id == "" || c.BgTasks == nil {
		return Result{Content: "task_id or shell_id required", IsError: true}
	}
	t := c.BgTasks.Get(id)
	if t == nil {
		return Result{Content: "no such background task: " + id, IsError: true}
	}
	select {
	case <-t.Done:
		return Result{Content: fmt.Sprintf("Task %s already finished", id)}
	default:
	}
	if t.cancel != nil {
		t.cancel()
	}
	select {
	case <-t.Done:
	case <-time.After(3 * time.Second):
	}
	return Result{Content: fmt.Sprintf("Task %s stopped", id)}
}

// DrainNotifications returns and clears queued task-exit notes.
func (b *Background) DrainNotifications() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.notif
	b.notif = nil
	return out
}
