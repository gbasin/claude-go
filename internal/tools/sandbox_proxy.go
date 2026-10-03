// Session-shared egress proxy for the Bash sandbox — the wiring counterpart
// of real CC's sandbox-runtime (SRT) launcher (mod_0222.js). One proxy per
// process, started lazily on the first sandboxed command whose
// sandbox.network block grants a destination; rules are refreshed on every
// call so later settings merges apply.
package tools

import (
	"claude-go/internal/netproxy"
	"fmt"
	"sync"
)

var sessionProxy struct {
	mu     sync.Mutex
	px     *netproxy.Proxy
	caNote string // non-fatal CA problem (proxy still runs, sans MITM)
	err    error  // hard failure — do not retry every command
	inited bool
}

// sharedNetProxy returns the session proxy with rules applied from n. The
// string return is a non-fatal note (e.g. CA unavailable → CONNECT
// passthrough mode). A failed proxy is a conservative degradation: the
// caller emits a deny-all network profile instead.
func sharedNetProxy(n *SandboxNetwork) (*netproxy.Proxy, string, error) {
	sessionProxy.mu.Lock()
	defer sessionProxy.mu.Unlock()
	if !sessionProxy.inited {
		sessionProxy.inited = true
		ca, caErr := netproxy.LoadOrCreateCA(netproxy.CADir())
		if caErr != nil {
			// no CA → passthrough proxy: CONNECT authority is still
			// filtered, TLS payloads just aren't inspected.
			sessionProxy.caNote = fmt.Sprintf(
				"sandbox CA unavailable (%v); HTTPS runs in CONNECT passthrough mode (Host filtering only)", caErr)
		}
		px := netproxy.New(ca)
		if err := px.Start(); err != nil {
			sessionProxy.err = err
		} else {
			sessionProxy.px = px
		}
	}
	if sessionProxy.px == nil {
		return nil, "", sessionProxy.err
	}
	sessionProxy.px.SetRules(netproxy.Rules{
		AllowedDomains: n.AllowedDomains,
		DeniedDomains:  n.DeniedDomains,
		AllowedCidrs:   n.AllowedCidrs,
		AllowAllLocal:  n.AllowAllLocal,
	})
	return sessionProxy.px, sessionProxy.caNote, nil
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
