package voice

import (
	"sync"
	"testing"
	"time"
)

// fakeCapture records the injected callbacks so tests can drive audio.
type fakeCapture struct {
	onChunk func([]byte)
	onEnd   func()
	started bool
	stopped bool
}

type fakeDeps struct {
	cap    *fakeCapture
	cb     *Callbacks // captured stream callbacks
	opts   *Options
	stream *Stream

	interims []string
	finals   []string
	errs     []string
	states   []State
	mu       sync.Mutex
	events   chan struct{} // tick on every recorded event
}

func newFakeDeps() *fakeDeps {
	return &fakeDeps{cap: &fakeCapture{}, events: make(chan struct{}, 100)}
}

func (d *fakeDeps) tick() {
	select {
	case d.events <- struct{}{}:
	default:
	}
}

func (d *fakeDeps) controller() *Controller {
	return &Controller{
		CheckAvailability: func() Availability { return Availability{Available: true} },
		StartCapture: func(onChunk func([]byte), onEnd func(), _ bool) (*Recorder, CaptureStart) {
			d.cap.onChunk, d.cap.onEnd = onChunk, onEnd
			return &Recorder{}, CaptureStart{Started: true}
		},
		StopCapture: func(r *Recorder) {
			d.mu.Lock()
			d.cap.stopped = true
			d.mu.Unlock()
			d.tick()
		},
		ConnectStream: func(cb *Callbacks, opts *Options) *Stream {
			d.mu.Lock()
			d.cb, d.opts = cb, opts
			d.mu.Unlock()
			d.stream = &Stream{}
			cb.OnReady(d.stream)
			d.tick()
			return d.stream
		},
		Keyterms: func() []string { return []string{"foo"} },
		Language: func() string { return "en" },
		Mode:     func() string { return "tap" },
		OnInterim: func(t string) {
			d.mu.Lock()
			d.interims = append(d.interims, t)
			d.mu.Unlock()
			d.tick()
		},
		OnFinal: func(t string) {
			d.mu.Lock()
			d.finals = append(d.finals, t)
			d.mu.Unlock()
			d.tick()
		},
		OnError: func(msg string) {
			d.mu.Lock()
			d.errs = append(d.errs, msg)
			d.mu.Unlock()
			d.tick()
		},
		OnState: func(s State) {
			d.mu.Lock()
			d.states = append(d.states, s)
			d.mu.Unlock()
			d.tick()
		},
	}
}

func (d *fakeDeps) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		ok := cond()
		d.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-d.events:
		case <-time.After(20 * time.Millisecond):
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	t.Fatalf("timed out waiting for %s (states=%v finals=%v errs=%v)",
		what, d.states, d.finals, d.errs)
}

// pcmWithSignal is a loud-ish s16le chunk that trips the 0.01 level gate.
func pcmWithSignal() []byte {
	b := make([]byte, 400)
	for i := 0; i+1 < len(b); i += 2 {
		b[i], b[i+1] = 0xff, 0x30
	}
	return b
}

func TestControllerLifecycle(t *testing.T) {
	d := newFakeDeps()
	old := minErrorDuration
	minErrorDuration = 10 * time.Millisecond
	defer func() { minErrorDuration = old }()

	c := d.controller()
	c.Toggle() // idle → start (async)
	d.waitFor(t, "recording", func() bool { return c.CurrentState() == StateRecording })
	d.waitFor(t, "stream ready", func() bool { return d.cb != nil })

	if d.opts == nil || d.opts.Language != "en" {
		t.Fatalf("opts = %+v", d.opts)
	}

	// audio flows: pending until ready, then direct send (dead stream drops)
	d.cap.onChunk(pcmWithSignal())

	// interim transcript → OnInterim; final → accumulated and reported
	d.cb.OnTranscript("hello", false)
	d.cb.OnTranscript("hello world", true)

	c.Toggle() // recording → finish → processing → idle
	d.waitFor(t, "idle", func() bool { return c.CurrentState() == StateIdle })
	d.waitFor(t, "final", func() bool { return len(d.finals) == 1 })

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.finals[0] != "hello world" {
		t.Fatalf("final = %q", d.finals[0])
	}
	if !d.cap.stopped {
		t.Fatal("capture not stopped")
	}
	// interim display showed accumulated transcript along the way
	found := false
	for _, it := range d.interims {
		if it == "hello world" {
			found = true
		}
	}
	if !found {
		t.Fatalf("interims = %v", d.interims)
	}
}

func TestControllerNoSpeechError(t *testing.T) {
	d := newFakeDeps()
	old := minErrorDuration
	minErrorDuration = 1 * time.Millisecond // don't wait 2s
	defer func() { minErrorDuration = old }()

	c := d.controller()
	c.Toggle()
	d.waitFor(t, "recording", func() bool { return c.CurrentState() == StateRecording })
	d.cap.onChunk(pcmWithSignal()) // audio but no transcript
	time.Sleep(5 * time.Millisecond)
	c.Toggle()
	d.waitFor(t, "idle", func() bool { return c.CurrentState() == StateIdle })
	d.waitFor(t, "err", func() bool { return len(d.errs) == 1 })
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.errs[0] != "No speech detected." {
		t.Fatalf("errs = %v", d.errs)
	}
}

func TestControllerNoAudioError(t *testing.T) {
	d := newFakeDeps()
	old := minErrorDuration
	minErrorDuration = 1 * time.Millisecond
	defer func() { minErrorDuration = old }()
	// ConnectStream never invoked — ws never connected AND no audio
	c := d.controller()
	c.ConnectStream = func(cb *Callbacks, opts *Options) *Stream { return &Stream{} }
	c.Toggle()
	d.waitFor(t, "recording", func() bool { return c.CurrentState() == StateRecording })
	time.Sleep(5 * time.Millisecond)
	c.Toggle()
	d.waitFor(t, "idle", func() bool { return c.CurrentState() == StateIdle })
	d.waitFor(t, "err", func() bool { return len(d.errs) == 1 })
	d.mu.Lock()
	defer d.mu.Unlock()
	want := "No audio detected from microphone. Check that the correct input device is selected and that Claude Code has microphone access."
	if d.errs[0] != want {
		t.Fatalf("errs = %v", d.errs)
	}
}

func TestControllerCancel(t *testing.T) {
	d := newFakeDeps()
	c := d.controller()
	c.Toggle()
	d.waitFor(t, "recording", func() bool { return c.CurrentState() == StateRecording })
	c.Cancel()
	if c.CurrentState() != StateIdle {
		t.Fatalf("state = %s", c.CurrentState())
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.finals) != 0 {
		t.Fatalf("cancel delivered finals: %v", d.finals)
	}
}

func TestControllerUnavailable(t *testing.T) {
	d := newFakeDeps()
	c := d.controller()
	c.CheckAvailability = func() Availability {
		return Availability{Available: false, Reason: "no mic here"}
	}
	c.Toggle()
	d.waitFor(t, "err", func() bool { return len(d.errs) == 1 })
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.errs[0] != "no mic here" || c.CurrentState() != StateIdle {
		t.Fatalf("errs=%v state=%s", d.errs, c.CurrentState())
	}
}

func TestControllerNoAuth(t *testing.T) {
	d := newFakeDeps()
	c := d.controller()
	c.ConnectStream = func(cb *Callbacks, opts *Options) *Stream { return nil }
	c.Toggle()
	d.waitFor(t, "err", func() bool { return len(d.errs) == 1 })
	d.mu.Lock()
	defer d.mu.Unlock()
	want := "Voice mode requires a Claude.ai account. Please run /login to sign in."
	if d.errs[0] != want {
		t.Fatalf("errs = %v", d.errs)
	}
}
