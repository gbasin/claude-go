package tools

import (
	"bytes"
	"claude-go/internal/config"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

// ---------- background task manager ----------

type BgTask struct {
	outputFile *os.File
	ID         string
	Command    string
	Started    time.Time
	Done       chan struct{}
	Out        bytes.Buffer
	Err        error
	ExitCode   int
	TimedOut   bool
	cancel     context.CancelFunc
	// Kind distinguishes remote-agent tasks ("remote_agent"): their work
	// runs in a cloud session, and OnKill performs real's uRe.kill side
	// effect (archive the session via tR).
	Kind   string
	OnKill func(t *BgTask)
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

// KillAll cancels every running background task; returns count.
func (b *Background) KillAll() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, t := range b.tasks {
		select {
		case <-t.Done:
			continue
		default:
			if t.cancel != nil {
				t.cancel()
				n++
			}
			if t.OnKill != nil {
				t.OnKill(t)
			}
		}
	}
	return n
}

// AddRemote registers a cloud-session-backed remote-agent task (real's
// RemoteAgentTask, uRe — remoteTaskType "ultraplan"|"ultrareview"). The
// "process" lives server-side: the polling goroutine closes Done, and
// killing the task invokes onKill (archive) rather than a local kill.
// Returns the task plus a ctx the poller should respect (cancel fires on
// KillAll/TaskStop).
func (b *Background) AddRemote(command string, onKill func(ctx context.Context) error) (*BgTask, context.Context) {
	id := fmt.Sprintf("r%d", b.seq.Add(1))
	ctx, cancel := context.WithCancel(context.Background())
	t := &BgTask{ID: id, Command: command, Started: time.Now(),
		Done: make(chan struct{}), cancel: cancel, Kind: "remote_agent"}
	t.OnKill = func(*BgTask) {
		kctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		// best-effort — real logs "RemoteAgentTask archive failed" only
		_ = onKill(kctx)
	}
	b.mu.Lock()
	b.tasks[id] = t
	b.mu.Unlock()
	return t, ctx
}

func (b *Background) Get(id string) *BgTask {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tasks[id]
}

var sleepRe = regexp.MustCompile(`(^|[;&|]\s*)sleep\b`)

// ---------- Bash ----------

func init() {
	Register(&Tool{Def: loadSchema("Bash"), Exec: execBash})
	Register(&Tool{Def: loadSchema("TaskStop"), Exec: execTaskStop})
}

const (
	fgDefaultTimeout = 120_000 // ms — BASH_DEFAULT_TIMEOUT_MS overrides
	bgDefaultTimeout = 1_800_000
)

// env-configurable caps like real CC
func fgMaxTimeout() int {
	if v := os.Getenv("BASH_MAX_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return max(n, fgDefault())
		}
	}
	return 600_000
}
func fgDefault() int {
	if v := os.Getenv("BASH_DEFAULT_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fgDefaultTimeout
}
func bgMaxTimeout() int {
	return 7_200_000 // BASH_* env doesn't raise the bg ceiling in our impl
}
func bashOutputCap() int {
	if v := os.Getenv("BASH_MAX_OUTPUT_LENGTH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return min(max(n, 4000), 128000)
		}
	}
	// real's bashOutputMaxChars setting overrides the default cap
	if n := intSetting("bashOutputMaxChars", 0); n > 0 {
		return n
	}
	return 30000
}

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
	for _, r := range in.Command {
		if unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\r' {
			return Result{Content: "Command contains control characters that could hide content in the approval UI.", IsError: true}
		}
	}
	if !in.RunInBackground && sleepRe.MatchString(in.Command) {
		return Result{Content: "Blocked: foreground `sleep` is not allowed. To wait for a condition, use Monitor with an until-loop (e.g. `until <check>; do sleep 2; done`). To wait for a command you started, use run_in_background: true. Do not chain shorter sleeps to work around this block.", IsError: true}
	}
	if c.BgTasks == nil {
		c.BgTasks = NewBackground()
	}

	timeout := fgDefault()
	if in.Timeout > 0 {
		timeout = int(in.Timeout)
	}
	if in.RunInBackground {
		if in.Timeout <= 0 {
			timeout = bgDefaultTimeout
		}
		if timeout > bgMaxTimeout() {
			timeout = bgMaxTimeout()
		}
	} else if timeout > fgMaxTimeout() {
		timeout = fgMaxTimeout()
	}

	if in.RunInBackground {
		return c.runBackground(in.Command, timeout, in.DangerouslyDisableSandbox)
	}
	// CLAUDE_CODE_AUTO_BACKGROUND_TIMEOUT_MS: long fg commands flip to bg
	if v := os.Getenv("CLAUDE_CODE_AUTO_BACKGROUND_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 2000 {
			r := c.runForegroundTimed(in.Command, timeout, n, in.DangerouslyDisableSandbox)
			if r.autoBgID != "" {
				return Result{Content: fmt.Sprintf("Command did not complete within its %ds timeout and was moved to the background (ID: %s). Output is being written to: %s.",
					n/1000, r.autoBgID, r.autoBgFile)}
			}
			return r.Result
		}
	}
	return c.runForeground(in.Command, timeout, in.DangerouslyDisableSandbox)
}

// shellCmd builds a command that runs in the persisted cwd and reports the
// resulting cwd back via a sentinel so the session can track `cd`.
const cwdSentinel = "__CLAUDE_GO_CWD_SENTINEL__"

// shellCmd wraps command with the cwd sentinel, applies the configured
// sandbox (seatbelt on macOS, bwrap on Linux; see sandbox*.go) and returns
// the exec.Cmd plus a non-fatal warning note to prepend to the output.
func (c *Context) shellCmd(ctx context.Context, command string, disableSandbox bool) (*exec.Cmd, string, error) {
	script := command + "\nprintf '" + cwdSentinel + "%s' \"$PWD\""
	wrapped, env, warn, err := c.sandboxWrap(command, script, disableSandbox)
	if err != nil {
		return nil, "", err
	}
	shell := os.Getenv("SHELL")
	if shell == "" || !strings.HasSuffix(shell, "sh") {
		shell = "/bin/bash"
	}
	cmd := exec.CommandContext(ctx, shell, "-c", wrapped)
	cmd.Dir = c.CWD
	// Set vars appended last win over duplicates; Unset vars drop out of
	// the inherited env (credential mode:"deny" entries).
	cmd.Env = env.applyTo(os.Environ())
	return cmd, warn, nil
}

func (c *Context) runForeground(command string, timeoutMs int, disableSandbox bool) Result {
	ctx, cancel := context.WithTimeout(c.Ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	cmd, warn, err := c.shellCmd(ctx, command, disableSandbox)
	if err != nil {
		return Result{Content: err.Error(), IsError: true}
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	res := c.finishBash(&out, command, err, ctx.Err() == context.DeadlineExceeded, timeoutMs)
	// proxy-recorded network denies tagged to this command (real appends
	// the same <sandbox_violations> block to the tool output)
	if vs := sandboxViolations(command); len(vs) > 0 {
		res.Content += "\n<sandbox_violations>\n" + strings.Join(vs, "\n") + "\n</sandbox_violations>"
	}
	if warn != "" {
		res.Content = warn + "\n" + res.Content
	}
	return res
}

func (c *Context) finishBash(out *bytes.Buffer, command string, err error, timedOut bool, timeoutMs int) Result {
	raw := out.Bytes()
	newCWD := ""
	if i := bytes.LastIndex(raw, []byte(cwdSentinel)); i >= 0 {
		newCWD = strings.TrimSpace(string(raw[i+len(cwdSentinel):]))
		raw = raw[:i]
	}
	if newCWD != "" && newCWD != c.CWD {
		if fi, err2 := os.Stat(newCWD); err2 == nil && fi.IsDir() {
			c.CWD = newCWD
			if c.OnCwdChanged != nil {
				c.OnCwdChanged(newCWD)
			}
		}
	}
	text := string(bytes.TrimRight(raw, "\n"))
	persisted := ""
	if len(text) > bashOutputCap() {
		// real spills large output to a persisted file with a marker
		if out, err := spillOutput(c, text); err == nil {
			persisted = out
		}
		text = text[:bashOutputCap()]
	}
	var b strings.Builder
	if timedOut {
		b.WriteString(fmt.Sprintf("Command timed out after %s\n", time.Duration(timeoutMs)*time.Millisecond))
	}
	if err != nil && !timedOut {
		if ee, ok := err.(*exec.ExitError); ok {
			b.WriteString(fmt.Sprintf("Exit code %d\n", ee.ExitCode()))
		} else {
			b.WriteString("Error: " + err.Error() + "\n")
		}
	}
	b.WriteString(text)
	if persisted != "" {
		b.WriteString("\n<persisted-output>" + persisted + "</persisted-output>")
	}
	res := Result{Content: b.String()}
	if err != nil || timedOut {
		res.IsError = true
	}
	return res
}

func (c *Context) runBackground(command string, timeoutMs int, disableSandbox bool) Result {
	id := fmt.Sprintf("b%d", c.BgTasks.seq.Add(1))
	t := &BgTask{ID: id, Command: command, Started: time.Now(), Done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	t.cancel = cancel
	cmd, warn, err := c.shellCmd(ctx, command, disableSandbox)
	if err != nil {
		cancel()
		return Result{Content: err.Error(), IsError: true}
	}
	// real writes bg output to <tmp>/claude-501/<slug>/<sid>/tasks/<id>.output
	// and tells the model the path (it later cats/Reads it back)
	outPath := ""
	if f, perr := taskOutputFile(c, id); perr == nil {
		outPath = f.Name()
		cmd.Stdout = io.MultiWriter(&t.Out, f)
		cmd.Stderr = io.MultiWriter(&t.Out, f)
		t.outputFile = f
	} else {
		cmd.Stdout = &t.Out
		cmd.Stderr = &t.Out
	}
	if err := cmd.Start(); err != nil {
		if t.outputFile != nil {
			t.outputFile.Close()
		}
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
	msg := fmt.Sprintf("Command running in background with ID %s", id)
	if outPath != "" {
		msg += "\nOutput file: " + outPath // real tells the model the path
	}
	if warn != "" {
		msg = warn + "\n" + msg
	}
	return Result{Content: msg}
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
	if id == "" {
		return Result{Content: "task_id or shell_id required", IsError: true}
	}
	if c.BgTasks == nil {
		return Result{Content: "no such background task: " + id, IsError: true}
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
	if t.OnKill != nil {
		t.OnKill(t)
	}
	select {
	case <-t.Done:
	case <-time.After(3 * time.Second):
	}
	return Result{Content: fmt.Sprintf("Task %s stopped", id)}
}

// Notify queues a "Task <id> finished/failed" status note (real shows
// these as ⎿-prefixed reminders on the next input row).
func (b *Background) Notify(t *BgTask) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// real: `⏺ Background command "<desc>" completed (exit code 0)` —
	// rendered as an assistant-style line, not a ⎿ note
	if t.TimedOut || t.Err != nil || t.ExitCode != 0 {
		b.notif = append(b.notif, fmt.Sprintf("Background command %q failed (exit code %d)", t.Command, t.ExitCode))
	} else {
		b.notif = append(b.notif, fmt.Sprintf("Background command %q completed (exit code 0)", t.Command))
	}
}

// DrainNotifications returns and clears queued task-exit notes.
func (b *Background) DrainNotifications() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.notif
	b.notif = nil
	return out
}

// spillOutput writes large output to a session file like real CC.
func spillOutput(c *Context, text string) (string, error) {
	// real writes <project-dir>/<sid>/tool-results/<rand>.txt so the
	// transcript's persisted-output marker resolves after resume
	slug := strings.ReplaceAll(filepath.Clean(c.CWD), "/", "-")
	dir := filepath.Join(config.ClaudeDir(), "projects", slug, c.Session, "tool-results")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "output-*.txt")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// runForegroundTimed runs fg with an auto-background handoff at autoAfterMs.
type fgTimedResult struct {
	Result
	autoBgID   string
	autoBgFile string
}

func (c *Context) runForegroundTimed(command string, timeoutMs, autoAfterMs int, disableSandbox bool) fgTimedResult {
	if c.BgTasks == nil {
		c.BgTasks = NewBackground()
	}
	id := fmt.Sprintf("b%d", c.BgTasks.seq.Add(1))
	t := &BgTask{ID: id, Command: command, Started: time.Now(), Done: make(chan struct{})}
	// the process gets the BACKGROUND ceiling — it may outlive the fg window
	procTimeout := timeoutMs
	if procTimeout < bgDefaultTimeout {
		procTimeout = bgDefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(procTimeout)*time.Millisecond)
	t.cancel = cancel
	cmd, warn, err := c.shellCmd(ctx, command, disableSandbox)
	if err != nil {
		cancel()
		return fgTimedResult{Result: Result{Content: err.Error(), IsError: true}}
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		cancel()
		return fgTimedResult{Result: Result{Content: "Error: " + err.Error(), IsError: true}}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		cancel()
		res := c.finishBash(&out, command, err, ctx.Err() == context.DeadlineExceeded, timeoutMs)
		return fgTimedResult{Result: res}
	case <-time.After(time.Duration(autoAfterMs) * time.Millisecond):
		// hand off to background — task owns the process now
		c.BgTasks.tasks[id] = t
		dir := filepath.Join(os.TempDir(), "claude-go", c.Session)
		os.MkdirAll(dir, 0o755)
		outFile := filepath.Join(dir, id+".out")
		go func() {
			err := <-done
			f, _ := os.Create(outFile)
			f.Write(out.Bytes())
			f.Close()
			t.Err = err
			close(t.Done)
		}()
		_ = warn
		return fgTimedResult{autoBgID: id, autoBgFile: outFile}
	}
}

// intSetting reads a numeric key from ~/.claude/settings.json.
func intSetting(key string, def int) int {
	b, err := os.ReadFile(filepath.Join(config.ClaudeDir(), "settings.json"))
	if err != nil {
		return def
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return def
	}
	if f, ok := m[key].(float64); ok {
		return int(f)
	}
	return def
}

// taskOutputFile opens the session-scoped output file real creates for
// background tasks: <tmp>/claude-<uid>/<slug>/<session>/tasks/<id>.output
func taskOutputFile(c *Context, id string) (*os.File, error) {
	slug := strings.ReplaceAll(filepath.Clean(c.CWD), "/", "-")
	sid := "default"
	if c.Session != "" {
		sid = c.Session
	}
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("claude-%d", os.Getuid()),
		slug, sid, "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.Create(filepath.Join(dir, id+".output"))
}
