// Package bridge implements the client-verifiable wire surface of Claude
// Code's Remote Control feature — the cloud bridge that lets claude.ai/code
// and the mobile app reach sessions running on this machine.
//
// Sources (extracted from Claude Code 2.1.288 bundle):
//   - mod_1258.js — BridgeAPI: environment register/poll/ack/stop/heartbeat,
//     bridge/reconnect, archive; error mapping + 401-refresh retry wrapper.
//   - mod_0829.js — code-session CRUD: POST /v1/code/sessions,
//     POST /v1/code/sessions/{id}/bridge (worker credential mint),
//     archive/unarchive (v2 cse_ + v1 session_ id paths).
//   - mod_1211.js — tengu_bridge_poll_interval_config defaults, work-secret
//     (Ute) decode, session URL (mAe), worker/register (Wqe).
//   - mod_1215.js — tengu_bridge_repl_v2_config defaults, connect/reattach
//     flow, min-version floor, user-facing strings.
//   - mod_1269.js — `claude remote-control` server: pacing constants, poll
//     loop, heartbeat "token expired -> bridge/reconnect" path, shutdown.
//   - mod_0690.js — CLI gate order (disableRemoteControl -> OAuth ->
//     diagnosis -> min_version -> allow_remote_control org policy).
//   - mod_0265.js / mod_0259.js / mod_1589.js — message constants, policy keys.
//   - mod_2173.js — teleport gate on the session-file bridgeSessionId field.
//   - mod_1214.js — CCR v2 worker transport (SSE /worker/events/stream +
//     /worker/* POSTs). The data-plane frame protocol inside the stream is
//     backend-gated protobuf/JSON we can't fully verify; see transport.go.
package bridge

import (
	"errors"
	"fmt"
	"regexp"
)

// ---------------------------------------------------------------------------
// User-facing strings (verbatim from the bundle).
// ---------------------------------------------------------------------------

// PIe (mod_0265.js): suffix real appends to auth failures and throws when no
// OAuth access token is available.
const ErrNoOAuthToken = "Remote Control is only available with claude.ai subscriptions. " +
	"Please use `/login` to sign in with your claude.ai account."

// E_t / BRIDGE_LOGIN_ERROR (mod_0265.js): `claude remote-control` refuses
// before any network traffic when there's no stored OAuth token.
const ErrLoginRequired = "Error: You must be logged in to use Remote Control.\n\n" +
	"Remote Control is only available with claude.ai subscriptions. " +
	"Run `claude auth login` to sign in with your claude.ai account."

// o8 (mod_0259.js): settings.disableRemoteControl === true (any tier).
const ErrDisabledManaged = "Remote Control is disabled by your organization's policy " +
	"(managed setting `disableRemoteControl`)."

// Org-policy refusal fallback — the 4th arg real passes to
// policyDeniedReason("allow_remote_control", "Remote Control", "is", …)
// in mod_0690.js.
const ErrOrgPolicyDenied = "Remote Control is disabled by your organization's policy."

// C_t (mod_0265.js): server answered with a shape we don't understand.
const ErrMalformedResponse = "Remote Control got an unexpected server response — " +
	"try again after updating Claude Code"

// v_t (mod_0265.js): error.type on the 409 from POST /v1/environments/bridge.
const ErrTypeFolderAlreadyServed = "bridge_folder_already_served"

// fr() (mod_1269.js): suffix appended to fatal poll-loop errors.
func reconnectHint(msg string) string {
	return msg + ". Re-run `claude remote-control` to reconnect."
}

// ---------------------------------------------------------------------------
// Session-id helpers (mod_0090.js).
// ---------------------------------------------------------------------------

// idRe is real's _B/a: `/^[a-zA-Z0-9_-]+$/`. Environment, work, and session
// ids are validated before being spliced into URL paths (Nh()).
var idRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// checkID mirrors Nh(e, name): reject ids that would break path encoding.
func checkID(id, name string) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("invalid %s: contains unsafe characters", name)
	}
	return nil
}

// codeSessionID mirrors mc(): session_X -> cse_X; anything else passes
// through unchanged (v2 API form).
func codeSessionID(id string) string {
	if len(id) > 8 && id[:8] == "session_" {
		return "cse_" + id[8:]
	}
	return id
}

// legacySessionID mirrors Zd(): cse_X -> session_X (v1 API form).
func legacySessionID(id string) string {
	if len(id) > 4 && id[:4] == "cse_" {
		return "session_" + id[4:]
	}
	return id
}

// bareSessionID mirrors sr(): strip a leading session_/cse_ prefix so the
// two id forms compare equal.
func bareSessionID(id string) string {
	for _, p := range []string{"session_", "cse_"} {
		if len(id) > len(p) && id[:len(p)] == p {
			return id[len(p):]
		}
	}
	return id
}

// SessionURL mirrors mAe(): the worker-facing URL for a code session,
// `${apiBase}/v1/code/sessions/{cse_id}`.
func SessionURL(apiBase, sessionID string) string {
	for len(apiBase) > 0 && apiBase[len(apiBase)-1] == '/' {
		apiBase = apiBase[:len(apiBase)-1]
	}
	return apiBase + "/v1/code/sessions/" + codeSessionID(sessionID)
}

// ---------------------------------------------------------------------------
// Errors.
// ---------------------------------------------------------------------------

// FatalError mirrors zR / BridgeFatalError: a rejected (non-transient)
// response. Status is the HTTP code; ErrorType is the server's
// error.type when present.
type FatalError struct {
	Status    int
	ErrorType string
	Message   string
}

func (e *FatalError) Error() string { return e.Message }

// RateLimitError mirrors the 429 branch of A(): carries the Retry-After
// hint when the server sent one.
type RateLimitError struct {
	Status       int
	RetryAfterMs int64
	Message      string
}

func (e *RateLimitError) Error() string { return e.Message }

// statusError mirrors the `default:` branch of A() — a non-fatal failure
// that still carries an HTTP status (e.g. 5xx, or 4xx outside the mapped
// set such as 408/429 handled separately).
type statusError struct {
	Status  int
	Message string
}

func (e *statusError) Error() string { return e.Message }

// ErrNoToken is thrown by getAccessToken when there's nothing to send —
// real throws `Error(PIe)` which the classifier maps to "no_token".
var ErrNoToken = errors.New(ErrNoOAuthToken)

// errorKind mirrors S(): auth_failed | no_token | http_error |
// request_failed — used for the tengu_* telemetry error_type buckets.
func errorKind(err error) string {
	var fe *FatalError
	if errors.As(err, &fe) {
		if fe.Status == 401 {
			return "auth_failed"
		}
		return "http_error"
	}
	if errors.Is(err, ErrNoToken) {
		return "no_token"
	}
	var se *statusError
	var re *RateLimitError
	if errors.As(err, &se) || errors.As(err, &re) {
		return "http_error"
	}
	return "request_failed"
}

// fatalStatus mirrors D(): the 4xx statuses that kill a bridge loop.
// 408 and 429 are transient (retried); everything else 400-499 is fatal.
func fatalStatus(err error) (int, bool) {
	var fe *FatalError
	if errors.As(err, &fe) {
		return fe.Status, true
	}
	var se *statusError
	if errors.As(err, &se) && se.Status >= 400 && se.Status < 500 &&
		se.Status != 408 && se.Status != 429 {
		return se.Status, true
	}
	return 0, false
}

// isExpiredType mirrors Dst(): error types containing "expired" or
// "lifetime" mean the Remote Control session itself lapsed.
func isExpiredType(errorType string) bool {
	return errorType != "" &&
		(regexp.MustCompile(`expired`).MatchString(errorType) ||
			regexp.MustCompile(`lifetime`).MatchString(errorType))
}

// envGone mirrors Qt(): a 404/410 FatalError (or a "environment ... not
// found" message) means the registered environment no longer exists and
// must be re-registered in place.
func envGone(err error) bool {
	var fe *FatalError
	if errors.As(err, &fe) && (fe.Status == 404 || fe.Status == 410) {
		return true
	}
	return regexp.MustCompile(`(?i)environment .* not found`).MatchString(err.Error())
}
