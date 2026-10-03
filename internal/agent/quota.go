package agent

import (
	"context"

	"claude-go/internal/api"
)

// QuotaCheck mirrors CC's startup probe: max_tokens=1, "quota".
// Runs in background; failure is non-fatal.
func QuotaCheck(ctx context.Context, c *api.Client, model string) {
	go func() {
		m := model
		if m == "" {
			m = defaultModel
		}
		req := &api.Request{
			Model:     m,
			MaxTokens: 1,
			Messages:  []api.Message{api.TextMessage("user", "quota")},
			Metadata:  Metadata(c.SessionID, UserConfig{}),
		}
		c.StreamMessage(ctx, req, func(*api.StreamEvent) error { return nil })
	}()
}
