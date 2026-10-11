package tools

import (
	"strings"

	"github.com/gbasin/claude-go/internal/bridge"
	"github.com/gbasin/claude-go/internal/config"
)

// PolicyCheck mirrors real's sn(key) + lp(key,label,verb,fallback) pair for
// call sites that need the refusal text instead of a bool: consults the
// managed-settings deny surface (policyGate + managed permission deny
// rules) and the injectable bridge.OrgPolicyVerdict (real's server-fetched
// policy-limits verdict). Returns the refusal message, or nil when allowed.
//
// Keys seen in the bundle: "allow_remote_sessions" (cloud sessions,
// RemoteTrigger), "allow_routines" (JX, routines/RemoteTrigger),
// "allow_remote_control" (Remote Control).
func PolicyCheck(cwd, key string) *string {
	label, verb, fallback := policyKeyMeta(key)
	if !policyGate(cwd, key) || managedPolicyDenies(cwd, key) {
		msg := fallback
		return &msg
	}
	return bridge.OrgPolicyVerdict(key, label, verb, fallback)
}

// policyKeyMeta maps a policy key to the (label, verb, fallback) triple
// real passes to policyDeniedReason.
func policyKeyMeta(key string) (label, verb, fallback string) {
	switch key {
	case bridge.PolicyAllowRemoteSessions:
		return "Cloud sessions", "are", bridge.ErrCloudSessionsPolicyDenied
	case bridge.PolicyAllowRoutines:
		return "Routines", "are", bridge.ErrRoutinesPolicyDenied
	case bridge.PolicyAllowRemoteControl:
		return "Remote Control", "is", bridge.ErrOrgPolicyDenied
	}
	return key, "is", "This feature is disabled by your organization's policy."
}

// managedPolicyDenies reports whether a managed (highest-tier) permission
// deny rule names this policy key or its user-facing feature — e.g. a
// managed "RemoteTrigger" deny also closes allow_routines and
// allow_remote_sessions, matching real's policy-tier behavior where the
// tool is refused outright.
func managedPolicyDenies(cwd, key string) bool {
	s := config.Load(cwd)
	if s == nil {
		return false
	}
	aliases := map[string][]string{
		bridge.PolicyAllowRoutines:       {"RemoteTrigger"},
		bridge.PolicyAllowRemoteSessions: {"RemoteTrigger", "ultraplan", "ultrareview"},
	}
	for _, rule := range s.ManagedDeny {
		if rule == key {
			return true
		}
		for _, a := range aliases[key] {
			if strings.EqualFold(rule, a) {
				return true
			}
		}
	}
	return false
}
