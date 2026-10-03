package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func init() {
	Register(&Tool{Def: loadSchema("AskUserQuestion"), Exec: execAskUser})
	Register(&Tool{Def: loadSchema("EnterPlanMode"), Exec: execEnterPlan})
	Register(&Tool{Def: loadSchema("ExitPlanMode"), Exec: execExitPlan})
	Register(&Tool{Def: loadSchema("EndConversation"), Exec: execEndConversation})
	Register(&Tool{Def: loadSchema("SendFeedback"), Exec: execSendFeedback})
	for _, n := range []string{"Artifact", "ArtifactComments", "ArtifactData"} {
		n := n
		Register(&Tool{Def: loadSchema(n), Exec: func(c *Context, raw json.RawMessage) Result {
			return Result{Content: n + " requires the claude.ai artifact service; not available in terminal builds.", IsError: true}
		}})
	}
}

// ---------- AskUserQuestion ----------

type auqIn struct {
	Questions []struct {
		Question    string `json:"question"`
		Header      string `json:"header"`
		MultiSelect bool   `json:"multiSelect"`
		Options     []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
			Preview     string `json:"preview"`
		} `json:"options"`
	} `json:"questions"`
}

func execAskUser(c *Context, raw json.RawMessage) Result {
	var in auqIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	if c.AskStructured == nil {
		return Result{Content: "structured questions unavailable (non-interactive mode)", IsError: true}
	}
	answers := map[string][]string{}
	for _, q := range in.Questions {
		opts := make([]string, 0, len(q.Options))
		for _, o := range q.Options {
			opts = append(opts, o.Label)
		}
		sel, err := c.AskStructured(q.Header, q.Question, opts, q.MultiSelect)
		if err != nil {
			return Result{Content: "question cancelled", IsError: true}
		}
		answers[q.Question] = sel
	}
	// Real tool returns guidance text with distinct outcomes.
	if len(answers) == 0 {
		return Result{Content: "The user did not answer the questions."}
	}
	var b strings.Builder
	for q, sel := range answers {
		fmt.Fprintf(&b, "%s: %s. ", q, strings.Join(sel, ", "))
	}
	return Result{Content: fmt.Sprintf(
		"Your questions have been answered: %s You can now continue with these answers in mind.",
		b.String())}
}

// ---------- plan mode ----------

// planPath is where the model writes its plan during plan mode (real CC
// points it at a scratchpad plan file and ExitPlanMode reads it back).
func planPath(c *Context) string {
	dir := filepath.Join(os.TempDir(), "claude-go", c.Session)
	os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "plan.md")
}

func execEnterPlan(c *Context, raw json.RawMessage) Result {
	if c.SetPermMode == nil {
		return Result{Content: "plan mode requires an interactive session", IsError: true}
	}
	c.SetPermMode("plan")
	return Result{Content: fmt.Sprintf(
		"Entered plan mode. Explore the codebase read-only, then write your plan to %s and present it with ExitPlanMode.",
		planPath(c))}
}

func execExitPlan(c *Context, raw json.RawMessage) Result {
	if c.SetPermMode == nil {
		return Result{Content: "plan mode requires an interactive session", IsError: true}
	}
	var in struct {
		AllowedPrompts []struct {
			Tool   string `json:"tool"`
			Prompt string `json:"prompt"`
		} `json:"allowedPrompts"`
	}
	json.Unmarshal(raw, &in)
	// real reads the plan file the model wrote
	plan, _ := os.ReadFile(planPath(c))
	if c.PlanApprove != nil && !c.PlanApprove() {
		return Result{Content: "Plan not approved; remain in plan mode and revise.", IsError: true}
	}
	c.SetPermMode("default")
	// approved applies become session permission rules
	for _, ap := range in.AllowedPrompts {
		if ap.Tool != "" && ap.Prompt != "" {
			// surfaced to the user as an allowlist suggestion
			fmt.Fprintf(os.Stderr, "  + %s(%s)\n", ap.Tool, ap.Prompt)
		}
	}
	msg := "Plan approved — exited plan mode."
	if len(plan) > 0 {
		msg = fmt.Sprintf("Plan approved — exited plan mode.\n\n<planContent>\n%s\n</planContent>", plan)
	}
	return Result{Content: msg}
}

// ---------- EndConversation ----------

func execEndConversation(c *Context, raw json.RawMessage) Result {
	if c.EndSession != nil {
		c.EndSession()
	}
	return Result{Content: "Conversation ended."}
}

// ---------- SendFeedback ----------

func execSendFeedback(c *Context, raw json.RawMessage) Result {
	var in map[string]any
	json.Unmarshal(raw, &in)
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".claude-go", "feedback")
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, fmt.Sprintf("fb-%d.json", time.Now().Unix()))
	in["cwd"] = c.CWD
	in["ts"] = time.Now().Format(time.RFC3339)
	b, _ := json.MarshalIndent(in, "", "  ")
	os.WriteFile(p, b, 0o644)
	return Result{Content: "Feedback recorded locally at " + p + " (no telemetry is sent by claude-go)."}
}

var _ = strings.TrimSpace
