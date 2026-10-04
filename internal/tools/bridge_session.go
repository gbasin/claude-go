// Bridge session ID on the sessions/<pid>.json registry entry — Remote
// Control adoption stamps bridgeSessionId (read by the teleport gate).
// Stored in the registry file by read-modify-write so it survives whatever
// else wrote the entry.
package tools

import (
	"encoding/json"
	"os"
	"sync/atomic"
)

var bridgeSessionID atomic.Value // string

// SetBridgeSessionID records the bridge session ID ("" clears) and rewrites
// this session's registry file with/without the bridgeSessionId field.
func SetBridgeSessionID(id string) {
	bridgeSessionID.Store(id)
	p := sessionFile(os.Getpid())
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return
	}
	if id == "" {
		delete(m, "bridgeSessionId")
	} else {
		m["bridgeSessionId"] = id
	}
	if out, err := json.MarshalIndent(m, "", "  "); err == nil {
		os.WriteFile(p, out, 0o644)
	}
}

// currentBridgeSessionID reports the adopted bridge session, if any.
func currentBridgeSessionID() string {
	if v := bridgeSessionID.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// BridgeSessionID exposes the adopted bridge session to the /teleport
// gate (mod_2173.js: a?.bridgeSessionId on the app-state bridge record).
func BridgeSessionID() string { return currentBridgeSessionID() }
