package bridge

import (
	"fmt"
	"strconv"
	"strings"
)

// Entitlement gating, mirroring the `claude remote-control` preflight in
// mod_0690.js (O = h -> b -> C):
//
//   1. refuseRemoteControlLocally (h): settings.disableRemoteControl in
//      ANY tier (o8 message), then a stored OAuth token (E_t message).
//      Real also refuses API-key-only auth — cloud sessions need a
//      claude.ai account.
//   2. refuseRemoteControlIneligible (b): getBridgeDisabledDiagnosis()
//      (residency/provider diagnosis — not client-verifiable for us; the
//      parts we can reproduce are the same disableRemoteControl latch and
//      the org-policy verdict), then checkBridgeMinVersion()
//      (tengu_bridge_repl_v2_config min_version floor), then
//      policyDeniedReason("allow_remote_control","Remote Control","is",
//      ErrOrgPolicyDenied).
//   3. startRemoteControl (C): trusted-device preflight (backend-gated —
//      skipped, documented), then bridgeMain.
//
// What claude-go can verify locally is implemented; the org-policy verdict
// needs the server-fetched policy-limits cache (chunk-18yvrj3m +
// chunk-dbtyk3as, not extracted) and is injectable via OrgPolicyVerdict.

// OrgPolicyVerdict mirrors policyDeniedReason(key, label, verb, fallback):
// return the refusal string to deny, nil to allow. The default nil verdict
// (no policy-limits backend) permits — matching real's "no verdict cached,
// don't deny" behavior when the limits fetch hasn't landed.
var OrgPolicyVerdict = func(key, label, verb, fallback string) *string {
	return nil
}

// Org policy keys (mod_1589.js xe).
const (
	PolicyAllowRemoteControl  = "allow_remote_control"
	PolicyAllowRemoteSessions = "allow_remote_sessions"
)

// CheckDisabledLocally is refuseRemoteControlLocally's first half:
// disableRemoteControl===true in any settings tier -> the managed message.
func CheckDisabledLocally(disabled bool) error {
	if disabled {
		return fmt.Errorf("Error: %s", ErrDisabledManaged)
	}
	return nil
}

// CheckOAuth is the second half: a usable claude.ai OAuth token must exist.
// API-key credentials do not satisfy it ("API key authentication is not
// sufficient" — mod_0132.js).
func CheckOAuth(hasOAuth bool) error {
	if !hasOAuth {
		return fmt.Errorf("%s", ErrLoginRequired)
	}
	return nil
}

// CheckOrgPolicy is refuseRemoteControlIneligible's tail:
// policyDeniedReason("allow_remote_control", "Remote Control", "is",
// "Remote Control is disabled by your organization's policy.").
func CheckOrgPolicy() error {
	if reason := OrgPolicyVerdict(PolicyAllowRemoteControl,
		"Remote Control", "is", ErrOrgPolicyDenied); reason != nil {
		return fmt.Errorf("Error: %s", *reason)
	}
	return nil
}

// CheckMinVersion mirrors checkBridgeMinVersion / fQt(): remote config
// min_version floors the CLI version. Returns "" when OK.
func CheckMinVersion(cfg REPLConfig, version string) string {
	if cfg.MinVersion == "" || cfg.MinVersion == "0.0.0" {
		return ""
	}
	if semverLess(version, cfg.MinVersion) {
		return fmt.Sprintf(
			"Your version of Claude Code (%s) is too old for Remote Control.\n"+
				"Version %s or higher is required. Run `claude update` to update.",
			version, cfg.MinVersion)
	}
	return ""
}

// semverLess does the oO(version, floor) compare: dotted numeric triples.
func semverLess(v, floor string) bool {
	parse := func(s string) [3]int {
		var out [3]int
		parts := strings.SplitN(s, ".", 4)
		for i := 0; i < 3 && i < len(parts); i++ {
			// strip pre-release/build suffixes on the compared component
			p := parts[i]
			if j := strings.IndexAny(p, "-+"); j >= 0 {
				p = p[:j]
			}
			out[i], _ = strconv.Atoi(p)
		}
		return out
	}
	a, b := parse(v), parse(floor)
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// Gate composes the locally-verifiable checks in real's order. hasOAuth is
// "a stored claude.ai OAuth token exists" (not an API key). disabled is
// settings.disableRemoteControl===true from any tier.
func Gate(disabled, hasOAuth bool, cfg REPLConfig, version string) error {
	if err := CheckDisabledLocally(disabled); err != nil {
		return err
	}
	if err := CheckOAuth(hasOAuth); err != nil {
		return err
	}
	if v := CheckMinVersion(cfg, version); v != "" {
		return fmt.Errorf("%s", v)
	}
	return CheckOrgPolicy()
}
