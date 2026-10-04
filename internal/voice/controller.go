package voice

import (
	"strings"
	"sync"
	"time"
)

// State is the voiceState store value: idle → recording → processing → idle.
type State string

const (
	StateIdle       State = "idle"
	StateRecording  State = "recording"
	StateProcessing State = "processing"
)

// tap-mode timers from mod_1711 (q/W constants): silence auto-finish and
// the max-duration cap. Armed only when Mode == "tap".
var (
	TapSilenceTimeout = 15 * time.Second
	TapMaxDuration    = 120 * time.Second
)

// coalesceLimit is the u=32000 buffered-chunk coalesce bound real uses
// when flushing pre-connect audio on stream open.
const coalesceLimit = 32000

// minErrorDuration is the o>2000 gate in #g: below it an empty transcript
// is silently dropped instead of reporting a no-speech error.
var minErrorDuration = 2000 * time.Millisecond

// Circuit-breaker constants — real's L=1e4/w=3 in mod_1711: N "early
// failures" (a session that died before any transcript data arrived)
// inside the window suppress new sessions until one succeeds.
var (
	breakerWindow    = 10 * time.Second
	breakerThreshold = 3
)

// msgStreamUnavailable surfaces when the connectivity probe (iwr) says
// the voice stream endpoint is unreachable — reported before recording
// starts instead of a mid-record connection failure.
const msgStreamUnavailable = "Voice unavailable. Check your network and try again."

// No-transcript error mapping — Z() in mod_1711. Selection depends on
// whether the mic produced signal and whether the stream ever connected.
func noTranscriptError(hadAudioSignal, wsConnected bool) string {
	if !hadAudioSignal {
		return "No audio detected from microphone. Check that the correct input device is selected and that Claude Code has microphone access."
	}
	if !wsConnected {
		return "Voice connection failed. Check your network and try again."
	}
	return "No speech detected."
}

// Controller is the voice session state machine — class A in mod_1711,
// reduced to the chat composer path (no focus mode, no circuit breaker,
// no silent-drop replay — those are residuals).
//
// Dependencies are function fields so tests can stub each leg.
type Controller struct {
	// CheckAvailability mirrors voiceModule.checkRecordingAvailability.
	CheckAvailability func() Availability
	// StartCapture mirrors voiceModule.startRecording.
	StartCapture func(onChunk func([]byte), onEnd func(), silenceDetection bool) (*Recorder, CaptureStart)
	// StopCapture mirrors voiceModule.stopRecording.
	StopCapture func(r *Recorder)
	// ConnectStream mirrors connectVoiceStream (lwr). A nil return means
	// no OAuth token → the login-required error.
	ConnectStream func(cb *Callbacks, opts *Options) *Stream
	// ProbeStream is the cached reachability check for the voice stream
	// endpoint (the gating side of real's iwr). Nil disables the probe.
	// A false verdict aborts the session pre-capture with
	// msgStreamUnavailable.
	ProbeStream func() bool
	// Keyterms supplies x-config-keyterms candidates.
	Keyterms func() []string
	// Language is the resolved STT language code (hMe of settings.language).
	Language func() string
	// Mode is settings.voice.mode ("hold"|"tap").
	Mode func() string

	// UI hooks — called from controller goroutines; the editor routes them
	// onto its key-loop via a channel.
	OnInterim func(text string) // voiceInterimTranscript updates
	OnFinal   func(text string) // committed transcript → insert
	OnError   func(msg string)  // notification text (real texts)
	OnState   func(state State) // voiceState changes

	mu       sync.Mutex
	state    State
	gen      int
	stream   *Stream
	rec      *Recorder
	acc      string // #o — accumulated final transcript
	sawData  bool   // any transcript event seen (v — retry gate)
	hadAudio bool   // #k — RMS > 0.01 observed
	wsConn   bool   // #B — stream reached onReady
	retried  bool   // #F — early-error retry used
	sawChunk bool   // T — capture produced at least one audio chunk
	startAt  time.Time
	pending  [][]byte // h — audio buffered while the ws connects
	silenceT *time.Timer
	maxT     *time.Timer

	// circuit breaker — real's host-state class P: recent early-failure
	// timestamps, the once-per-trip log flag, and the last actionable
	// hint (folded into the tripped message when present).
	earlyFailures    []time.Time
	breakerLogged    bool
	lastExpectedHint string
}

// CurrentState reports voiceState (uninitialized counts as idle — real's
// #e starts at "idle").
func (c *Controller) CurrentState() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == "" {
		return StateIdle
	}
	return c.state
}

func (c *Controller) setState(s State) {
	c.mu.Lock()
	c.state = s
	c.mu.Unlock()
	if c.OnState != nil {
		c.OnState(s)
	}
}

func (c *Controller) stale(gen int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen != gen
}

// recordEarlyFailure is P.recordEarlyFailure: pushes a failure timestamp
// and keeps hint as lastExpectedHint ("" clears it).
func (c *Controller) recordEarlyFailure(hint string) {
	c.mu.Lock()
	c.earlyFailures = append(c.earlyFailures, time.Now())
	c.lastExpectedHint = hint
	c.mu.Unlock()
}

// clearEarlyFailures is P.clearEarlyFailures — called once transcript
// data proves a session works.
func (c *Controller) clearEarlyFailures() {
	c.mu.Lock()
	c.earlyFailures = nil
	c.breakerLogged = false
	c.lastExpectedHint = ""
	c.mu.Unlock()
}

// breakerTripped prunes failures older than the window and reports
// whether new sessions are suppressed — the check at the top of #D.
func (c *Controller) breakerTripped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	keep := c.earlyFailures[:0]
	for _, t := range c.earlyFailures {
		if now.Sub(t) <= breakerWindow {
			keep = append(keep, t)
		}
	}
	c.earlyFailures = keep
	if len(c.earlyFailures) < breakerThreshold {
		c.breakerLogged = false
		return false
	}
	c.breakerLogged = true // real gates its log/event on first trip only
	return true
}

// breakerMessage is real's tripped text: the last actionable hint plus
// the pause note, or the generic message.
func (c *Controller) breakerMessage() string {
	c.mu.Lock()
	hint := c.lastExpectedHint
	c.mu.Unlock()
	if hint != "" {
		return hint + " Voice input is paused for a moment."
	}
	return "Voice input is failing repeatedly and has been paused. Check your microphone and try again in a moment."
}

// Toggle is the push-to-talk action reduced to tap semantics (real hold
// uses key-release events terminals don't report): idle → start,
// recording → finish, processing → ignore.
func (c *Controller) Toggle() {
	switch c.CurrentState() {
	case StateProcessing:
		return
	case StateIdle:
		go c.start()
	case StateRecording:
		c.finish()
	}
}

// Cancel is cancelRecording: discard without submit.
func (c *Controller) Cancel() {
	if c.CurrentState() == StateIdle {
		return
	}
	c.cleanup()
	c.setState(StateIdle)
}

// cleanup mirrors #S: bump generation, clear timers, stop capture, close
// stream, reset transcript scratch.
func (c *Controller) cleanup() {
	c.mu.Lock()
	c.gen++
	if c.silenceT != nil {
		c.silenceT.Stop()
		c.silenceT = nil
	}
	if c.maxT != nil {
		c.maxT.Stop()
		c.maxT = nil
	}
	stream := c.stream
	rec := c.rec
	c.stream = nil
	c.rec = nil
	c.acc = ""
	c.pending = nil
	c.mu.Unlock()
	if c.StopCapture != nil && rec != nil {
		c.StopCapture(rec)
	}
	if stream != nil {
		stream.Close()
	}
	if c.OnInterim != nil {
		c.OnInterim("")
	}
}

// start mirrors #D: circuit-breaker gate, availability check,
// connectivity probe, capture start with buffered audio, then async
// stream connect.
func (c *Controller) start() {
	c.mu.Lock()
	c.gen++
	gen := c.gen
	c.mu.Unlock()

	// circuit breaker: >=3 early failures in the window suppress new
	// sessions (real's recentEarlyFailures check at the top of #D)
	if c.breakerTripped() {
		if c.OnError != nil {
			c.OnError(c.breakerMessage())
		}
		return
	}

	av := c.CheckAvailability()
	if c.stale(gen) {
		return
	}
	if !av.Available {
		reason := av.Reason
		if reason == "" {
			reason = "Audio recording is not available."
		}
		if c.OnError != nil {
			c.OnError(reason)
		}
		c.recordEarlyFailure("")
		c.setState(StateIdle)
		return
	}
	// connectivity probe (iwr): unreachable endpoint → "voice
	// unavailable" up front instead of a mid-record stream failure.
	if c.ProbeStream != nil && !c.ProbeStream() {
		if c.stale(gen) {
			return
		}
		if c.OnError != nil {
			c.OnError(msgStreamUnavailable)
		}
		c.recordEarlyFailure(msgStreamUnavailable)
		c.setState(StateIdle)
		return
	}
	c.setState(StateRecording)
	c.mu.Lock()
	c.startAt = time.Now()
	c.acc = ""
	c.sawData = false
	c.hadAudio = false
	c.wsConn = false
	c.retried = false
	c.sawChunk = false
	c.pending = nil
	c.mu.Unlock()

	rec, res := c.StartCapture(
		func(chunk []byte) {
			if AudioLevel(chunk) > 0.01 {
				c.mu.Lock()
				c.hadAudio = true
				c.mu.Unlock()
			}
			c.mu.Lock()
			c.sawChunk = true
			s := c.stream
			if s == nil {
				c.pending = append(c.pending, append([]byte(nil), chunk...))
			}
			c.mu.Unlock()
			if s != nil {
				s.Send(chunk)
			}
		},
		func() {
			if c.CurrentState() == StateRecording {
				// capture ended having produced no audio at all — an
				// early failure (real's !T check before #g)
				c.mu.Lock()
				empty := !c.sawChunk
				c.mu.Unlock()
				if empty && !c.stale(gen) {
					c.recordEarlyFailure("")
				}
				c.finish()
			}
		},
		false, // real session recording passes {silenceDetection:false}
	)
	if c.stale(gen) {
		if res.Started && c.StopCapture != nil {
			c.StopCapture(rec)
		}
		return
	}
	if !res.Started {
		hint := res.Hint
		if hint == "" {
			hint = "Failed to start audio capture. Check that your microphone is accessible."
		}
		if c.OnError != nil {
			c.OnError(hint)
		}
		// real: recordEarlyFailure(V.expected ? hint : null)
		if res.Expected {
			c.recordEarlyFailure(hint)
		} else {
			c.recordEarlyFailure("")
		}
		c.cleanup()
		c.setState(StateIdle)
		return
	}
	c.mu.Lock()
	c.rec = rec
	tap := c.Mode != nil && c.Mode() == "tap"
	c.mu.Unlock()
	if tap {
		c.mu.Lock()
		c.silenceT = time.AfterFunc(TapSilenceTimeout, func() {
			c.mu.Lock()
			rec := c.CurrentState() == StateRecording
			c.mu.Unlock()
			if rec {
				c.finish()
			}
		})
		c.maxT = time.AfterFunc(TapMaxDuration, func() {
			if c.CurrentState() == StateRecording {
				c.finish()
			}
		})
		c.mu.Unlock()
	}
	go c.connect(gen)
}

// connect mirrors the `R` closure inside #D: lwr + early-error retry.
func (c *Controller) connect(gen int) {
	var lang string
	if c.Language != nil {
		lang = c.Language()
	}
	var keyterms []string
	if c.Keyterms != nil {
		keyterms = c.Keyterms()
	}
	opts := &Options{
		Language:        lang,
		Keyterms:        keyterms,
		ForwardInterims: false, // set from env inside StreamURL? keep parity
	}
	if envForwardInterims() {
		opts.ForwardInterims = true
	}
	s := c.ConnectStream(c.callbacks(gen), opts)
	if s != nil && c.stale(gen) {
		s.Close() // superseded mid-connect (real: s?.close() in the .then)
		return
	}
	if s == nil {
		// lwr returned null — no OAuth token
		if c.stale(gen) {
			return
		}
		if c.OnError != nil {
			c.OnError("Voice mode requires a Claude.ai account. Please run /login to sign in.")
		}
		c.recordEarlyFailure("Voice mode could not read your Claude.ai login.")
		c.cleanup()
		c.setState(StateIdle)
	}
}

func envForwardInterims() bool {
	return getenvBool("CLAUDE_CODE_VOICE_FORWARD_INTERIMS_TYPED")
}

// callbacks is the e handler object lwr receives.
func (c *Controller) callbacks(gen int) *Callbacks {
	return &Callbacks{
		OnReady: func(s *Stream) {
			if c.stale(gen) || c.CurrentState() != StateRecording {
				s.Close()
				return
			}
			c.mu.Lock()
			c.stream = s
			c.wsConn = true
			pending := c.pending
			c.pending = nil
			c.mu.Unlock()
			// flush buffered chunks coalesced to <=32000 bytes per frame
			var frame []byte
			for _, ch := range pending {
				if len(frame) > 0 && len(frame)+len(ch) > coalesceLimit {
					s.Send(frame)
					frame = nil
				}
				frame = append(frame, ch...)
			}
			if len(frame) > 0 {
				s.Send(frame)
			}
		},
		OnTranscript: func(text string, final bool) {
			if c.stale(gen) {
				return
			}
			c.mu.Lock()
			c.sawData = true
			c.earlyFailures = nil // transcript data: session works (clearEarlyFailures)
			c.breakerLogged = false
			c.lastExpectedHint = ""
			if final {
				trimmed := strings.TrimSpace(text)
				if trimmed != "" {
					if c.acc != "" {
						c.acc += " "
					}
					c.acc += trimmed
				}
			}
			disp := c.acc
			if !final {
				r := strings.TrimSpace(text)
				if c.acc != "" {
					if r != "" {
						disp = c.acc + " " + r
					}
				} else {
					disp = r
				}
			}
			silT := c.silenceT
			c.mu.Unlock()
			// tap mode: transcript activity re-arms the silence timer
			if silT != nil {
				silT.Reset(TapSilenceTimeout)
			}
			if c.OnInterim != nil {
				c.OnInterim(disp)
			}
		},
		OnError: func(msg string, info *ErrorInfo) {
			if c.stale(gen) {
				return
			}
			c.mu.Lock()
			fatal := info != nil && info.Fatal
			retry := !fatal && !c.sawData && !c.retried &&
				c.state == StateRecording
			if retry {
				c.retried = true
			}
			sawData := c.sawData
			c.mu.Unlock()
			if retry {
				// early voice_stream error (pre-transcript) — retry once
				go func() {
					time.Sleep(250 * time.Millisecond)
					if !c.stale(gen) && c.CurrentState() == StateRecording {
						c.connect(gen)
					}
				}()
				return
			}
			if !sawData {
				// real: !v → recordEarlyFailure() (a session that died
				// before any transcript data)
				c.recordEarlyFailure("")
			}
			// salvage mid-stream transcript before cleanup
			c.mu.Lock()
			salvaged := strings.TrimSpace(c.acc)
			c.mu.Unlock()
			if salvaged != "" && c.OnFinal != nil {
				c.OnFinal(salvaged)
			}
			if c.OnError != nil {
				c.OnError("Voice stream error: " + msg)
			}
			c.cleanup()
			c.setState(StateIdle)
		},
		OnClose: func() {},
	}
}

// finish mirrors #g: stop capture, transition to processing, finalize the
// stream, then deliver the assembled transcript or the mapped error.
func (c *Controller) finish() {
	c.mu.Lock()
	if c.state != StateRecording {
		c.mu.Unlock()
		return
	}
	c.state = StateProcessing
	gen := c.gen
	stream := c.stream
	rec := c.rec
	c.stream = nil
	c.rec = nil
	if c.silenceT != nil {
		c.silenceT.Stop()
		c.silenceT = nil
	}
	if c.maxT != nil {
		c.maxT.Stop()
		c.maxT = nil
	}
	dur := time.Since(c.startAt)
	hadAudio := c.hadAudio
	wsConn := c.wsConn
	c.mu.Unlock()

	if c.OnState != nil {
		c.OnState(StateProcessing)
	}
	if c.StopCapture != nil && rec != nil {
		c.StopCapture(rec)
	}
	go func() {
		if stream != nil {
			stream.Finalize()
		}
		if c.stale(gen) {
			return
		}
		final := strings.TrimSpace(c.acc)
		c.mu.Lock()
		c.acc = ""
		c.mu.Unlock()
		if final != "" {
			if c.OnFinal != nil {
				c.OnFinal(final)
			}
		} else if dur > minErrorDuration && c.OnError != nil {
			c.OnError(noTranscriptError(hadAudio, wsConn))
		}
		if c.OnInterim != nil {
			c.OnInterim("")
		}
		c.setState(StateIdle)
	}()
}
