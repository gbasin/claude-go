package tools

// Durable scheduled tasks — the real tengu_kairos_cron_durable path
// (mod_0320.js gates, mod_0317.js chunk-jj2bb9f2 store, mod_1918.js
// scheduler+lock). Session-only behavior is unchanged when the gate is off.
//
// File layout (real):
//   <cwd>/.claude/scheduled_tasks.json — {"tasks":[<task>,...]}
//   <cwd>/.claude/scheduled_tasks.lock — {sessionId,pid,procStart,acquiredAt}
//
// Task record (real bXe/eBt whitelist):
//   {id, cron, prompt, createdAt(ms), recurring?, permanent?, lastFiredAt?,
//    createdBySessionId?, createdByPid?, createdByProcStart?, createdInProject?}
//
// The lock is advisory: one live session per project dir fires the durable
// tasks. A second session retries every 5s (real Ne=5000) and takes over when
// the holder's pid dies. Missed one-shot durable tasks are surfaced once on
// lock acquisition, then removed — matching real's aWo catch-up path.

import (
	"claude-go/internal/config"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- gates (real UH/ope in mod_0320.js) ----------

// cronEnabled mirrors UH() = !CLAUDE_CODE_DISABLE_CRON && tengu_kairos_cron.
// The statsig half defaults true; locally the env kill-switch is what counts.
func cronEnabled() bool {
	return !envTruthy(os.Getenv("CLAUDE_CODE_DISABLE_CRON"))
}

// cronDurableEnabled mirrors ope() = tengu_kairos_cron_durable. Locally:
// CLAUDE_GO_CRON_DURABLE=1 or a truthy `cronDurable`/`cron.durable` setting.
func cronDurableEnabled() bool {
	if envTruthy(os.Getenv("CLAUDE_GO_CRON_DURABLE")) {
		return true
	}
	cwd, _ := os.Getwd()
	for _, k := range []string{"cronDurable", "cron.durable", "scheduledTasks.enabled"} {
		if v, ok := settingAny(cwd, k); ok {
			if b, isBool := v.(bool); isBool {
				return b
			}
			if s, isStr := v.(string); isStr {
				return envTruthy(s)
			}
		}
	}
	return false
}

// settingAny reads a (possibly dotted) key from project then user settings.
// First hit wins — project overrides user like config.Load.
func settingAny(cwd, key string) (any, bool) {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	for _, p := range []string{
		filepath.Join(cwd, ".claude", "settings.json"),
		filepath.Join(cwd, ".claude", "settings.local.json"),
		filepath.Join(config.ClaudeDir(), "settings.json"),
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		v := any(m)
		for _, k := range strings.Split(key, ".") {
			mm, ok := v.(map[string]any)
			if !ok {
				v = nil
				break
			}
			v = mm[k]
		}
		if v != nil {
			return v, true
		}
	}
	return nil, false
}

// policyGate mirrors sn(name) — real reads managed policy limits
// (allow_remote_sessions / allow_routines). Locally checkable surfaces:
// CLAUDE_GO_<NAME> env, policy-limits.json (managed, user, project), and a
// settings.json key of the same name. Default allow, like real.
func policyGate(cwd, name string) bool {
	env := "CLAUDE_GO_" + strings.ToUpper(regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(name, "_"))
	if v := os.Getenv(env); v != "" {
		return envTruthy(v)
	}
	paths := []string{
		filepath.Join(config.ClaudeDir(), "policy-limits.json"),
		filepath.Join(cwd, ".claude", "policy-limits.json"),
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		if v, ok := m[name]; ok {
			if bb, isBool := v.(bool); isBool {
				return bb
			}
		}
	}
	if v, ok := settingAny(cwd, name); ok {
		if bb, isBool := v.(bool); isBool {
			return bb
		}
	}
	return true
}

// ---------- cron engine (real T/d2/Agt/eA in mod_0317.js) ----------

var cronFieldBounds = [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}

// parseCronField mirrors T(): `*`, `*/n`, `a-b`, `a-b/n`, `a`, comma lists.
// dow accepts 7 as 0. nil on any malformed part.
func parseCronField(f string, lo, hi int) map[int]bool {
	out := map[int]bool{}
	for _, part := range strings.Split(f, ",") {
		if m := regexp.MustCompile(`^\*(?:/(\d+))?$`).FindStringSubmatch(part); m != nil {
			step := 1
			if m[1] != "" {
				step, _ = strconv.Atoi(m[1])
			}
			if step < 1 {
				return nil
			}
			for v := lo; v <= hi; v += step {
				out[v] = true
			}
			continue
		}
		if m := regexp.MustCompile(`^(\d+)-(\d+)(?:/(\d+))?$`).FindStringSubmatch(part); m != nil {
			a, _ := strconv.Atoi(m[1])
			b, _ := strconv.Atoi(m[2])
			step := 1
			if m[3] != "" {
				step, _ = strconv.Atoi(m[3])
			}
			isDow := lo == 0 && hi == 6
			max := hi
			if isDow {
				max = 7
			}
			if a > b || step < 1 || a < lo || b > max {
				return nil
			}
			for v := a; v <= b; v += step {
				if isDow && v == 7 {
					out[0] = true
				} else {
					out[v] = true
				}
			}
			continue
		}
		if regexp.MustCompile(`^\d+$`).MatchString(part) {
			v, _ := strconv.Atoi(part)
			if lo == 0 && hi == 6 && v == 7 {
				v = 0
			}
			if v < lo || v > hi {
				return nil
			}
			out[v] = true
			continue
		}
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// cronSpec is real's d2() parse result.
type cronSpec struct {
	minute, hour, dom, month, dow map[int]bool
}

// parseCron mirrors d2(): 5 whitespace-separated fields, all must parse.
func parseCron(expr string) *cronSpec {
	fs := strings.Fields(expr)
	if len(fs) != 5 {
		return nil
	}
	s := &cronSpec{}
	sets := make([]map[int]bool, 5)
	for i, f := range fs {
		sets[i] = parseCronField(f, cronFieldBounds[i][0], cronFieldBounds[i][1])
		if sets[i] == nil {
			return nil
		}
	}
	s.minute, s.hour, s.dom, s.month, s.dow = sets[0], sets[1], sets[2], sets[3], sets[4]
	return s
}

// nextCronTime mirrors Agt(): first match strictly after `after`, scanning up
// to 366 days (527040 minutes). dom/dow use real's OR-semantics: both
// restricted → either matches; one restricted → it must match.
func nextCronTime(s *cronSpec, after time.Time) *time.Time {
	if s == nil {
		return nil
	}
	domFree := len(s.dom) == 31
	dowFree := len(s.dow) == 7
	u := after.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 527040; i++ {
		if !s.month[int(u.Month())] {
			u = time.Date(u.Year(), u.Month()+1, 1, 0, 0, 0, 0, u.Location())
			continue
		}
		dayOK := true
		switch {
		case domFree && dowFree:
		case domFree:
			dayOK = s.dow[int(u.Weekday())]
		case dowFree:
			dayOK = s.dom[u.Day()]
		default:
			dayOK = s.dom[u.Day()] || s.dow[int(u.Weekday())]
		}
		if !dayOK {
			u = time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, u.Location())
			continue
		}
		if !s.hour[u.Hour()] {
			u = u.Add(time.Hour).Truncate(time.Hour)
			continue
		}
		if !s.minute[u.Minute()] {
			u = u.Add(time.Minute)
			continue
		}
		return &u
	}
	return nil
}

// humanSchedule mirrors eA(): the human-readable cadence shown in results.
var cronWeekdays = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}

func cronLocalTime(min, hour int) string {
	return time.Date(2000, 1, 1, hour, min, 0, 0, time.Local).Format("3:04 PM")
}

func humanSchedule(expr string) string {
	e := strings.Fields(expr)
	if len(e) != 5 {
		return expr
	}
	minS, hourS, dom, mon, dow := e[0], e[1], e[2], e[3], e[4]
	isNum := func(s string) bool { return regexp.MustCompile(`^\d+$`).MatchString(s) }
	if hourS == "*" && dom == "*" && mon == "*" && dow == "*" {
		if minS == "*" {
			return "Every minute"
		}
		if m := regexp.MustCompile(`^\*/(\d+)$`).FindStringSubmatch(minS); m != nil {
			n, _ := strconv.Atoi(m[1])
			if n == 1 {
				return "Every minute"
			}
			return fmt.Sprintf("Every %d minutes", n)
		}
	}
	if isNum(minS) && hourS == "*" && dom == "*" && mon == "*" && dow == "*" {
		m, _ := strconv.Atoi(minS)
		if m == 0 {
			return "Every hour"
		}
		return fmt.Sprintf("Every hour at :%02d", m)
	}
	if m := regexp.MustCompile(`^\*/(\d+)$`).FindStringSubmatch(hourS); isNum(minS) && m != nil && dom == "*" && mon == "*" && dow == "*" {
		l, _ := strconv.Atoi(m[1])
		mn, _ := strconv.Atoi(minS)
		h := ""
		if mn != 0 {
			h = fmt.Sprintf(" at :%02d", mn)
		}
		if l == 1 {
			return "Every hour" + h
		}
		return fmt.Sprintf("Every %d hours%s", l, h)
	}
	if !isNum(minS) || !isNum(hourS) {
		return expr
	}
	mn, _ := strconv.Atoi(minS)
	hr, _ := strconv.Atoi(hourS)
	if dom == "*" && mon == "*" && dow == "*" {
		return fmt.Sprintf("Every day at %s", cronLocalTime(mn, hr))
	}
	if dom == "*" && mon == "*" && regexp.MustCompile(`^\d$`).MatchString(dow) {
		d, _ := strconv.Atoi(dow)
		return fmt.Sprintf("Every %s at %s", cronWeekdays[d%7], cronLocalTime(mn, hr))
	}
	if dom == "*" && mon == "*" && dow == "1-5" {
		return fmt.Sprintf("Weekdays at %s", cronLocalTime(mn, hr))
	}
	return expr
}

// ---------- durable task store (real eBt/jhn/bXe/spe/iWo/wQ) ----------

// scheduledTask is the real persisted record shape (mod_0317.js eBt/jhn).
type scheduledTask struct {
	ID                 string `json:"id"`
	Cron               string `json:"cron"`
	Prompt             string `json:"prompt"`
	CreatedAt          int64  `json:"createdAt"` // ms epoch
	LastFiredAt        int64  `json:"lastFiredAt,omitempty"`
	Recurring          bool   `json:"recurring,omitempty"`
	Permanent          bool   `json:"permanent,omitempty"`
	CreatedBySessionID string `json:"createdBySessionId,omitempty"`
	CreatedByPid       int    `json:"createdByPid,omitempty"`
	CreatedByProcStart string `json:"createdByProcStart,omitempty"`
	CreatedInProject   string `json:"createdInProject,omitempty"`
	AgentID            string `json:"agentId,omitempty"`
	Durable            bool   `json:"-"` // merge marker only — real strips it on write
}

// schedTasksPath mirrors sbe(): <dir>/.claude/scheduled_tasks.json.
func schedTasksPath(dir string) string {
	return filepath.Join(dir, ".claude", "scheduled_tasks.json")
}

func schedLockPath(dir string) string {
	return filepath.Join(dir, ".claude", "scheduled_tasks.lock")
}

// loadScheduledTasks mirrors eBt(): read + whitelist-normalize each record;
// malformed rows and bad cron exprs are skipped.
func loadScheduledTasks(dir string) []scheduledTask {
	b, err := os.ReadFile(schedTasksPath(dir))
	if err != nil {
		return nil
	}
	var env struct {
		Tasks []scheduledTask `json:"tasks"`
	}
	if json.Unmarshal(b, &env) != nil {
		return nil
	}
	var out []scheduledTask
	for _, t := range env.Tasks {
		if t.ID == "" || t.Cron == "" || t.Prompt == "" || t.CreatedAt == 0 {
			continue
		}
		if parseCron(t.Cron) == nil {
			continue
		}
		out = append(out, t)
	}
	return out
}

// saveScheduledTasks mirrors jhn(): {"tasks":[...]} at 2-space indent; the
// in-memory-only `durable` marker is stripped (it's a non-serialized field).
func saveScheduledTasks(dir string, tasks []scheduledTask) error {
	if tasks == nil {
		tasks = []scheduledTask{}
	}
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(struct {
		Tasks []scheduledTask `json:"tasks"`
	}{tasks}, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := schedTasksPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, schedTasksPath(dir))
}

// addScheduledTask mirrors bXe() durable path.
func addScheduledTask(dir, cronExpr, prompt string, recurring bool, sessionID string) (string, error) {
	tasks := loadScheduledTasks(dir)
	t := scheduledTask{
		ID:                 newTaskID(),
		Cron:               cronExpr,
		Prompt:             prompt,
		CreatedAt:          time.Now().UnixMilli(),
		Recurring:          recurring,
		CreatedBySessionID: sessionID,
		CreatedByPid:       os.Getpid(),
		CreatedInProject:   dir,
	}
	if s, ok := procStart().(string); ok {
		t.CreatedByProcStart = s
	}
	tasks = append(tasks, t)
	if err := saveScheduledTasks(dir, tasks); err != nil {
		return "", err
	}
	return t.ID, nil
}

// removeScheduledTasks mirrors spe().
func removeScheduledTasks(dir string, ids []string) bool {
	if len(ids) == 0 {
		return false
	}
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	tasks := loadScheduledTasks(dir)
	keep := tasks[:0]
	for _, t := range tasks {
		if !set[t.ID] {
			keep = append(keep, t)
		}
	}
	if len(keep) == len(tasks) {
		return false
	}
	return saveScheduledTasks(dir, keep) == nil
}

// touchLastFired mirrors iWo().
func touchLastFired(dir string, ids []string, at int64) {
	if len(ids) == 0 {
		return
	}
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	tasks := loadScheduledTasks(dir)
	changed := false
	for i := range tasks {
		if set[tasks[i].ID] {
			tasks[i].LastFiredAt = at
			changed = true
		}
	}
	if changed {
		saveScheduledTasks(dir, tasks)
	}
}

// newTaskID mirrors bXe's randomUUID().slice(0,8) — 8 hex chars.
func newTaskID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano()%0xffffffff)
	}
	return fmt.Sprintf("%x", b[:])
}

// ---------- scheduler lock (real oe/U in mod_1918.js) ----------

type schedLockRec struct {
	SessionID  string `json:"sessionId"`
	Pid        int    `json:"pid"`
	ProcStart  string `json:"procStart,omitempty"`
	AcquiredAt int64  `json:"acquiredAt"`
}

func readSchedLock(dir string) *schedLockRec {
	b, err := os.ReadFile(schedLockPath(dir))
	if err != nil {
		return nil
	}
	var r schedLockRec
	if json.Unmarshal(b, &r) != nil || r.SessionID == "" {
		return nil
	}
	return &r
}

// writeSchedLock creates the lock with O_EXCL — false when it already exists.
func writeSchedLock(dir string, rec schedLockRec) bool {
	p := schedLockPath(dir)
	b, _ := json.Marshal(rec)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			os.MkdirAll(filepath.Dir(p), 0o755)
			if f, err = os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); err == nil {
				defer f.Close()
				f.Write(b)
				return true
			}
		}
		return false
	}
	defer f.Close()
	f.Write(b)
	return true
}

// acquireSchedLock mirrors oe(): create-or-inspect. Same-sessionId reclaim,
// live-holder block, stale-holder recovery.
func acquireSchedLock(dir, sessionID string) bool {
	ps, _ := procStart().(string)
	rec := schedLockRec{SessionID: sessionID, Pid: os.Getpid(),
		ProcStart: ps, AcquiredAt: time.Now().UnixMilli()}
	if writeSchedLock(dir, rec) {
		excludeRuntimeFiles(dir)
		return true
	}
	cur := readSchedLock(dir)
	if cur == nil {
		// unreadable/corrupt — treat as stale and retry once
		os.Remove(schedLockPath(dir))
		if writeSchedLock(dir, rec) {
			excludeRuntimeFiles(dir)
			return true
		}
		return false
	}
	if cur.SessionID == sessionID {
		if cur.Pid != os.Getpid() {
			os.WriteFile(schedLockPath(dir), mustJSON(rec), 0o644)
		}
		return true
	}
	if pidAlive(cur.Pid) && (cur.ProcStart == "" || procStartMatches(cur.Pid, cur.ProcStart)) {
		return false // live holder
	}
	os.Remove(schedLockPath(dir)) // stale
	if writeSchedLock(dir, rec) {
		excludeRuntimeFiles(dir)
		return true
	}
	return false
}

// releaseSchedLock mirrors U(): only the owning session removes the file.
func releaseSchedLock(dir, sessionID string) {
	cur := readSchedLock(dir)
	if cur == nil || cur.SessionID != sessionID {
		return
	}
	os.Remove(schedLockPath(dir))
}

// excludeRuntimeFiles mirrors ge(): adds the runtime-file globs to
// .git/info/exclude so lock/tasks files never show in git status.
func excludeRuntimeFiles(dir string) {
	root, err := gitRoot(dir)
	if err != nil || root == "" {
		return
	}
	gd, err := gitDir(root)
	if err != nil {
		return
	}
	ex := filepath.Join(gd, "info", "exclude")
	b, _ := os.ReadFile(ex)
	if strings.Contains(string(b), "# claude-code-runtime") {
		return
	}
	os.MkdirAll(filepath.Dir(ex), 0o755)
	f, err := os.OpenFile(ex, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if len(b) > 0 && b[len(b)-1] != '\n' {
		f.WriteString("\n")
	}
	f.WriteString("# claude-code-runtime\n" +
		"**/.claude/scheduled_tasks.lock\n" +
		"**/.claude/scheduled_tasks.json\n")
}

func gitDir(root string) (string, error) {
	out, err := exec.Command("git", "-C", root, "rev-parse", "--git-dir").Output()
	if err != nil {
		return "", err
	}
	p := strings.TrimSpace(string(out))
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	return p, nil
}

// ---------- durable scheduler (real Jdr subset) ----------

var sched = struct {
	mu        sync.Mutex
	dir       string
	sessionID string
	held      bool
	started   bool
	stop      chan struct{}
}{}

const (
	schedLockRetryMs     = 5000      // real Ne — lock retry interval
	schedRecurringMaxAge = 604800000 // real D3.recurringMaxAgeMs = 7d
)

// StartScheduledTasks mirrors the scheduler's start() path (mod_1918.js re):
// when the durable gate is on, acquire the project lock, surface missed
// one-shots, then arm the shared cron tick (which also drives session jobs).
// A blocked session retries the lock every 5s like real.
func StartScheduledTasks(dir, sessionID string) {
	if !cronDurableEnabled() || dir == "" {
		return
	}
	sched.mu.Lock()
	if sched.started {
		sched.mu.Unlock()
		return
	}
	sched.dir = dir
	sched.sessionID = sessionID
	sched.started = true
	sched.stop = make(chan struct{})
	sched.mu.Unlock()

	ensureCronTicker()
	go schedLockLoop()
}

// StopScheduledTasks mirrors stop(): release the lock if we hold it.
func StopScheduledTasks() {
	sched.mu.Lock()
	defer sched.mu.Unlock()
	if !sched.started {
		return
	}
	if sched.stop != nil {
		close(sched.stop)
		sched.stop = nil
	}
	if sched.held {
		sched.held = false
		releaseSchedLock(sched.dir, sched.sessionID)
	}
	sched.started = false
	sched.dir, sched.sessionID = "", ""
}

func schedLockLoop() {
	schedTryAcquire()
	for {
		sched.mu.Lock()
		held, stop := sched.held, sched.stop
		sched.mu.Unlock()
		if held || stop == nil {
			return
		}
		select {
		case <-stop:
			return
		case <-time.After(schedLockRetryMs * time.Millisecond):
			schedTryAcquire()
		}
	}
}

func schedTryAcquire() {
	sched.mu.Lock()
	dir, sid := sched.dir, sched.sessionID
	sched.mu.Unlock()
	if dir == "" {
		return
	}
	if acquireSchedLock(dir, sid) {
		sched.mu.Lock()
		first := !sched.held
		sched.held = true
		sched.mu.Unlock()
		if first {
			surfaceMissedOneShots(dir)
		}
	}
}

// schedLockHeld reports whether this session currently fires durable tasks.
func schedLockHeld() bool {
	sched.mu.Lock()
	defer sched.mu.Unlock()
	return sched.held
}

// schedDir returns the durable project dir ("" when the scheduler is off).
func schedDir() string {
	sched.mu.Lock()
	defer sched.mu.Unlock()
	return sched.dir
}

// surfaceMissedOneShots mirrors the aWo catch-up inside Z(): one-shot durable
// tasks whose first fire already passed get surfaced (not executed), then
// removed from the file. Only fires for tasks owned by this project.
func surfaceMissedOneShots(dir string) {
	now := time.Now().UnixMilli()
	tasks := loadScheduledTasks(dir)
	var missed, owned []scheduledTask
	for _, t := range tasks {
		if t.Recurring {
			continue
		}
		if !taskOwnedHere(t, dir) {
			continue
		}
		owned = append(owned, t)
		spec := parseCron(t.Cron)
		nf := nextCronTime(spec, time.UnixMilli(t.CreatedAt))
		if nf != nil && nf.UnixMilli() < now {
			missed = append(missed, t)
		}
	}
	if len(missed) == 0 {
		return
	}
	ids := make([]string, 0, len(missed))
	for _, t := range missed {
		ids = append(ids, t.ID)
	}
	removeScheduledTasks(dir, ids)
	// real's qe() catch-up prompt — the model must ASK before running these.
	plural := len(missed) > 1
	var sb strings.Builder
	if plural {
		sb.WriteString("The following one-shot scheduled tasks were missed while Claude was not running. They have already been removed from .claude/scheduled_tasks.json.\n\nDo NOT execute these prompts yet. First use the AskUserQuestion tool to ask whether to run each one now. Only execute if the user confirms.")
	} else {
		sb.WriteString("The following one-shot scheduled task was missed while Claude was not running. It has already been removed from .claude/scheduled_tasks.json.\n\nDo NOT execute this prompt yet. First use the AskUserQuestion tool to ask whether to run it now. Only execute if the user confirms.")
	}
	for _, t := range missed {
		fmt.Fprintf(&sb, "\n\n[%s, created %s]\n%s",
			humanSchedule(t.Cron),
			time.UnixMilli(t.CreatedAt).Format("1/2/2006, 3:04:05 PM"),
			t.Prompt)
	}
	queuePrompt(sb.String())
}

// taskOwnedHere mirrors fe(): task belongs to this project/session context.
// No createdInProject → treated as ours only while we hold the lock (real x).
func taskOwnedHere(t scheduledTask, dir string) bool {
	if t.CreatedBySessionID == "" && t.CreatedInProject == "" {
		return true // foreign/minimal records fire under the lock holder
	}
	if t.CreatedBySessionID == sched.sessionID {
		return true
	}
	if t.CreatedInProject != "" {
		return filepath.Clean(t.CreatedInProject) == filepath.Clean(dir)
	}
	return true
}

// cronTickDurable is the durable half of the shared tick — runs only while
// this session holds the scheduler lock.
func cronTickDurable() {
	dir := schedDir()
	if dir == "" || !schedLockHeld() {
		return
	}
	now := time.Now()
	nowMs := now.UnixMilli()
	tasks := loadScheduledTasks(dir)
	var firedRecurring, firedOnce []string
	for _, t := range tasks {
		if !taskOwnedHere(t, dir) {
			continue
		}
		spec := parseCron(t.Cron)
		base := t.CreatedAt
		if t.LastFiredAt > base {
			base = t.LastFiredAt
		}
		nf := nextCronTime(spec, time.UnixMilli(base))
		if nf == nil || nf.UnixMilli() > nowMs {
			continue
		}
		// due — fire once
		queuePrompt(t.Prompt)
		agedOut := t.Recurring && !t.Permanent && nowMs-t.CreatedAt >= schedRecurringMaxAge
		if t.Recurring && !agedOut {
			firedRecurring = append(firedRecurring, t.ID)
		} else {
			// one-shot or final-fire age-out → remove (real spe/xV)
			firedOnce = append(firedOnce, t.ID)
		}
	}
	if len(firedRecurring) > 0 {
		touchLastFired(dir, firedRecurring, nowMs)
	}
	if len(firedOnce) > 0 {
		removeScheduledTasks(dir, firedOnce)
	}
}
