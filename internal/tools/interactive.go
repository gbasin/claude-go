package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func init() {
	Register(&Tool{Def: loadSchema("AskUserQuestion"), Exec: execAskUser})
	Register(&Tool{Def: loadSchema("EnterPlanMode"), Exec: execEnterPlan})
	Register(&Tool{Def: loadSchema("ExitPlanMode"), Exec: execExitPlan})
	Register(&Tool{Def: loadSchema("EndConversation"), Exec: execEndConversation})
	Register(&Tool{Def: loadSchema("SendFeedback"), Exec: execSendFeedback})
}

// ---------- AskUserQuestion (mod_0552.js, mod_0292.js) ----------
//
// The real tool does not ask the questions itself: it validates the
// questions, the host UI collects `answers`/`annotations`/`response`/
// `followUp`, and mapToolResultToToolResultBlockParam renders one of four
// outcomes. In claude-go's terminal build the asking happens inline via
// Context.AskStructured, then the same result strings are produced.

const auqNotesOnly = "(notes only)" // EOt in the extracted module

type auqOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Preview     string `json:"preview"`
}

type auqQuestion struct {
	Question     string      `json:"question"`
	Header       string      `json:"header"`
	Kind         string      `json:"kind"` // omitted/"choice" | "text" | "number"
	MultiSelect  bool        `json:"multiSelect"`
	Options      []auqOption `json:"options"`
	Description  string      `json:"description"`
	Placeholder  string      `json:"placeholder"`
	Min          *float64    `json:"min"`
	Max          *float64    `json:"max"`
	Step         *float64    `json:"step"`
	DefaultValue *float64    `json:"defaultValue"`
	Unit         string      `json:"unit"`
}

type auqAnnotation struct {
	Preview string `json:"preview"`
	Notes   string `json:"notes"`
}

type auqIn struct {
	Title        string                     `json:"title"`
	Questions    []auqQuestion              `json:"questions"`
	Answers      map[string]json.RawMessage `json:"answers"`
	Annotations  map[string]auqAnnotation   `json:"annotations"`
	Response     string                     `json:"response"`
	FollowUp     bool                       `json:"followUp"`
	AfkTimeoutMs int64                      `json:"afkTimeoutMs"`
	Metadata     map[string]any             `json:"metadata"`
}

// auqAnswer decodes one answers[] entry: a string or a list of strings
// (multiSelect). Returns the raw value as string or []string.
func auqAnswer(raw json.RawMessage) any {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var l []string
	if json.Unmarshal(raw, &l) == nil {
		return l
	}
	return nil
}

// auqAnswerString renders an answer the way the real formatter does:
// single answers verbatim, lists joined ", " with JSON quoting of any
// entry containing ", " or a quote (glt()).
func auqAnswerString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []string:
		parts := make([]string, 0, len(t))
		for _, s := range t {
			if strings.Contains(s, ", ") || strings.Contains(s, `"`) {
				b, _ := json.Marshal(s)
				parts = append(parts, string(b))
			} else {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	}
	return ""
}

// auqParseList is L() from mod_0552: parses "a, b, \"c, d\"" back into
// entries; returns nil when the string is not that exact shape.
func auqParseList(s string) []string {
	var out []string
	for {
		if strings.HasPrefix(s, `"`) {
			end := -1
			esc := false
			for i := 1; i < len(s); i++ {
				if esc {
					esc = false
					continue
				}
				if s[i] == '\\' {
					esc = true
					continue
				}
				if s[i] == '"' {
					end = i
					break
				}
			}
			if end == -1 {
				return nil
			}
			var v string
			if json.Unmarshal([]byte(s[:end+1]), &v) != nil {
				return nil
			}
			out = append(out, v)
			s = s[end+1:]
		} else {
			i := strings.Index(s, ", ")
			seg := s
			if i != -1 {
				seg = s[:i]
			}
			if strings.Contains(seg, `"`) {
				return nil
			}
			out = append(out, seg)
			if i == -1 {
				s = ""
			} else {
				s = s[i:]
			}
		}
		if s == "" {
			break
		}
		if !strings.HasPrefix(s, ", ") {
			return nil
		}
		s = s[2:]
		if s == "" {
			return nil
		}
	}
	return out
}

var auqNumRe = regexp.MustCompile(`^-?\d+(\.\d+)?$`)

// auqCleanlyAnswered mirrors the every() check in mod_0552: true only when
// every question got a conforming answer (valid options for choice, in-range
// numeric for number); any notes annotation or off-list answer forces the
// "The user answered:" clarification wording.
func auqCleanlyAnswered(in *auqIn, answers map[string]any) bool {
	for _, q := range in.Questions {
		if a, ok := in.Annotations[q.Question]; ok && a.Notes != "" {
			return false
		}
		labels := map[string]bool{}
		for _, o := range q.Options {
			labels[o.Label] = true
		}
		v := answers[q.Question]
		switch q.Kind {
		case "text", "number":
			s, _ := v.(string)
			if v == nil || s == "" {
				continue // unanswered counts as answered-skip
			}
			if q.Kind == "text" {
				return false // any free text may be a clarification request
			}
			u := strings.TrimSpace(s)
			f, err := strconv.ParseFloat(u, 64)
			if !auqNumRe.MatchString(u) || err != nil {
				return false
			}
			if q.Min != nil && f < *q.Min {
				return false
			}
			if q.Max != nil && f > *q.Max {
				return false
			}
		default: // choice
			if l, isList := v.([]string); isList {
				if !q.MultiSelect || len(l) == 0 {
					return false
				}
				for _, s := range l {
					if !labels[s] {
						return false
					}
				}
				continue
			}
			s, _ := v.(string)
			if s == "" || s == auqNotesOnly {
				continue
			}
			if labels[s] {
				continue
			}
			if !q.MultiSelect {
				return false
			}
			l := auqParseList(s)
			if l == nil || len(l) < 1 || auqAnswerString(l) != s {
				return false
			}
			for _, e := range l {
				if !labels[e] {
					return false
				}
			}
		}
	}
	return true
}

// auqFormatAnswers builds the "Q"="a", "Q"=(no option selected) notes: …
// digest used by every answered/idle/follow-up outcome string.
func auqFormatAnswers(in *auqIn, answers map[string]any) string {
	var parts []string
	for _, q := range in.Questions {
		s := auqAnswerString(answers[q.Question])
		annot, hasAnnot := in.Annotations[q.Question]
		answered := s != "" && s != auqNotesOnly
		if !answered && (!hasAnnot || annot.Notes == "") {
			continue
		}
		var p []string
		if answered {
			p = append(p, fmt.Sprintf(`"%s"="%s"`, q.Question, s))
		} else {
			p = append(p, fmt.Sprintf(`"%s"=(no option selected)`, q.Question))
		}
		if hasAnnot && annot.Preview != "" {
			p = append(p, "selected preview:\n"+annot.Preview)
		}
		if hasAnnot && annot.Notes != "" {
			p = append(p, "notes: "+annot.Notes)
		}
		parts = append(parts, strings.Join(p, " "))
	}
	return strings.Join(parts, ", ")
}

// auqResultText produces the four real outcome strings of mod_0552.
func auqResultText(in *auqIn, answers map[string]any) string {
	c := auqFormatAnswers(in, answers)
	resp := strings.TrimSpace(in.Response)
	if in.AfkTimeoutMs > 0 {
		idle := fmt.Sprintf("No response after %ds — the user may be away from keyboard. "+
			"Proceed using your best judgment based on the context so far; you can re-ask "+
			"this question later if it's still relevant.", in.AfkTimeoutMs/1000)
		if c != "" {
			return idle + "\n\nBefore going idle the user had selected: " + c + "."
		}
		return idle
	}
	if in.FollowUp {
		l := ""
		if resp != "" {
			l = fmt.Sprintf(` They also wrote: "%s".`, in.Response)
		}
		if c != "" {
			return fmt.Sprintf("The user asked for more questions before you proceed. "+
				"So far they answered: %s.%s Call AskUserQuestion again with follow-up "+
				"questions that build on these answers (do not repeat these); do not start "+
				"the task yet.", c, l)
		}
		return "The user asked for more questions before answering." + l +
			" Call AskUserQuestion again now with further questions about this decision " +
			"(do not repeat these); do not proceed with the task yet."
	}
	if resp != "" {
		return "The user responded: " + in.Response
	}
	if c != "" {
		if auqCleanlyAnswered(in, answers) {
			return fmt.Sprintf("Your questions have been answered: %s. "+
				"You can now continue with these answers in mind.", c)
		}
		return fmt.Sprintf("The user answered: %s. Read the answers carefully — "+
			"they may request clarification, changes, or that you not proceed — "+
			"and follow what they actually say.", c)
	}
	return "The user did not answer the questions."
}

// auqDisplay renders a question for the terminal prompt, carrying the
// extended fields the structured-question UI has no slot for.
func auqDisplay(q auqQuestion) string {
	s := q.Question
	if q.Description != "" {
		s += "\n" + q.Description
	}
	if q.Kind == "number" {
		var b []string
		if q.Min != nil {
			b = append(b, "min "+strconv.FormatFloat(*q.Min, 'f', -1, 64))
		}
		if q.Max != nil {
			b = append(b, "max "+strconv.FormatFloat(*q.Max, 'f', -1, 64))
		}
		if q.Step != nil {
			b = append(b, "step "+strconv.FormatFloat(*q.Step, 'f', -1, 64))
		}
		if q.DefaultValue != nil {
			b = append(b, "default "+strconv.FormatFloat(*q.DefaultValue, 'f', -1, 64))
		}
		if q.Unit != "" {
			b = append(b, "unit "+q.Unit)
		}
		if len(b) > 0 {
			s += " [" + strings.Join(b, ", ") + "]"
		}
	}
	return s
}

func execAskUser(c *Context, raw json.RawMessage) Result {
	var in auqIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	// validationErrorSteer (extended/extended-host variant): a choice
	// question with fewer than 2 options is rejected before the user sees it.
	for _, q := range in.Questions {
		kind := q.Kind
		if kind == "" {
			kind = "choice"
		}
		if kind != "choice" && kind != "text" && kind != "number" {
			return Result{Content: fmt.Sprintf(
				`unknown question kind %q (expected "choice", "text" or "number")`, q.Kind),
				IsError: true}
		}
		if kind == "choice" && len(q.Options) < 2 {
			return Result{Content: "This call included a choice question with fewer than 2 options, " +
				"so it was rejected and the person never saw it. If that question is open-ended, " +
				"re-ask it with \"kind\": \"text\" and no options; if there is really only one path, " +
				"state it as the approach you are taking and continue with the task. Do not invent " +
				"a filler second option.", IsError: true}
		}
		// previews are single-select only
		if q.MultiSelect {
			for _, o := range q.Options {
				if o.Preview != "" {
					return Result{Content: fmt.Sprintf(
						"Option %q in question %q: previews are only supported for single-select questions (not multiSelect).",
						o.Label, q.Question), IsError: true}
				}
			}
		}
	}
	// Merge host-provided answers into the working map first.
	answers := map[string]any{}
	for k, raw := range in.Answers {
		answers[k] = auqAnswer(raw)
	}
	// Host-driven path: answers/annotations/response/followUp/afk already
	// filled in — go straight to the real outcome strings, no prompting.
	hostResolved := len(in.Answers) > 0 || len(in.Annotations) > 0 ||
		strings.TrimSpace(in.Response) != "" || in.FollowUp || in.AfkTimeoutMs > 0
	if !hostResolved {
		if c.AskStructured == nil {
			return Result{Content: "structured questions unavailable (non-interactive mode)", IsError: true}
		}
		for _, q := range in.Questions {
			kind := q.Kind
			if kind == "" {
				kind = "choice"
			}
			var opts []string
			if kind == "choice" {
				for _, o := range q.Options {
					opts = append(opts, o.Label)
				}
			}
			sel, err := c.AskStructured(q.Header, auqDisplay(q), opts, q.MultiSelect)
			if err != nil {
				// dismissed with no answers → real "did not answer" outcome
				return Result{Content: "The user did not answer the questions."}
			}
			if q.MultiSelect {
				answers[q.Question] = sel
			} else if len(sel) > 0 {
				answers[q.Question] = sel[0]
			}
		}
	}
	return Result{Content: auqResultText(&in, answers)}
}

// ---------- plan mode ----------

// planPath is where the model writes its plan during plan mode (real CC
// points it at a scratchpad plan file and ExitPlanMode reads it back).
// Real CC tracks this on the session (Context.PlanFile equivalent); this
// build derives it deterministically from the session id.
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
	// real reads the plan file the model wrote (path tracked per session)
	pf := planPath(c)
	plan, _ := os.ReadFile(pf)
	if c.PlanApprove != nil && !c.PlanApprove() {
		// denial stays in plan mode
		return Result{Content: "The agent proposed a plan that was rejected by the user. The user chose to stay in plan mode rather than proceed with implementation.", IsError: true}
	}
	c.SetPermMode("default")
	// allowedPrompts (deprecated upstream) are still surfaced as
	// permission-rule suggestions, matching the interactive prompt.
	for _, ap := range in.AllowedPrompts {
		if ap.Tool != "" && ap.Prompt != "" {
			fmt.Fprintf(os.Stderr, "  + %s(%s)\n", ap.Tool, ap.Prompt)
		}
	}
	msg := "## Exited Plan Mode\nYou have exited plan mode. You can now make edits, run tools, and take actions."
	if len(plan) > 0 {
		msg += fmt.Sprintf(" The plan file is located at %s if you need to reference it.", pf)
		msg += fmt.Sprintf("\n\n<planContent>\n%s\n</planContent>", plan)
	}
	// resetTrigger marker consumed by the session loop, as in real CC where
	// exiting plan mode attaches {type:"plan_mode_exit"} and the approval
	// triggers a context reset carrying the plan content.
	msg += "\n\n<resetTrigger>plan_mode_exit</resetTrigger>"
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
//
// Real CC queues a local draft for explicit user approval — it never sends
// anything and produces no UI. claude-go keeps the same contract: the draft
// lands in ~/.claude-go/feedback/pending/ for the user to inspect/submit,
// and the result announces nothing beyond the recording.

func execSendFeedback(c *Context, raw json.RawMessage) Result {
	var in map[string]any
	json.Unmarshal(raw, &in)
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".claude-go", "feedback", "pending")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{Content: "Feedback recorded locally"}
	}
	p := filepath.Join(dir, fmt.Sprintf("fb-%d.json", time.Now().UnixNano()))
	in["cwd"] = c.CWD
	in["ts"] = time.Now().Format(time.RFC3339)
	b, _ := json.MarshalIndent(in, "", "  ")
	os.WriteFile(p, b, 0o600)
	return Result{Content: "Feedback recorded locally"}
}
