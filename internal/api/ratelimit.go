package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RateLimit captures the unified rate-limit headers returned on every
// Messages API response (mod_0132.js Pp class, mod_0278.js header readers).
// The zero value / nil means no unified headers were present.
type RateLimit struct {
	// anthropic-ratelimit-unified-status: "allowed" | "allowed_warning" | "rejected"
	Status string
	// anthropic-ratelimit-unified-claim (repeatable): the limit windows that
	// were exceeded — "five_hour", "seven_day", "seven_day_opus",
	// "seven_day_sonnet".
	Claims []string
	// anthropic-ratelimit-unified-representative-claim
	RepresentativeClaim string
	// anthropic-ratelimit-unified-reset (unix seconds)
	ResetsAt time.Time
	// anthropic-ratelimit-unified-overage-status
	OverageStatus string
	// anthropic-ratelimit-unified-overage-reset (unix seconds)
	OverageResetsAt time.Time
	// anthropic-ratelimit-unified-overage-disabled-reason
	OverageDisabledReason string
	// anthropic-ratelimit-unified-overage-scope
	OverageScope string
	// anthropic-ratelimit-unified-overage-in-use ("true")
	OverageInUse bool
	// anthropic-ratelimit-unified-fallback == "available"
	FallbackAvailable bool

	// Slow-lane headers (anthropic-ratelimit-unified-slow-*).
	SlowOffer             string    // "treatment" | "control"
	SlowStatus            string    // "active" | "not_needed" | "slot_busy" | "weekly_limit" | ...
	SlowRetryAfter        float64   // seconds
	SlowMaxWait           float64   // seconds
	SlowBudgetUtilization float64   // 0..1
	SlowBudgetResetsAt    time.Time // anthropic-ratelimit-unified-slow-budget-reset

	// anthropic-usage-limit (raw value)
	UsageLimit string
	// retry-after (raw header value)
	RetryAfter string
}

// HasUnifiedHeaders reports whether any anthropic-ratelimit-unified-*
// headers were present on the response (mirrors the `get("...-status")===null`
// gate in the JS limiter).
func (r *RateLimit) HasUnifiedHeaders() bool {
	return r.Status != "" || r.RepresentativeClaim != "" || r.OverageStatus != ""
}

// parseRateLimitHeaders builds a RateLimit from response headers. Returns
// nil when no rate-limit headers are present at all.
func parseRateLimitHeaders(h http.Header) *RateLimit {
	get := func(k string) string { return strings.TrimSpace(h.Get(k)) }
	num := func(k string) (float64, bool) {
		v := get(k)
		if v == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	epoch := func(k string) time.Time {
		f, ok := num(k)
		if !ok || f <= 0 {
			return time.Time{}
		}
		return time.Unix(int64(f), 0)
	}

	rl := &RateLimit{
		Status:                get("anthropic-ratelimit-unified-status"),
		Claims:                h.Values("anthropic-ratelimit-unified-claim"),
		RepresentativeClaim:   get("anthropic-ratelimit-unified-representative-claim"),
		ResetsAt:              epoch("anthropic-ratelimit-unified-reset"),
		OverageStatus:         get("anthropic-ratelimit-unified-overage-status"),
		OverageResetsAt:       epoch("anthropic-ratelimit-unified-overage-reset"),
		OverageDisabledReason: get("anthropic-ratelimit-unified-overage-disabled-reason"),
		OverageScope:          get("anthropic-ratelimit-unified-overage-scope"),
		OverageInUse:          get("anthropic-ratelimit-unified-overage-in-use") == "true",
		FallbackAvailable:     get("anthropic-ratelimit-unified-fallback") == "available",
		SlowOffer:             get("anthropic-ratelimit-unified-slow-offer"),
		SlowStatus:            get("anthropic-ratelimit-unified-slow-status"),
		SlowBudgetResetsAt:    epoch("anthropic-ratelimit-unified-slow-budget-reset"),
		UsageLimit:            get("anthropic-usage-limit"),
		RetryAfter:            get("retry-after"),
	}
	if f, ok := num("anthropic-ratelimit-unified-slow-retry-after"); ok {
		rl.SlowRetryAfter = f
	}
	if f, ok := num("anthropic-ratelimit-unified-slow-max-wait"); ok {
		rl.SlowMaxWait = f
	}
	if f, ok := num("anthropic-ratelimit-unified-slow-budget-utilization"); ok && f >= 0 {
		if f > 1 {
			f = 1
		}
		rl.SlowBudgetUtilization = f
	}
	for i, c := range rl.Claims {
		rl.Claims[i] = strings.TrimSpace(c)
	}

	// Nothing rate-limit-related on this response?
	if !rl.HasUnifiedHeaders() && len(rl.Claims) == 0 && rl.SlowOffer == "" &&
		rl.SlowStatus == "" && rl.UsageLimit == "" && rl.RetryAfter == "" {
		return nil
	}
	return rl
}
