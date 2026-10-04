// Coordinator mode — CLAUDE_CODE_COORDINATOR_MODE (mod_0334.js Ta/AQ,
// mod_0335.js lfs, mod_0575.js U1 + standaloneAgentContext, mod_1512.js
// uIn). A coordinator session orchestrates worker agents instead of doing
// tool work itself; real swaps in a dedicated system prompt, a restricted
// tool surface (worker-spawn / message / comms-roled MCP tools), and marks
// the session entry kind "coordinator".
//
// Honest subset implemented here: the env gate, the resume-time mode
// matcher, the session-entry kind, and the fork refusal. The coordinator
// system prompt (dfs/getCoordinatorSystemPrompt) and the coordinator tool
// filter (mod_0562.js VO, mod_1369.js nAt) are extracted but not wired —
// their tool-name plumbing needs the full agent-context map.
package tools

import (
	"os"
	"strings"
)

// envTruthy mirrors Le() (mod_0019.js): "1"|"true"|"yes"|"on",
// case-insensitive, trimmed. Also accepts a bool-ish "" guard.
func envTruthy(v string) bool {
	if v == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Injectable launch-surface predicates — real's Ta() reads
// launchOptions.isInteractive() (Xu) and
// surfaceCapabilities.caps().workspace==="remote" (jn). The chunk those
// names resolve from isn't extracted, so these default to our closest
// equivalents and tests may replace them.
var (
	// CoordinatorIsInteractive approximates Xu(): true when this process
	// is an interactive REPL session.
	CoordinatorIsInteractive = func() bool { return Interactive }
	// CoordinatorWorkspaceRemote approximates jn(): true when the session
	// runs inside a remote/cloud workspace.
	CoordinatorWorkspaceRemote = func() bool {
		return envTruthy(os.Getenv("CLAUDE_CODE_REMOTE")) ||
			os.Getenv("CLAUDE_CODE_WORKSPACE") == "remote"
	}
)

// IsCoordinatorMode mirrors Ta() (mod_0334.js):
//
//	env gate:   CLAUDE_CODE_COORDINATOR_MODE must be truthy
//	block gate: interactive local sessions without CLAUDE_CODE_REMOTE
//	            refuse — coordinator mode is for remote/headless surfaces
func IsCoordinatorMode() bool {
	if !envTruthy(os.Getenv("CLAUDE_CODE_COORDINATOR_MODE")) {
		return false
	}
	if CoordinatorIsInteractive() && !CoordinatorWorkspaceRemote() &&
		os.Getenv("CLAUDE_CODE_REMOTE") == "" {
		return false
	}
	return true
}

// IsCoordinatorAgent mirrors AQ(e) = Ta() && e.agentId === undefined:
// the root agent (no agentId) in coordinator mode.
func IsCoordinatorAgent(agentID string) bool {
	return IsCoordinatorMode() && agentID == ""
}

// SessionKind mirrors the U1() call sites (mod_0686/0575/1071/1405/1512/
// 1806): the sessions/<pid>.json kind is "coordinator" or "normal".
// claude-go's registry already uses "interactive"/"bg" for the peer
// surface, so callers fold this in only when it wins.
func SessionKind() string {
	if IsCoordinatorMode() {
		return "coordinator"
	}
	return "normal"
}

// MatchSessionMode mirrors lfs() (mod_0335.js): when a resumed session's
// recorded mode disagrees with the current env gate, flip
// CLAUDE_CODE_COORDINATOR_MODE to match and warn. Returns "" when no
// switch happened (or the gate still fails — real deletes the env and
// returns undefined silently in that edge).
func MatchSessionMode(mode string) string {
	if mode == "" {
		return ""
	}
	was := IsCoordinatorMode()
	want := mode == "coordinator"
	if was == want {
		return ""
	}
	if want {
		os.Setenv("CLAUDE_CODE_COORDINATOR_MODE", "1")
	} else {
		os.Unsetenv("CLAUDE_CODE_COORDINATOR_MODE")
	}
	now := IsCoordinatorMode()
	if now == was {
		// gate refused the flip — real deletes the env var and stays quiet
		if want {
			os.Unsetenv("CLAUDE_CODE_COORDINATOR_MODE")
		}
		return ""
	}
	if now {
		return "Entered coordinator mode to match resumed session."
	}
	return "Exited coordinator mode to match resumed session."
}

// MsgForkCoordinator is uIn (mod_1512.js): keepParent fork refusal in
// coordinator sessions (reason "coordinator_mode").
const MsgForkCoordinator = "Forking is not available in coordinator sessions. Use /branch instead."
