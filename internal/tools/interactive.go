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
	out, _ := json.Marshal(map[string]any{
		"questions": in.Questions, "answers": answers,
	})
	return Result{Content: string(out)}
}

// ---------- plan mode ----------

func execEnterPlan(c *Context, raw json.RawMessage) Result {
	if c.SetPermMode == nil {
		return Result{Content: "plan mode requires an interactive session", IsError: true}
	}
	c.SetPermMode("plan")
	return Result{Content: "Entered plan mode. You may explore the codebase read-only; present your plan with ExitPlanMode."}
}

func execExitPlan(c *Context, raw json.RawMessage) Result {
	if c.SetPermMode == nil {
		return Result{Content: "plan mode requires an interactive session", IsError: true}
	}
	var in struct {
		AllowedPrompts []string `json:"allowedPrompts"`
	}
	json.Unmarshal(raw, &in)
	if c.PlanApprove != nil && !c.PlanApprove() {
		return Result{Content: "Plan not approved; remain in plan mode and revise.", IsError: true}
	}
	c.SetPermMode("auto")
	return Result{Content: "Plan approved — exiting plan mode."}
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
