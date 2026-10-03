package agent

import (
	"context"

	"claude-go/internal/api"
)

// QuotaCheck mirrors CC's startup probe: non-streamed max_tokens=1, content
// ".", billing+identity system blocks, metadata. Runs in background;
// failure is non-fatal.
func QuotaCheck(ctx context.Context, c *api.Client, model string) {
	go func() {
		m := model
		if m == "" {
			m = defaultModel
		}
		req := &api.Request{
			Model:     m,
			MaxTokens: 1,
			Messages:  []api.Message{api.TextMessage("user", ".")},
			Metadata:  Metadata(c.SessionID, UserConfig{}),
			System:    SystemBlocks(string(c.Entrypoint), "", m, "", nil),
		}
		c.SendMessage(ctx, req)
	}()
}
