package api

import (
	"net/http"
	"regexp"
	"strings"
)

// ErrAPI is a typed API error. Type mirrors the Anthropic error envelope;
// when the body carries no type it is mapped from the HTTP status (the
// gateway error map in mod_1351.js: 400→invalid_request_error, etc.).
type ErrAPI struct {
	E          *APIError
	StatusCode int
	Headers    http.Header
}

func (e *ErrAPI) Error() string {
	if e.E != nil {
		if e.StatusCode != 0 {
			return e.E.Type + " (" + http.StatusText(e.StatusCode) + "): " + e.E.Message
		}
		return e.E.Type + ": " + e.E.Message
	}
	return "api error"
}

// Type returns the error type, falling back to the status-code mapping when
// the response body had no error.type.
func (e *ErrAPI) Type() string {
	if e != nil && e.E != nil && e.E.Type != "" {
		return e.E.Type
	}
	return errorTypeForStatus(e.StatusCode)
}

// errorTypeForStatus is the shared status→type map (mod_1351.js KL).
func errorTypeForStatus(code int) string {
	switch code {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 501:
		return "not_supported"
	case 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// SanitizeHint tells the caller which part of the request a 400 points at,
// so it can strip the offending content and retry. Mirrors the 400-error
// classifier cluster in mod_0386.js (bEt/dMe/YO/X0/D2/SEt/hMe/Tne/...).
type SanitizeHint string

const (
	SanitizeNone        SanitizeHint = ""
	SanitizeThinking    SanitizeHint = "thinking"             // thinking/redacted_thinking blocks (bad signature, block_binding, display)
	SanitizeConvBinding SanitizeHint = "conversation_binding" // blocks "bound to a different conversation"
	SanitizeAdvisor     SanitizeHint = "advisor_tool_result"  // advisor_tool_result content blocks
	SanitizeEncrypted   SanitizeHint = "encrypted_content"    // encrypted web-search / code-execution content
	SanitizeClearAt     SanitizeHint = "clear_at"             // turn-scoped system message clear_at
	SanitizeSafeguards  SanitizeHint = "safeguards"           // safeguards field rejected
	SanitizeOutputCfg   SanitizeHint = "output_config"        // output_config / structured outputs
	SanitizeMidConv     SanitizeHint = "mid_conversation"     // mid-conversation system/tool changes
	SanitizeBetaFlag    SanitizeHint = "invalid_beta_flag"    // unrecognized anthropic-beta value
)

// --- classifiers (mod_0386.js) ---

// reClearAt is heo: clear_at / turn-scoped system message field errors.
var reClearAt = regexp.MustCompile(
	`^messages\.\d+(?:\.content\.\d+)?(?:\.clear_at:|: .*(?:turn-scoped system message|\(clear_at: |clear_at is only permitted))`)

// reSafeguardsPath is peo: message-path errors under a safeguards field —
// those are schema errors, not the strippable kind.
var reSafeguardsPath = regexp.MustCompile(`messages\.\d+\.content\.\d+(?:\.\w+)*\.safeguards\b`)

// reThinkingDisplay is TEt: thinking display field errors.
var reThinkingDisplay = regexp.MustCompile(`thinking\.(adaptive|enabled)\.display: Input should be `)

// reThinkingBlockBinding: thinking block_binding errors (mod_0386 hMe).
var reThinkingBlockBinding = regexp.MustCompile(
	`thinking\.(adaptive|enabled)\.block_binding(\.prefix_mismatch_behavior)?: Extra inputs are not permitted`)

// advisor tool result 400s (dMe).
func isAdvisorError(msg string) bool {
	return strings.Contains(msg, "Advisor tool result content could not be processed") ||
		strings.Contains(msg, "found in advisor_tool_result blocks")
}

// encrypted content 400s (YO/X0), matched on lowercased, backtick-stripped text.
var encryptedContentErrors = []string{
	"invalid encrypted_content in search_result block",
	"invalid encrypted_index in text block",
	"failed to decrypt web search result content",
	"invalid encrypted_stdout in encrypted_code_execution_result block", // _eo
}

func normMsg(s string) string { // wEt
	return strings.ReplaceAll(strings.ToLower(s), "`", "")
}

func isEncryptedContentError(msg string) bool {
	n := normMsg(msg)
	for _, p := range encryptedContentErrors {
		if strings.Contains(n, p) {
			return true
		}
	}
	return false
}

// conversation binding 400s (XO).
func isConversationBindingError(msg string) bool {
	n := strings.ToLower(msg)
	return strings.Contains(n, "not created in this conversation") ||
		strings.Contains(n, "bound to a different conversation")
}

// thinking-block 400s (XT/KFr "thinking_signature", TEt, hMe, Pne's
// thinking_type:enabled|adaptive).
func isThinkingError(msg string) bool {
	if reThinkingDisplay.MatchString(msg) || reThinkingBlockBinding.MatchString(msg) {
		return true
	}
	n := normMsg(msg)
	if strings.Contains(n, "thinking") && strings.Contains(n, "signature") {
		return true // e.g. "Invalid signature in thinking block"
	}
	if strings.Contains(n, "thinking.type") &&
		(strings.Contains(n, "enabled") || strings.Contains(n, "adaptive")) {
		return true
	}
	if strings.Contains(n, "redacted_thinking") {
		return true
	}
	return false
}

// clear_at / turn-scoped system message 400s (bEt).
func isClearAtError(msg string) bool {
	if reClearAt.MatchString(msg) {
		return true
	}
	return strings.HasPrefix(msg, "Unexpected value(s) ") &&
		strings.Contains(msg, "clear_at")
}

// safeguards 400s (SEt): the word appears but the failing path is not a
// per-block safeguards field.
func isSafeguardsError(msg string) bool {
	if !regexp.MustCompile(`\bsafeguards\b`).MatchString(msg) {
		return false
	}
	return !reSafeguardsPath.MatchString(msg)
}

// Sanitizable returns a hint for which request content the caller may strip
// and retry, or SanitizeNone. Only 400 invalid-request errors are
// classifiable (mod_0386.js classifiers all gate on status===400).
func (e *ErrAPI) Sanitizable() SanitizeHint {
	if e == nil || e.E == nil || e.StatusCode != 400 {
		return SanitizeNone
	}
	msg := strings.TrimPrefix(e.E.Message, "400 ")
	switch {
	case isAdvisorError(msg):
		return SanitizeAdvisor
	case isEncryptedContentError(msg):
		return SanitizeEncrypted
	case isThinkingError(msg) && !isConversationBindingError(msg):
		return SanitizeThinking
	case isConversationBindingError(msg):
		return SanitizeConvBinding
	case isClearAtError(msg):
		return SanitizeClearAt
	case isSafeguardsError(msg):
		return SanitizeSafeguards
	case strings.Contains(msg, "output_config") ||
		strings.Contains(normMsg(msg), "structured_outputs_unsupported"):
		return SanitizeOutputCfg
	case strings.Contains(normMsg(msg), "mid_conv_system") ||
		strings.Contains(normMsg(msg), "mid-conversation"):
		return SanitizeMidConv
	case strings.Contains(normMsg(msg), "invalid beta flag"): // Tne
		return SanitizeBetaFlag
	default:
		return SanitizeNone
	}
}
