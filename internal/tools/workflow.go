package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func init() {
	Register(&Tool{Def: loadSchema("Workflow"), Exec: execWorkflow})
}

// The real Workflow contract: a script beginning with
//   export const meta = {name, description, phases}
// whose body uses agent()/parallel()/pipeline()/phase() against an `args`
// JSON global. Runs in background; results journal to journal.jsonl;
// resumeFromRunId replays unchanged agent() calls. Determinism guards:
// Math.random/Date.now throw.

const wfPrelude = `
// ---- claude-go workflow runtime prelude ----
const args = JSON.parse(process.env.WORKFLOW_ARGS ?? "null");
const runId = process.env.WORKFLOW_RUN_ID;
const journalPath = process.env.WORKFLOW_JOURNAL;
const sessionDir = process.env.WORKFLOW_SESSION_DIR;
const agentBin = process.env.WORKFLOW_AGENT_BIN;
const __cache = new Map();
import { appendFileSync, readFileSync } from "fs";

function __journal(rec) {
  try { appendFileSync(journalPath, JSON.stringify(rec) + "\n"); } catch {}
}
try {
  for (const line of readFileSync(journalPath, "utf8").split("\n")) {
    if (!line.trim()) continue;
    const rec = JSON.parse(line);
    if (rec.kind === "agent" && rec.done) __cache.set(rec.key, rec.result);
  }
} catch {}

// determinism guards (real runtime stubs these out too)
Math.random = () => { throw new Error("Math.random() is not allowed in workflows — non-deterministic"); };
Date.now = () => { throw new Error("Date.now() is not allowed in workflows — non-deterministic"); };

async function agent(prompt, opts = {}) {
  const key = JSON.stringify([prompt, opts]);
  if (__cache.has(key)) { __journal({kind:"agent",key,replayed:true}); return __cache.get(key); }
  const a = [agentBin, "-p", "--permission-mode", "auto", "--output-format", "json", prompt];
  if (opts.model) a.splice(1, 0, "--model", opts.model);
  const proc = Bun.spawnSync(a, { stdout: "pipe", stderr: "pipe" });
  const out = proc.stdout.toString();
  let result = out;
  try { result = JSON.parse(out).result ?? out; } catch {}
  __journal({kind:"agent",key,done:true,result});
  return result;
}
const parallel = (tasks) => Promise.all(tasks);
async function pipeline(items, fn) {
  const out = [];
  for (const it of items) out.push(await fn(it));
  return out;
}
async function phase(name, fn) {
  __journal({kind:"phase",name,begin:true});
  const r = await fn();
  __journal({kind:"phase",name,done:true});
  return r;
}
// ---- end prelude ----
`

var wfMetaRe = regexp.MustCompile(`export\s+const\s+meta\s*=`)

func execWorkflow(c *Context, raw json.RawMessage) Result {
	var in struct {
		Script          string          `json:"script"`
		ScriptPath      string          `json:"scriptPath"`
		Name            string          `json:"name"`
		Description     string          `json:"description"`
		Title           string          `json:"title"`
		Args            json.RawMessage `json:"args"`
		ResumeFromRunID string          `json:"resumeFromRunId"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	runtime, args := findJSRuntime()
	if runtime == "" {
		return Result{Content: "Workflow requires a JS runtime (bun or node) which was not found", IsError: true}
	}
	if !strings.Contains(runtime, "bun") {
		return Result{Content: "Workflow requires bun (agent() uses Bun.spawnSync)", IsError: true}
	}

	var script, scriptPath string
	switch {
	case in.ScriptPath != "":
		scriptPath = in.ScriptPath
		if !filepath.IsAbs(scriptPath) {
			scriptPath = filepath.Join(c.CWD, scriptPath)
		}
		b, err := os.ReadFile(scriptPath)
		if err != nil {
			return Result{Content: "cannot read scriptPath: " + err.Error(), IsError: true}
		}
		script = string(b)
	case in.Name != "":
		// named workflows resolve from .claude/workflows/
		p := filepath.Join(c.CWD, ".claude", "workflows", in.Name+".js")
		if b, err := os.ReadFile(p); err == nil {
			script, scriptPath = string(b), p
		}
		if script == "" {
			home, _ := os.UserHomeDir()
			p = filepath.Join(home, ".claude", "workflows", in.Name+".js")
			if b, err := os.ReadFile(p); err == nil {
				script, scriptPath = string(b), p
			}
		}
		if script == "" {
			return Result{Content: "workflow not found: " + in.Name, IsError: true}
		}
	case in.Script != "":
		script = in.Script
	default:
		return Result{Content: "script, scriptPath, or name is required", IsError: true}
	}
	if !wfMetaRe.MatchString(script) {
		return Result{Content: "workflow script must begin with `export const meta = {…}`", IsError: true}
	}

	runID := "wf_" + randID(8)
	if in.ResumeFromRunID != "" {
		if !regexp.MustCompile(`^wf_[a-z0-9-]{6,}$`).MatchString(in.ResumeFromRunID) {
			return Result{Content: "invalid resumeFromRunId", IsError: true}
		}
		runID = in.ResumeFromRunID
	}
	dir := filepath.Join(workflowDir(c), runID)
	os.MkdirAll(dir, 0o755)
	journal := filepath.Join(dir, "journal.jsonl")

	full := wfPrelude + "\n" + script
	scriptFile := filepath.Join(dir, "run.mjs")
	if err := os.WriteFile(scriptFile, []byte(full), 0o644); err != nil {
		return Result{Content: "failed to write script: " + err.Error(), IsError: true}
	}

	// background task via the BgTasks registry
	id := fmt.Sprintf("wf%d", time.Now().UnixNano()%1e6)
	exe, _ := os.Executable()
	env := append(os.Environ(),
		"WORKFLOW_ARGS="+string(in.Args),
		"WORKFLOW_RUN_ID="+runID,
		"WORKFLOW_JOURNAL="+journal,
		"WORKFLOW_SESSION_DIR="+dir,
		"WORKFLOW_AGENT_BIN="+exe,
	)
	cmd := exec.CommandContext(c.Ctx, runtime, append(args, scriptFile)...)
	cmd.Dir = c.CWD
	cmd.Env = env
	var outBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf

	t := &BgTask{ID: id, Command: "workflow " + runID, Started: time.Now(), Done: make(chan struct{})}
	if c.BgTasks == nil {
		c.BgTasks = NewBackground()
	}
	c.BgTasks.mu.Lock()
	c.BgTasks.tasks[id] = t
	c.BgTasks.mu.Unlock()
	go func() {
		t.Err = cmd.Run()
		if s, ok := cmd.ProcessState.Sys().(interface{ ExitCode() int }); ok {
			t.ExitCode = s.ExitCode()
		}
		t.Out.WriteString(outBuf.String())
		close(t.Done)
		if cb := c.BgTasks.OnExit; cb != nil {
			cb(t)
		}
	}()
	return Result{Content: fmt.Sprintf(
		"Workflow launched in background (task %s, run %s).\nJournal: %s\nScript: %s\nResume with resumeFromRunId=%q to replay unchanged agent calls.",
		id, runID, journal, scriptFile, runID)}
}

func workflowDir(c *Context) string {
	base := filepath.Join(c.CWD, ".claude", "workflows", "runs")
	if c.Session != "" {
		base = filepath.Join(os.TempDir(), "claude-go-workflows", c.Session)
	}
	os.MkdirAll(base, 0o755)
	return base
}

func randID(n int) string {
	const alnum = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	f, _ := os.Open("/dev/urandom")
	f.Read(b)
	f.Close()
	for i := range b {
		b[i] = alnum[int(b[i])%len(alnum)]
	}
	return string(b)
}

func findJSRuntime() (string, []string) {
	for _, c := range [][2]string{
		{"bun", ""}, {"node", ""},
	} {
		if p, err := exec.LookPath(c[0]); err == nil {
			return p, nil
		}
	}
	return "", nil
}
