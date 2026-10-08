package agent

import "sync"

// subagentStats mirrors real's mod_0583 recorder (EWe.of(session).snapshot())
// — emitted as `subagent_stats` on the stream-json result event.
type subagentStats struct {
	Spawned   int `json:"spawned"`
	Requested struct {
		Background int `json:"background"`
		Foreground int `json:"foreground"`
		Unset      int `json:"unset"`
	} `json:"requested"`
	StartedInBackground int `json:"started_in_background"`
	MaxDepth            int `json:"max_depth"`
	SpawnedBySubagents  int `json:"spawned_by_subagents"`
	Completed           int `json:"completed"`
	Failed              int `json:"failed"`
	Killed              struct {
		Parent int `json:"parent"`
		User   int `json:"user"`
		System int `json:"system"`
	} `json:"killed"`
	Refused struct {
		DepthLimit       int `json:"depth_limit"`
		ConcurrencyLimit int `json:"concurrency_limit"`
		Budget           int `json:"budget"`
	} `json:"refused"`
	ByType map[string]int `json:"by_type"`
}

var statsRegistry = struct {
	sync.Mutex
	m map[string]*subagentStats
}{m: map[string]*subagentStats{}}

// statsFor returns the session's recorder ("" → an anonymous one — real's
// LX(e) returns nil for an absent session, so callers gate on session id).
func statsFor(sessionID string) *subagentStats {
	statsRegistry.Lock()
	defer statsRegistry.Unlock()
	s, ok := statsRegistry.m[sessionID]
	if !ok {
		s = &subagentStats{ByType: map[string]int{}}
		statsRegistry.m[sessionID] = s
	}
	return s
}

// SubagentStats returns the session's snapshot — real's LX(session).
func SubagentStats(sessionID string) any {
	if sessionID == "" {
		return nil
	}
	s := statsFor(sessionID)
	statsRegistry.Lock()
	defer statsRegistry.Unlock()
	cp := *s
	cp.Requested = s.Requested
	cp.Killed = s.Killed
	cp.Refused = s.Refused
	cp.ByType = map[string]int{}
	for k, v := range s.ByType {
		cp.ByType[k] = v
	}
	return &cp
}

// recordSpawn is real's recordSpawn: runInBackground tri-state
// (unset when the model omits the field), background flag when actually
// run in background, depth = parent depth + 1, by_type per agentType.
func recordSpawn(sessionID, agentType string, runInBackground *bool, runsInBackground bool, depth int) {
	s := statsFor(sessionID)
	statsRegistry.Lock()
	defer statsRegistry.Unlock()
	s.Spawned++
	switch {
	case runInBackground == nil:
		s.Requested.Unset++
	case *runInBackground:
		s.Requested.Background++
	default:
		s.Requested.Foreground++
	}
	if runsInBackground {
		s.StartedInBackground++
	}
	if depth > s.MaxDepth {
		s.MaxDepth = depth
	}
	if depth > 1 {
		s.SpawnedBySubagents++
	}
	s.ByType[agentType]++
}

// recordComplete marks one spawned agent's finish — real's handle
// records completed OR failed OR killed once (#n guard).
func recordComplete(sessionID string, failed bool) {
	s := statsFor(sessionID)
	statsRegistry.Lock()
	defer statsRegistry.Unlock()
	if failed {
		s.Failed++
	} else {
		s.Completed++
	}
}

// recordKilled marks a spawned agent killed by parent/user/system.
func recordKilled(sessionID, by string) {
	s := statsFor(sessionID)
	statsRegistry.Lock()
	defer statsRegistry.Unlock()
	switch by {
	case "parent", "system":
		if by == "parent" {
			s.Killed.Parent++
		} else {
			s.Killed.System++
		}
	default:
		s.Killed.User++
	}
}
