package tools

import (
	"bytes"
	"claude-go/internal/config"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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

// SessionQueue holds prompts injected by timers/cron/monitors —
// the REPL drains it after each turn and asks them as user turns.
var SessionQueue = make(chan string, 64)

// QueuePrompt injects a session prompt (Stop-hook continuation, cron, wakeup).
func QueuePrompt(p string) { queuePrompt(p) }

func queuePrompt(p string) {
	select {
	case SessionQueue <- p:
	default:
	}
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
	d := in.DelaySeconds
	if d < 60 {
		d = 60
	}
	if d > 3600 {
		d = 3600
	}
	note := in.Prompt
	if note == "" {
		note = "Scheduled wakeup"
	}
	// the prompt is re-fired as a new turn when the timer fires
	time.AfterFunc(time.Duration(d*float64(time.Second)), func() {
		queuePrompt(note)
	})
	return Result{Content: fmt.Sprintf("Wakeup scheduled in %.0fs.", d)}
}

// ---------- PushNotification ----------

var UserActive atomic.Bool

func execPushNotification(c *Context, raw json.RawMessage) Result {
	var in struct {
		Message string `json:"message"`
		Status  string `json:"status"`
	}
	json.Unmarshal(raw, &in)
	if in.Message == "" {
		return Result{Content: "message is required", IsError: true}
	}
	// real skips the push when the user is actively at the terminal
	if UserActive.Load() && Interactive {
		return Result{Content: "Notification not sent — the user is actively at the terminal."}
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

var (
	cronMu   sync.Mutex
	sessJobs []cronJob
	cronOnce sync.Once
)

func cronTickSession() {
	now := time.Now()
	cronMu.Lock()
	var keep []cronJob
	for _, j := range sessJobs {
		if matchCron(j.Cron, now) {
			queuePrompt(j.Prompt)
			j.LastRun = now
			if !j.Recurring {
				continue // one-shot: auto-delete
			}
		}
		keep = append(keep, j)
	}
	sessJobs = keep
	cronMu.Unlock()
	cronTickDurable() // lock-gated durable half (mod_1918.js)
}

func ensureCronTicker() {
	cronOnce.Do(func() {
		go func() {
			for range time.Tick(30 * time.Second) {
				cronTickSession()
			}
		}()
	})
}

// CronCreate mirrors the real tool (mod_1601.js): session-only by default;
// `durable: true` persists to <cwd>/.claude/scheduled_tasks.json when the
// tengu_kairos_cron_durable gate (ope()) is on. recurring defaults true.
// Validation and result text match real; max 50 jobs across both stores.
func execCronCreate(c *Context, raw json.RawMessage) Result {
	if !cronEnabled() {
		return Result{Content: "CronCreate is disabled.", IsError: true}
	}
	var in struct {
		Cron      string `json:"cron"`
		Prompt    string `json:"prompt"`
		Recurring *bool  `json:"recurring"`
		Durable   bool   `json:"durable"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	spec := parseCron(in.Cron)
	if spec == nil {
		return Result{Content: fmt.Sprintf("Invalid cron expression '%s'. Expected 5 fields: M H DoM Mon DoW.", in.Cron), IsError: true}
	}
	if nextCronTime(spec, time.Now()) == nil {
		return Result{Content: fmt.Sprintf("Cron expression '%s' does not match any calendar date in the next year.", in.Cron), IsError: true}
	}
	recurring := true // real default
	if in.Recurring != nil {
		recurring = *in.Recurring
	}
	durable := in.Durable && cronDurableEnabled()
	dir := c.CWD
	if dir == "" {
		dir, _ = os.Getwd()
	}
	total := len(loadScheduledTasks(dir))
	cronMu.Lock()
	total += len(sessJobs)
	cronMu.Unlock()
	if total >= 50 {
		return Result{Content: "Too many scheduled jobs (max 50). Cancel one first.", IsError: true}
	}
	ensureCronTicker()
	human := humanSchedule(in.Cron)
	var id string
	if durable {
		var err error
		id, err = addScheduledTask(dir, in.Cron, in.Prompt, recurring, c.Session)
		if err != nil {
			return Result{Content: "failed to persist scheduled task: " + err.Error(), IsError: true}
		}
	} else {
		j := cronJob{
			ID: newTaskID(), Cron: in.Cron, Prompt: in.Prompt,
			Recurring: recurring, Durable: false, Created: time.Now(),
		}
		cronMu.Lock()
		sessJobs = append(sessJobs, j)
		cronMu.Unlock()
		id = j.ID
	}
	persistNote := "Session-only (not written to disk, dies when Claude exits)"
	if durable {
		persistNote = "Persisted to .claude/scheduled_tasks.json"
	}
	if recurring {
		return Result{Content: fmt.Sprintf(
			"Scheduled recurring job %s (%s). %s. Auto-expires after 7 days. Use CronDelete to cancel sooner.",
			id, human, persistNote)}
	}
	return Result{Content: fmt.Sprintf(
		"Scheduled one-shot task %s (%s). %s. It will fire once then auto-delete.",
		id, human, persistNote)}
}

func execCronDelete(c *Context, raw json.RawMessage) Result {
	if !cronEnabled() {
		return Result{Content: "CronDelete is disabled.", IsError: true}
	}
	var in struct {
		ID string `json:"id"`
	}
	json.Unmarshal(raw, &in)
	cronMu.Lock()
	out := sessJobs[:0]
	var found bool
	for _, j := range sessJobs {
		if j.ID == in.ID {
			found = true
			continue
		}
		out = append(out, j)
	}
	sessJobs = out
	cronMu.Unlock()
	if !found {
		dir := c.CWD
		if dir == "" {
			dir, _ = os.Getwd()
		}
		found = removeScheduledTasks(dir, []string{in.ID})
	}
	if !found {
		return Result{Content: fmt.Sprintf("No scheduled job with id '%s'", in.ID), IsError: true}
	}
	return Result{Content: "Cancelled job " + in.ID + "."}
}

func execCronList(c *Context, raw json.RawMessage) Result {
	if !cronEnabled() {
		return Result{Content: "CronList is disabled.", IsError: true}
	}
	dir := c.CWD
	if dir == "" {
		dir, _ = os.Getwd()
	}
	type row struct {
		id, cron, prompt string
		recurring        bool
		sessionOnly      bool
	}
	var rows []row
	for _, t := range loadScheduledTasks(dir) {
		rows = append(rows, row{t.ID, t.Cron, t.Prompt, t.Recurring, false})
	}
	cronMu.Lock()
	for _, j := range sessJobs {
		rows = append(rows, row{j.ID, j.Cron, j.Prompt, j.Recurring, true})
	}
	cronMu.Unlock()
	if len(rows) == 0 {
		return Result{Content: "No scheduled jobs."}
	}
	var b strings.Builder
	for _, r := range rows {
		kind := "one-shot"
		if r.recurring {
			kind = "recurring"
		}
		so := ""
		if r.sessionOnly {
			so = " [session-only]"
		}
		fmt.Fprintf(&b, "%s — %s (%s)%s: %s\n", r.id, humanSchedule(r.cron), kind, so, r.prompt)
	}
	return Result{Content: strings.TrimRight(b.String(), "\n")}
}

// ---------- ReportFindings ----------

func execReportFindings(c *Context, raw json.RawMessage) Result {
	var in struct {
		Level    string            `json:"level"`
		Findings []json.RawMessage `json:"findings"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	if len(in.Findings) == 0 {
		return Result{Content: "findings array is required", IsError: true}
	}
	if len(in.Findings) > 32 {
		return Result{Content: "too many findings (max 32)", IsError: true}
	}
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
// Addressing, per the real tool (mod_1590/mod_0308):
//   - `to` resolves to an in-process agent (by name or agentId), "main"
//     (the session's main conversation), or another local session registered
//     in ~/.claude-go/agent-registry.json. A "name [ref]" form pins a session
//     by its registry ref (sessionId prefix).
//   - In-process sends go through AgentBus records (channel semantics).
//   - Cross-session sends append to ~/.claude-go/mailbox/<sessionId>.jsonl;
//     the receiving session drains its mailbox between turns.
//   - A send to a completed agent appends to its mailbox — it is "resumed
//     from its transcript".
//   - notify_when_idle registers a one-shot subscription; when the target
//     session's turn finishes it fires a notice record into the subscriber's
//     mailbox (NotifyOnIdle / FireIdleNotifications / DrainMailbox — the
//     turn-boundary calls are wired in repl.go, elsewhere).

// ---------- in-process agent bus ----------

// AgentRecord is an addressable in-process agent (spawned via the Agent tool).
type AgentRecord struct {
	Name    string `json:"name"`
	AgentID string `json:"agentId"`
	Type    string `json:"type"`    // subagent_type that spawned it
	Session string `json:"session"` // owning session id
	Kind    string `json:"kind"`    // "in-process"
	Started time.Time

	// Inbox carries live sends; Send falls back to queued when full.
	Inbox chan string
	Done  chan struct{}

	mu        sync.Mutex
	queued    []string
	completed bool
	result    string
}

// Send delivers a message to a live agent (channel semantics; queued if full).
func (a *AgentRecord) Send(text string) {
	select {
	case a.Inbox <- text:
	default:
		a.mu.Lock()
		a.queued = append(a.queued, text)
		a.mu.Unlock()
	}
}

// TakeInbox drains queued messages (drained by whoever resumes the agent).
func (a *AgentRecord) TakeInbox() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.queued
	a.queued = nil
	return out
}

// Completed reports whether the agent finished its turn.
func (a *AgentRecord) Completed() bool {
	select {
	case <-a.Done:
		return true
	default:
		return false
	}
}

// Result returns the agent's final text after completion.
func (a *AgentRecord) Result() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.result
}

// Finish marks the agent completed with its final text.
func (a *AgentRecord) Finish(result string) {
	a.mu.Lock()
	if a.completed {
		a.mu.Unlock()
		return
	}
	a.completed = true
	a.result = result
	a.mu.Unlock()
	close(a.Done)
	if cb := OnAgentIdle; cb != nil {
		cb(a) // TeammateIdle hook fires from the agent package (loop context)
	}
}

// OnAgentIdle fires when a registered agent finishes — the REPL sets it
// to run TeammateIdle hooks with the loop's session context.
var OnAgentIdle func(*AgentRecord)

// agentBus is the per-process registry of spawned agents.
var agentBus = struct {
	mu sync.Mutex
	m  map[string]*AgentRecord // keyed by AgentID
}{m: map[string]*AgentRecord{}}

// NewAgentID returns an agentId in the real "a…-…" format.
func NewAgentID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("a%d-%d", time.Now().UnixNano()%1e8, os.Getpid())
	}
	return fmt.Sprintf("a%x-%x", b[:4], b[4:])
}

// RegisterAgent makes an in-process agent addressable by SendMessage.
func RegisterAgent(a *AgentRecord) {
	a.Kind = "in-process"
	a.Started = time.Now()
	agentBus.mu.Lock()
	agentBus.m[a.AgentID] = a
	agentBus.mu.Unlock()
}

// LookupAgent finds an in-process agent by name or agentId. A bare name that
// collides returns the most recently registered (latest wins).
func LookupAgent(to string) *AgentRecord {
	agentBus.mu.Lock()
	defer agentBus.mu.Unlock()
	if a, ok := agentBus.m[to]; ok {
		return a
	}
	var found *AgentRecord
	for _, a := range agentBus.m {
		if a.Name == to && (found == nil || a.Started.After(found.Started)) {
			found = a // latest wins on name collision
		}
	}
	return found
}

// ListInProcessAgents returns all registered agents, oldest first.
func ListInProcessAgents() []*AgentRecord {
	agentBus.mu.Lock()
	defer agentBus.mu.Unlock()
	var out []*AgentRecord
	for _, a := range agentBus.m {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// ---------- cross-session registry ----------
// ~/.claude-go/agent-registry.json: [{"sessionId","cwd","name","pid","ts"}]
// Mutations are serialized with an O_EXCL lockfile.

type sessionEntry struct {
	SessionID           string    `json:"sessionId"`
	CWD                 string    `json:"cwd"`
	Name                string    `json:"name"`
	PID                 int       `json:"pid"`
	TS                  time.Time `json:"ts"`
	MessagingSocketPath string    `json:"messagingSocketPath,omitempty"`
	Kind                string    `json:"kind,omitempty"`       // interactive|bg|daemon|daemon-worker
	Entrypoint          string    `json:"entrypoint,omitempty"` // CLAUDE_CODE_ENTRYPOINT
	StartedAt           int64     `json:"startedAt,omitempty"`  // unix ms, real field
	UpdatedAt           int64     `json:"updatedAt,omitempty"`
	Version             string    `json:"version,omitempty"`
	PeerProtocol        int       `json:"peerProtocol,omitempty"` // real gates on >=1 (Onr)
	PeerFeatures        []string  `json:"peerFeatures,omitempty"`
	PidDomain           string    `json:"pidDomain,omitempty"`
	// ProcStart is the platform start-token real computes via
	// `ps -o lstart= -p <pid>` (string) on posix / a filetime on windows —
	// used by peers to detect pid reuse. any: type varies by platform.
	ProcStart     any    `json:"procStart,omitempty"`
	HostSessionID string `json:"hostSessionId,omitempty"`
	NameSource    string `json:"nameSource,omitempty"` // user|derived|collision|auto|hook|peer
	NameSince     int64  `json:"nameSince,omitempty"`
	LogPath       string `json:"logPath,omitempty"`
	Agent         string `json:"agent,omitempty"`
	JobID         string `json:"jobId,omitempty"`
	Tmux          string `json:"tmux,omitempty"`
	Spare         bool   `json:"spare,omitempty"`
}

// Real CC keeps the live-session registry at ~/.claude/sessions/<pid>.json and
// the project mailbox under <cwd>/.claude/mailbox/ — we follow suit so real
// sessions can see ours.
func goHome() string {
	return config.ClaudeDir()
}

func registryPath() string { return filepath.Join(goHome(), "sessions") }

func withRegistryLock(fn func() error) error {
	os.MkdirAll(goHome(), 0o755)
	lock := registryPath() + ".lock"
	var f *os.File
	for i := 0; i < 200; i++ {
		var err error
		f, err = os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			break
		}
		if i == 199 {
			return fmt.Errorf("could not acquire registry lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.Close()
	defer os.Remove(lock)
	return fn()
}

// sessions dir holds one <pid>.json per live session (real layout); reads
// glob the dir; also accepts our older single-file agent-registry.json.
func readRegistry() []sessionEntry {
	var out []sessionEntry
	if ents, err := filepath.Glob(filepath.Join(registryPath(), "*.json")); err == nil {
		for _, p := range ents {
			var e sessionEntry
			if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &e) == nil && e.SessionID != "" {
				out = append(out, e)
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(goHome(), "agent-registry.json")); err == nil {
		var legacy []sessionEntry
		json.Unmarshal(b, &legacy)
		out = append(out, legacy...)
	}
	return out
}

func sessionFile(pid int) string {
	return filepath.Join(registryPath(), fmt.Sprint(pid)+".json")
}

func writeRegistry(_ []sessionEntry) error { return nil } // per-file writes now

// RegisterSession publishes this session so other local claude-go sessions
// (and real CC peers, which read the same fields) can SendMessage to it.
// Call once at startup (wired elsewhere).
func RegisterSession(sessionID, cwd, name string) error {
	os.MkdirAll(registryPath(), 0o700) // real creates sessions/ mode 0700
	nameSource := "user"
	if name == "" {
		// real derived name: Aa(cwd, sessionId) = <basename>-<rand-suffix>
		name = derivedSessionName(cwd, sessionID)
		nameSource = "derived"
	}
	// real kind: "interactive" | "bg" (CLAUDE_JOB_DIR marks a bg job session)
	kind, jobID := "interactive", ""
	if jd := os.Getenv("CLAUDE_JOB_DIR"); jd != "" {
		kind, jobID = "bg", filepath.Base(jd)
	}
	// U1(isCoordinatorMode()?"coordinator":"normal") — mod_0686.js gn();
	// coordinator wins over the interactive/bg split like real's mode field.
	if IsCoordinatorMode() {
		kind = "coordinator"
	}
	now := time.Now().UnixMilli()
	e := sessionEntry{
		SessionID: sessionID, CWD: cwd, Name: name,
		PID: os.Getpid(), TS: time.Now(),
		Kind:                kind,
		Entrypoint:          sessionEntrypoint(),
		StartedAt:           now,
		UpdatedAt:           now,
		Version:             "2.1.292",
		PeerProtocol:        peerProtoVersion, // Onr — real peer-protocol version gate
		PeerFeatures:        peerFeatures(),
		PidDomain:           pidDomain(),
		ProcStart:           procStart(),
		HostSessionID:       os.Getenv("CLAUDE_CODE_HOST_SESSION_ID"),
		NameSource:          nameSource,
		NameSince:           now,
		LogPath:             os.Getenv("CLAUDE_CODE_SESSION_LOG"),
		Agent:               os.Getenv("CLAUDE_CODE_AGENT"),
		JobID:               jobID,
		Tmux:                tmuxLabel(),
		MessagingSocketPath: socketPath(), // peers can push instantly
	}
	// stash for the outbound <cross-session-message> envelope — real
	// renders sender attribution + reply addressing from these attrs
	selfPeerInfo.Store(struct{ Name, SessionID string }{Name: name, SessionID: sessionID})
	// default rename handler — a peer "rename" control frame updates the
	// registry record (nameSource "peer" like real's fleetview rename)
	// and the outbound envelope name. The REPL may override with a
	// richer handler via SetSessionRenamer.
	SetSessionRenamer(func(n string) {
		n = strings.TrimSpace(n)
		if n == "" {
			return
		}
		updateSessionFile(map[string]any{
			"name": n, "nameSource": "peer",
			"nameSince": time.Now().UnixMilli(),
		})
		selfPeerInfo.Store(struct{ Name, SessionID string }{Name: n, SessionID: sessionID})
	})
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(sessionFile(e.PID), b, 0o644); err != nil {
		return err
	}
	startSessionHeartbeat()
	return nil
}

// derivedSessionName mirrors real Aa(): sanitized cwd basename + "-" +
// a short random suffix (real: 1 byte hex, or a sha256 pin when the
// session-pinned flag is on).
func derivedSessionName(cwd, sessionID string) string {
	base := filepath.Base(cwd)
	if base == "" || base == "." || base == "/" {
		base = "claude"
	}
	var b [1]byte
	rand.Read(b[:])
	return fmt.Sprintf("%s-%x", base, b[0])
}

// tmuxLabel is real's qL(): when inside tmux, "session:window.pane".
func tmuxLabel() string {
	pane := os.Getenv("TMUX_PANE")
	if os.Getenv("TMUX") == "" || pane == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "display-message", "-p", "-t",
		pane, "#{session_name}:#{window_id}.#{pane_id}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Real refreshes the pid file via updatePidFile (fn()) on every mutation and
// stamps updatedAt; we additionally tick updatedAt on a 30s heartbeat so
// peers see a live "last active" signal.
var heartbeat struct {
	once sync.Once
	stop chan struct{}
}

var heartbeatMu sync.Mutex

func startSessionHeartbeat() {
	heartbeatMu.Lock()
	defer heartbeatMu.Unlock()
	heartbeat.once.Do(func() {
		stop := make(chan struct{})
		heartbeat.stop = stop
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					updateSessionFile(map[string]any{"updatedAt": time.Now().UnixMilli()})
				case <-stop: // captured: closing+nil'ing heartbeat.stop races the read
					return
				}
			}
		}()
	})
}

func stopSessionHeartbeat() {
	heartbeatMu.Lock()
	defer heartbeatMu.Unlock()
	if heartbeat.stop != nil {
		close(heartbeat.stop)
		heartbeat.stop = nil
	}
}

// updateSessionFile merges patch into sessions/<pid>.json — real's fn().
func updateSessionFile(patch map[string]any) {
	p := sessionFile(os.Getpid())
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return
	}
	for k, v := range patch {
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	if b, err = json.MarshalIndent(m, "", "  "); err == nil {
		os.WriteFile(p, b, 0o644)
	}
}

// UnregisterSession removes this session from the registry (call at exit).
func UnregisterSession(sessionID string) {
	stopSessionHeartbeat()
	os.Remove(sessionFile(os.Getpid()))
	removePeerKey()
}

// liveSessions returns registry entries whose process is still alive,
// pruning dead rows in the same locked pass.
func liveSessions() []sessionEntry {
	var out []sessionEntry
	for _, e := range readRegistry() {
		if pidAlive(e.PID) {
			out = append(out, e)
		} else {
			os.Remove(sessionFile(e.PID)) // prune dead sessions
		}
	}
	return out
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// ---------- per-session mailbox ----------
// ~/.claude-go/mailbox/<sessionId>.jsonl — one mailMsg per line; the owner
// drains it between turns.

type mailMsg struct {
	From    string    `json:"from"`
	To      string    `json:"to"`
	Kind    string    `json:"kind"` // message|idle|agent-done|main|resume
	Summary string    `json:"summary,omitempty"`
	Message string    `json:"message"`
	TS      time.Time `json:"ts"`
}

func mailboxDir() string { return filepath.Join(goHome(), "mailbox") } // ~/.claude/mailbox/

func mailboxPath(sessionID string) string {
	if sessionID == "" {
		sessionID = "main"
	}
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9':
			return r
		case r == '-' || r == '_' || r == '.':
			return r
		}
		return '_'
	}, sessionID)
	return filepath.Join(mailboxDir(), safe+".jsonl")
}

func mailboxAppend(sessionID string, m mailMsg) error {
	os.MkdirAll(mailboxDir(), 0o755)
	f, err := os.OpenFile(mailboxPath(sessionID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if m.TS.IsZero() {
		m.TS = time.Now()
	}
	b, _ := json.Marshal(m)
	_, err = f.Write(append(b, '\n'))
	return err
}

func renderMail(m mailMsg) string {
	switch m.Kind {
	case "idle":
		return "[Cross-session idle notice] " + m.Message
	case "agent-done":
		return m.Message
	case "main", "resume", "message":
		if m.Kind == "resume" {
			return fmt.Sprintf("<cross-session-message from=%q>[resume] %s</cross-session-message>", m.From, m.Message)
		}
		return fmt.Sprintf("<cross-session-message from=%q>%s</cross-session-message>", m.From, m.Message)
	default:
		return m.Message
	}
}

// DrainMailbox reads and clears a session's mailbox; the REPL calls it
// between turns and injects the returned notes (wired elsewhere).
func DrainMailbox(sessionID string) []string {
	p := mailboxPath(sessionID)
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	os.Remove(p)
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		var m mailMsg
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, renderMail(m))
		}
	}
	return out
}

// NotifyOnIdle appends an idle-notification record to a session's mailbox —
// the "local push-notification" a one-shot subscription fires.
func NotifyOnIdle(sessionID, msg string) error {
	return mailboxAppend(sessionID, mailMsg{From: sessionID, Kind: "idle", Message: msg})
}

// ---------- one-shot idle subscriptions ----------

type idleSub struct {
	subscriber string // session that gets the notice
	text       string
}

var idleSubs = struct {
	mu sync.Mutex
	m  map[string][]idleSub // target sessionID → subscriptions
}{m: map[string][]idleSub{}}

// SubscribeIdle registers a one-shot notice: when target's turn next finishes
// (FireIdleNotifications), text lands in subscriber's mailbox.
func SubscribeIdle(target, subscriber, text string) {
	idleSubs.mu.Lock()
	idleSubs.m[target] = append(idleSubs.m[target], idleSub{subscriber, text})
	idleSubs.mu.Unlock()
}

// FireIdleNotifications delivers all one-shot subscriptions for a session
// that just went idle (or exited). Called by the session's turn boundary
// (wired elsewhere). Also fires peer_idle_notice frames to socket
// subscribers (real's jDn("idle")).
func FireIdleNotifications(sessionID string) {
	idleSubs.mu.Lock()
	subs := idleSubs.m[sessionID]
	delete(idleSubs.m, sessionID)
	idleSubs.mu.Unlock()
	for _, s := range subs {
		_ = NotifyOnIdle(s.subscriber, s.text)
	}
	// prompt boundary: this session is no longer mid-turn — held peer
	// messages release into the queue with "delivered" receipts
	// (real's held→delivered on approval / boundary)
	peerSessionBusy.Store(false)
	FlushHeldPeerMessages()
	firePeerIdleNotices(sessionID, "idle")
}

// ---------- legacy channel bus (fallback for unmatched names) ----------

type busMsg struct {
	From    string    `json:"from"`
	To      string    `json:"to"`
	Summary string    `json:"summary"`
	Message string    `json:"message"`
	TS      time.Time `json:"ts"`
	Unread  bool      `json:"unread"`
}

func busDir() string { return filepath.Join(goHome(), "bus") }

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

// splitAgentRef parses "name" and "name [ref]" address forms.
func splitAgentRef(to string) (name, ref string) {
	if i := strings.LastIndex(to, " ["); i > 0 && strings.HasSuffix(to, "]") {
		return to[:i], to[i+2 : len(to)-1]
	}
	return to, ""
}

func shortRef(id string) string {
	id = strings.TrimPrefix(id, "a")
	if len(id) > 6 {
		return id[:6]
	}
	return id
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
	if in.To == "" {
		return Result{Content: "to is required", IsError: true}
	}
	if in.Message == "" && !in.NotifyWhenIdle {
		return Result{Content: "to and message are required", IsError: true}
	}
	name, ref := splitAgentRef(in.To)
	sender := c.Session
	if sender == "" {
		sender = "session"
	}
	wrapped := fmt.Sprintf("<cross-session-message from=%q>%s</cross-session-message>", sender, in.Message)

	// 1. in-process agent — a bare name always resolves here first.
	rec := LookupAgent(name)
	if rec == nil && ref != "" {
		rec = LookupAgent(ref)
	}
	if rec == nil {
		rec = LookupAgent(in.To) // raw agentId
	}
	if rec != nil {
		if rec.Completed() {
			// names keep working after completion — queue for resume
			_ = mailboxAppend(rec.AgentID, mailMsg{
				From: sender, Kind: "resume", Message: in.Message, Summary: in.Summary,
			})
			return Result{Content: fmt.Sprintf(
				"Agent %q already completed — your message was queued and the agent will be resumed from its transcript when it is next woken.", rec.Name)}
		}
		if in.Message != "" {
			rec.Send(wrapped)
		}
		note := ""
		if in.NotifyWhenIdle {
			// one-shot: first idle after this send notifies the caller
			go func() {
				<-rec.Done
				_ = NotifyOnIdle(c.Session,
					fmt.Sprintf("agent %q (%s) finished its turn.", rec.Name, rec.AgentID))
			}()
			note = " Subscribed for one idle notice."
		}
		if in.Message == "" {
			return Result{Content: fmt.Sprintf("Subscribed — one idle notice will arrive when %s next goes idle.%s", rec.Name, note)}
		}
		return Result{Content: fmt.Sprintf("Message sent to %s [%s] (in-process agent).%s", rec.Name, shortRef(rec.AgentID), note)}
	}

	// 2. the main conversation of this session
	if name == "main" || name == "main-agent" {
		if err := mailboxAppend(c.Session, mailMsg{
			From: sender, Kind: "main", Message: in.Message, Summary: in.Summary,
		}); err != nil {
			return Result{Content: "send failed: " + err.Error(), IsError: true}
		}
		return Result{Content: "Message sent to the main conversation."}
	}

	// 3. other local sessions via the registry file
	var matches []sessionEntry
	for _, e := range liveSessions() {
		switch {
		case e.Name == name || e.SessionID == name:
			matches = append(matches, e)
		case ref != "" && e.Name == name && strings.HasPrefix(e.SessionID, ref):
			matches = append(matches, e)
		}
	}
	if len(matches) > 1 {
		var rows []string
		for _, e := range matches {
			rows = append(rows, fmt.Sprintf("%s [%s]", e.Name, shortRef(e.SessionID)))
		}
		return Result{Content: fmt.Sprintf(
			"Ambiguous recipient %q — matches multiple local sessions: %s. Re-send with the full \"name [ref]\" form.",
			in.To, strings.Join(rows, ", ")), IsError: true}
	}
	if len(matches) == 1 {
		e := matches[0]
		statusNote := ""
		if in.Message != "" {
			m := mailMsg{From: sender, To: e.SessionID, Kind: "message",
				Message: in.Message, Summary: in.Summary}
			ok, note := socketDeliver(e, m)
			statusNote = note
			if !ok {
				// peer offline — persist to the mailbox for its next turn
				if err := mailboxAppend(e.SessionID, m); err != nil {
					return Result{Content: "send failed: " + err.Error(), IsError: true}
				}
			}
		}
		note := ""
		if in.NotifyWhenIdle {
			// real pushes a notify_when_idle control frame at the peer's
			// inbox and records the outbound sub so its peer_idle_notice
			// correlates; mailbox subscription is the offline fallback
			if e.MessagingSocketPath == "" || !func() bool {
				_, ok := sendNotifyWhenIdle(e.MessagingSocketPath)
				return ok
			}() {
				SubscribeIdle(e.SessionID, sender,
					fmt.Sprintf("session %q (%s) went idle.", e.Name, e.SessionID))
			}
			note = " Subscribed — one idle notice will arrive when it next goes idle."
		}
		if in.Message == "" {
			return Result{Content: fmt.Sprintf("Subscribed to %s [%s].%s", e.Name, shortRef(e.SessionID), note)}
		}
		if statusNote != "" {
			statusNote = " " + statusNote + "."
		}
		return Result{Content: fmt.Sprintf(
			"Message sent to %s [%s] (local-session) — it drains its inbox at its next tool round.%s%s",
			e.Name, shortRef(e.SessionID), note, statusNote)}
	}

	// 4. unresolved — queue on the legacy channel bus so the message is not
	// dropped; ListAgents surfaces it.
	if err := busAppend("main", busMsg{
		From: sender, To: in.To, Summary: in.Summary,
		Message: in.Message, TS: time.Now(), Unread: true,
	}); err != nil {
		return Result{Content: "send failed: " + err.Error(), IsError: true}
	}
	return Result{Content: fmt.Sprintf(
		"No live agent or local session named %q — message queued on the local bus.", in.To)}
}

func execListAgents(c *Context, raw json.RawMessage) Result {
	var in struct {
		Channel string `json:"channel"`
		Q       string `json:"q"`
	}
	json.Unmarshal(raw, &in)
	var b strings.Builder
	// in-process agents spawned this session
	for _, a := range ListInProcessAgents() {
		st := "running"
		if a.Completed() {
			st = "completed"
		}
		fmt.Fprintf(&b, "%s [%s]  in-process  %s  type=%s\n",
			a.Name, shortRef(a.AgentID), st, a.Type)
	}
	// other live local sessions
	for _, e := range liveSessions() {
		label := "local-session"
		if c.Session != "" && e.SessionID == c.Session {
			label = "local-session (this session)"
		}
		fmt.Fprintf(&b, "%s [%s]  %s  cwd=%s pid=%d\n",
			e.Name, shortRef(e.SessionID), label, e.CWD, e.PID)
	}
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
			fmt.Fprintf(&b, "%s  background-task  [%s] %s\n", t.ID, st, t.Command)
		}
	}
	b.WriteString("cloud-sessions  [unavailable]  cloud agent listing is not available in this build\n")
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

// ---------- unix-socket peer messaging ----------
// Real wire protocol (mod_1488 / mod_0120): line-delimited JSON on
// <XDG_RUNTIME_DIR|CLAUDE_CODE_TMPDIR|/tmp>/cc-socks/<pid>.sock — an optional
// {"type":"auth","token"} first line, then {"type":"user"} / {"type":"control"}
// frames. Auth tokens publish at sessions/<pid>.<sha256(canonSock)>.key.

// Wire constants lifted from the real implementation.
const (
	peerLineCap       = 1 << 20                // fUt: pending-buffer char cap → drop conn
	peerSockPathMax   = 103                    // Y: unix sun_path byte cap
	peerSubCap        = 32                     // lst: idle-subscription table cap
	peerSendTimeout   = 5 * time.Second        // real's per-connect write timeout
	peerStatusWait    = 750 * time.Millisecond // brief listen for peer_message_status
	peerProtoVersion  = 1                      // Onr
	peerMaxDroppedIDs = 256                    // qjr
	firstLineDeadline = 30 * time.Second       // UMn = 30000: silent-conn deadline
	peerSubTTL        = 12 * time.Hour         // T = 43200000
)

// firstLineDeadlineMs is real's configurable state field (i.firstLineDeadlineMs
// ?? UMn); tests may shrink it. Atomic: servePeer goroutines read it while
// tests write it.
var firstLineDeadlineMs atomic.Int64

// messengerAuthRequired mirrors real's u().authRequired = requireAuth ?? OAn()
// — OAn() is "windows" only; single-user posix accepts unauthenticated conns.
// Atomic for the same reason.
var messengerAuthRequired atomic.Bool

func init() {
	firstLineDeadlineMs.Store(int64(firstLineDeadline))
	messengerAuthRequired.Store(authRequired())
}

func authRequired() bool { return runtime.GOOS == "windows" }

// peerFeatures is real's UL(): ["notify_idle", "reply_across_default_dirs"?,
// "artifact_yield"] — the middle entry requires working peer-pid creds
// (Bun.ant.getPeerPid), which we implement via peercred_*.go.
func peerFeatures() []string {
	feats := []string{"notify_idle"}
	if peerCredOSSupport {
		feats = append(feats, "reply_across_default_dirs")
	}
	return append(feats, "artifact_yield")
}

// Real socket layout (Uns/Lfo): join(XDG_RUNTIME_DIR || CLAUDE_CODE_TMPDIR ||
// "/tmp", "cc-socks", "<pid>.sock") — falling back to the per-uid dir
// /tmp/cc-socks-<uid> ($PREFIX/tmp on Termux) when that path exceeds the
// 103-byte sun_path cap or the shared dir can't be vetted (owned by another
// user etc.). Returns "" when every candidate is refused — messaging off.
func socketDir() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.Getenv("CLAUDE_CODE_TMPDIR")
	}
	if base == "" {
		base = "/tmp"
	}
	d := filepath.Join(base, "cc-socks")
	if len(filepath.Join(d, fmt.Sprint(os.Getpid())+".sock")) <= peerSockPathMax {
		if err := vetSocketDir(d); err == nil {
			return d
		} else {
			peerLogf("sockets dir %s refused (%v) — falling back to the per-uid dir", d, err)
		}
	}
	fb := peerFallbackSocketDir()
	if err := vetSocketDir(fb); err != nil {
		peerLogf("fallback sockets dir %s refused (%v) — cross-session messaging OFF", fb, err)
		return ""
	}
	return fb
}

func peerFallbackSocketDir() string {
	tmp := "/tmp"
	if os.Getenv("TERMUX_VERSION") != "" {
		if p := os.Getenv("PREFIX"); p != "" {
			tmp = filepath.Join(p, "tmp")
		}
	}
	return filepath.Join(tmp, fmt.Sprintf("cc-socks-%d", os.Getuid()))
}

// vetSocketDir is real's Re(): the dir must be absolute; every existing
// component must be a directory owned by us or root that is not
// group/other-writable without the sticky bit (mid-path symlinks must be
// owned by us or root and resolve inside a chain passing the same rules);
// the leaf must exist or be creatable as a REAL directory we own and is
// normalized to mode 0700. Missing tail components are created 0700.
func vetSocketDir(d string) error {
	if !filepath.IsAbs(d) {
		return errors.New("sockets directory must be absolute")
	}
	return vetSocketDirChain(d, 0)
}

// vetSocketDirChain validates/creates the chain rooted at an absolute dir.
// depth bounds symlink recursion (real bails at >16).
func vetSocketDirChain(d string, depth int) error {
	if depth > 16 {
		return errors.New("too many levels of symlinks in sockets path")
	}
	uid := os.Getuid()
	// find the deepest existing component; collect the missing tail
	var missing []string
	probe := d
	for {
		if _, err := os.Lstat(probe); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		next := filepath.Dir(probe)
		if next == probe {
			break
		}
		missing = append(missing, probe)
		probe = next
	}
	// every existing component root→probe obeys the directory_rule
	if err := vetExistingChain(probe, depth); err != nil {
		return err
	}
	// create the missing tail outermost-first at 0700 (real: te(R,{mode:448}))
	for i := len(missing) - 1; i >= 0; i-- {
		p := missing[i]
		if err := os.Mkdir(p, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("sockets-path component %s appeared but is not our directory", p)
		}
		if u, ok := statUID(fi); ok && u != uid {
			return fmt.Errorf("sockets-path component %s appeared owned by another user", p)
		}
	}
	// leaf rules (real's i()): a real directory we own, normalized to 0700
	fi, err := os.Lstat(d)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("sockets directory %s is a symlink — refusing to use it", d)
	}
	if !fi.IsDir() {
		return fmt.Errorf("sockets directory %s exists but is not a directory", d)
	}
	if u, ok := statUID(fi); ok && u != uid {
		return fmt.Errorf("sockets directory %s is owned by another user (uid %d)", d, u)
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// vetExistingChain applies real's directory_rule to every existing
// component of absolute path p, root→p: each must be a directory owned by
// us or root that is not group/other-writable without the sticky bit;
// symlinks must be owned by us or root and resolve into a chain that
// passes the same rules.
func vetExistingChain(p string, depth int) error {
	if depth > 16 {
		return errors.New("too many levels of symlinks in sockets path")
	}
	uid := os.Getuid()
	var comps []string
	for x := p; ; x = filepath.Dir(x) {
		comps = append([]string{x}, comps...)
		if filepath.Dir(x) == x {
			break
		}
	}
	for _, c := range comps {
		fi, err := os.Lstat(c)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			if u, ok := statUID(fi); ok && u != uid && u != 0 {
				return fmt.Errorf("sockets-path symlink %s is owned by another user (uid %d)", c, u)
			}
			tgt, err := filepath.EvalSymlinks(c)
			if err != nil {
				return fmt.Errorf("sockets-path symlink %s does not resolve", c)
			}
			// the resolved chain's own ancestors were never walked — vet them
			if err := vetExistingChain(tgt, depth+1); err != nil {
				return err
			}
			continue
		}
		if !fi.IsDir() {
			return fmt.Errorf("sockets-path component %s is not a directory", c)
		}
		if u, ok := statUID(fi); ok && u != uid && u != 0 {
			return fmt.Errorf("sockets-path directory %s is owned by another user (uid %d)", c, u)
		}
		if m := fi.Mode(); m&os.ModeSticky == 0 && m.Perm()&0o022 != 0 {
			return fmt.Errorf("sockets-path directory %s is group/other-writable without the sticky bit", c)
		}
	}
	return nil
}

// vetExplicitSocketPath validates a --messaging-socket-path value (real's
// Bns): absolute, no '..' segments, names a socket file inside a directory,
// fits sun_path, and the parent must exist (or be creatable 0700 under an
// ancestor owned by us or root) as a real directory we own at mode 0700.
func vetExplicitSocketPath(p string) error {
	if !filepath.IsAbs(p) {
		return errors.New("must be an absolute path")
	}
	segs := strings.Split(p, "/")
	for _, seg := range segs {
		if seg == ".." {
			return errors.New("must not contain '..' segments")
		}
	}
	if last := segs[len(segs)-1]; last == "" || last == "." {
		return errors.New("must name a socket file inside a directory")
	}
	if len(p) > peerSockPathMax {
		return fmt.Errorf("too long for a unix socket (%d bytes, max %d)", len(p), peerSockPathMax)
	}
	d := filepath.Dir(p)
	fi, err := os.Lstat(d)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return createPrivateDirChain(d)
	}
	return checkExplicitSockDir(d, fi)
}

// checkExplicitSockDir applies real's h(): a real directory (no symlink),
// owned by us, mode 0700 (no group/other bits at all).
func checkExplicitSockDir(d string, fi os.FileInfo) error {
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("directory %s is a symlink — must be a real directory", d)
	}
	if !fi.IsDir() {
		return fmt.Errorf("parent %s is not a directory", d)
	}
	if u, ok := statUID(fi); ok && u != os.Getuid() {
		return fmt.Errorf("directory %s is not owned by you (uid %d)", d, u)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("directory %s is not private (mode %04o) — the socket directory must be 0700", d, fi.Mode().Perm())
	}
	return nil
}

// createPrivateDirChain creates a missing explicit socket dir: the nearest
// existing ancestor must be owned by us or root; each missing component is
// made 0700 and must come up ours (real's loop in Bns).
func createPrivateDirChain(d string) error {
	var missing []string
	probe := d
	for {
		fi, err := os.Lstat(probe)
		if err == nil {
			if u, ok := statUID(fi); ok && u != os.Getuid() && u != 0 {
				return fmt.Errorf("%s is owned by another user — refusing to create the sockets directory inside it", probe)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		next := filepath.Dir(probe)
		if next == probe {
			break
		}
		missing = append(missing, probe)
		probe = next
	}
	for i := len(missing) - 1; i >= 0; i-- {
		p := missing[i]
		if err := os.Mkdir(p, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if err := checkExplicitSockDir(p, fi); err != nil {
			return fmt.Errorf("%s appeared while being created and is unusable: %w", p, err)
		}
	}
	return nil
}

// explicitSocketPath is the --messaging-socket-path value, when given.
func explicitSocketPath() string {
	return os.Getenv("CLAUDE_GO_MESSAGING_SOCKET_PATH")
}

// socketPath is the bound inbox path: the explicit --messaging-socket-path
// when given, else <socketDir>/<pid>.sock. "" means messaging is off.
func socketPath() string {
	if p := explicitSocketPath(); p != "" {
		return p
	}
	d := socketDir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, fmt.Sprint(os.Getpid())+".sock")
}

// activeSock records the bound inbox path (real's u().activeSocketPath) —
// reply-target vetting ($Mn/uXr) compares against its directory.
var activeSock atomic.Value // string

// StartMessenger binds the real-protocol unix inbox. We publish our auth key
// at sessions/<pid>.<sha256(sock)>.key and accept the same frames real
// sessions speak, so real CC peers can deliver into us.
func StartMessenger(sessionID string) (cleanup func()) {
	var p string
	if explicit := explicitSocketPath(); explicit != "" {
		// real's Bns: a bad explicit path refuses startup — messaging off
		if err := vetExplicitSocketPath(explicit); err != nil {
			peerLogf("explicit messaging socket path %s refused: %v — messaging OFF", explicit, err)
			return func() {}
		}
		p = explicit
	} else {
		p = socketPath() // socketDir already vetted/chooses the dir
		if p == "" {
			peerLogf("no usable sockets directory — cross-session messaging OFF")
			return func() {}
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return func() {}
	}
	os.Remove(p)
	ln, err := net.Listen("unix", p)
	if err != nil {
		return func() {}
	}
	activeSock.Store(p)
	keyPath := publishPeerKey(p)
	// real exports these so spawned children can reach + authenticate
	os.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", p)
	os.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", peerTokens.child)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go servePeer(conn, sessionID)
		}
	}()
	return func() {
		firePeerIdleNotices(sessionID, "exited") // real's jDn("exited")
		ln.Close()
		os.Remove(p)
		os.Remove(keyPath)
		activeSock.Store("")
		os.Unsetenv("CLAUDE_CODE_MESSAGING_SOCKET")
		os.Unsetenv("CLAUDE_CODE_MESSAGING_TOKEN")
	}
}

// peerTokens is real's _Xo(): a peerToken peers authenticate with and a
// childToken exported to spawned children. 16 random bytes → 32 hex
// (real validates /^[0-9a-f]{32}$/).
var peerTokens = struct{ peer, child string }{
	peer:  randHex(16),
	child: randHex(16),
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// peerTokenClass is real's CXo(): constant-time classify against the active
// tokens → "peer" | "child" | "".
func peerTokenClass(tok string) string {
	if subtle.ConstantTimeCompare([]byte(tok), []byte(peerTokens.peer)) == 1 {
		return "peer"
	}
	if subtle.ConstantTimeCompare([]byte(tok), []byte(peerTokens.child)) == 1 {
		return "child"
	}
	return ""
}

// peerKeyName is real's V(): "<pid>.<sha256(sock path)>.key" —
// the hash pins the key to the socket it vouches for. Real hashes the
// RAW path string (verified live: its key for /tmp/cc-socks/P.sock
// hashes that literal path, not the /private/tmp EvalSymlinks form) —
// canonicalizing made our published key invisible to real's
// reply-target lookup, which is why `from` was dropped on delivery.
func peerKeyName(pid int, sock string) string {
	if !filepath.IsAbs(sock) {
		sock, _ = filepath.Abs(sock)
	}
	sum := sha256.Sum256([]byte(sock))
	return fmt.Sprintf("%d.%x.key", pid, sum)
}

// publishPeerKey is real's SXo(): writes sessions/<pid>.<hash>.key containing
// {peerToken, procStart, pidDomain} mode 0600.
func publishPeerKey(sock string) string {
	if err := os.MkdirAll(registryPath(), 0o700); err != nil {
		return ""
	}
	p := filepath.Join(registryPath(), peerKeyName(os.Getpid(), sock))
	b, _ := json.Marshal(map[string]any{
		"peerToken": peerTokens.peer,
		"procStart": procStart(),
		"pidDomain": pidDomain(),
	})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return ""
	}
	activeKeyFile.Store(p)
	return p
}

var activeKeyFile atomic.Value // string

func removePeerKey() {
	if p, ok := activeKeyFile.Load().(string); ok && p != "" {
		os.Remove(p)
	}
}

// peerFrame is the real wire envelope (line-delimited JSON over the socket).
type peerFrame struct {
	Type      string          `json:"type"`                 // auth|user|control
	Token     string          `json:"token,omitempty"`      // auth
	SessionID string          `json:"session_id,omitempty"` // if present must match recipient
	From      string          `json:"from,omitempty"`       // "uds:<encoded sock path>"
	MsgV      int             `json:"msgV,omitempty"`       // jG(): always 1 on sends
	MsgID     string          `json:"msg_id,omitempty"`     // uuid
	UUID      string          `json:"uuid,omitempty"`
	Priority  string          `json:"priority,omitempty"` // now|next|later (z4e)
	Action    string          `json:"action,omitempty"`   // control action
	Name      string          `json:"name,omitempty"`     // rename
	Message   json.RawMessage `json:"message,omitempty"`  // user: {role:"user",content:"..."}
	// peer_message_status
	Status        string   `json:"status,omitempty"`        // held|denied|expired|delivered|refused|dropped
	StatusDetail  string   `json:"status_detail,omitempty"` // e.g. "refused" under "expired"
	OrigMsgID     string   `json:"orig_msg_id,omitempty"`
	Reason        string   `json:"reason,omitempty"`
	DropReason    string   `json:"drop_reason,omitempty"` // rate-limited|duplicate|hop-loop|hop-runaway|queue-full
	DroppedMsgIDs []string `json:"dropped_msg_ids,omitempty"`
	// peer_idle_notice
	FinishedAt int64  `json:"finished_at,omitempty"`
	State      string `json:"state,omitempty"` // idle|exited|unavailable
	Detail     string `json:"detail,omitempty"`
	FromMode   string `json:"from_mode,omitempty"`
	// artifact reply-yield trio (aEo/cEo/lEo schemas)
	Slugs     []string `json:"slugs,omitempty"`      // yield/unyield: slugs being handed over
	SentAt    *float64 `json:"sent_at,omitempty"`    // yield: request epoch ms (staleness gate)
	ClaimedAt *float64 `json:"claimed_at,omitempty"` // yield: claimant's claim timestamp
	Yielded   []string `json:"yielded,omitempty"`    // yielded reply: slugs handed over
	NotHeld   []string `json:"not_held,omitempty"`   // yielded reply: slugs not held here
	Refused   bool     `json:"refused,omitempty"`    // yielded reply: request refused
	Stopped   bool     `json:"stopped,omitempty"`    // unyield: handback because it stopped
	// user extras we accept but don't materialize
	FileAttachments json.RawMessage `json:"file_attachments,omitempty"`
}

// servePeer is real's en()/S() connection handler: a first-line deadline,
// a cumulative line-size cap, an optional auth first line, then dispatch.
// On platforms where auth is optional (all but windows) unauthenticated
// connections proceed like real's.
func servePeer(c net.Conn, sessionID string) {
	defer c.Close()
	// firstLineDeadlineMs: a conn that sends no complete line in time is
	// closed (real logs cross_session_inbox_auth/silent_connection_deadline).
	c.SetReadDeadline(time.Now().Add(time.Duration(firstLineDeadlineMs.Load())))
	var (
		buf       []byte
		parsed    int // parsed-line count; auth is honored only on line 1
		authCls   string
		peerPid   int
		peerUid   = -1
		credsDone bool
		gotLine   bool
	)
	// creds lazily resolves kernel peer credentials once (real reads
	// Bun.ant.getPeerPid/getPeerUid per conn). Returns ok=false when the
	// peer's uid is known to differ from ours — a same-host foreign process
	// is not our session and the conn is dropped (real's getPeerUid check).
	creds := func() bool {
		if !credsDone {
			credsDone = true
			p, u, _, ok := peerCreds(c)
			if ok {
				peerPid = p
				// real's uid_collapse rule: a peer in a user namespace
				// without a uid map reads as the kernel overflow uid —
				// its creds are unverifiable, not foreign
				if classifyPeerUID(u, os.Getuid()) == peerUIDOverflow {
					u = -1
				}
				peerUid = u
			}
		}
		if peerUid >= 0 && peerUid != os.Getuid() {
			peerLogf("dropping connection from foreign uid %d (pid %d)", peerUid, peerPid)
			return false
		}
		return true
	}
	// selfSent lazily resolves real's ce()/ye() verdict once per conn —
	// after creds() ran, so peerPid and the (line-1-only) auth class are
	// final. Self-sent frames are still delivered but get no reply-status
	// receipts back (real's Oe() skips registering their reply route).
	var selfSentVal, selfSentSet bool
	selfSent := func() bool {
		if !selfSentSet {
			selfSentSet = true
			selfSentVal = peerSelfSent(peerPid, authCls)
		}
		return selfSentVal
	}
	// handle returns false to drop the connection (real's f()/destroy()).
	handle := func(line []byte) bool {
		if len(bytes.TrimSpace(line)) == 0 {
			// a blank line before auth when auth is required → drop
			return !(messengerAuthRequired.Load() && authCls == "")
		}
		var f peerFrame
		if json.Unmarshal(line, &f) != nil {
			if messengerAuthRequired.Load() && authCls == "" {
				return false // unparseable line before auth
			}
			return true
		}
		parsed++
		if f.Type == "auth" {
			if parsed == 1 {
				authCls = peerTokenClass(f.Token)
				if authCls == "" && messengerAuthRequired.Load() {
					return false // bad auth frame when auth is required
				}
			}
			return true // later auth lines are ignored (real: only first line)
		}
		if messengerAuthRequired.Load() && authCls == "" {
			return false // non-auth line before authenticating
		}
		if !creds() {
			return false
		}
		dispatchPeerFrame(f, sessionID, peerPid, authCls, selfSent)
		return true
	}

	chunk := make([]byte, 64<<10)
	for {
		n, err := c.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			if len(buf) > peerLineCap {
				return // real: "Line exceeded 1048576 chars; dropping connection"
			}
			for {
				i := bytes.IndexByte(buf, '\n')
				if i < 0 {
					break
				}
				line := buf[:i]
				buf = buf[i+1:]
				if !gotLine {
					gotLine = true
					c.SetReadDeadline(time.Time{}) // deadline cleared on first complete line
				}
				if !handle(line) {
					return
				}
			}
		}
		if err != nil {
			// EOF/timeout — real's 'end': parse a non-empty final fragment.
			if len(bytes.TrimSpace(buf)) > 0 && len(buf) <= peerLineCap {
				if line := bytes.TrimSpace(buf); len(line) > 0 {
					var f peerFrame
					if json.Unmarshal(line, &f) == nil {
						parsed++
						if f.Type != "auth" && !(messengerAuthRequired.Load() && authCls == "") && creds() {
							dispatchPeerFrame(f, sessionID, peerPid, authCls, selfSent)
						}
					}
				}
			}
			return
		}
	}
}

// peerPIDOf returns the connecting peer's pid — real's Jgn() via
// getsockopt(LOCAL_PEERPID) on darwin / SO_PEERCRED ucred on linux
// (build-tagged peercred_*.go). Other platforms return 0; their peers are
// verified only by their sessions/<pid>.json claim — pid-reuse can then go
// undetected.
func peerPIDOf(c net.Conn) int {
	pid, _, _, ok := peerCredsOf(c)
	if !ok || pid <= 0 {
		return 0
	}
	return pid
}

// peerUIDOf returns the connecting peer's uid — real's Bun.ant.getPeerUid —
// or -1 when the platform cannot report one.
func peerUIDOf(c net.Conn) int {
	_, uid, _, ok := peerCredsOf(c)
	if !ok {
		return -1
	}
	return uid
}

// peerCreds resolves inbound conn creds for servePeer; tests stub it via
// stubPeerCreds (the kernel call can't produce foreign/overflow uids or a
// non-self pid for our own loopback conns). Atomic pointer: servePeer
// goroutines read it while tests swap it.
type peerCredsFunc func(net.Conn) (pid, uid, gid int, ok bool)

var peerCredsStub atomic.Pointer[peerCredsFunc]

func stubPeerCreds(fn func(net.Conn) (pid, uid, gid int, ok bool)) {
	if fn == nil {
		peerCredsStub.Store(nil)
		return
	}
	f := peerCredsFunc(fn)
	peerCredsStub.Store(&f)
}

func peerCreds(c net.Conn) (pid, uid, gid int, ok bool) {
	if f := peerCredsStub.Load(); f != nil {
		return (*f)(c)
	}
	return peerCredsOf(c)
}

// peerOverflowUID is the kernel overflow uid ("nobody"): a peer inside a
// user namespace without a uid map presents it. Real's uid_collapse rule
// treats those creds as unverifiable rather than foreign — the conn is not
// dropped for it, but the uid vouches for nothing.
const peerOverflowUID = 65534

type peerUIDClass int

const (
	peerUIDSelf     peerUIDClass = iota // same uid as us
	peerUIDUnknown                      // platform can't report one
	peerUIDOverflow                     // overflow nobody — unverifiable
	peerUIDForeign                      // a different real uid — drop
)

// classifyPeerUID is real's getPeerUid check plus the uid_collapse carve-out.
func classifyPeerUID(uid, own int) peerUIDClass {
	switch {
	case uid < 0:
		return peerUIDUnknown
	case uid == own:
		return peerUIDSelf
	case uid == peerOverflowUID:
		return peerUIDOverflow
	default:
		return peerUIDForeign
	}
}

// peerSelfSent is real's ce()/ye(): a frame is self-sent when the verified
// peer pid is our own pid or sits in our descendant chain — our pid appears
// in the peer's ppid ancestry (a spawned child dialing back). With no pid
// evidence a presented child token counts as self on macos (real's
// "no-evidence" && childTokenPresented fallback); pid 1 / windows fall to
// the child token directly.
func peerSelfSent(peerPid int, authCls string) bool {
	self := os.Getpid()
	if self == 1 || runtime.GOOS == "windows" {
		return authCls == "child"
	}
	if !peerCredOSSupport {
		return false // !needsVerdict
	}
	if peerPid <= 0 {
		// no evidence: verdict path runs on macos only
		return runtime.GOOS == "darwin" && authCls == "child"
	}
	if peerPid == self {
		return true
	}
	anc := procAncestors(peerPid)
	if anc == nil {
		// ancestry unreadable → "no-evidence"; macos accepts a child token
		return runtime.GOOS == "darwin" && authCls == "child"
	}
	for _, p := range anc {
		if p == self {
			return true
		}
	}
	return false
}

// procAncestors returns pid's ppid chain — real's jAn ancestor read —
// bounded at 4 levels. nil when the first read fails (no evidence).
func procAncestors(pid int) []int {
	out := []int{} // non-nil: an empty chain (e.g. pid 1) is still evidence
	seen := map[int]bool{pid: true}
	for i := 0; i < 4 && pid > 1; i++ {
		ppid := ppidOf(pid)
		if ppid < 0 {
			if i == 0 {
				return nil // ancestry walk failed — no evidence
			}
			break
		}
		if ppid == 0 || seen[ppid] {
			break
		}
		seen[ppid] = true
		out = append(out, ppid)
		pid = ppid
	}
	return out
}

// ppidOf shells `ps -o ppid= -p <pid>` like real; -1 when unreadable.
func ppidOf(pid int) int {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "ppid=", "-p",
		fmt.Sprint(pid)).Output()
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return -1
	}
	return n
}

// peerSessionOK is real's Ie(): a session_id, when present, must be ours.
func peerSessionOK(f peerFrame, sessionID string) bool {
	return f.SessionID == "" || f.SessionID == sessionID
}

// dispatchPeerFrame is real's be()/ze(): user frames route to the prompt
// queue; control frames dispatch on action. Ingress dedup (real's Vjr)
// drops a second delivery of the same msg_id inside a 5-minute window.
// selfSent is the conn's lazy ce() verdict — self-sent frames deliver
// normally but earn no reply-status receipts (real's Oe()).
func dispatchPeerFrame(f peerFrame, sessionID string, peerPid int, authCls string, selfSent func() bool) {
	if f.MsgID != "" && peerMsgIDSeen(f.MsgID) {
		peerLogf("dropping re-delivered msg_id %s", f.MsgID)
		if f.Type == "user" && !selfSent() {
			sendPeerStatusReply(f.From, peerPid, "dropped", f.MsgID, "duplicate")
		}
		return
	}
	switch f.Type {
	case "user":
		var msg struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		// real: missing/non-string content → ignore (before the session gate)
		if json.Unmarshal(f.Message, &msg) != nil || msg.Content == "" {
			return
		}
		if !peerSessionOK(f, sessionID) {
			return
		}
		// real's Hpt gate can hold a peer message for recipient approval;
		// we hold while mid-turn, in a restrictive permission mode, or
		// when the peer is kernel-unverifiable.
		if shouldHoldPeerMsg(peerPid, authCls) {
			holdPeerMsg(f, msg.Content, peerPid, selfSent())
			return
		}
		// real validates priority via z4e (now|next|later) — all land in the
		// same queue here.
		QueuePrompt(msg.Content)
	case "control":
		if !peerSessionOK(f, sessionID) {
			return
		}
		switch f.Action {
		case "rename":
			if rn := sessionRename.Load(); rn != nil {
				(*rn)(f.Name)
			}
		case "peer_message_status":
			handlePeerMessageStatus(f)
		case "notify_when_idle":
			handleNotifyWhenIdle(f, peerPid, authCls)
		case "peer_idle_notice":
			handlePeerIdleNotice(f)
		case "release_held":
			// claude-go extension: force-flush held peer messages early
			// (real releases on user approval / prompt boundary only)
			FlushHeldPeerMessages()
		case "yield_artifact_replies":
			handleYieldArtifactReplies(f, sessionID, peerPid)
		case "unyield_artifact_replies":
			handleUnyieldArtifactReplies(f, peerPid)
		case "artifact_replies_yielded":
			handleArtifactRepliesYielded(f)
		default:
			peerLogf("unhandled control action: %s", f.Action)
		}
	default:
		peerLogf("unhandled message type: %q", f.Type)
	}
}

var peerDebugLog = os.Getenv("CLAUDE_GO_PEER_DEBUG") != ""

func peerLogf(format string, args ...any) {
	if peerDebugLog {
		fmt.Fprintf(os.Stderr, "[uds-messaging] "+format+"\n", args...)
	}
}

// ---------- peer_message_status receipts ----------
// Real correlates status frames to outstanding sends (Zjr) keyed by msg_id
// and moves "held" sends to an awaiting-terminal list. Ours resolves a
// per-send waiter so socketDeliver can surface the disposition.

type peerReceipt struct {
	status       string
	statusDetail string
	reason       string
	dropReason   string
}

var msgReceipts = struct {
	mu sync.Mutex
	m  map[string]chan peerReceipt
}{m: map[string]chan peerReceipt{}}

func trackSend(msgID string) chan peerReceipt {
	ch := make(chan peerReceipt, 4)
	msgReceipts.mu.Lock()
	msgReceipts.m[msgID] = ch
	msgReceipts.mu.Unlock()
	return ch
}

func untrackSend(msgID string) {
	msgReceipts.mu.Lock()
	delete(msgReceipts.m, msgID)
	msgReceipts.mu.Unlock()
}

// validPeerStatuses: real's held|denied|expired|delivered|refused|dropped.
var validPeerStatuses = map[string]bool{
	"held": true, "denied": true, "expired": true,
	"delivered": true, "refused": true, "dropped": true,
}

// validDropReasons: real's Re set (Qjo).
var validDropReasons = map[string]bool{
	"rate-limited": true, "duplicate": true, "hop-loop": true,
	"hop-runaway": true, "queue-full": true,
}

func handlePeerMessageStatus(f peerFrame) {
	status := f.Status
	if !validPeerStatuses[status] {
		return
	}
	// real: expired + status_detail "refused" is reported as "refused"
	if status == "expired" && f.StatusDetail == "refused" {
		status = "refused"
	}
	if status == "dropped" && !validDropReasons[f.DropReason] {
		f.DropReason = "" // real: Qjo() → undefined for unknown reasons
	}
	r := peerReceipt{status: status, statusDetail: f.StatusDetail,
		reason: f.Reason, dropReason: f.DropReason}
	resolveReceipt(f.OrigMsgID, r)
	// real: dropped frames may name additional victims in dropped_msg_ids
	// (capped at qjr=256)
	if status == "dropped" {
		for i, id := range f.DroppedMsgIDs {
			if i >= peerMaxDroppedIDs {
				break
			}
			if len(id) > 0 {
				resolveReceipt(id, r)
			}
		}
	}
}

func resolveReceipt(msgID string, r peerReceipt) {
	msgReceipts.mu.Lock()
	ch, ok := msgReceipts.m[msgID]
	if ok {
		delete(msgReceipts.m, msgID)
	}
	msgReceipts.mu.Unlock()
	if ok {
		select {
		case ch <- r:
		default:
		}
	} else {
		peerLogf("peer_message_status dropped: no outstanding send matches orig_msg_id=%q", msgID)
	}
}

// peerStatusReason is real's mn(): the human-readable reason string carried
// on outbound peer_message_status receipts.
func peerStatusReason(status string) string {
	switch status {
	case "held":
		return "Your message is held for the recipient user's approval before it reaches their Claude session (permission-mode parity)."
	case "denied":
		return "The recipient user declined your message; it was not delivered to their Claude session."
	case "expired":
		return "Your held message expired without approval and was not delivered to the recipient's Claude session."
	case "delivered":
		return "Your previously-held message was approved and released to the recipient's Claude session."
	case "refused":
		return "The recipient session is not accepting cross-session messages (the feature is off there, or a setting or policy there refuses them); your message was not delivered to its Claude."
	case "dropped":
		return "The recipient's session dropped your message at its inbox (rate limit, duplicate, relay loop, or full queue); it was not delivered and will not be."
	}
	return ""
}

// sendPeerStatusReply replies to a peer frame's sender with a
// peer_message_status receipt — real answers on a fresh connection to the
// frame's `from` address, never on the inbound conn. dropReason is only
// meaningful for status "dropped". Real wires "refused" as
// status="expired" + status_detail="refused"; other statuses verbatim.
func sendPeerStatusReply(fromRaw string, peerPid int, status, origMsgID, dropReason string) {
	if !isUUID(origMsgID) {
		return // nothing for the sender to correlate against
	}
	target, ok := shapePeerReplyTarget(fromRaw, peerPid)
	if !ok {
		peerLogf("peer_message_status %s not sent: unshapable reply target %q", status, fromRaw)
		return
	}
	wireStatus, detail := status, ""
	if status == "refused" {
		wireStatus, detail = "expired", "refused"
	}
	socketWriteFrame(target, peerFrame{
		Type: "control", MsgV: 1, MsgID: newUUID(),
		Action: "peer_message_status", Status: wireStatus,
		StatusDetail: detail, Reason: peerStatusReason(status),
		OrigMsgID: origMsgID, DropReason: dropReason,
		From: udsAddr(socketPath()),
	})
}

// ---------- ingress dedup (real's Vjr) ----------
// Re-delivered frames carrying the same msg_id inside a 5-minute window are
// dropped; the map is capped so a hostile peer can't grow it unboundedly.

const (
	peerDedupWindow = 5 * time.Minute
	peerDedupCap    = 1024
)

// selfPeerInfo carries this session's registered identity for the
// outbound <cross-session-message> envelope — real renders the sender
// from `from-name` (and replies to `from`), which live in the message
// content wrapper, not the frame's `from` field.
var selfPeerInfo atomic.Value // struct{Name, SessionID string}

// wrapPeerOutbound builds real's cross-session envelope:
//   <cross-session-message from="uds:..." from-session="<uuid>"
//     hop-chain="<64-hex>" from-name="<name>" from-mode="prompting">
//     body </cross-session-message>
// Live-verified: without it real renders the body but drops all sender
// attribution, so the receiving session can't reply to the sender.
func wrapPeerOutbound(body string) string {
	// real's CWe(): break any tag-shape in the body that would re-parse
	esc := strings.NewReplacer(
		"</cross-session-message", "<\\/cross-session-message",
		"<cross-session-message", "<\\cross-session-message",
	).Replace(body)
	var attrs []string
	if from := udsAddr(socketPath()); from != "" {
		attrs = append(attrs, fmt.Sprintf(`from=%q`, from))
	}
	if self, ok := selfPeerInfo.Load().(struct{ Name, SessionID string }); ok {
		if isUUID(self.SessionID) {
			attrs = append(attrs, fmt.Sprintf(`from-session=%q`, self.SessionID))
		}
		sum := sha256.Sum256([]byte(self.SessionID))
		// real's hop entries are 24-hex (12 bytes) — observed live:
		// `hop-chain="fd5a71268e410e98c7d2fdb5"`
		attrs = append(attrs, fmt.Sprintf(`hop-chain=%q`, hex.EncodeToString(sum[:12])))
		name := strings.Map(func(r rune) rune {
			if strings.ContainsRune(`"<>`+"\n\r", r) {
				return -1
			}
			return r
		}, self.Name)
		if name != "" {
			attrs = append(attrs, fmt.Sprintf(`from-name=%q`, name))
		}
	}
	mode := "prompting"
	if currentPermMode() == "bypassPermissions" {
		mode = "bypass"
	}
	attrs = append(attrs, fmt.Sprintf(`from-mode=%q`, mode))
	return fmt.Sprintf("<cross-session-message %s> %s </cross-session-message>",
		strings.Join(attrs, " "), esc)
}

var peerSeenIDs = struct {
	mu sync.Mutex
	m  map[string]time.Time
}{m: map[string]time.Time{}}

// peerMsgIDSeen records id and reports whether it was already delivered
// within the dedup window.
func peerMsgIDSeen(id string) bool {
	now := time.Now()
	peerSeenIDs.mu.Lock()
	defer peerSeenIDs.mu.Unlock()
	if t, ok := peerSeenIDs.m[id]; ok && now.Sub(t) < peerDedupWindow {
		return true
	}
	if len(peerSeenIDs.m) >= peerDedupCap {
		for k, t := range peerSeenIDs.m { // evict expired first
			if now.Sub(t) >= peerDedupWindow {
				delete(peerSeenIDs.m, k)
			}
		}
		if len(peerSeenIDs.m) >= peerDedupCap {
			for k := range peerSeenIDs.m { // still full — evict one
				delete(peerSeenIDs.m, k)
				break
			}
		}
	}
	peerSeenIDs.m[id] = now
	return false
}

// ---------- held-message queue ----------
// Real's peer_message_status "held": a peer user message that needs
// recipient approval (its Hpt permission gate) waits instead of landing in
// the prompt queue; the sender gets a "held" receipt and a "delivered" one
// when it is released. We hold while the session is mid-turn, while a
// restrictive permission mode is active (real's hold decision gates on the
// RECIPIENT's permission context — "permission-mode parity"), or when the
// peer is kernel-unverifiable on a creds platform; flush at the prompt
// boundary (FireIdleNotifications) or on a control release.

var peerSessionBusy atomic.Bool

// SetPeerSessionBusy marks whether this session is mid-turn — wired by the
// REPL at turn start/end so inbound peer messages hold for the boundary.
func SetPeerSessionBusy(b bool) { peerSessionBusy.Store(b) }

// peerPermModeFn reports this session's permission mode — wired by main
// from loop.PermMode (real's Ci().inbound.getCurrentMode). Restrictive
// modes — where inbound automation must wait for the user — hold peer
// messages for approval rather than delivering mid-conversation.
var peerPermModeFn atomic.Pointer[func() string]

// peerIdleHookFn fires the TeammateIdle hook when a peer session's
// idle/exited notice is admitted — wired from main to the loop's
// hook runner (payload: teammate_name = sender address).
var peerIdleHookFn atomic.Pointer[func(from string)]

// SetPeerIdleHook registers the TeammateIdle hook callback.
func SetPeerIdleHook(fn func(from string)) {
	if fn == nil {
		peerIdleHookFn.Store(nil)
		return
	}
	peerIdleHookFn.Store(&fn)
}

// SetPeerPermMode registers the permission-mode getter used by the
// messenger's hold gate (and, elsewhere, from_mode on outbound notices).
func SetPeerPermMode(fn func() string) {
	if fn == nil {
		peerPermModeFn.Store(nil)
		return
	}
	peerPermModeFn.Store(&fn)
}

func currentPermMode() string {
	if g := peerPermModeFn.Load(); g != nil {
		return (*g)()
	}
	return ""
}

// shouldHoldPeerMsg decides whether an inbound user frame waits for the
// prompt boundary instead of queueing immediately.
func shouldHoldPeerMsg(peerPid int, authCls string) bool {
	if peerSessionBusy.Load() {
		return true
	}
	// permission-mode parity: "plan"/"dontAsk"-style modes gate inbound
	// automation behind the user — peer messages wait for approval
	switch currentPermMode() {
	case "plan", "dontAsk":
		return true
	}
	// unverified: on a platform where kernel creds exist, a peer with no
	// verified pid AND no valid auth token is held for approval
	if peerCredOSSupport && peerPid <= 0 && authCls == "" {
		return true
	}
	return false
}

type heldPeerMsg struct {
	content   string
	fromRaw   string // original `from` (for logging / future re-shape)
	replyTo   string // shaped sock path for receipts ("" = unreachable)
	origMsgID string
	heldAt    time.Time
}

const peerHeldCap = 32

var heldPeerMsgs = struct {
	mu sync.Mutex
	q  []heldPeerMsg
}{}

// selfSent suppresses receipts (real's Oe() never registers a reply route
// for self-sent origins — there is no peer session to answer).
func holdPeerMsg(f peerFrame, content string, peerPid int, selfSent bool) {
	replyTo := ""
	if !selfSent {
		replyTo, _ = shapePeerReplyTarget(f.From, peerPid)
	}
	heldPeerMsgs.mu.Lock()
	if len(heldPeerMsgs.q) >= peerHeldCap {
		heldPeerMsgs.mu.Unlock()
		if !selfSent {
			sendPeerStatusReply(f.From, peerPid, "dropped", f.MsgID, "queue-full")
		}
		return
	}
	heldPeerMsgs.q = append(heldPeerMsgs.q, heldPeerMsg{
		content: content, fromRaw: f.From, replyTo: replyTo,
		origMsgID: f.MsgID, heldAt: time.Now(),
	})
	heldPeerMsgs.mu.Unlock()
	if !selfSent {
		sendPeerStatusReply(f.From, peerPid, "held", f.MsgID, "")
	}
}

// FlushHeldPeerMessages releases every held peer message at a prompt
// boundary — each is queued and its sender gets a "delivered" receipt.
func FlushHeldPeerMessages() {
	heldPeerMsgs.mu.Lock()
	q := heldPeerMsgs.q
	heldPeerMsgs.q = nil
	heldPeerMsgs.mu.Unlock()
	for _, h := range q {
		QueuePrompt(h.content)
		if h.replyTo != "" && isUUID(h.origMsgID) {
			socketWriteFrame(h.replyTo, peerFrame{
				Type: "control", MsgV: 1, MsgID: newUUID(),
				Action: "peer_message_status", Status: "delivered",
				Reason:    peerStatusReason("delivered"),
				OrigMsgID: h.origMsgID,
				From:      udsAddr(socketPath()),
			})
		}
	}
}

// ---------- notify_when_idle / peer_idle_notice ----------
// Real's idle-subscription table (_yr): a peer sends control
// notify_when_idle {from:"uds:...", msg_id:<uuid>}; when we next go idle (or
// exit) it gets one peer_idle_notice {orig_msg_id, state, finished_at?} at
// its reply address. Cap lst=32, TTL 12h.

type peerIdleSub struct {
	replyAddress    string // decoded sock path to dial
	replyAddressRaw string // original uds:... address (log)
	origMsgID       string
	peerPid         int
	authed          bool
	requestedAt     int64
}

var peerIdleSubs = struct {
	mu   sync.Mutex
	subs []peerIdleSub
}{}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return false
			}
		}
	}
	return true
}

// shapePeerReplyTarget is real's $Mn(): the uds: address must decode to a
// .sock path inside our own socket directory (uXr) — cross-namespace or
// non-uds reply targets are dropped. The verifiedPeerPid+ownerUids escape
// hatch (reply_across_default_dirs) is approximated: a verified peer pid
// lets any .sock path under our uid's dirs through.
func shapePeerReplyTarget(addr string, peerPid int) (string, bool) {
	target, ok := decodeUDSAddr(addr)
	if !ok {
		return "", false
	}
	own, _ := activeSock.Load().(string)
	if own == "" {
		return "", false // own inbox not bound
	}
	if filepath.Dir(target) == filepath.Dir(own) && strings.HasSuffix(target, ".sock") {
		return target, true
	}
	if peerPid > 0 && strings.HasSuffix(target, ".sock") {
		if d := filepath.Dir(target); d == filepath.Dir(own) ||
			d == filepath.Join("/tmp", fmt.Sprintf("cc-socks-%d", os.Getuid())) {
			return target, true
		}
	}
	return "", false
}

func handleNotifyWhenIdle(f peerFrame, peerPid int, authCls string) {
	if !isUUID(f.MsgID) {
		peerLogf("notify_when_idle dropped: malformed frame (bad msg_id)")
		return
	}
	target, ok := shapePeerReplyTarget(f.From, peerPid)
	if !ok {
		peerLogf("notify_when_idle dropped: reply address unshaped or outside our socket namespace")
		return
	}
	if own, _ := activeSock.Load().(string); canonSock(target) == canonSock(own) {
		peerLogf("notify_when_idle dropped: reply target is this session (self-target)")
		return
	}
	res := recordIdleSub(peerIdleSub{
		replyAddress: target, replyAddressRaw: f.From,
		origMsgID: f.MsgID, peerPid: peerPid,
		authed: authCls != "", requestedAt: time.Now().UnixMilli(),
	})
	if res == "full" {
		// real's gJt(): tell the requester its subscription was refused
		sendPeerIdleNotice(target, f.MsgID, "unavailable", 0, "")
	}
}

func canonSock(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	abs, _ := filepath.Abs(p)
	return abs
}

// recordIdleSub returns "recorded" | "refreshed" | "full" (real's _yr).
func recordIdleSub(s peerIdleSub) string {
	peerIdleSubs.mu.Lock()
	defer peerIdleSubs.mu.Unlock()
	now := time.Now().UnixMilli()
	// TTL prune (real: subscriptions older than T=12h expire)
	keep := peerIdleSubs.subs[:0]
	for _, x := range peerIdleSubs.subs {
		if now-x.requestedAt < int64(peerSubTTL/time.Millisecond) {
			keep = append(keep, x)
		}
	}
	peerIdleSubs.subs = keep
	for i, x := range peerIdleSubs.subs {
		if x.replyAddress == s.replyAddress && x.peerPid == s.peerPid {
			peerIdleSubs.subs[i] = s // refresh (real splices + repushes)
			return "refreshed"
		}
	}
	if len(peerIdleSubs.subs) >= peerSubCap {
		return "full"
	}
	peerIdleSubs.subs = append(peerIdleSubs.subs, s)
	return "recorded"
}

// firePeerIdleNotices sends each recorded subscriber one peer_idle_notice —
// real's jDn("idle"|"exited").
func firePeerIdleNotices(sessionID, state string) {
	peerIdleSubs.mu.Lock()
	subs := peerIdleSubs.subs
	peerIdleSubs.subs = nil
	peerIdleSubs.mu.Unlock()
	var finishedAt int64
	if state == "idle" {
		finishedAt = time.Now().UnixMilli()
	}
	for _, s := range subs {
		sendPeerIdleNotice(s.replyAddress, s.origMsgID, state, finishedAt, "")
	}
}

// ---------- outbound idle subscriptions (real's d.outstanding / Syr) ----------
// When WE send a notify_when_idle to a peer, its msg_id lands here; an
// inbound peer_idle_notice is admitted only while correlated to an
// outstanding subscription (gRt) and consumes it one-shot (byr).

type outboundIdleSub struct {
	msgID       string
	target      string // canonical sock path the sub was sent to
	requestedAt int64
}

var outboundIdleSubs = struct {
	mu   sync.Mutex
	subs []outboundIdleSub
}{}

// recordOutboundIdleSub registers a sent notify_when_idle (real's Syr):
// TTL peerSubTTL, table cap peerSubCap.
func recordOutboundIdleSub(msgID, target string) bool {
	outboundIdleSubs.mu.Lock()
	defer outboundIdleSubs.mu.Unlock()
	now := time.Now().UnixMilli()
	keep := outboundIdleSubs.subs[:0]
	for _, s := range outboundIdleSubs.subs {
		if now-s.requestedAt < int64(peerSubTTL/time.Millisecond) {
			keep = append(keep, s)
		}
	}
	outboundIdleSubs.subs = keep
	if len(outboundIdleSubs.subs) >= peerSubCap {
		return false
	}
	outboundIdleSubs.subs = append(outboundIdleSubs.subs, outboundIdleSub{
		msgID: msgID, target: canonSock(target), requestedAt: now,
	})
	return true
}

// outboundIdleOutstanding is real's gRt: is this orig_msg_id one of our
// outstanding subscriptions?
func outboundIdleOutstanding(msgID string) bool {
	outboundIdleSubs.mu.Lock()
	defer outboundIdleSubs.mu.Unlock()
	for _, s := range outboundIdleSubs.subs {
		if s.msgID == msgID {
			return true
		}
	}
	return false
}

// consumeOutboundIdleSub is real's byr: remove the named subscription; a
// terminal notice (idle|exited) also clears other subs to the same target.
func consumeOutboundIdleSub(msgID, state string) bool {
	outboundIdleSubs.mu.Lock()
	defer outboundIdleSubs.mu.Unlock()
	idx := -1
	for i, s := range outboundIdleSubs.subs {
		if s.msgID == msgID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	target := outboundIdleSubs.subs[idx].target
	outboundIdleSubs.subs = append(outboundIdleSubs.subs[:idx], outboundIdleSubs.subs[idx+1:]...)
	if state == "idle" || state == "exited" {
		keep := outboundIdleSubs.subs[:0]
		for _, s := range outboundIdleSubs.subs {
			if s.target != target {
				keep = append(keep, s)
			}
		}
		outboundIdleSubs.subs = keep
	}
	return true
}

// sendNotifyWhenIdle pushes a notify_when_idle control frame at a peer
// inbox and records the outbound subscription so its reply correlates.
func sendNotifyWhenIdle(sockPath string) (string, bool) {
	msgID := newUUID()
	if !recordOutboundIdleSub(msgID, sockPath) {
		return "", false
	}
	ok := socketWriteFrame(sockPath, peerFrame{
		Type: "control", MsgV: 1, MsgID: msgID,
		Action: "notify_when_idle",
		From:   udsAddr(socketPath()),
	})
	if !ok {
		consumeOutboundIdleSub(msgID, "")
		return "", false
	}
	return msgID, true
}

// handlePeerIdleNotice renders an inbound idle notice like real's
// Cyr/oe(): an automated harness notice queued as a meta prompt. Notices
// uncorrelated to an outstanding notify_when_idle we sent are dropped
// (real's gRt) — a peer cannot inject idle prompts unprompted.
func handlePeerIdleNotice(f peerFrame) {
	if f.OrigMsgID == "" {
		return
	}
	if !outboundIdleOutstanding(f.OrigMsgID) {
		peerLogf("peer_idle_notice: dropped (uncorrelated / already delivered / expired)")
		return
	}
	if !consumeOutboundIdleSub(f.OrigMsgID, f.State) {
		peerLogf("peer_idle_notice not admitted: subscription for orig_msg_id=%q was already consumed", f.OrigMsgID)
		return
	}
	label := f.From
	if label == "" {
		label = "(peer session)"
	}
	// real's TeammateIdle hook — a peer session going idle
	if f.State == "idle" || f.State == "exited" {
		if fn := peerIdleHookFn.Load(); fn != nil {
			(*fn)(f.From)
		}
	}
	var text string
	switch f.State {
	case "idle":
		text = fmt.Sprintf("[Cross-session idle notice] %q is idle now — it finished a turn.", label)
		if f.Detail != "" {
			text += fmt.Sprintf(" Its harness reports: «%s».", f.Detail)
		}
	case "exited":
		text = fmt.Sprintf("[Cross-session idle notice] %q exited before going idle; it will not process further messages at that address.", label)
	case "unavailable":
		text = fmt.Sprintf("[Cross-session idle notice] %q is not holding your idle subscription — no idle notice will arrive from it.", label)
	default:
		peerLogf("peer_idle_notice dropped: unrecognized state %q", f.State)
		return
	}
	QueuePrompt(text)
}

// sendPeerIdleNotice writes a control frame to a peer inbox (real's zDn
// sender → aoe): {type:"control", msgV:1, msg_id:<uuid>, action:
// "peer_idle_notice", orig_msg_id, state, finished_at?, detail?, from}.
func sendPeerIdleNotice(sockPath, origMsgID, state string, finishedAt int64, detail string) {
	f := peerFrame{
		Type: "control", MsgV: 1, MsgID: newUUID(),
		Action: "peer_idle_notice", OrigMsgID: origMsgID,
		State: state, FinishedAt: finishedAt, Detail: detail,
		From: udsAddr(socketPath()),
	}
	socketWriteFrame(sockPath, f)
}

// ---------- artifact reply-yield trio ----------
// Real's reply-lane handoff (mod_1488 + aEo/cEo/lEo schemas): a peer session
// of the SAME conversation asks us to yield artifact-reply routing for a set
// of slugs via yield_artifact_replies {from, msg_id, session_id, slugs,
// reason:resume|claim, sent_at, claimed_at?, requester?}; we answer
// artifact_replies_yielded {orig_msg_id, yielded, not_held, refused?} on a
// fresh conn to the requester's `from`. We run no artifact service — holder
// === null in real's terms — so a verified, fresh request is granted empty:
// every asked slug answers not_held.

const (
	peerYieldSlugCap = 16   // lIt: max slugs per yield frame
	peerYieldStaleMs = 3500 // A-500: requests older than this are refused
	peerYieldSkewMs  = 1000 // sent_at this far in the future is refused
	yieldGrantCap    = 64   // granted-yield table cap
)

// artifactYieldGrant records a yield we answered, so a later
// unyield_artifact_replies (real's yEo) can correlate by orig_msg_id and the
// granting peer's verified pid, then clear it.
type artifactYieldGrant struct {
	target  string // canonical requester sock
	peerPid int
	slugs   []string
	at      int64
}

var artifactYieldGrants = struct {
	mu sync.Mutex
	m  map[string]artifactYieldGrant
}{m: map[string]artifactYieldGrant{}}

// liveSessionForSock resolves the registry entry bound at sock — real's
// yUt: the session record keyed by its inbox path.
func liveSessionForSock(sock string) *sessionEntry {
	canon := canonSock(sock)
	for _, e := range readRegistry() {
		if e.MessagingSocketPath == "" || canonSock(e.MessagingSocketPath) != canon {
			continue
		}
		if !pidAlive(e.PID) {
			return nil
		}
		cp := e
		return &cp
	}
	return nil
}

// handleYieldArtifactReplies is real's yield_artifact_replies arm of be():
// validate the aEo frame, shape the reply target, require the requester to
// be a verified live session of this conversation, refuse stale requests,
// else grant (empty — we hold nothing).
func handleYieldArtifactReplies(f peerFrame, sessionID string, peerPid int) {
	if f.MsgID == "" || len(f.MsgID) > 128 || f.SessionID == "" ||
		len(f.SessionID) > 512 || len(f.From) > 512 || f.Slugs == nil ||
		len(f.Slugs) > peerYieldSlugCap || f.SentAt == nil ||
		math.IsNaN(*f.SentAt) || math.IsInf(*f.SentAt, 0) {
		peerLogf("yield_artifact_replies dropped: malformed frame")
		return
	}
	for _, s := range f.Slugs {
		if len(s) > 128 {
			peerLogf("yield_artifact_replies dropped: malformed frame")
			return
		}
	}
	own, _ := activeSock.Load().(string)
	if own == "" {
		peerLogf("yield_artifact_replies dropped: own inbox not bound (%q)", f.From)
		return
	}
	target, ok := shapePeerReplyTarget(f.From, peerPid)
	if !ok {
		peerLogf("yield_artifact_replies dropped: reply address unshaped or outside our socket namespace (%q)", f.From)
		return
	}
	if canonSock(target) == canonSock(own) {
		peerLogf("yield_artifact_replies dropped: reply target is this session")
		return
	}
	// real's check: the requester must be a verified live session of THIS
	// conversation — registry entry at the sock, session id match, and a
	// kernel-verified pid must equal the record's pid
	if req := liveSessionForSock(target); req == nil ||
		req.SessionID != sessionID || (peerPid > 0 && req.PID != peerPid) {
		peerLogf("yield_artifact_replies refused: requester is not a verified live session of this conversation (%q)", f.From)
		answerArtifactYield(target, f.MsgID, nil, nil, true)
		return
	}
	// real's staleness gate (Wvr): older than A-500ms, from the future, or
	// claimed_at after sent_at+1s → refuse
	now, sentAt := float64(time.Now().UnixMilli()), *f.SentAt
	if now-sentAt > peerYieldStaleMs || sentAt > now+peerYieldSkewMs ||
		(f.ClaimedAt != nil && *f.ClaimedAt > sentAt+peerYieldSkewMs) {
		peerLogf("yield_artifact_replies refused: stale request")
		answerArtifactYield(target, f.MsgID, nil, nil, true)
		return
	}
	notHeld := append([]string(nil), f.Slugs...)
	recordArtifactYield(f.MsgID, target, peerPid, notHeld)
	answerArtifactYield(target, f.MsgID, nil, notHeld, false)
}

func recordArtifactYield(origMsgID, target string, peerPid int, slugs []string) {
	artifactYieldGrants.mu.Lock()
	defer artifactYieldGrants.mu.Unlock()
	if len(artifactYieldGrants.m) >= yieldGrantCap {
		var oldest string
		var oldestAt int64 = -1
		for k, g := range artifactYieldGrants.m {
			if oldestAt < 0 || g.at < oldestAt {
				oldest, oldestAt = k, g.at
			}
		}
		delete(artifactYieldGrants.m, oldest)
	}
	artifactYieldGrants.m[origMsgID] = artifactYieldGrant{
		target: canonSock(target), peerPid: peerPid,
		slugs: slugs, at: time.Now().UnixMilli(),
	}
}

// answerArtifactYield sends artifact_replies_yielded to the requester —
// real's sendAnswer (jvr): refused answers carry refused:true; grants carry
// the not_held slugs.
func answerArtifactYield(sock, origMsgID string, yielded, notHeld []string, refused bool) {
	f := peerFrame{
		Type: "control", MsgV: 1, MsgID: newUUID(),
		Action: "artifact_replies_yielded", OrigMsgID: origMsgID,
		From: udsAddr(socketPath()),
	}
	if refused {
		f.Refused = true
	} else {
		f.Yielded, f.NotHeld = yielded, notHeld
	}
	socketWriteFrame(sock, f)
}

// handleUnyieldArtifactReplies is real's unyield arm (cEo): the holder hands
// the named slugs back — must correlate to a yield we granted this peer.
func handleUnyieldArtifactReplies(f peerFrame, peerPid int) {
	if f.OrigMsgID == "" || len(f.OrigMsgID) > 128 || f.Slugs == nil ||
		len(f.Slugs) > peerYieldSlugCap {
		peerLogf("unyield_artifact_replies dropped: malformed, uncorrelated or already handed back")
		return
	}
	for _, s := range f.Slugs {
		if len(s) > 128 {
			peerLogf("unyield_artifact_replies dropped: malformed, uncorrelated or already handed back")
			return
		}
	}
	artifactYieldGrants.mu.Lock()
	g, ok := artifactYieldGrants.m[f.OrigMsgID]
	if ok && g.peerPid > 0 && peerPid > 0 && g.peerPid != peerPid {
		ok = false // correlation binds to the granting peer's verified pid
	}
	if ok {
		delete(artifactYieldGrants.m, f.OrigMsgID)
	}
	artifactYieldGrants.mu.Unlock()
	if !ok {
		peerLogf("unyield_artifact_replies dropped: malformed, uncorrelated or already handed back")
		return
	}
	peerLogf("unyield_artifact_replies: %d slugs handed back (stopped=%v)",
		len(f.Slugs), f.Stopped)
}

// ---------- outbound artifact yields (real's outstanding/fEo) ----------
// If we ever ask a peer to yield (we have no artifact service, so this stays
// empty in practice), the ask's msg_id lands here; an inbound
// artifact_replies_yielded settles it one-shot.

var outboundYieldAsks = struct {
	mu sync.Mutex
	m  map[string]time.Time
}{m: map[string]time.Time{}}

func recordOutboundYieldAsk(msgID string) {
	outboundYieldAsks.mu.Lock()
	outboundYieldAsks.m[msgID] = time.Now()
	outboundYieldAsks.mu.Unlock()
}

func consumeOutboundYieldAsk(msgID string) bool {
	outboundYieldAsks.mu.Lock()
	defer outboundYieldAsks.mu.Unlock()
	if _, ok := outboundYieldAsks.m[msgID]; !ok {
		return false
	}
	delete(outboundYieldAsks.m, msgID)
	return true
}

// handleArtifactRepliesYielded is real's lEo arm: settle an outstanding ask.
func handleArtifactRepliesYielded(f peerFrame) {
	if f.OrigMsgID == "" || len(f.OrigMsgID) > 128 {
		peerLogf("artifact_replies_yielded dropped: malformed frame")
		return
	}
	if !consumeOutboundYieldAsk(f.OrigMsgID) {
		peerLogf("artifact_replies_yielded dropped: uncorrelated or already settled")
		return
	}
	peerLogf("artifact_replies_yielded: refused=%v yielded=%d not_held=%d",
		f.Refused, len(f.Yielded), len(f.NotHeld))
}

// sessionRename is wired by the REPL so a peer "rename" control frame works.
// Atomic: servePeer goroutines read it while the REPL/tests register it.
var sessionRename atomic.Pointer[func(string)]

// SetSessionRenamer registers the rename callback.
func SetSessionRenamer(fn func(string)) {
	if fn == nil {
		sessionRename.Store(nil)
		return
	}
	sessionRename.Store(&fn)
}

// newUUID: real's crypto.randomUUID — msg_id must match /^[0-9a-f]{8}-
// [0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i to correlate receipts.
func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// udsAddr is real's _W(): "uds:" + percent-encoding that leaves
// [A-Za-z0-9:_/.\\-] unescaped (everything else → %XX, uppercase).
func udsAddr(path string) string {
	const keep = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789:_/.\\-"
	var b strings.Builder
	b.WriteString("uds:")
	for i := 0; i < len(path); i++ {
		c := path[i]
		if strings.IndexByte(keep, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// decodeUDSAddr parses a "uds:" address back to a socket path.
func decodeUDSAddr(addr string) (string, bool) {
	if !strings.HasPrefix(addr, "uds:") {
		return "", false
	}
	s := addr[4:]
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := hex.DecodeString(s[i+1 : i+3]); err == nil {
				b.WriteByte(v[0])
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	out := b.String()
	if !strings.HasSuffix(out, ".sock") {
		return "", false
	}
	return out, true
}

// socketWriteFrame performs real's we(): connect (5s cap), prepend the auth
// line when the peer's key file yields a token, write auth+frame in a single
// buffer, close.
func socketWriteFrame(sockPath string, f peerFrame) bool {
	c, err := net.DialTimeout("unix", sockPath, peerSendTimeout)
	if err != nil {
		return false
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(peerSendTimeout))
	line, _ := json.Marshal(f)
	payload := append(authLineFor(sockPath), append(line, '\n')...)
	_, err = c.Write(payload)
	return err == nil
}

// authLineFor is real's wXo+nXr: {"type":"auth","token":<peerToken>}\n or "".
func authLineFor(sockPath string) []byte {
	tok := peerTokenForSock(sockPath, 0)
	if tok == "" {
		return nil
	}
	b, _ := json.Marshal(peerFrame{Type: "auth", Token: tok})
	return append(b, '\n')
}

// socketDeliver pushes a user frame to a live session's socket, using its
// published key file for auth when present — real's hUt/we. After writing we
// briefly listen for a peer_message_status receipt arriving on our own inbox
// and report it; a real peer replies via a fresh connection to our `from`
// address (it never answers on the same conn).
func socketDeliver(e sessionEntry, m mailMsg) (bool, string) {
	if e.MessagingSocketPath == "" {
		return false, ""
	}
	c, err := net.DialTimeout("unix", e.MessagingSocketPath, peerSendTimeout)
	if err != nil {
		return false, ""
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(peerSendTimeout))

	// real verifies the connected endpoint's pid (expectPeerPid) via
	// LOCAL_PEERPID before writing — a mismatched pid means a recycled or
	// forged socket and the send is refused.
	if pid := peerPIDOf(c); pid > 0 && e.PID > 0 && pid != e.PID {
		return false, fmt.Sprintf("connected endpoint is pid %d, expected %d — refusing to write", pid, e.PID)
	}

	content := m.Message
	if m.Summary != "" {
		content = m.Summary + ": " + m.Message
	}
	// real's hUt(): the envelope lives in message.content — sender
	// attribution + reply addressing ride inside, not in frame fields
	content = wrapPeerOutbound(content)
	msgID := newUUID()
	frame := peerFrame{
		Type: "user", MsgV: 1, MsgID: msgID,
		From:     udsAddr(socketPath()),
		Priority: "next", // real's default priority in hUt
		Message:  mustJSON(map[string]any{"role": "user", "content": content}),
	}
	line, _ := json.Marshal(frame)
	payload := append(authLineFor(e.MessagingSocketPath), append(line, '\n')...)
	waiter := trackSend(msgID)
	defer untrackSend(msgID)
	if _, err := c.Write(payload); err != nil {
		return false, ""
	}
	select {
	case r := <-waiter:
		return true, peerReceiptText(r)
	case <-time.After(peerStatusWait):
		return true, ""
	}
}

// peerReceiptText renders a status like real's mn() reason strings.
func peerReceiptText(r peerReceipt) string {
	if r.status == "dropped" && r.dropReason != "" {
		return fmt.Sprintf("peer reported the message %s (drop_reason=%s)", r.status, r.dropReason)
	}
	switch r.status {
	case "held":
		return "peer is holding the message for its user's approval"
	case "denied":
		return "the peer's user declined the message"
	case "expired":
		return "the held message expired without approval"
	case "delivered":
		return "a previously-held message was approved and delivered"
	case "refused":
		return "the peer refused the message (policy or feature off)"
	case "dropped":
		return "the peer dropped the message at its inbox"
	}
	return ""
}

// peerTokenFor reads the peer's sessions/<pid>.<sha256(sock)>.key — real's
// wXo. Multiple same-hash keys rank by live-owner/procStart verification.
func peerTokenFor(e sessionEntry) string {
	return peerTokenForSock(e.MessagingSocketPath, e.PID)
}

func peerTokenForSock(sockPath string, pidHint int) string {
	if sockPath == "" {
		return ""
	}
	// real's key-name hash is over the RAW socket path — see peerKeyName
	sum := sha256.Sum256([]byte(sockPath))
	suffix := fmt.Sprintf(".%x.key", sum)
	ents, err := os.ReadDir(registryPath())
	if err != nil {
		return peerTokenLegacy(sockPath, pidHint)
	}
	type cand struct {
		path string
		pid  int
		tok  string
		rank int
	}
	var best *cand
	for _, ent := range ents {
		name := ent.Name()
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		var pid int
		if n, _ := fmt.Sscanf(name, "%d.", &pid); n != 1 {
			continue
		}
		b, err := os.ReadFile(filepath.Join(registryPath(), name))
		if err != nil || len(b) > 4096 { // real reads at most x=4096 bytes
			continue
		}
		var k struct {
			PeerToken string `json:"peerToken"`
			ProcStart any    `json:"procStart"`
		}
		if json.Unmarshal(b, &k) != nil || k.PeerToken == "" {
			continue
		}
		c := &cand{path: name, pid: pid, tok: k.PeerToken}
		switch {
		case !pidAlive(pid):
			c.rank = 0 // dead owner — real's dead-owner, last resort
		case procStartMatches(pid, k.ProcStart):
			c.rank = 2 // live owner with a matching start token
		default:
			c.rank = 1
		}
		if best == nil || c.rank > best.rank {
			best = c
		}
	}
	if best != nil {
		return best.tok
	}
	return peerTokenLegacy(sockPath, pidHint)
}

// procStartMatches compares a key file's procStart claim with the live
// start-token for pid (real's zS/gWe). Unknown token → not a match.
func procStartMatches(pid int, claim any) bool {
	cur := procStartTokenOf(pid)
	if cur == "" {
		return false
	}
	switch v := claim.(type) {
	case string:
		return v == cur
	default:
		return false
	}
}

// peerTokenLegacy reads our older <pid>.<rand>.key beside the socket — kept
// so older claude-go peers still resolve; real never writes there.
func peerTokenLegacy(sockPath string, pidHint int) string {
	dir := filepath.Dir(sockPath)
	pat := fmt.Sprintf("%d.*.key", pidHint)
	if pidHint <= 0 {
		pat = "*.key"
	}
	for _, p := range globOrNil(filepath.Join(dir, pat)) {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var k struct {
			PeerToken string `json:"peerToken"`
		}
		if json.Unmarshal(b, &k) == nil && k.PeerToken != "" {
			return k.PeerToken
		}
	}
	return ""
}

func globOrNil(pat string) []string {
	m, _ := filepath.Glob(pat)
	return m
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// pidDomain is real's dC()/BXo(): "darwin" on macOS, "darwin:<machine-id>:
// <pidns>" on linux, "darwin:<hostname>" on windows (the prefix is a legacy
// name — real literally emits "darwin:..." everywhere).
var pidDomainOnce = sync.OnceValue(func() string {
	switch runtime.GOOS {
	case "windows":
		h, _ := os.Hostname()
		return "darwin:" + strings.ToLower(h)
	case "linux":
		id, _ := os.ReadFile("/etc/machine-id")
		ns, _ := os.Readlink("/proc/self/ns/pid")
		return fmt.Sprintf("darwin:%s:%s", strings.TrimSpace(string(id)), ns)
	default:
		return "darwin"
	}
})

func pidDomain() string { return pidDomainOnce() }

// procStart is real's Tl()/O_(): the process start-token used to detect pid
// reuse. On posix real runs `ps -o lstart= -p <pid>` (LC_ALL=C TZ=UTC) and
// stores the STRING; on windows a kernel32 filetime. We shell out exactly
// like real on posix — no cgo needed.
var procStartOnce = sync.OnceValue(func() any {
	return procStartTokenOf(os.Getpid())
})

func procStart() any { return procStartOnce() }

// procStartTokenOf returns the live start-token for pid, "" if unreadable.
func procStartTokenOf(pid int) string {
	if runtime.GOOS == "windows" {
		return "" // real uses kernel32 GetProcessTimes — not replicated
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-o", "lstart=", "-p",
		fmt.Sprint(pid))
	// real runs ps with LC_ALL=C TZ=UTC — the token is UTC-formatted.
	// Without this the record fails real's pid-reuse vet and peers
	// can't send to us (procStart mismatch → torn record)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ActiveSocketPath returns the bound inbox path ("" if messaging is off).
func ActiveSocketPath() string {
	s, _ := activeSock.Load().(string)
	return s
}

// sessionEntrypoint: real writes "cli" into its session record when
// CLAUDE_CODE_ENTRYPOINT is unset — omitempty dropped it, which may
// mark the record as torn for peers that expect it.
func sessionEntrypoint() string {
	if v := os.Getenv("CLAUDE_CODE_ENTRYPOINT"); v != "" {
		return v
	}
	return "cli"
}
