package voice

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func stubProbeDial(t *testing.T, ok bool, calls *atomic.Int32) {
	t.Helper()
	old := probeDial
	probeDial = func(u string) (bool, string) {
		calls.Add(1)
		if ok {
			return true, "ok"
		}
		return false, "fetch_failed"
	}
	resetProbeCache()
	t.Cleanup(func() { probeDial = old; resetProbeCache() })
}

func TestStreamProbeCachesVerdict(t *testing.T) {
	var calls atomic.Int32
	stubProbeDial(t, true, &calls)
	old := probeTTL
	probeTTL = 50 * time.Millisecond
	defer func() { probeTTL = old }()

	u := "wss://example.test/voice"
	if ok, _ := streamProbeResult(u); !ok {
		t.Fatal("first probe should reach")
	}
	if ok, _ := streamProbeResult(u); !ok {
		t.Fatal("cached probe should reach")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("dial called %d times, want 1 (cached)", n)
	}
	time.Sleep(60 * time.Millisecond)
	streamProbeResult(u)
	if n := calls.Load(); n != 2 {
		t.Fatalf("stale cache not re-probed: %d dials", n)
	}
	// a different URL is a different cache key
	streamProbeResult("wss://other.test/voice")
	if n := calls.Load(); n != 3 {
		t.Fatalf("new URL should probe: %d dials", n)
	}
}

func TestStreamProbeCachesFailure(t *testing.T) {
	var calls atomic.Int32
	stubProbeDial(t, false, &calls)
	u := "wss://down.test/voice"
	ok, detail := streamProbeResult(u)
	if ok || detail != "fetch_failed" {
		t.Fatalf("got %v %q", ok, detail)
	}
	ok, _ = streamProbeResult(u)
	if ok || calls.Load() != 1 {
		t.Fatalf("failure not cached: ok=%v calls=%d", ok, calls.Load())
	}
}

func TestProbeDialHTTPResponseCountsAsReachable(t *testing.T) {
	// any HTTP answer — even an auth rejection — proves reachability
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()
	u := "ws" + strings.TrimPrefix(srv.URL, "http")
	ok, detail := probeDial(u)
	if !ok || detail != "http_401" {
		t.Fatalf("got %v %q", ok, detail)
	}
}

func TestProbeDialTransportErrorUnreachable(t *testing.T) {
	// a closed port: transport error, no HTTP response → unreachable
	ok, detail := probeDial("ws://127.0.0.1:1/")
	if ok || detail != "fetch_failed" {
		t.Fatalf("got %v %q", ok, detail)
	}
}

func TestControllerProbeUnavailable(t *testing.T) {
	d := newFakeDeps()
	c := d.controller()
	called := 0
	c.ProbeStream = func() bool { called++; return false }
	c.Toggle()
	d.waitFor(t, "err", func() bool { return len(d.errs) == 1 })
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.errs[0] != msgStreamUnavailable {
		t.Fatalf("errs = %v", d.errs)
	}
	if called != 1 {
		t.Fatalf("probe called %d times", called)
	}
	if c.CurrentState() != StateIdle {
		t.Fatalf("state = %s", c.CurrentState())
	}
	if d.cap.onChunk != nil || d.cb != nil {
		t.Fatal("capture/stream started despite probe failure")
	}
}

func TestControllerProbePassThrough(t *testing.T) {
	d := newFakeDeps()
	c := d.controller()
	c.ProbeStream = func() bool { return true }
	c.Toggle()
	d.waitFor(t, "recording", func() bool { return c.CurrentState() == StateRecording })
	c.Cancel()
}

func TestControllerCircuitBreaker(t *testing.T) {
	d := newFakeDeps()
	c := d.controller()
	var availCalls atomic.Int32
	c.CheckAvailability = func() Availability {
		availCalls.Add(1)
		return Availability{Available: false, Reason: "nope"}
	}
	for i := 0; i < 3; i++ {
		c.Toggle()
		d.waitFor(t, "err", func() bool { return len(d.errs) == i+1 })
	}
	if n := availCalls.Load(); n != 3 {
		t.Fatalf("availability called %d times", n)
	}
	// fourth toggle: breaker is tripped — no availability call, the
	// generic paused message (no actionable hint recorded)
	c.Toggle()
	d.waitFor(t, "breaker err", func() bool { return len(d.errs) == 4 })
	d.mu.Lock()
	last := d.errs[len(d.errs)-1]
	d.mu.Unlock()
	if availCalls.Load() != 3 {
		t.Fatalf("availability called after breaker trip")
	}
	if last != "Voice input is failing repeatedly and has been paused. Check your microphone and try again in a moment." {
		t.Fatalf("breaker msg = %q", last)
	}
}

func TestControllerBreakerHint(t *testing.T) {
	d := newFakeDeps()
	c := d.controller()
	// failures with an actionable hint (Expected) fold the hint into the
	// breaker message
	c.CheckAvailability = func() Availability { return Availability{Available: true} }
	c.StartCapture = func(func([]byte), func(), bool) (*Recorder, CaptureStart) {
		return nil, CaptureStart{Started: false, Hint: "plug in a mic", Expected: true}
	}
	for i := 0; i < 3; i++ {
		c.Toggle()
		// the error fires before the state returns to idle — a Toggle
		// issued in between would finish() instead of start()
		d.waitFor(t, "idle", func() bool {
			return c.CurrentState() == StateIdle && len(d.errs) == i+1
		})
	}
	c.Toggle()
	d.waitFor(t, "breaker err", func() bool { return len(d.errs) == 4 })
	d.mu.Lock()
	last := d.errs[len(d.errs)-1]
	d.mu.Unlock()
	if last != "plug in a mic Voice input is paused for a moment." {
		t.Fatalf("breaker msg = %q", last)
	}
}

func TestControllerBreakerClearsOnSuccess(t *testing.T) {
	d := newFakeDeps()
	c := d.controller()
	// two failures, then a successful session with transcript data —
	// the breaker window must not trip on the next failure streak start
	var failNext atomic.Bool
	failNext.Store(true)
	c.StartCapture = func(onChunk func([]byte), onEnd func(), _ bool) (*Recorder, CaptureStart) {
		if failNext.Load() {
			return nil, CaptureStart{Started: false}
		}
		d.cap.onChunk, d.cap.onEnd = onChunk, onEnd
		return &Recorder{}, CaptureStart{Started: true}
	}
	idleErrs := func(n int) func() bool {
		return func() bool { return c.CurrentState() == StateIdle && len(d.errs) == n }
	}
	c.Toggle() // fail 1
	d.waitFor(t, "fail 1", idleErrs(1))
	c.Toggle() // fail 2
	d.waitFor(t, "fail 2", idleErrs(2))
	failNext.Store(false)
	c.Toggle() // succeeds
	d.waitFor(t, "recording", func() bool { return c.CurrentState() == StateRecording })
	d.waitFor(t, "stream ready", func() bool { return d.cb != nil })
	d.cap.onChunk(pcmWithSignal())
	d.cb.OnTranscript("hi", true) // clears early failures
	c.Cancel()
	// two more failures — not yet at threshold
	failNext.Store(true)
	c.Toggle()
	d.waitFor(t, "fail 3", idleErrs(3))
	c.Toggle()
	d.waitFor(t, "fail 4", idleErrs(4))
	d.mu.Lock()
	defer d.mu.Unlock()
	// still generic failure text, not the breaker pause message
	if strings.Contains(d.errs[len(d.errs)-1], "paused") {
		t.Fatalf("breaker tripped after a cleared window: %v", d.errs)
	}
}

func TestRequestMicrophonePermission(t *testing.T) {
	if runtime.GOOS != "darwin" {
		if !RequestMicrophonePermission() {
			t.Fatal("non-darwin must skip the probe")
		}
		return
	}
	old := micProbe
	defer func() { micProbe = old }()
	micProbe = func() error { return nil }
	if !RequestMicrophonePermission() {
		t.Fatal("probe success should grant")
	}
	micProbe = func() error { return errors.New("tcc denied") }
	if RequestMicrophonePermission() {
		t.Fatal("probe failure should deny")
	}
}
