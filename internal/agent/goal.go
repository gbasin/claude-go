package agent

// goal.go: /goal stop-hook. When l.Goal is set, each finished turn is judged
// by the model; a CONTINUE verdict re-prompts the loop ("keep working"),
// capped at 20 iterations like the real stop-hook guardrail.

import (
	"context"
	"strings"

	"github.com/gbasin/claude-go/internal/api"
)

const maxGoalChecks = 20

// maybeGoalCheck runs after a turn completes: asks the main model whether the
// goal holds; CONTINUE pushes a re-prompt and runs again.
func (l *Loop) maybeGoalCheck(ctx context.Context, resp *api.Response) (*api.Response, error) {
	var err error
	for i := 0; l.Goal != "" && i < maxGoalChecks; i++ {
		done, derr := l.goalSatisfied(ctx)
		if derr != nil || done {
			return resp, derr
		}
		l.PushUser([]api.ContentBlock{{
			Type: "text",
			Text: "Keep working toward: " + l.Goal,
		}})
		resp, err = l.run(ctx, 0)
		if err != nil {
			return resp, err
		}
	}
	return resp, nil
}

// goalSatisfied asks the model (same request shape, no tools) whether the
// current conversation state satisfies the goal. DONE = satisfied.
func (l *Loop) goalSatisfied(ctx context.Context) (bool, error) {
	req := l.buildRequest()
	req.Tools = nil
	req.Messages = append(append([]api.Message{}, l.Messages...),
		api.TextMessage("user",
			"Does the current state satisfy this goal: "+l.Goal+
				"? Answer CONTINUE or DONE."))
	r, err := l.Client.StreamMessage(ctx, req, func(*api.StreamEvent) error { return nil })
	if err != nil {
		return false, err
	}
	var text string
	for _, b := range r.Content {
		if b.Type == "text" {
			text += b.Text
		}
	}
	up := strings.ToUpper(text)
	if strings.Contains(up, "CONTINUE") {
		return false, nil
	}
	// DONE or an ambiguous answer ends the loop — never spin on mush.
	return true, nil
}
