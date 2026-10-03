package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func init() {
	Register(&Tool{Def: loadSchema("Agent"), Exec: execAgent})
	Register(&Tool{Def: loadSchema("Skill"), Exec: execSkill})
	Register(&Tool{Def: loadSchema("EnterWorktree"), Exec: execEnterWorktree})
	Register(&Tool{Def: loadSchema("ExitWorktree"), Exec: execExitWorktree})
	Register(&Tool{Def: loadSchema("Monitor"), Exec: execMonitor})

	// Stubs: return a faithful "unavailable in this environment" result
	// rather than failing, matching how CC degrades without the service.
	for _, n := range []string{
		"CronCreate", "CronDelete", "CronList", "DesignSync", "LSP",
		"ListAgents", "ListMcpResourcesTool", "PushNotification",
		"ReadMcpResourceDirTool", "ReadMcpResourceTool", "RemoteTrigger",
		"ReportFindings", "ScheduleWakeup", "SendMessage", "Workflow",
	} {
		n := n
		Register(&Tool{Def: loadSchema(n), Exec: func(c *Context, raw json.RawMessage) Result {
			return Result{Content: fmt.Sprintf("%s is not available in this build of claude-go.", n), IsError: true}
		}})
	}
}

// ---------- Agent ----------

type agentIn struct {
	Description     string `json:"description"`
	Prompt          string `json:"prompt"`
	SubagentType    string `json:"subagent_type"`
	Model           string `json:"model"`
	RunInBackground bool   `json:"run_in_background"`
	Isolation       string `json:"isolation"`
}

func execAgent(c *Context, raw json.RawMessage) Result {
	var in agentIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	if c.Agent == nil {
		return Result{Content: "sub-agents are not available", IsError: true}
	}
	st := in.SubagentType
	if st == "" {
		st = "general-purpose"
	}
	return c.Agent.RunAgent(c, in.Description, in.Prompt, st, in.Model, in.RunInBackground)
}

// ---------- Skill ----------

type skillIn struct {
	Skill string `json:"skill"`
	Args  string `json:"args"`
}

func execSkill(c *Context, raw json.RawMessage) Result {
	var in skillIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	home, _ := os.UserHomeDir()
	for _, base := range []string{
		filepath.Join(home, ".claude", "skills"),
		filepath.Join(c.CWD, ".claude", "skills"),
		filepath.Join(c.CWD, ".agents", "skills"),
	} {
		p := filepath.Join(base, in.Skill, "SKILL.md")
		b, err := os.ReadFile(p)
		if err == nil {
			args := in.Args
			if args == "" {
				args = "(none)"
			}
			return Result{Content: fmt.Sprintf("Skill %q loaded from %s\nUser args: %s\n\n%s", in.Skill, p, args, string(b))}
		}
	}
	return Result{Content: "Unknown skill: " + in.Skill, IsError: true}
}

// ---------- worktrees ----------

func gitRoot(cwd string) (string, error) {
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func execEnterWorktree(c *Context, raw json.RawMessage) Result {
	var in struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	json.Unmarshal(raw, &in)
	root, err := gitRoot(c.CWD)
	if err != nil {
		return Result{Content: "not inside a git worktree", IsError: true}
	}
	name := in.Name
	if name == "" {
		name = "claude-" + fmt.Sprint(time.Now().Unix()%100000)
	}
	wtPath := in.Path
	if wtPath == "" {
		wtPath = filepath.Join(filepath.Dir(root), ".claude-worktrees", name)
	}
	branch := "claude/" + name
	cmd := exec.Command("git", "-C", root, "worktree", "add", "-b", branch, wtPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return Result{Content: "git worktree add failed: " + string(out), IsError: true}
	}
	c.CWD = wtPath
	return Result{Content: fmt.Sprintf("Entered worktree at %s (branch %s)", wtPath, branch)}
}

func execExitWorktree(c *Context, raw json.RawMessage) Result {
	var in struct {
		Action         string `json:"action"` // keep|remove
		DiscardChanges bool   `json:"discard_changes"`
	}
	json.Unmarshal(raw, &in)
	root, err := gitRoot(c.CWD)
	if err != nil {
		return Result{Content: "not inside a git worktree", IsError: true}
	}
	if in.Action == "remove" {
		args := []string{"-C", filepath.Dir(root), "worktree", "remove"}
		if in.DiscardChanges {
			args = append(args, "--force")
		}
		args = append(args, root)
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			return Result{Content: "git worktree remove failed: " + string(out), IsError: true}
		}
		c.CWD = filepath.Dir(root)
		return Result{Content: "Worktree removed"}
	}
	return Result{Content: "Stayed in worktree " + root}
}

// ---------- Monitor ----------

type monitorIn struct {
	Description string `json:"description"`
	TimeoutMs   int    `json:"timeout_ms"`
	Command     string `json:"command"`
}

func execMonitor(c *Context, raw json.RawMessage) Result {
	var in monitorIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	if in.Command == "" {
		return Result{Content: "command is required", IsError: true}
	}
	// Monitor = background command whose exit re-invokes the agent.
	in2, _ := json.Marshal(bashInput{
		Command:         in.Command,
		RunInBackground: true,
		Timeout:         float64(orZero(in.TimeoutMs, bgDefaultTimeout)),
	})
	return execBash(c, in2)
}

func orZero(v, d int) int {
	if v <= 0 {
		return d
	}
	return v
}
