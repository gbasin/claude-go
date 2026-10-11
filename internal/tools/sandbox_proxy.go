// Session-shared egress proxy for the Bash sandbox — the wiring counterpart
// of real CC's sandbox-runtime (SRT) launcher (mod_0222.js). One proxy per
// process, started lazily on the first sandboxed command whose
// sandbox.network block grants a destination; rules are refreshed on every
// call so later settings merges apply.
package tools

import (
	"fmt"
	"github.com/gbasin/claude-go/internal/netproxy"
	"sync"
)

var sessionProxy struct {
	mu     sync.Mutex
	px     *netproxy.Proxy
	caNote string // non-fatal CA problem (proxy still runs, sans MITM)
	err    error  // hard failure — do not retry every command
	inited bool
}

// sharedNetProxy returns the session proxy with rules applied from s. The
// string return is a non-fatal note (e.g. CA unavailable → CONNECT
// passthrough mode). A failed proxy is a conservative degradation: the
// caller emits a deny-all network profile instead.
func sharedNetProxy(s *SandboxSettings) (*netproxy.Proxy, string, error) {
	sessionProxy.mu.Lock()
	defer sessionProxy.mu.Unlock()
	if !sessionProxy.inited {
		sessionProxy.inited = true
		var extraCAs []string
		if s.Network != nil {
			extraCAs = s.Network.ExtraCACertPaths
		}
		ca, caErr := netproxy.LoadOrCreateCA(netproxy.CADir(), extraCAs...)
		if caErr != nil {
			// no CA → passthrough proxy: CONNECT authority is still
			// filtered, TLS payloads just aren't inspected.
			sessionProxy.caNote = fmt.Sprintf(
				"sandbox CA unavailable (%v); HTTPS runs in CONNECT passthrough mode (Host filtering only)", caErr)
		}
		px := netproxy.New(ca)
		// network.httpProxyPort/socksProxyPort pin the listener ports
		// (real's fixed-port proxy config; 0 = ephemeral)
		var hp, sp int
		if s.Network != nil {
			hp, sp = s.Network.HTTPProxyPort, s.Network.SOCKSProxyPort
		}
		if err := px.StartOn(hp, sp); err != nil {
			sessionProxy.err = err
		} else {
			sessionProxy.px = px
		}
	}
	if sessionProxy.px == nil {
		return nil, "", sessionProxy.err
	}
	n := s.Network
	if n == nil {
		n = &SandboxNetwork{}
	}
	sessionProxy.px.SetRules(netproxy.Rules{
		AllowedDomains:          n.effectiveAllowed(),
		DeniedDomains:           n.DeniedDomains,
		DeniedDomainReasons:     n.DeniedDomainReasons,
		AllowedCidrs:            n.AllowedCidrs,
		DeniedResolvedAddresses: n.DeniedResolvedAddresses,
		AllowAllLocal:           n.AllowAllLocal,
		StrictAllowlist:         n.StrictAllowlist,
		ParentProxy:             n.ParentProxy,
		IgnoreViolations:        s.IgnoreViolations,
	})
	return sessionProxy.px, sessionProxy.caNote, nil
}

// SandboxAllowHost backs `/sandbox allow <host>`: adds the host to the
// session proxy's allowlist so later sandboxed commands may reach it for
// the rest of the session.
func SandboxAllowHost(host string) string {
	sessionProxy.mu.Lock()
	defer sessionProxy.mu.Unlock()
	if sessionProxy.px == nil {
		return "no sandbox network proxy is running yet (it starts with the first networked sandboxed command)"
	}
	sessionProxy.px.AllowHost(host)
	return fmt.Sprintf("allowed %s for this session", host)
}

// sandboxViolations returns the proxy-recorded deny lines for command —
// real's fc.getViolationsForCommand feeding the <sandbox_violations> block.
func sandboxViolations(command string) []string {
	sessionProxy.mu.Lock()
	defer sessionProxy.mu.Unlock()
	if sessionProxy.px == nil {
		return nil
	}
	return sessionProxy.px.ViolationsFor(command)
}

// resetNetProxyForTest tears the singleton down between tests.
func resetNetProxyForTest() {
	sessionProxy.mu.Lock()
	defer sessionProxy.mu.Unlock()
	if sessionProxy.px != nil {
		sessionProxy.px.Close()
	}
	sessionProxy.px = nil
	sessionProxy.caNote = ""
	sessionProxy.err = nil
	sessionProxy.inited = false
}
