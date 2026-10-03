package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func init() {
	Register(&Tool{Def: loadSchema("Workflow"), Exec: execWorkflow})
}

// execWorkflow runs a workflow script through the local JS runtime (bun, then
// node), matching the Workflow tool's script/scriptPath inputs.
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
	if in.Script == "" && in.ScriptPath == "" {
		return Result{Content: "script or scriptPath is required", IsError: true}
	}
	runtime, args := findJSRuntime()
	if runtime == "" {
		return Result{Content: "Workflow requires a JS runtime (bun or node) which was not found", IsError: true}
	}
	var scriptFile string
	if in.ScriptPath != "" {
		scriptFile = in.ScriptPath
		if !filepath.IsAbs(scriptFile) {
			scriptFile = filepath.Join(c.CWD, scriptFile)
		}
	} else {
		dir := filepath.Join(os.TempDir(), "claude-go-workflows")
		os.MkdirAll(dir, 0o755)
		scriptFile = filepath.Join(dir, fmt.Sprintf("wf-%d.mjs", time.Now().UnixNano()))
		if err := os.WriteFile(scriptFile, []byte(in.Script), 0o644); err != nil {
			return Result{Content: "failed to write script: " + err.Error(), IsError: true}
		}
	}
	argv := append(args, scriptFile)
	cmd := exec.CommandContext(c.Ctx, runtime, argv...)
	cmd.Dir = c.CWD
	if len(in.Args) > 0 {
		cmd.Env = append(os.Environ(), "WORKFLOW_ARGS="+string(in.Args))
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	res := Result{Content: out.String()}
	if err != nil {
		res.IsError = true
		if res.Content == "" {
			res.Content = "workflow failed: " + err.Error()
		}
	}
	return res
}

func findJSRuntime() (string, []string) {
	for _, r := range [][2]string{
		{"bun", "run"},
		{"node", ""},
	} {
		if p, err := exec.LookPath(r[0]); err == nil {
			if r[1] == "" {
				return p, nil
			}
			return p, []string{r[1]}
		}
	}
	return "", nil
}
