// Teleport to cloud — mod_2173.js (j/W/L/Y/P/G/te) extracted from Claude
// Code 2.1.288: the /teleport flow that moves the current bridge-attached
// session into a cloud environment via POST .../move-to-cloud.
//
// Fidelity notes:
//   - Real does NOT push the outcome branch from the CLI: the server clones
//     from the repo's remote and creates it. The CLI only sends
//     outcome_branch in the move request body. The branch name is
//     claude/teleport-<last 8 chars of the bridge session id> (not a sha).
//   - Real's environment lookup (xAt, chunk-kqp83qd2 — not extracted) reads
//     the selected/available cloud targets; we map it to
//     GET /v1/environments?beta=true (mod_0278 SDK surface) and take the
//     first managed (non-bridge, non-self-hosted) target. The selected-
//     target preference and the "self-hosted default" message suffix are
//     approximated — see residuals.
//   - Real's reconnect predicate (kQ) is "this REPL's bridge data plane is
//     live". Our data plane is stubbed, so we poll for a NEW
//     bridgeSessionId stamp (the remote-control loop writes it via
//     OnSessionID) for up to 10s — matching te(oldSessionId)'s poll.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/gbasin/claude-go/internal/telemetry"
)

// ---------------------------------------------------------------------------
// User-facing strings (verbatim from mod_2173.js / chunk-acxptg39.js).
// ---------------------------------------------------------------------------

// g3n (chunk-acxptg39.js): restricted-session precondition.
const MsgTeleportRestricted = "Cloud sessions cannot be created from a --restricted session: they would not enforce it."

// no_bridge precondition.
const MsgTeleportNoBridge = "This session isn’t connected to Remote Control. Run /remote-control first, then try again."

// no_auth precondition.
const MsgTeleportNoAuth = "Not signed in to claude.ai. Run /login with your claude.ai account and try again."

// no_environment precondition.
const MsgTeleportNoEnvironment = "No cloud environment available. Create one from the web UI (or run /web-setup), then try again."

// O — appended to the "now in the cloud" line when no reconnection lands
// (also appended to mayHaveCommitted failures).
const MsgRCInactive = "/remote-control is no longer active. Run /remote-control to start a new session."

// MsgTeleportNoGit mirrors the "no-git" precondition text — oe() is cwd.
func MsgTeleportNoGit(cwd string) string {
	return fmt.Sprintf("%s isn’t a git repository. The cloud session clones your repo from its remote, "+
		"so there’s nothing for it to check out here — try again from inside a git repo that has a remote.", cwd)
}

// MsgTeleportDirty mirrors the "git_dirty" precondition text; real falls
// back to "this branch" when the branch name is empty.
func MsgTeleportDirty(branch string) string {
	if branch == "" {
		branch = "this branch"
	}
	return fmt.Sprintf("You have uncommitted changes on %s. The cloud session clones from the remote, "+
		"so local edits won’t be visible there. Commit and push them, or stash them if they shouldn’t "+
		"go to the cloud. Then try again.", branch)
}

// MsgTeleportUnpushed mirrors the "git_unpushed" precondition text.
func MsgTeleportUnpushed(branch string) string {
	if branch == "" {
		branch = "This branch"
	}
	return fmt.Sprintf("%s has commits that haven’t been pushed. The cloud session clones from the "+
		"remote, so it won’t have them. Push, then try again.", branch)
}

// ---------------------------------------------------------------------------
// Restricted session ($T in mod_2173 → launchOptions.restrictedSession()).
// claude-go's --restricted flag lands as CLAUDE_GO_RESTRICTED; the real env
// name (stripped from spawned children, mod_0417.js) is
// CLAUDE_CODE_RESTRICTED. Honor both.
// ---------------------------------------------------------------------------

func RestrictedSession() bool {
	for _, e := range []string{"CLAUDE_CODE_RESTRICTED", "CLAUDE_GO_RESTRICTED"} {
		if EnvTruthy(os.Getenv(e)) {
			return true
		}
	}
	return false
}

// EnvTruthy mirrors real's Le(): "1"|"true"|"yes"|"on" case-insensitive.
func EnvTruthy(v string) bool {
	if v == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Worktree state machine — function Y() in mod_2173.js.
//   !isGitRepo(cwd)                                -> no-git
//   !workingTreeClean({ignoreUntracked:true})      -> dirty
//   hasUnpushedCommits || !hasUpstream             -> unpushed
//   else                                           -> ready + outcome branch
// ---------------------------------------------------------------------------

// WorktreeKind enumerates Y()'s verdicts.
type WorktreeKind string

const (
	WtNoGit    WorktreeKind = "no-git"
	WtDirty    WorktreeKind = "dirty"
	WtUnpushed WorktreeKind = "unpushed"
	WtReady    WorktreeKind = "ready"
)

// WorktreeStatus is Y()'s return shape.
type WorktreeStatus struct {
	Kind          WorktreeKind
	Branch        string // dirty/unpushed verdicts carry it
	OutcomeBranch string // ready: claude/teleport-<id.slice(-8)>
}

// CheckWorktree runs Y()'s checks against cwd for the given bridge session
// id (the outcome branch derives from it).
func CheckWorktree(cwd, bridgeSessionID string) WorktreeStatus {
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--is-inside-work-tree").Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return WorktreeStatus{Kind: WtNoGit}
	}
	var branch string
	if b, err := exec.Command("git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
		branch = strings.TrimSpace(string(b))
		if branch == "HEAD" { // detached — real's ad() would report no branch
			branch = ""
		}
	}
	// U6e({ignoreUntracked:!0}): tracked-file dirt only. A status error is
	// treated as dirty — we can't prove clean, so we refuse like real.
	if st, err := exec.Command("git", "-C", cwd, "status", "--porcelain", "--untracked-files=no").Output(); err != nil || strings.TrimSpace(string(st)) != "" {
		return WorktreeStatus{Kind: WtDirty, Branch: branch}
	}
	// X3t()||!Qir(): commits ahead of upstream, or no upstream to verify.
	upstream, uerr := exec.Command("git", "-C", cwd, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}").Output()
	if uerr != nil || strings.TrimSpace(string(upstream)) == "" {
		return WorktreeStatus{Kind: WtUnpushed, Branch: branch}
	}
	ahead, aerr := exec.Command("git", "-C", cwd, "rev-list", "--count", "@{u}..HEAD").Output()
	if aerr != nil || strings.TrimSpace(string(ahead)) != "0" {
		return WorktreeStatus{Kind: WtUnpushed, Branch: branch}
	}
	tail := bridgeSessionID
	if len(tail) > 8 {
		tail = tail[len(tail)-8:]
	}
	return WorktreeStatus{Kind: WtReady, OutcomeBranch: "claude/teleport-" + tail}
}

// ---------------------------------------------------------------------------
// move-to-cloud — function L() in mod_2173.js.
// POST {base}/v1/code/sessions/{sessionId}/move-to-cloud
//   {environment_id, outcome_branch?}   (15s timeout, validateStatus <600)
// ---------------------------------------------------------------------------

// MoveToCloudParams is L()'s argument object.
type MoveToCloudParams struct {
	SessionID          string // the bridge session id, used verbatim in the path
	EnvironmentID      string
	OutcomeBranch      string // omitted when empty (real: ...b&&{outcome_branch:b})
	TrustedDeviceToken string // X-Trusted-Device-Token when non-empty
	Timeout            time.Duration
}

// MoveToCloudResult is L()'s outcome union.
type MoveToCloudResult struct {
	OK               bool
	Message          string // failure text (real's failure.message)
	Reason           string // error.type parsed off the body (real's reason)
	MayHaveCommitted bool   // move may have gone through — check the web UI
	EnvironmentKind  string
	EnvironmentID    string
	SessionID        string
}

var notBridgeSessionRe = regexp.MustCompile(`(?i)not a bridge session`)
var invalidRequestRe = regexp.MustCompile(`(?i)^the request was invalid\.?$`)

// MoveToCloud mirrors L(). Network errors produce real's "Couldn't reach
// the server" text; ENOTFOUND/ECONNREFUSED (DNS/connect-level failures)
// are the only errors that rule out the commit.
func (c *Client) MoveToCloud(ctx context.Context, p *MoveToCloudParams) *MoveToCloudResult {
	tok, err := c.oauthToken()
	if err != nil {
		return &MoveToCloudResult{Message: err.Error()}
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	h, herr := c.codeSessionHeaders(tok)
	if herr != nil {
		return &MoveToCloudResult{Message: herr.Error()}
	}
	if p.TrustedDeviceToken != "" {
		h.Set(headerTrustedDevice, p.TrustedDeviceToken)
	}
	body := map[string]any{"environment_id": p.EnvironmentID}
	if p.OutcomeBranch != "" {
		body["outcome_branch"] = p.OutcomeBranch
	}
	url := c.BaseURL + "/v1/code/sessions/" + p.SessionID + "/move-to-cloud"
	r, err := c.do(ctx, http.MethodPost, url, body, h, timeout)
	if err != nil {
		// real inspects err.code for ENOTFOUND/ECONNREFUSED; Go surfaces
		// DNS/connect failures as *net.DNSError / connection-refused text.
		unreachable := isUnreachableErr(err)
		msg := fmt.Sprintf("Couldn’t reach the server — %v", err)
		if !unreachable {
			msg += ". The move may still have gone through — check the session in the web UI."
		}
		return &MoveToCloudResult{Message: msg, MayHaveCommitted: !unreachable}
	}
	if r.Status == 200 || r.Status == 201 {
		var v struct {
			Session *struct {
				EnvironmentKind string `json:"environment_kind"`
				EnvironmentID   string `json:"environment_id"`
			} `json:"session"`
		}
		json.Unmarshal(r.Body, &v)
		kind, envID := "cloud", p.EnvironmentID
		if v.Session != nil {
			if v.Session.EnvironmentKind != "" {
				kind = v.Session.EnvironmentKind
			}
			if v.Session.EnvironmentID != "" {
				envID = v.Session.EnvironmentID
			}
		}
		c.debugf("[code-session] move-to-cloud %s → %s (env %s)", p.SessionID, kind, envID)
		return &MoveToCloudResult{OK: true, EnvironmentKind: kind, EnvironmentID: envID, SessionID: p.SessionID}
	}
	msg := bodyErrorMessage(r.Body)
	reason := bodyErrorType(r.Body)
	dbg := fmt.Sprintf("[code-session] move-to-cloud %s failed %d", p.SessionID, r.Status)
	if reason != "" {
		dbg += " [" + reason + "]"
	}
	if msg != "" {
		dbg += ": " + msg
	}
	c.debugf("%s", dbg)
	if r.Status >= 500 {
		return &MoveToCloudResult{
			Message:          fmt.Sprintf("The server responded with %d. The move may still have gone through — check the session in the web UI.", r.Status),
			MayHaveCommitted: true}
	}
	if r.Status == 400 && notBridgeSessionRe.MatchString(msg) {
		return &MoveToCloudResult{
			Message:          "This session is already on cloud — refresh the web UI to see it.",
			MayHaveCommitted: true}
	}
	if invalidRequestRe.MatchString(msg) {
		msg = ""
	}
	if msg == "" {
		msg = fmt.Sprintf("The server couldn’t move this session (HTTP %d). Run with --debug to see the raw response.", r.Status)
	}
	return &MoveToCloudResult{Message: msg, MayHaveCommitted: false, Reason: reason}
}

// isUnreachableErr approximates real's err.code ENOTFOUND|ECONNREFUSED —
// failures that prove the request never reached the server.
func isUnreachableErr(err error) bool {
	s := err.Error()
	return strings.Contains(s, "no such host") ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "ENOTFOUND") || strings.Contains(s, "ECONNREFUSED")
}

// ---------------------------------------------------------------------------
// Session web URL — lc() + Nmt() in mod_0304.js.
// ---------------------------------------------------------------------------

// SessionWebURL mirrors lc(sessionId, ingressUrl, {from:"cli"}):
// {webBase}/code/{compatId}?from=cli. The compat id is the cse_ form.
func SessionWebURL(sessionID, ingressURL string) string {
	return webBase(sessionID, ingressURL) + "/code/" + codeSessionID(sessionID) + "?from=cli"
}

// webBase mirrors Nmt(): local session ids or localhost ingress urls go to
// the dev host; staging likewise; everything else is claude.ai.
func webBase(sessionID, ingressURL string) string {
	if strings.Contains(sessionID, "_local_") || strings.Contains(ingressURL, "localhost") {
		return "http://localhost:4000"
	}
	if strings.Contains(sessionID, "_staging_") || strings.Contains(ingressURL, "staging") {
		return "https://claude-ai.staging.ant.dev"
	}
	return "https://claude.ai"
}

// ---------------------------------------------------------------------------
// Environment lookup — xAt() subset. Real returns {selectedTarget,
// availableTargets, environmentsError} and picks the selected target when
// it's eligible, else the first non-bridge non-self-hosted target.
// GET /v1/environments?beta=true (mod_0278.js, managed-agents beta).
// ---------------------------------------------------------------------------

// EnvironmentTarget is one row of the environments list.
type EnvironmentTarget struct {
	ID         string `json:"environment_id"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	SelfHosted bool   `json:"self_hosted"`
}

// teleportEligible mirrors R() = kind!=="bridge" && !selfHosted(h).
func (e EnvironmentTarget) teleportEligible() bool {
	return e.Kind != "bridge" && !e.SelfHosted && e.Kind != "self_hosted"
}

// ListEnvironments: GET /v1/environments?beta=true — OAuth lane. The exact
// envelope is inferred (real's lookup lives in an unextracted chunk); we
// accept {environments:[…]} or {data:[…]}.
func (c *Client) ListEnvironments(ctx context.Context) ([]EnvironmentTarget, error) {
	tok, err := c.oauthToken()
	if err != nil {
		return nil, err
	}
	h, err := c.codeSessionHeaders(tok)
	if err != nil {
		return nil, err
	}
	h.Set("anthropic-beta", "managed-agents-2026-04-01")
	r, err := c.do(ctx, http.MethodGet, c.BaseURL+"/v1/environments?beta=true", nil, h, 10*time.Second)
	if err != nil {
		return nil, err
	}
	if r.Status != 200 {
		return nil, fmt.Errorf("environments list failed with status %d", r.Status)
	}
	var body struct {
		Environments []EnvironmentTarget `json:"environments"`
		Data         []EnvironmentTarget `json:"data"`
	}
	if err := json.Unmarshal(r.Body, &body); err != nil {
		return nil, err
	}
	if body.Environments != nil {
		return body.Environments, nil
	}
	return body.Data, nil
}

// PickTeleportEnvironment mirrors real's selection: the first eligible
// target (the selectedTarget preference isn't modeled — no settings slot
// carries it in claude-go).
func PickTeleportEnvironment(targets []EnvironmentTarget) *EnvironmentTarget {
	for i := range targets {
		if targets[i].teleportEligible() && targets[i].ID != "" {
			return &targets[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Orchestration — j() + W() + G()/P()/te() in mod_2173.js.
// ---------------------------------------------------------------------------

// TeleportDeps injects the session/side-effect boundaries so tests stub
// everything except the HTTP client (which httptest covers).
type TeleportDeps struct {
	CWD string
	// Restricted is $T() — launchOptions.restrictedSession().
	Restricted func() bool
	// BridgeSessionID is Hl()?.bridgeSessionId gated on !outboundOnly (Q()).
	BridgeSessionID func() string
	// StampBridgeSession is Ngn()/Mjo(): "" drops this session's bridge
	// claim before the move; on a failure that can't have committed the
	// claim is re-stamped with the old id.
	StampBridgeSession func(id string)
	// Environment is xAt(): returns the selected cloud environment id, or
	// error when the lookup itself failed.
	Environment func(ctx context.Context) (string, error)
	// SessionIngressURL is the adopted bridge's api_base_url (drives the
	// localhost/staging web-base pick in lc()). Empty => claude.ai.
	SessionIngressURL string
	// ReconnectWait is J=10s in te(); 0 => skip polling (immediate
	// "disconnected" outcome).
	ReconnectWait time.Duration
	// PollInterval is z=100ms in te().
	PollInterval time.Duration
}

// TeleportToCloud runs the full /teleport "send" flow and returns the
// display text real's menu prints via onExit(..., {display:"system"}).
func TeleportToCloud(ctx context.Context, c *Client, d *TeleportDeps) string {
	if d == nil {
		d = &TeleportDeps{}
	}
	// m() is a mark/counter event; tengu_teleport_to_cloud only fires for
	// start/success/failed/maybe_committed — never for preconditions.
	mark := func(reason string) { telemetry.Event("teleport_to_cloud", map[string]any{"reason": reason}) }
	precondition := func(reason, msg string) string {
		mark(reason)
		return msg
	}

	// j(): precondition order — restricted, bridge, auth, worktree, env.
	if d.Restricted != nil && d.Restricted() {
		return precondition("restricted_session", MsgTeleportRestricted)
	}
	sessionID := ""
	if d.BridgeSessionID != nil {
		sessionID = d.BridgeSessionID()
	}
	if sessionID == "" {
		return precondition("no_bridge", MsgTeleportNoBridge)
	}
	if _, err := c.oauthToken(); err != nil {
		return precondition("no_auth", MsgTeleportNoAuth)
	}
	cwd := d.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	st := CheckWorktree(cwd, sessionID)
	switch st.Kind {
	case WtNoGit:
		return precondition("no_git", MsgTeleportNoGit(cwd))
	case WtDirty:
		return precondition("git_dirty", MsgTeleportDirty(st.Branch))
	case WtUnpushed:
		return precondition("git_unpushed", MsgTeleportUnpushed(st.Branch))
	}
	var envID string
	if d.Environment != nil {
		id, err := d.Environment(ctx)
		if err != nil {
			return precondition("env_lookup_failed",
				fmt.Sprintf("Couldn’t look up cloud environments — %v", err))
		}
		envID = id
	}
	if envID == "" {
		return precondition("no_environment", MsgTeleportNoEnvironment)
	}

	telemetry.Event("tengu_teleport_to_cloud", map[string]any{"action": "start"})
	stamp := func(id string) {
		if d.StampBridgeSession != nil {
			d.StampBridgeSession(id)
		}
	}
	stamp("") // Ngn(u) — the bridge claim leaves this process before the move
	res := c.MoveToCloud(ctx, &MoveToCloudParams{
		SessionID:     sessionID,
		EnvironmentID: envID,
		OutcomeBranch: st.OutcomeBranch,
	})
	if !res.OK {
		if res.MayHaveCommitted {
			mark("maybe_committed")
			telemetry.Event("tengu_teleport_to_cloud", map[string]any{"action": "maybe_committed"})
			return fmt.Sprintf("Couldn’t teleport: %s\n%s", res.Message, MsgRCInactive)
		}
		stamp(sessionID) // Mjo(u) — move provably didn't happen; claim restored
		reason := res.Reason
		if reason == "" {
			reason = "request_failed"
		}
		mark(reason)
		telemetry.Event("tengu_teleport_to_cloud", map[string]any{"action": "failed"})
		return "Couldn’t teleport: " + res.Message
	}

	telemetry.Event("tengu_teleport_to_cloud", map[string]any{
		"action": "success", "environment_kind": res.EnvironmentKind,
	})
	url := SessionWebURL(sessionID, d.SessionIngressURL)
	base := "Session now in the cloud: " + url

	// G()/te(): poll for a replacement bridge session id (the remote-control
	// loop stamps it via OnSessionID when it adopts a fresh session). A new
	// id within the wait => "reconnected"; otherwise "disconnected".
	wait := d.ReconnectWait
	if wait <= 0 {
		wait = 10 * time.Second
	}
	interval := d.PollInterval
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			break
		}
		if d.BridgeSessionID != nil {
			if id := d.BridgeSessionID(); id != "" && id != sessionID {
				newURL := SessionWebURL(id, d.SessionIngressURL)
				return fmt.Sprintf("%s\nNew /remote-control session is active · Continue here, on your phone, or at %s",
					base, newURL)
			}
		}
		time.Sleep(interval)
	}
	return base + "\n" + MsgRCInactive
}
