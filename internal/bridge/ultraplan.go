// Ultraplan / ultrareview — mod_0386.js (WJt/jJt/Fv/_lr/Clr + VQo/f8t/YQo
// + uRe) and mod_0274.js (h8n/Mgn create-body shape). The cloud plan/review
// flows mint a code session with a permission-mode control request + the
// initial user message inside the create body's events array, then poll
// GET /v1/code/sessions/{id}/events until ExitPlanMode resolves.
package bridge

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Org-policy gates (mod_0386.js sn()/lp() at the ultraplan/ultrareview entries).
// ---------------------------------------------------------------------------

// PolicyAllowRoutines is the mod_0465.js JX policy key gating RemoteTrigger.
const PolicyAllowRoutines = "allow_routines"

// ErrCloudSessionsPolicyDenied is SJo (mod_0386.js) — the Xde policy_blocked
// fallback real prints as `ultraplan: <this>`.
const ErrCloudSessionsPolicyDenied = "Cloud sessions are disabled by your organization's policy. " +
	"Contact your organization admin to enable them."

// ErrRoutinesPolicyDenied is Mho (mod_0465.js) — the allow_routines denial.
const ErrRoutinesPolicyDenied = "Routines are disabled by your organization's policy."

// RemoteSessionsPolicyDenial mirrors
// lp("allow_remote_sessions","Cloud sessions","are", SJo): the refusal
// string when the org denies cloud sessions, nil when allowed. Used by
// /ultraplan, /ultrareview, /teleport and RemoteTrigger.
func RemoteSessionsPolicyDenial() *string {
	return OrgPolicyVerdict(PolicyAllowRemoteSessions,
		"Cloud sessions", "are", ErrCloudSessionsPolicyDenied)
}

// RoutinesPolicyDenial mirrors
// lp("allow_routines","Routines","are", Mho).
func RoutinesPolicyDenial() *string {
	return OrgPolicyVerdict(PolicyAllowRoutines,
		"Routines", "are", ErrRoutinesPolicyDenied)
}

// ---------------------------------------------------------------------------
// Create-session events (mod_0274.js h8n + mod_0386.js RJt).
// ---------------------------------------------------------------------------

// newUUIDv4 mirrors oR()/xe() — crypto-random RFC4122 v4.
func newUUIDv4() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// setPermissionModeEvent is h8n(mode, ultraplan): the control_request the
// create body carries first so the session starts in the requested mode.
func setPermissionModeEvent(mode string, ultraplan bool) map[string]any {
	req := map[string]any{"subtype": "set_permission_mode"}
	if mode != "" {
		req["mode"] = mode
	}
	if ultraplan {
		req["ultraplan"] = true
	}
	return map[string]any{
		"type":       "control_request",
		"request_id": "set-mode-" + newUUIDv4(),
		"request":    req,
	}
}

// initialUserEvent is RJt's user-event tail: uuid, session_id:"",
// parent_tool_use_id:null, message:{role:"user",content:<prompt>}.
func initialUserEvent(content string) map[string]any {
	return map[string]any{
		"type":               "user",
		"uuid":               newUUIDv4(),
		"session_id":         "",
		"parent_tool_use_id": nil,
		"message":            map[string]any{"role": "user", "content": content},
	}
}

// ---------------------------------------------------------------------------
// Event polling — Wfe (mod_0132.js): the session-events page turner.
// ---------------------------------------------------------------------------

// SessionEvent is one decoded payload from the events feed.
type SessionEvent map[string]any

// SessionEventsResult is Wfe's return shape.
type SessionEventsResult struct {
	NewEvents     []SessionEvent    // transcript events (payloads with session_id)
	ControlFrames []json.RawMessage // control-frame payloads
	LastEventID   string            // last sequence_num seen ("" = none yet)
	SessionStatus string            // session.session_status (metadata fetch)
	PendingAction json.RawMessage   // session.pending_action
	MetadataError error             // non-fatal metadata-fetch failure
}

// eventsHTTPError marks fetch failures that real's ibt() would NOT retry —
// the response arrived but wasn't usable (non-200, malformed body).
type eventsHTTPError struct{ msg string }

func (e *eventsHTTPError) Error() string { return e.msg }

// IsRetriablePollErr mirrors ibt(e): transport-level failures (DNS,
// refused, reset, timeout — the zQo set) are retriable; an HTTP response
// that arrived but failed is not.
func IsRetriablePollErr(err error) bool {
	var he *eventsHTTPError
	return !errors.As(err, &he)
}

const (
	ultraplanPollInterval = 3000 * time.Millisecond // HJt
	ultraplanPollRetries  = 5                       // rlr
	ultraplanTimeoutSec   = 5400                    // tengu_ultraplan_timeout_seconds default
	eventsPageLimit       = 50                      // Wfe's per-call page cap
)

// FetchSessionEvents is Wfe(): GET /v1/code/sessions/{id}/events
// ?sort_order=asc&cursor=<lastEventID>, following next_cursor for up to
// eventsPageLimit pages. Payloads that parse as control frames land in
// ControlFrames; payloads carrying session_id land in NewEvents (real's
// uF()/session_id split). When skipMetadata is false it also fetches
// GET /v1/code/sessions/{id} for session_status/pending_action — a failure
// there is tolerated into MetadataError (real logs a warn and continues).
func (c *Client) FetchSessionEvents(ctx context.Context, sessionID, cursor string, skipMetadata bool) (*SessionEventsResult, error) {
	if err := checkID(sessionID, "sessionId"); err != nil {
		return nil, err
	}
	tok, err := c.oauthToken()
	if err != nil {
		return nil, err
	}
	h, err := c.codeSessionHeaders(tok)
	if err != nil {
		return nil, err
	}
	out := &SessionEventsResult{LastEventID: cursor}
	base := c.BaseURL + "/v1/code/sessions/" + codeSessionID(sessionID) + "/events"
	for page := 0; page < eventsPageLimit; page++ {
		u := base + "?sort_order=asc"
		if out.LastEventID != "" {
			u += "&cursor=" + out.LastEventID
		}
		r, err := c.do(ctx, http.MethodGet, u, nil, h, 30*time.Second)
		if err != nil {
			return nil, err
		}
		if r.Status != 200 {
			return nil, &eventsHTTPError{
				msg: fmt.Sprintf("Failed to fetch session events: %d", r.Status)}
		}
		var body struct {
			Data []struct {
				SequenceNum any             `json:"sequence_num"`
				Payload     json.RawMessage `json:"payload"`
			} `json:"data"`
			NextCursor string `json:"next_cursor"`
		}
		if json.Unmarshal(r.Body, &body) != nil || body.Data == nil {
			return nil, &eventsHTTPError{msg: "Invalid events response"}
		}
		for _, item := range body.Data {
			switch v := item.SequenceNum.(type) {
			case float64:
				out.LastEventID = strconv.FormatFloat(v, 'f', -1, 64)
			case string:
				out.LastEventID = v
			case json.Number:
				out.LastEventID = v.String()
			}
			var payload map[string]any
			if json.Unmarshal(item.Payload, &payload) != nil {
				continue
			}
			if _, ok := payload["type"]; !ok {
				continue
			}
			if isControlFrame(payload) {
				out.ControlFrames = append(out.ControlFrames, item.Payload)
			} else if _, has := payload["session_id"]; has {
				out.NewEvents = append(out.NewEvents, SessionEvent(payload))
			}
		}
		if body.NextCursor == "" {
			break
		}
	}
	if skipMetadata {
		return out, nil
	}
	meta, merr := c.GetCodeSessionMeta(ctx, sessionID)
	if merr != nil {
		out.MetadataError = merr
		return out, nil
	}
	out.SessionStatus = meta.SessionStatus
	out.PendingAction = meta.PendingAction
	return out, nil
}

// isControlFrame approximates uF().safeParse: control-channel payloads are
// the control_* event types; everything else with a session_id is transcript.
func isControlFrame(payload map[string]any) bool {
	t, _ := payload["type"].(string)
	switch t {
	case "control_request", "control_response", "control_cancel_request":
		return true
	}
	return false
}

// CodeSessionMeta is the Bx()/getCodeSession metadata Wfe merges into each
// poll — {session_status, pending_action, startup_failure}.
type CodeSessionMeta struct {
	SessionStatus  string          `json:"session_status"`
	PendingAction  json.RawMessage `json:"pending_action"`
	StartupFailure json.RawMessage `json:"startup_failure"`
}

// GetCodeSessionMeta: GET /v1/code/sessions/{id} — OAuth lane.
func (c *Client) GetCodeSessionMeta(ctx context.Context, sessionID string) (*CodeSessionMeta, error) {
	if err := checkID(sessionID, "sessionId"); err != nil {
		return nil, err
	}
	tok, err := c.oauthToken()
	if err != nil {
		return nil, err
	}
	h, err := c.codeSessionHeaders(tok)
	if err != nil {
		return nil, err
	}
	r, err := c.do(ctx, http.MethodGet,
		c.BaseURL+"/v1/code/sessions/"+codeSessionID(sessionID), nil, h, 10*time.Second)
	if err != nil {
		return nil, err
	}
	if r.Status != 200 {
		return nil, &eventsHTTPError{
			msg: fmt.Sprintf("Failed to fetch session: %d", r.Status)}
	}
	var wrap struct {
		CodeSessionMeta
		Session *CodeSessionMeta `json:"session"`
	}
	if err := json.Unmarshal(r.Body, &wrap); err != nil {
		return nil, fmt.Errorf("session metadata malformed: %w", err)
	}
	if wrap.Session != nil {
		return wrap.Session, nil
	}
	m := wrap.CodeSessionMeta
	return &m, nil
}

// ---------------------------------------------------------------------------
// jJt — the ExitPlanMode tracker.
// ---------------------------------------------------------------------------

// exitPlanToolName is gu (mod_0174.js): the tool whose tool_use/result pair
// carries the plan approval.
const exitPlanToolName = "ExitPlanMode"

// teleportLocalMarker is ilr: an is_error tool_result carrying this line
// means the user chose "send the plan back to the CLI".
const teleportLocalMarker = "__ULTRAPLAN_TELEPORT_LOCAL__"

// approvedPlanMarkers are the dlr() headings the remote's ExitPlanMode
// tool_result renders before the plan text.
var approvedPlanMarkers = []string{
	"## Approved Plan (edited by user):\n",
	"## Approved Plan:\n",
}

// ingestResult mirrors jJt.ingest's union:
// pending|approved|teleport|rejected|terminated|unchanged.
type ingestResult struct {
	kind    string // pending|approved|teleport|rejected|terminated|unchanged
	plan    string // approved|teleport
	id      string // rejected
	subtype string // terminated
}

// toolResult pairs a captured ExitPlanMode tool_result.
type toolResult struct {
	isError bool
	content string
}

// textOf mirrors GJt(): content is a string, or an array of {text} blocks.
func textOf(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	}
	return ""
}

// planAfterApprovedMarker is dlr(): plan text after an Approved Plan
// heading; missing marker is the extract_marker_missing failure.
func planAfterApprovedMarker(content string) (string, error) {
	for _, m := range approvedPlanMarkers {
		if i := strings.Index(content, m); i >= 0 {
			return strings.TrimRight(content[i+len(m):], " \t\n"), nil
		}
	}
	preview := content
	if len(preview) > 200 {
		preview = preview[:200]
	}
	return "", fmt.Errorf("ExitPlanMode approved but tool_result has no \"## Approved Plan:\" marker — "+
		"remote may have hit the empty-plan or isAgent branch. Content preview: %s", preview)
}

// teleportPlanAfterMarker is llr(): plan text after the local-exec marker.
func teleportPlanAfterMarker(content string) (string, bool) {
	m := teleportLocalMarker + "\n"
	i := strings.Index(content, m)
	if i < 0 {
		return "", false
	}
	return strings.TrimRight(content[i+len(m):], " \t\n"), true
}

// ultrapanTracker is the Go jJt: it tracks ExitPlanMode tool_use ids, their
// tool_results, rejections and terminal session results.
type ultrapanTracker struct {
	exitPlanCalls        []string
	results              map[string]toolResult
	rejectedIDs          map[string]bool
	terminated           string // result subtype, "" = alive
	rescanAfterRejection bool
	everSeenPending      bool
}

func newUltraplanTracker() *ultrapanTracker {
	return &ultrapanTracker{
		results:     map[string]toolResult{},
		rejectedIDs: map[string]bool{},
	}
}

func (t *ultrapanTracker) rejectCount() int { return len(t.rejectedIDs) }

// hasPendingPlan mirrors the getter: the newest unrejected ExitPlanMode call
// without a captured tool_result.
func (t *ultrapanTracker) hasPendingPlan() bool {
	for i := len(t.exitPlanCalls) - 1; i >= 0; i-- {
		id := t.exitPlanCalls[i]
		if t.rejectedIDs[id] {
			continue
		}
		_, ok := t.results[id]
		return !ok
	}
	return false
}

func eventString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// ingest ports jJt.ingest verbatim: scan the batch for ExitPlanMode
// tool_uses (assistant), their tool_results (user) and terminal results,
// then classify the newest unrejected call.
func (t *ultrapanTracker) ingest(events []SessionEvent) (ingestResult, error) {
	for _, ev := range events {
		switch eventString(ev, "type") {
		case "assistant":
			msg, _ := ev["message"].(map[string]any)
			content, _ := msg["content"].([]any)
			for _, b := range content {
				blk, _ := b.(map[string]any)
				if eventString(blk, "type") != "tool_use" {
					continue
				}
				if eventString(blk, "name") == exitPlanToolName {
					t.exitPlanCalls = append(t.exitPlanCalls, eventString(blk, "id"))
				}
			}
		case "user":
			msg, _ := ev["message"].(map[string]any)
			content, _ := msg["content"].([]any)
			for _, b := range content {
				blk, _ := b.(map[string]any)
				if eventString(blk, "type") != "tool_result" {
					continue
				}
				id := eventString(blk, "tool_use_id")
				if !contains(t.exitPlanCalls, id) {
					continue
				}
				isErr, _ := blk["is_error"].(bool)
				t.results[id] = toolResult{isError: isErr, content: textOf(blk["content"])}
			}
		case "result":
			if sub := eventString(ev, "subtype"); sub != "success" {
				t.terminated = sub
			}
		}
	}
	rescan := len(events) > 0 || t.rescanAfterRejection
	t.rescanAfterRejection = false
	var r *ingestResult
	if rescan {
		for i := len(t.exitPlanCalls) - 1; i >= 0; i-- {
			id := t.exitPlanCalls[i]
			if t.rejectedIDs[id] {
				continue
			}
			res, ok := t.results[id]
			if !ok {
				r = &ingestResult{kind: "pending"}
			} else if res.isError {
				if plan, hit := teleportPlanAfterMarker(res.content); hit {
					r = &ingestResult{kind: "teleport", plan: plan}
				} else {
					r = &ingestResult{kind: "rejected", id: id}
				}
			} else {
				plan, err := planAfterApprovedMarker(res.content)
				if err != nil {
					return ingestResult{}, err
				}
				r = &ingestResult{kind: "approved", plan: plan}
			}
			break
		}
		if r != nil && (r.kind == "approved" || r.kind == "teleport") {
			return *r, nil
		}
	}
	if r != nil && r.kind == "rejected" {
		t.rejectedIDs[r.id] = true
		t.rescanAfterRejection = true
	}
	if t.terminated != "" {
		return ingestResult{kind: "terminated", subtype: t.terminated}, nil
	}
	if r != nil && r.kind == "rejected" {
		return *r, nil
	}
	if r != nil && r.kind == "pending" {
		t.everSeenPending = true
		return *r, nil
	}
	return ingestResult{kind: "unchanged"}, nil
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// WJt — the ultraplan poll loop, and Fv — its error type.
// ---------------------------------------------------------------------------

// UltraplanPollError is Fv: the poll failure carries a machine reason, the
// plan-rejection count observed, and the event stats.
type UltraplanPollError struct {
	Reason      string // network_or_unknown|terminated|timeout_pending|timeout_no_plan|extract_marker_missing
	RejectCount int
	Message     string
}

func (e *UltraplanPollError) Error() string { return e.Message }

// UltraplanOutcome is WJt's return: the approved plan and where the user
// chose to execute it.
type UltraplanOutcome struct {
	Plan            string
	RejectCount     int
	ExecutionTarget string // "remote" | "local"
}

// DefaultUltraplanTimeout is T("tengu_ultraplan_timeout_seconds",5400)*1000.
const DefaultUltraplanTimeout = ultraplanTimeoutSec * time.Second

// PollUltraplan mirrors WJt(sessionId, timeoutMs, onPhase, shouldStop,
// credentials): poll events + session metadata every interval (HJt=3s),
// report phase transitions running -> needs_input -> plan_ready, and return
// once an ExitPlanMode result lands. timeout <= 0 uses the 5400s flag
// default; interval <= 0 uses 3s. shouldStop is real's K.get(taskId)
// !=="running" check — a nil shouldStop only honors ctx.
func (c *Client) PollUltraplan(ctx context.Context, sessionID string, timeout, interval time.Duration, onPhase func(phase string), shouldStop func() bool) (*UltraplanOutcome, error) {
	if timeout <= 0 {
		timeout = DefaultUltraplanTimeout
	}
	if interval <= 0 {
		interval = ultraplanPollInterval
	}
	deadline := time.Now().Add(timeout)
	tr := newUltraplanTracker()
	lastEventID := ""
	consecErrs := 0
	phase := "running"
	for time.Now().Before(deadline) {
		if shouldStop != nil && shouldStop() {
			return nil, fmt.Errorf("poll stopped by caller")
		}
		var events []SessionEvent
		var status string
		res, err := c.FetchSessionEvents(ctx, sessionID, lastEventID, false)
		if err != nil {
			if !IsRetriablePollErr(err) {
				return nil, &UltraplanPollError{
					Reason: "network_or_unknown", RejectCount: tr.rejectCount(),
					Message: err.Error()}
			}
			consecErrs++
			if consecErrs >= ultraplanPollRetries {
				return nil, &UltraplanPollError{
					Reason: "network_or_unknown", RejectCount: tr.rejectCount(),
					Message: "Lost connection to the cloud session after repeated retries — " +
						"the session may still be running"}
			}
			if !sleepCtx(ctx, interval) {
				return nil, ctx.Err()
			}
			continue
		}
		events = res.NewEvents
		lastEventID = res.LastEventID
		status = res.SessionStatus
		consecErrs = 0
		got, err := tr.ingest(events)
		if err != nil {
			return nil, &UltraplanPollError{
				Reason: "extract_marker_missing", RejectCount: tr.rejectCount(),
				Message: err.Error()}
		}
		switch got.kind {
		case "approved":
			return &UltraplanOutcome{Plan: got.plan,
				RejectCount: tr.rejectCount(), ExecutionTarget: "remote"}, nil
		case "teleport":
			return &UltraplanOutcome{Plan: got.plan,
				RejectCount: tr.rejectCount(), ExecutionTarget: "local"}, nil
		case "terminated":
			return nil, &UltraplanPollError{
				Reason: "terminated", RejectCount: tr.rejectCount(),
				Message: fmt.Sprintf("cloud session ended (%s) before plan approval", got.subtype)}
		}
		idle := (status == "idle" || status == "requires_action") && len(events) == 0
		next := "running"
		if tr.hasPendingPlan() {
			next = "plan_ready"
		} else if idle {
			next = "needs_input"
		}
		if next != phase {
			c.debugf("[ultraplan] phase %s → %s", phase, next)
			phase = next
			if onPhase != nil {
				onPhase(phase)
			}
		}
		if !sleepCtx(ctx, interval) {
			return nil, ctx.Err()
		}
	}
	minutes := int(timeout.Minutes() + 0.5)
	word := "minutes"
	if minutes == 1 {
		word = "minute"
	}
	reason := "timeout_no_plan"
	msg := fmt.Sprintf("ExitPlanMode never reached after %d %s (the remote container failed to start, or session ID mismatch?)",
		minutes, word)
	if tr.everSeenPending {
		reason = "timeout_pending"
		msg = fmt.Sprintf("no approval after %d %s", minutes, word)
	}
	return nil, &UltraplanPollError{
		Reason: reason, RejectCount: tr.rejectCount(), Message: msg}
}

// RemoteAgentOutcome is the ultrareview poll's return: the terminal result
// subtype plus the last assistant text (real's wJo summary source).
type RemoteAgentOutcome struct {
	Subtype string // result.subtype
	Text    string
}

// PollRemoteAgent polls a remote-agent (ultrareview) session's events until
// a result event lands, the session archives, or the timeout hits. Mirrors
// vLr (mod_0386.js): events arrive via Wfe; archived/failed statuses and
// repeated fetch failures end the wait.
func (c *Client) PollRemoteAgent(ctx context.Context, sessionID string, timeout, interval time.Duration, shouldStop func() bool) (*RemoteAgentOutcome, error) {
	if timeout <= 0 {
		timeout = DefaultUltraplanTimeout
	}
	if interval <= 0 {
		interval = ultraplanPollInterval
	}
	deadline := time.Now().Add(timeout)
	lastEventID := ""
	consecErrs := 0
	var lastText string
	for time.Now().Before(deadline) {
		if shouldStop != nil && shouldStop() {
			return nil, fmt.Errorf("poll stopped by caller")
		}
		res, err := c.FetchSessionEvents(ctx, sessionID, lastEventID, false)
		if err != nil {
			if !IsRetriablePollErr(err) {
				return nil, err
			}
			consecErrs++
			if consecErrs >= ultraplanPollRetries {
				return nil, fmt.Errorf("Lost connection to the cloud session after repeated retries — " +
					"the session may still be running")
			}
			if !sleepCtx(ctx, interval) {
				return nil, ctx.Err()
			}
			continue
		}
		consecErrs = 0
		lastEventID = res.LastEventID
		for _, ev := range res.NewEvents {
			switch eventString(ev, "type") {
			case "assistant":
				msg, _ := ev["message"].(map[string]any)
				if t := textOf(msg["content"]); t != "" {
					lastText = t
				}
			case "result":
				return &RemoteAgentOutcome{
					Subtype: eventString(ev, "subtype"), Text: lastText}, nil
			}
		}
		if res.SessionStatus == "archived" {
			return nil, fmt.Errorf("cloud session was archived before producing output")
		}
		if !sleepCtx(ctx, interval) {
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("cloud session exceeded %s", timeout.Round(time.Minute))
}

// UltraplanPhaseText maps the poll phase to real's status line:
// running -> "ultraplan", needs_input -> "ultraplan needs your input",
// plan_ready -> "ultraplan ready".
func UltraplanPhaseText(phase string) string {
	switch phase {
	case "needs_input":
		return "ultraplan needs your input"
	case "plan_ready":
		return "ultraplan ready"
	default:
		return "ultraplan"
	}
}

// ---------------------------------------------------------------------------
// uRe — RemoteAgentTask: a cloud-session-backed task whose kill archives.
// ---------------------------------------------------------------------------

// RemoteAgentTask mirrors the uRe task record: type "remote_agent" with a
// remoteTaskType ("ultraplan"|"ultrareview"|"remote-agent"); kill() marks
// the task stopped and archives the cloud session (tR).
type RemoteAgentTask struct {
	TaskID    string
	Kind      string // remoteTaskType
	SessionID string
	Title     string
	Command   string
	client    *Client
}

// NewRemoteAgentTask registers the session-backed task (aRe).
func (c *Client) NewRemoteAgentTask(kind, sessionID, title, command string) *RemoteAgentTask {
	return &RemoteAgentTask{
		TaskID:    "ragent_" + newUUIDv4(),
		Kind:      kind,
		SessionID: sessionID,
		Title:     title,
		Command:   command,
		client:    c,
	}
}

// Kill is uRe.kill's archive half: POST .../archive, 409 tolerated.
func (t *RemoteAgentTask) Kill(ctx context.Context) error {
	if t.client == nil || t.SessionID == "" {
		return nil
	}
	return t.client.ArchiveSession(ctx, t.SessionID)
}

// ---------------------------------------------------------------------------
// Post-review-comment — BQo/VQo/f8t/YQo (mod_0386.js).
// POST /v1/code/github/post-review-comment {session_id}.
// ---------------------------------------------------------------------------

// ReviewPostOptions tunes VQo's retry budget. Zero values use real's:
// 90s deadline (p8t), 5s floor wait (HQo), 10s cap (jQo), 20s headroom (GQo).
type ReviewPostOptions struct {
	Deadline   time.Duration
	MinWait    time.Duration
	MaxWait    time.Duration
	Headroom   time.Duration
	RetryAfter func(headers http.Header) time.Duration // optional override
	Now        func() time.Time                        // test hook
	Sleep      func(time.Duration)                     // test hook
}

// ReviewPostResult is i3n's outcome: posted (comment_url, already_posted)
// or not-posted with the f8t kind and YQo reason text.
type ReviewPostResult struct {
	Posted        bool
	CommentURL    string
	AlreadyPosted bool
	Kind          string // posted|unconfirmed|or the f8t kind
	Status        int
	ServerMessage string
	Reason        string // YQo user-facing explanation ("" when posted)
}

// reviewError parses KQo: {error:{message,reason,details,resource_type}}.
type reviewError struct {
	Message      string `json:"message"`
	Reason       string `json:"reason"`
	Details      string `json:"details"`
	ResourceType string `json:"resource_type"`
}

func parseReviewError(body []byte) *reviewError {
	var v struct {
		Error *reviewError `json:"error"`
	}
	if json.Unmarshal(body, &v) != nil {
		return nil
	}
	return v.Error
}

// reviewRefusalKind is f8t(status, error): the refusal classification.
func reviewRefusalKind(status int, e *reviewError) string {
	switch status {
	case 400:
		switch reason(e) {
		case "findings_unavailable", "no_pull_request", "unsupported_host",
			"review_failed", "withheld_by_screen", "github_rejected":
			return reason(e)
		}
		return "unexpected_response"
	case 401:
		switch reason(e) {
		case "github_not_connected", "github_token_rejected":
			return reason(e)
		}
		if strings.HasPrefix(reason(e), "github_") {
			return "github_not_connected"
		}
		return "sign_in_rejected"
	case 403:
		return "permission_denied"
	case 404:
		if e != nil {
			switch e.ResourceType {
			case "review_session":
				return "session_not_found"
			case "github_pull_request":
				return "pull_request_not_found"
			}
		}
		return "route_unavailable"
	case 409:
		if e != nil && e.Details == "findings_pending" {
			return "findings_pending"
		}
		return "check_the_pr"
	case 429:
		return "rate_limited"
	default:
		if status >= 500 {
			return "unavailable"
		}
		return "unexpected_response"
	}
}

func reason(e *reviewError) string {
	if e == nil {
		return ""
	}
	return e.Reason
}

// reviewPostReason is YQo(kind,status,serverMessage): the user-facing text
// for each refusal kind.
func reviewPostReason(kind string, status int, serverMessage string) string {
	const eh = "The findings are above if you want to post them by hand."
	const checkPR = "Check the pull request before posting by hand — if the request got through, the comment may still appear."
	suffix := func() string {
		if serverMessage == "" {
			return ""
		}
		return " (" + serverMessage + ")"
	}
	switch kind {
	case "findings_pending":
		return "The review's findings weren't ready on the server yet, so nothing went to the pull request. " + eh
	case "findings_unavailable":
		return "This review ran on a path that doesn't keep its findings for posting, so nothing went to the pull request. " + eh
	case "no_pull_request":
		return "This review wasn't of a pull request, so there is nowhere to post the findings."
	case "unsupported_host":
		return "Posting is available for github.com pull requests only, so nothing was posted. " + eh
	case "review_failed":
		return "The server recorded this review as incomplete, so nothing went to the pull request. " + eh
	case "withheld_by_screen":
		return "The comment was withheld by a content safety check (a finding matched a secret-like or unsafe pattern), so nothing went to the pull request. " + eh
	case "github_rejected":
		return "GitHub didn't accept the comment, so nothing was posted. " + eh
	case "check_the_pr":
		return "An earlier attempt to post this review may already have gone through, so it wasn't posted again. Check the pull request before posting by hand."
	case "github_not_connected":
		return "Posting needs a GitHub account connected to your Claude account, and none is connected (or the connection expired). Connect one at https://claude.ai/settings/integrations to post next time; the findings are above if you want to post them by hand now."
	case "github_token_rejected":
		return "GitHub no longer accepts the connection stored for your account, so nothing was posted. Reconnect GitHub at https://claude.ai/settings/integrations to post next time; the findings are above if you want to post them by hand now."
	case "sign_in_rejected":
		return "The server didn't accept this session's Claude sign-in for the post, so nothing went to the pull request. Run /login to fix that for next time; the findings are above if you want to post them by hand now."
	case "session_not_found":
		return "The server couldn't find this review session under your account, so nothing went to the pull request. " + eh
	case "pull_request_not_found":
		return "GitHub couldn't find the pull request with your account (it may be private to you, deleted, or moved), so nothing was posted. " + eh
	case "permission_denied":
		return "Posting this comment isn't allowed for your account" + suffix() + ", so nothing went to the pull request. " + eh
	case "rate_limited":
		return "Posting is being rate-limited right now, so nothing went to the pull request. " + eh
	case "unavailable":
		return "Posting is temporarily unavailable on the server, so nothing went to the pull request. " + eh
	case "route_unavailable":
		return "This version of the server can't post review comments yet, so nothing went to the pull request. " + eh
	case "unexpected_response":
		m := serverMessage
		if m == "" {
			m = fmt.Sprintf("HTTP %d", status)
		}
		return fmt.Sprintf("The post didn't go through (%s), so nothing went to the pull request. %s", m, eh)
	case "unconfirmed":
		return "The server accepted the post but didn't say where the comment is. Check the pull request before posting by hand."
	case "disabled":
		return "Posting to the pull request is currently turned off, so nothing went to the PR. " + eh
	case "not_logged_in":
		return "Posting needs a working Claude sign-in here" + suffix() + ", so nothing went to the pull request. Run /login to fix that for next time; the findings are above if you want to post them by hand now."
	case "data_residency":
		return "Posting goes through Claude Code cloud sessions, which are unavailable on third-party providers."
	case "essential_traffic_only":
		return "Posting goes through Claude Code cloud sessions, which are unavailable while essential-traffic-only mode is active."
	case "not_sent":
		return "The post couldn't be sent (the request never reached the server), so nothing went to the pull request. " + eh
	case "no_answer":
		return "The post couldn't be confirmed (no answer came back from the server). " + checkPR
	case "deadline":
		return "The post took too long to confirm, so the findings are here without a confirmed post. " + checkPR
	}
	return kind
}

// PostReviewComment mirrors VQo: POST /v1/code/github/post-review-comment
// {session_id} on the OAuth lane; retries while the server answers
// findings_pending (429-style wait honoring retry-after, capped) until the
// 90s deadline. 401/403 route to raise() verdicts where useful — otherwise
// the f8t kind + server message is reported verbatim.
func (c *Client) PostReviewComment(ctx context.Context, sessionID string, o *ReviewPostOptions) *ReviewPostResult {
	if err := checkID(sessionID, "sessionId"); err != nil {
		return &ReviewPostResult{Kind: "not_sent", Reason: err.Error()}
	}
	opts := ReviewPostOptions{
		Deadline: 90 * time.Second,
		MinWait:  5 * time.Second,
		MaxWait:  10 * time.Second,
		Headroom: 20 * time.Second,
		Now:      time.Now,
		Sleep:    time.Sleep,
	}
	if o != nil {
		if o.Deadline > 0 {
			opts.Deadline = o.Deadline
		}
		if o.MinWait > 0 {
			opts.MinWait = o.MinWait
		}
		if o.MaxWait > 0 {
			opts.MaxWait = o.MaxWait
		}
		if o.Headroom > 0 {
			opts.Headroom = o.Headroom
		}
		if o.RetryAfter != nil {
			opts.RetryAfter = o.RetryAfter
		}
		if o.Now != nil {
			opts.Now = o.Now
		}
		if o.Sleep != nil {
			opts.Sleep = o.Sleep
		}
	}
	start := opts.Now()
	for {
		tok, err := c.oauthToken()
		if err != nil {
			return &ReviewPostResult{Kind: "not_logged_in",
				Reason: reviewPostReason("not_logged_in", 0, "")}
		}
		h, herr := c.codeSessionHeaders(tok)
		if herr != nil {
			return &ReviewPostResult{Kind: "not_sent", Reason: herr.Error()}
		}
		r, err := c.do(ctx, http.MethodPost,
			c.BaseURL+"/v1/code/github/post-review-comment",
			map[string]any{"session_id": sessionID}, h, 60*time.Second)
		if err != nil {
			return &ReviewPostResult{Kind: "no_answer",
				Reason: reviewPostReason("no_answer", 0, "")}
		}
		if r.Status >= 200 && r.Status < 300 {
			var ok struct {
				CommentURL    string `json:"comment_url"`
				AlreadyPosted bool   `json:"already_posted"`
			}
			if json.Unmarshal(r.Body, &ok) != nil || ok.CommentURL == "" {
				return &ReviewPostResult{Kind: "unconfirmed", Status: r.Status,
					Reason: reviewPostReason("unconfirmed", r.Status, "")}
			}
			return &ReviewPostResult{Posted: true, Kind: "posted",
				CommentURL: ok.CommentURL, AlreadyPosted: ok.AlreadyPosted,
				Status: r.Status}
		}
		e := parseReviewError(r.Body)
		kind := reviewRefusalKind(r.Status, e)
		msg := ""
		if e != nil {
			msg = e.Message
		}
		out := &ReviewPostResult{Kind: kind, Status: r.Status, ServerMessage: msg}
		if kind != "findings_pending" {
			out.Reason = reviewPostReason(kind, r.Status, msg)
			return out
		}
		// findings_pending: wait retry-after (floor 5s, cap 10s) and retry
		// while the 90s budget holds.
		wait := opts.MinWait
		if opts.RetryAfter != nil {
			if d := opts.RetryAfter(r.Headers); d > 0 {
				wait = d
			}
		} else if ra := r.Headers.Get("retry-after"); ra != "" {
			if secs, perr := strconv.ParseFloat(ra, 64); perr == nil && secs > 0 {
				wait = time.Duration(secs * float64(time.Second))
			}
		}
		if wait > opts.MaxWait {
			wait = opts.MaxWait
		}
		if opts.Now().Sub(start)+wait+opts.Headroom > opts.Deadline {
			out.Reason = reviewPostReason(kind, r.Status, msg)
			return out
		}
		opts.Sleep(wait)
	}
}
