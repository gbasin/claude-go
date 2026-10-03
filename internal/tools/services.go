package tools

import (
	"bufio"
	"claude-go/internal/config"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
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

// Real Cron* tools are session-only — "nothing is written to disk, and the
// job is gone when Claude exits". durable is a documented no-op.
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
	ensureCronTicker()
	j := cronJob{
		ID:   fmt.Sprintf("job_%d", time.Now().UnixNano()%1e8),
		Cron: in.Cron, Prompt: in.Prompt,
		Recurring: in.Recurring, Created: time.Now(),
	}
	cronMu.Lock()
	sessJobs = append(sessJobs, j)
	cronMu.Unlock()
	return Result{Content: fmt.Sprintf("Created session job %s (cron %q). Fires as a new turn in this session; gone when Claude exits.", j.ID, j.Cron)}
}

func execCronDelete(c *Context, raw json.RawMessage) Result {
	var in struct {
		ID string `json:"id"`
	}
	json.Unmarshal(raw, &in)
	cronMu.Lock()
	defer cronMu.Unlock()
	out := sessJobs[:0]
	var found bool
	for _, j := range sessJobs {
		if j.ID == in.ID {
			found = true
			continue
		}
		out = append(out, j)
	}
	if !found {
		return Result{Content: "no such job: " + in.ID, IsError: true}
	}
	sessJobs = out
	return Result{Content: "Deleted job " + in.ID}
}

func execCronList(c *Context, raw json.RawMessage) Result {
	cronMu.Lock()
	defer cronMu.Unlock()
	if len(sessJobs) == 0 {
		return Result{Content: "No scheduled jobs."}
	}
	var b strings.Builder
	for _, j := range sessJobs {
		fmt.Fprintf(&b, "%s  cron=%q  recurring=%t  prompt=%q\n",
			j.ID, j.Cron, j.Recurring, j.Prompt)
	}
	return Result{Content: b.String()}
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
}

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
	Kind                string    `json:"kind,omitempty"`       // interactive|bg
	Entrypoint          string    `json:"entrypoint,omitempty"` // cli
	StartedAt           int64     `json:"startedAt,omitempty"`  // unix ms, real field
	UpdatedAt           int64     `json:"updatedAt,omitempty"`
	Version             string    `json:"version,omitempty"`
	PeerProtocol        int       `json:"peerProtocol,omitempty"` // real gates on >=1
	PeerFeatures        []string  `json:"peerFeatures,omitempty"`
	PidDomain           string    `json:"pidDomain,omitempty"`
	ProcStart           int64     `json:"procStart,omitempty"`
	HostSessionID       string    `json:"hostSessionId,omitempty"`
	NameSource          string    `json:"nameSource,omitempty"` // user|derived
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
// can SendMessage to it. Call once at startup (wired elsewhere).
func RegisterSession(sessionID, cwd, name string) error {
	if name == "" {
		name = filepath.Base(cwd)
	}
	os.MkdirAll(registryPath(), 0o755)
	e := sessionEntry{
		SessionID: sessionID, CWD: cwd, Name: name,
		PID: os.Getpid(), TS: time.Now(),
		Kind: "interactive", Entrypoint: "cli",
		StartedAt:           time.Now().UnixMilli(),
		UpdatedAt:           time.Now().UnixMilli(),
		Version:             "2.1.288",
		PeerProtocol:        1, // real peer-protocol version gate
		PeerFeatures:        []string{},
		PidDomain:           pidDomain(),
		ProcStart:           procStart(),
		HostSessionID:       sessionID,
		NameSource:          "derived",
		MessagingSocketPath: socketPath(), // peers can push instantly
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(sessionFile(e.PID), b, 0o644)
}

// UnregisterSession removes this session from the registry (call at exit).
func UnregisterSession(sessionID string) {
	os.Remove(sessionFile(os.Getpid()))
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
// (wired elsewhere).
func FireIdleNotifications(sessionID string) {
	idleSubs.mu.Lock()
	subs := idleSubs.m[sessionID]
	delete(idleSubs.m, sessionID)
	idleSubs.mu.Unlock()
	for _, s := range subs {
		_ = NotifyOnIdle(s.subscriber, s.text)
	}
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
		if in.Message != "" {
			m := mailMsg{From: sender, To: e.SessionID, Kind: "message",
				Message: in.Message, Summary: in.Summary}
			if !socketDeliver(e, m) {
				// peer offline — persist to the mailbox for its next turn
				if err := mailboxAppend(e.SessionID, m); err != nil {
					return Result{Content: "send failed: " + err.Error(), IsError: true}
				}
			}
		}
		note := ""
		if in.NotifyWhenIdle {
			SubscribeIdle(e.SessionID, sender,
				fmt.Sprintf("session %q (%s) went idle.", e.Name, e.SessionID))
			note = " Subscribed — one idle notice will arrive when it next goes idle."
		}
		if in.Message == "" {
			return Result{Content: fmt.Sprintf("Subscribed to %s [%s].%s", e.Name, shortRef(e.SessionID), note)}
		}
		return Result{Content: fmt.Sprintf(
			"Message sent to %s [%s] (local-session) — it drains its inbox at its next tool round.%s",
			e.Name, shortRef(e.SessionID), note)}
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
// Each session listens on $TMPDIR/cc-socks/<pid>.sock and records the path in
// its sessions/<pid>.json entry (real field name: messagingSocketPath).
// mailboxAppend still persists; a live socket delivers instantly.

// Real layout: $XDG_RUNTIME_DIR/cc-socks/<pid>.sock, falling back to
// /tmp/cc-socks-<uid>/<pid>.sock when XDG_RUNTIME_DIR is unset (macOS).
func socketDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "cc-socks")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("cc-socks-%d", os.Getuid()))
}

func socketPath() string {
	return filepath.Join(socketDir(), fmt.Sprint(os.Getpid())+".sock")
}

// StartMessenger binds the real-protocol unix inbox: line-delimited JSON on
// $XDG_RUNTIME_DIR/cc-socks/<pid>.sock (fallback /tmp/cc-socks-<uid>/), an
// optional {"type":"auth","token"} first line, then {"type":"user"} /
// {"type":"control"} frames — the same wire real CC sessions speak, so real
// peers can deliver into us. We publish <pid>.<rand>.key with our peerToken.
func StartMessenger(sessionID string) (cleanup func()) {
	if err := os.MkdirAll(socketDir(), 0o700); err != nil {
		return func() {}
	}
	p := socketPath()
	os.Remove(p)
	ln, err := net.Listen("unix", p)
	if err != nil {
		return func() {}
	}
	keyPath := publishPeerKey(p)
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
		ln.Close()
		os.Remove(p)
		os.Remove(keyPath)
	}
}

var peerToken = func() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}()

// publishPeerKey writes <pid>.<rand>.key next to the socket — real publishes
// {peerToken, pidDomain, ...} mode 0600 so peers can authenticate.
func publishPeerKey(sock string) string {
	dir := filepath.Dir(sock)
	name := fmt.Sprintf("%d.%s.key", os.Getpid(), peerToken[:8])
	p := filepath.Join(dir, name)
	b, _ := json.Marshal(map[string]any{
		"peerToken": peerToken, "pidDomain": pidDomain(),
		"pid": os.Getpid(), "procStart": procStart(),
	})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return ""
	}
	return p
}

// peerFrame is the real wire envelope (line-delimited JSON over the socket).
type peerFrame struct {
	Type      string          `json:"type"`                 // auth|user|control
	Token     string          `json:"token,omitempty"`      // auth
	SessionID string          `json:"session_id,omitempty"` // must match recipient
	From      string          `json:"from,omitempty"`       // sender socket path
	MsgID     string          `json:"msg_id,omitempty"`
	Priority  string          `json:"priority,omitempty"`
	Action    string          `json:"action,omitempty"`  // control: rename|peer_message_status|notify_when_idle|peer_idle_notice
	Name      string          `json:"name,omitempty"`    // rename
	Message   json.RawMessage `json:"message,omitempty"` // user: {role:"user",content:"..."}
}

// servePeer reads line-delimited frames; an auth line (if present) must carry
// our peerToken. Like real, we accept unauthenticated connections on
// single-user platforms but always accept a valid auth line.
func servePeer(c net.Conn, sessionID string) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second)) // firstLineDeadline
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 1<<20), 2<<20) // real caps lines too
	authed := false
	for sc.Scan() {
		var f peerFrame
		if json.Unmarshal(sc.Bytes(), &f) != nil {
			continue // real drops unparseable lines
		}
		if f.Type == "auth" {
			authed = f.Token == peerToken
			continue
		}
		_ = authed // optional auth on this platform; the session-id check below gates delivery
		if f.SessionID != "" && f.SessionID != sessionID {
			continue // real drops session_id mismatches
		}
		switch f.Type {
		case "user":
			var msg struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			}
			if json.Unmarshal(f.Message, &msg) == nil && msg.Content != "" {
				QueuePrompt(msg.Content) // real routes to the prompt queue
			}
		case "control":
			switch f.Action {
			case "rename":
				if f.Name != "" && sessionRename != nil {
					sessionRename(f.Name)
				}
			case "peer_message_status", "notify_when_idle", "peer_idle_notice":
				// status/idle frames — currently informational only
			}
		}
	}
}

// sessionRename is wired by the REPL so a peer "rename" control frame works.
var sessionRename func(string)

// SetSessionRenamer registers the rename callback.
func SetSessionRenamer(fn func(string)) { sessionRename = fn }

// socketDeliver pushes a peerFrame to a live session's socket, using its
// published key file for auth when present. Falls back to no-auth (peers may
// accept). Returns false when the peer has no socket.
func socketDeliver(e sessionEntry, m mailMsg) bool {
	if e.MessagingSocketPath == "" {
		return false
	}
	c, err := net.DialTimeout("unix", e.MessagingSocketPath, 2*time.Second)
	if err != nil {
		return false
	}
	defer c.Close()
	// auth line: peer's key file is <pid>.<rand>.key beside the socket
	if tok := peerTokenFor(e); tok != "" {
		b, _ := json.Marshal(peerFrame{Type: "auth", Token: tok})
		c.Write(append(b, '\n'))
	}
	content := m.Message
	if m.Summary != "" {
		content = m.Summary + ": " + m.Message
	}
	frame := peerFrame{
		Type: "user", SessionID: e.SessionID, From: socketPath(),
		MsgID:   fmt.Sprintf("m%d", time.Now().UnixNano()),
		Message: mustJSON(map[string]any{"role": "user", "content": content}),
	}
	b, _ := json.Marshal(frame)
	_, err = c.Write(append(b, '\n'))
	return err == nil
}

// peerTokenFor reads the peer's <pid>.*.key beside its socket.
func peerTokenFor(e sessionEntry) string {
	dir := filepath.Dir(e.MessagingSocketPath)
	matches, _ := filepath.Glob(filepath.Join(dir, fmt.Sprintf("%d.*.key", e.PID)))
	for _, p := range matches {
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

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// pidDomain distinguishes pid namespaces (real uses sysctl/boot-uuid style
// values; hostname is a reasonable equivalent).
func pidDomain() string {
	h, _ := os.Hostname()
	return h
}

// procStart is the process start time in ms — real uses it to verify a peer's
// pid isn't reused. From the Go runtime we approximate with the binary's own
// process start (can't query cleanly without cgo on darwin).
var procStartAt = time.Now().UnixMilli()

func procStart() int64 { return procStartAt }
