package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

func init() {
	Register(&Tool{Def: loadSchema("ScheduleWakeup"), Exec: execScheduleWakeup})
	Register(&Tool{Def: loadSchema("PushNotification"), Exec: execPushNotification})
	Register(&Tool{Def: loadSchema("CronCreate"), Exec: execCronCreate})
	Register(&Tool{Def: loadSchema("CronDelete"), Exec: execCronDelete})
	Register(&Tool{Def: loadSchema("CronList"), Exec: execCronList})
	Register(&Tool{Def: loadSchema("ReportFindings"), Exec: execReportFindings})
	Register(&Tool{Def: loadSchema("SendMessage"), Exec: execSendMessage})
	Register(&Tool{Def: loadSchema("ListAgents"), Exec: execListAgents})
}

// ---------- ScheduleWakeup ----------
// Schedules a wake: after delaySeconds, a queued note is delivered to the
// agent as a system message on its next turn (or immediately if idle).

type WakeupManager struct {
	mu      sync.Mutex
	pending []string
	timers  []*time.Timer
}

var Wakeups = &WakeupManager{}

// Drain returns and clears queued wake notes.
func (w *WakeupManager) Drain() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.pending
	w.pending = nil
	return out
}

func (w *WakeupManager) schedule(delay time.Duration, note string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.timers = append(w.timers, time.AfterFunc(delay, func() {
		w.mu.Lock()
		w.pending = append(w.pending, note)
		w.mu.Unlock()
	}))
}

func execScheduleWakeup(c *Context, raw json.RawMessage) Result {
	var in struct {
		DelaySeconds float64 `json:"delaySeconds"`
		Reason       string  `json:"reason"`
		Prompt       string  `json:"prompt"`
		Stop         bool    `json:"stop"`
		Noop         bool    `json:"noop"`
	}
	json.Unmarshal(raw, &in)
	if in.Noop {
		return Result{Content: "No wakeup scheduled (noop)."}
	}
	if in.Stop {
		return Result{Content: "Wakeup scheduling is not active for this session."}
	}
	d := time.Duration(in.DelaySeconds * float64(time.Second))
	if d < 0 {
		d = 0
	}
	note := in.Prompt
	if note == "" {
		note = "Scheduled wakeup"
	}
	Wakeups.schedule(d, fmt.Sprintf("[scheduled wakeup fired] %s", note))
	return Result{Content: fmt.Sprintf("Wakeup scheduled in %.0fs.", in.DelaySeconds)}
}

// ---------- PushNotification ----------

func execPushNotification(c *Context, raw json.RawMessage) Result {
	var in struct {
		Message string `json:"message"`
		Status  string `json:"status"`
	}
	json.Unmarshal(raw, &in)
	if in.Message == "" {
		return Result{Content: "message is required", IsError: true}
	}
	title := "Claude Code"
	if in.Status != "" {
		title = "Claude Code — " + in.Status
	}
	var err error
	switch runtime.GOOS {
	case "darwin":
		err = exec.Command("osascript", "-e",
			fmt.Sprintf(`display notification %q with title %q`, in.Message, title)).Run()
	case "linux":
		err = exec.Command("notify-send", title, in.Message).Run()
	default:
		err = fmt.Errorf("notifications unsupported on %s", runtime.GOOS)
	}
	if err != nil {
		return Result{Content: "notification failed: " + err.Error(), IsError: true}
	}
	return Result{Content: "Notification sent."}
}

// ---------- Cron ----------
// Local scheduled jobs stored in ~/.claude-go/cron.json. A job fires by
// running `claude-go -p <prompt>`; `claude-go --cron-daemon` runs the loop.

type cronJob struct {
	ID        string    `json:"id"`
	Cron      string    `json:"cron"`
	Prompt    string    `json:"prompt"`
	Recurring bool      `json:"recurring"`
	Durable   bool      `json:"durable"`
	Created   time.Time `json:"created"`
	LastRun   time.Time `json:"last_run,omitempty"`
}

func cronPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude-go", "cron.json")
}

func loadCron() ([]cronJob, error) {
	b, err := os.ReadFile(cronPath())
	if err != nil {
		return nil, nil
	}
	var jobs []cronJob
	json.Unmarshal(b, &jobs)
	return jobs, nil
}

func saveCron(jobs []cronJob) error {
	if err := os.MkdirAll(filepath.Dir(cronPath()), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(jobs, "", "  ")
	return os.WriteFile(cronPath(), b, 0o644)
}

// matchCron evaluates a 5-field cron expr against t (min hour dom mon dow).
func matchCron(expr string, t time.Time) bool {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return false
	}
	vals := []int{t.Minute(), t.Hour(), t.Day(), int(t.Month()), int(t.Weekday())}
	for i, f := range fields {
		if !matchCronField(f, vals[i], i) {
			return false
		}
	}
	return true
}

func matchCronField(f string, v, fieldIdx int) bool {
	for _, part := range strings.Split(f, ",") {
		if matchCronPart(part, v, fieldIdx) {
			return true
		}
	}
	return false
}

func matchCronPart(p string, v, fieldIdx int) bool {
	step := 1
	if i := strings.Index(p, "/"); i >= 0 {
		fmt.Sscanf(p[i+1:], "%d", &step)
		p = p[:i]
	}
	lo, hi := 0, 59
	switch fieldIdx {
	case 1:
		hi = 23
	case 2:
		lo, hi = 1, 31
	case 3:
		lo, hi = 1, 12
	case 4:
		hi = 6
	}
	if p == "*" {
		return v%step == 0
	}
	if strings.Contains(p, "-") {
		var a, b int
		fmt.Sscanf(p, "%d-%d", &a, &b)
		return v >= a && v <= b && (v-a)%step == 0
	}
	var n int
	if _, err := fmt.Sscanf(p, "%d", &n); err == nil {
		if step > 1 {
			return v >= n && (v-n)%step == 0
		}
		return v == n
	}
	_ = lo
	_ = hi
	return false
}

func execCronCreate(c *Context, raw json.RawMessage) Result {
	var in struct {
		Cron      string `json:"cron"`
		Prompt    string `json:"prompt"`
		Recurring bool   `json:"recurring"`
		Durable   bool   `json:"durable"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	if len(strings.Fields(in.Cron)) != 5 {
		return Result{Content: "invalid cron expression (expected 5 fields)", IsError: true}
	}
	jobs, _ := loadCron()
	j := cronJob{
		ID:   fmt.Sprintf("job_%d", time.Now().UnixNano()%1e8),
		Cron: in.Cron, Prompt: in.Prompt,
		Recurring: in.Recurring, Durable: in.Durable, Created: time.Now(),
	}
	jobs = append(jobs, j)
	if err := saveCron(jobs); err != nil {
		return Result{Content: "failed to save: " + err.Error(), IsError: true}
	}
	return Result{Content: fmt.Sprintf("Created job %s (cron %q). Run `claude-go --cron-daemon` to execute scheduled jobs locally.", j.ID, j.Cron)}
}

func execCronDelete(c *Context, raw json.RawMessage) Result {
	var in struct {
		ID string `json:"id"`
	}
	json.Unmarshal(raw, &in)
	jobs, _ := loadCron()
	out := jobs[:0]
	var found bool
	for _, j := range jobs {
		if j.ID == in.ID {
			found = true
			continue
		}
		out = append(out, j)
	}
	if !found {
		return Result{Content: "no such job: " + in.ID, IsError: true}
	}
	saveCron(out)
	return Result{Content: "Deleted job " + in.ID}
}

func execCronList(c *Context, raw json.RawMessage) Result {
	jobs, _ := loadCron()
	if len(jobs) == 0 {
		return Result{Content: "No scheduled jobs."}
	}
	var b strings.Builder
	for _, j := range jobs {
		fmt.Fprintf(&b, "%s  cron=%q  recurring=%t  durable=%t  prompt=%q\n",
			j.ID, j.Cron, j.Recurring, j.Durable, j.Prompt)
	}
	return Result{Content: b.String()}
}

// CronTick runs due jobs once (used by --cron-daemon and tests).
func CronTick(run func(prompt string)) {
	jobs, _ := loadCron()
	now := time.Now()
	changed := false
	var keep []cronJob
	for _, j := range jobs {
		if matchCron(j.Cron, now) {
			go run(j.Prompt)
			j.LastRun = now
			changed = true
			if !j.Recurring {
				continue // one-shot: drop
			}
		}
		keep = append(keep, j)
	}
	if changed {
		saveCron(keep)
	}
}

// ---------- ReportFindings ----------

func execReportFindings(c *Context, raw json.RawMessage) Result {
	var in struct {
		Level    string `json:"level"`
		Findings string `json:"findings"`
	}
	json.Unmarshal(raw, &in)
	dir := filepath.Join(os.TempDir(), "claude-go-findings")
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, fmt.Sprintf("findings-%d.json", time.Now().Unix()))
	entry := map[string]any{
		"level": in.Level, "findings": in.Findings,
		"cwd": c.CWD, "ts": time.Now().Format(time.RFC3339),
	}
	b, _ := json.MarshalIndent(entry, "", "  ")
	os.WriteFile(p, b, 0o644)
	return Result{Content: "Findings recorded to " + p}
}

// ---------- SendMessage / ListAgents ----------
// Local agent bus: ~/.claude-go/bus/<channel>.jsonl

type busMsg struct {
	From    string    `json:"from"`
	To      string    `json:"to"`
	Summary string    `json:"summary"`
	Message string    `json:"message"`
	TS      time.Time `json:"ts"`
	Unread  bool      `json:"unread"`
}

func busDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude-go", "bus")
}

func busPath(channel string) string {
	if channel == "" {
		channel = "main"
	}
	return filepath.Join(busDir(), channel+".jsonl")
}

func busAppend(channel string, m busMsg) error {
	os.MkdirAll(busDir(), 0o755)
	f, err := os.OpenFile(busPath(channel), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(m)
	_, err = f.Write(append(b, '\n'))
	return err
}

func busRead(channel string) []busMsg {
	b, err := os.ReadFile(busPath(channel))
	if err != nil {
		return nil
	}
	var out []busMsg
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		var m busMsg
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func execSendMessage(c *Context, raw json.RawMessage) Result {
	var in struct {
		To             string `json:"to"`
		Summary        string `json:"summary"`
		Message        string `json:"message"`
		NotifyWhenIdle bool   `json:"notify_when_idle"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	if in.To == "" || in.Message == "" {
		return Result{Content: "to and message are required", IsError: true}
	}
	err := busAppend("main", busMsg{
		From: "agent", To: in.To, Summary: in.Summary,
		Message: in.Message, TS: time.Now(), Unread: true,
	})
	if err != nil {
		return Result{Content: "send failed: " + err.Error(), IsError: true}
	}
	return Result{Content: fmt.Sprintf("Message sent to %s", in.To)}
}

func execListAgents(c *Context, raw json.RawMessage) Result {
	var in struct {
		Channel string `json:"channel"`
		Q       string `json:"q"`
	}
	json.Unmarshal(raw, &in)
	var b strings.Builder
	// background tasks count as running agents
	if c.BgTasks != nil {
		tasks := c.BgTasks.List()
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
		for _, t := range tasks {
			st := "running"
			select {
			case <-t.Done:
				st = "exited"
			default:
			}
			fmt.Fprintf(&b, "%s  [%s] %s\n", t.ID, st, t.Command)
		}
	}
	msgs := busRead(in.Channel)
	for _, m := range msgs {
		if in.Q != "" && !strings.Contains(m.Message+m.To, in.Q) {
			continue
		}
		fmt.Fprintf(&b, "msg %s→%s: %s\n", m.From, m.To, m.Message)
	}
	if b.Len() == 0 {
		return Result{Content: "No agents or messages found."}
	}
	return Result{Content: b.String()}
}
