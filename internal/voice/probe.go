package voice

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Voice-stream connectivity probe — the reachable-check side of real's
// iwr (probeVoiceConnectivity). Real GETs ${BASE_API_URL}/api/hello with
// a 1500ms timeout purely to classify post-failure telemetry; here the
// probe is a short-lived ws connect+close against the stream URL, cached
// ~5 minutes, so the controller can surface "voice unavailable" before a
// recording starts instead of failing mid-record.
//
// Reachability means "the endpoint answered": a successful upgrade OR any
// HTTP response (401/403/5xx) counts — the stream auth failure is a
// different error class than "can't reach the service". Transport
// errors and timeouts mark the endpoint unreachable.
var (
	probeTimeout = 1500 * time.Millisecond // real's N
	probeTTL     = 5 * time.Minute

	// probeDial is the ws dial seam — tests stub it.
	probeDial = func(u string) (reachable bool, detail string) {
		d := &websocket.Dialer{
			HandshakeTimeout: probeTimeout,
			Proxy:            http.ProxyFromEnvironment,
			TLSClientConfig:  &tls.Config{MinVersion: tls.VersionTLS12},
		}
		conn, resp, err := d.Dial(u, nil)
		if err == nil {
			conn.Close()
			return true, "ok"
		}
		if resp != nil {
			if resp.Header.Get("cf-mitigated") != "" {
				return true, fmt.Sprintf("cf_mitigated_%d", resp.StatusCode)
			}
			return true, fmt.Sprintf("http_%d", resp.StatusCode)
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return false, "timeout"
		}
		return false, "fetch_failed"
	}
)

var probeCache struct {
	mu     sync.Mutex
	url    string
	ok     bool
	detail string
	at     time.Time
}

// probeStream is the raw (uncached) classifier — iwr's return shape.
func probeStream(u string) (bool, string) {
	return probeDial(u)
}

// StreamReachable reports whether the voice stream endpoint at baseURL is
// reachable, caching the verdict per URL for probeTTL (~5min).
func StreamReachable(baseURL string) bool {
	ok, _ := streamProbeResult(StreamURL(baseURL, nil))
	return ok
}

// streamProbeResult returns the cached or freshly probed verdict plus
// its iwr-style detail string.
func streamProbeResult(u string) (bool, string) {
	probeCache.mu.Lock()
	defer probeCache.mu.Unlock()
	if probeCache.url == u && time.Since(probeCache.at) < probeTTL {
		return probeCache.ok, probeCache.detail
	}
	ok, detail := probeStream(u)
	probeCache.url = u
	probeCache.ok = ok
	probeCache.detail = detail
	probeCache.at = time.Now()
	return ok, detail
}

// resetProbeCache clears the cached verdict (tests).
func resetProbeCache() {
	probeCache.mu.Lock()
	probeCache.url = ""
	probeCache.ok = false
	probeCache.detail = ""
	probeCache.at = time.Time{}
	probeCache.mu.Unlock()
}
