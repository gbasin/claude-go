package agent

// cost-state transcript row — real's s3t emit (mod_0416): the cumulative
// session cost/token ledger snapshot, written when the ledger is saved
// (conversation reset, session switch, graceful shutdown; the merged
// record is "last-wins" on resume). The /cost UI reads the live ledger,
// not this row; the row is what survives to a resumed session.

import "time"

// costModelUsage is real's per-model ledger entry — the value at
// modelUsage[model] on the cost-state row. Every field is emitted,
// including zeros.
type costModelUsage struct {
	InputTokens              int     `json:"inputTokens"`
	OutputTokens             int     `json:"outputTokens"`
	ThinkingTokens           int     `json:"thinkingTokens"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens"`
	WebSearchRequests        int     `json:"webSearchRequests"`
	CostUSD                  float64 `json:"costUSD"`
}

// costStateRecord mirrors the row verbatim — field order matches real's
// s3t object literal.
type costStateRecord struct {
	Type                           string                    `json:"type"`
	SessionID                      string                    `json:"sessionId"`
	TotalCostUSD                   float64                   `json:"totalCostUSD"`
	TotalAPIDuration               int64                     `json:"totalAPIDuration"`
	TotalAPIDurationWithoutRetries int64                     `json:"totalAPIDurationWithoutRetries"`
	TotalToolDuration              int64                     `json:"totalToolDuration"`
	TotalLinesAdded                int64                     `json:"totalLinesAdded"`
	TotalLinesRemoved              int64                     `json:"totalLinesRemoved"`
	TotalDuration                  int64                     `json:"totalDuration"`
	StartTime                      int64                     `json:"startTime"`
	ModelUsage                     map[string]costModelUsage `json:"modelUsage"`
	HasUnknownModelCost            bool                      `json:"hasUnknownModelCost"`
}

// CostState builds the cost-state row for this loop's session, or nil
// when there is no session to tag it to (real's s3t returns nothing when
// the ledger doesn't belong to the active session).
func (l *Loop) CostState() *costStateRecord {
	if l.Client == nil || l.Client.SessionID == "" {
		return nil
	}
	// startTime: real's ledger stamp R1e() is claimed at session start —
	// our nearest equivalents are first-turn start / first request
	start := l.sessionStarted
	if start.IsZero() {
		start = l.startAt
	}
	if start.IsZero() {
		start = time.Now()
	}
	startMs := start.UnixMilli()
	totalMs := time.Since(start).Milliseconds()
	l.costMu.Lock()
	usage := make(map[string]costModelUsage, len(l.costModels))
	for m, u := range l.costModels {
		usage[m] = *u
	}
	unknown := l.unknownModelCost
	l.costMu.Unlock()
	return &costStateRecord{
		Type:                           "cost-state",
		SessionID:                      l.Client.SessionID,
		TotalCostUSD:                   l.costUSD,
		TotalAPIDuration:               l.apiNanos.Load() / 1e6,
		TotalAPIDurationWithoutRetries: l.apiNanosNoRetry.Load() / 1e6,
		TotalToolDuration:              l.toolNanos.Load() / 1e6,
		TotalLinesAdded:                l.LinesAdded.Load(),
		TotalLinesRemoved:              l.LinesRemoved.Load(),
		TotalDuration:                  totalMs,
		StartTime:                      startMs,
		ModelUsage:                     usage,
		HasUnknownModelCost:            unknown,
	}
}
