package voice

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Wire constants from mod_1710: text frames, endpoint path, timings.
const (
	keepAliveFrame   = `{"type":"KeepAlive"}`
	closeStreamFrame = `{"type":"CloseStream"}`
	streamPath       = "/api/ws/speech_to_text/voice_stream"

	keepAliveInterval = 8000 * time.Millisecond // z in mod_1710
)

// R timeouts in mod_1710 — vars so tests can shorten them.
var (
	safetyTimeout = 5000 * time.Millisecond
	noDataTimeout = 1500 * time.Millisecond
)

// ErrorInfo is the optional second arg to Callbacks.OnError — mirrors the
// {fatal, connectFailureCode} object real passes.
type ErrorInfo struct {
	Fatal              bool
	ConnectFailureCode string
}

// Callbacks is the handler set `lwr` wires onto the WebSocket — the same
// five hooks the voice controller supplies.
type Callbacks struct {
	OnReady      func(*Stream)
	OnTranscript func(text string, final bool)
	OnError      func(msg string, info *ErrorInfo)
	OnClose      func()
}

// Options is `s` in lwr — per-connection stream parameters.
type Options struct {
	Language  string   // resolved STT language code (default "en")
	Keyterms  []string // sent as x-config-keyterms after sanitization
	UserAgent string   // claude-cli UA string
	// ForwardInterims adds forward_interims=typed (real: env
	// CLAUDE_CODE_VOICE_FORWARD_INTERIMS_TYPED or the tengu_brick_follow
	// gate).
	ForwardInterims bool
}

// Stream is the conn facade `V` in lwr: send/finalize/close/isConnected.
type Stream struct {
	conn *websocket.Conn
	cb   *Callbacks

	mu        sync.Mutex
	writeMu   sync.Mutex // gorilla allows one writer at a time
	opened    bool       // O in lwr — handshake completed
	closeSent bool       // S — CloseStream (or close) sent; drops audio
	finalized bool       // g — finalize() called
	rejected  bool       // w — upgrade rejected (non-101)
	closed    bool

	latest string // l — unreported interim, promoted to final on close/finalize

	finMu      sync.Mutex
	finResolve func(reason string)
	clearNo    func()
	safetyT    *time.Timer
	noDataT    *time.Timer

	done   chan struct{} // closed when the read pump exits
	doneMu sync.Once
}

// StreamURL builds the ws URL exactly like lwr:
// VOICE_STREAM_BASE_URL override else BASE_API_URL https→wss, plus the
// fixed URLSearchParams block (insertion order matters for the query
// string — Go's url.Values sorts keys, so build it manually).
func StreamURL(baseURL string, opts *Options) string {
	base := os.Getenv("VOICE_STREAM_BASE_URL")
	if base == "" {
		base = strings.NewReplacer("https://", "wss://", "http://", "ws://").
			Replace(baseURL)
	}
	lang := "en"
	if opts != nil && opts.Language != "" {
		lang = opts.Language
	}
	var q strings.Builder
	q.WriteString("encoding=linear16")
	q.WriteString("&sample_rate=16000")
	q.WriteString("&channels=1")
	q.WriteString("&endpointing_ms=300")
	q.WriteString("&utterance_end_ms=1000")
	q.WriteString("&language=" + url.QueryEscape(lang))
	q.WriteString("&use_conversation_engine=true")
	if opts != nil && opts.ForwardInterims {
		q.WriteString("&forward_interims=typed")
	}
	return base + streamPath + "?" + q.String()
}

// Connect mirrors lwr: nil return on missing OAuth token (the caller maps
// that to the login-required message); on dial failure it fires
// cb.OnError (upgrade-rejected or transport error with connectFailureCode)
// and returns a dead Stream; on success it fires OnReady and returns the
// live Stream. The dial is synchronous like the real connect path — call
// Connect from a goroutine if you must not block.
func Connect(cb *Callbacks, opts *Options, token, baseURL string) *Stream {
	if token == "" {
		return nil // real: no OAuth token → null → login-required message
	}
	u := StreamURL(baseURL, opts)
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	ua := "claude-cli"
	if opts != nil && opts.UserAgent != "" {
		ua = opts.UserAgent
	}
	h.Set("User-Agent", ua)
	h.Set("x-app", "cli")
	h.Set("anthropic-client-platform", "claude_code_cli") // Fh() for cli entrypoint
	if opts != nil && len(opts.Keyterms) > 0 {
		if kt := SanitizeKeyterms(opts.Keyterms); kt != "" {
			h.Set("x-config-keyterms", kt)
		}
	}
	s := &Stream{cb: cb, done: make(chan struct{})}
	dialer := &websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		Proxy:            http.ProxyFromEnvironment,
		TLSClientConfig:  &tls.Config{MinVersion: tls.VersionTLS12},
	}
	conn, resp, err := dialer.Dial(u, h)
	if err != nil {
		if resp != nil {
			// unexpected-response: non-101 HTTP status (real: w=true)
			s.rejected = true
			code := resp.StatusCode
			cf := fmt.Sprintf("upgrade_rejected_%d", intRange(code, 100, 599))
			if resp.Header.Get("cf-mitigated") != "" {
				cf = fmt.Sprintf("cf_mitigated_%d", intRange(code, 100, 599))
			}
			cb.OnError(fmt.Sprintf("WebSocket upgrade rejected with HTTP %d", code),
				&ErrorInfo{Fatal: code >= 400 && code < 500, ConnectFailureCode: cf})
		} else {
			cb.OnError("Voice stream connection error: "+err.Error(),
				&ErrorInfo{ConnectFailureCode: "ws_error_" + errKind(err)})
		}
		return s // dead stream; finalize resolves ws_already_closed
	}
	s.conn = conn
	s.opened = true
	go s.pump()
	// on("open"): initial KeepAlive + 8s keepalive interval + onReady
	s.writeText(keepAliveFrame)
	go s.keepalive()
	if cb.OnReady != nil {
		cb.OnReady(s)
	}
	return s
}

func intRange(v, lo, hi int) interface{} {
	if v >= lo && v <= hi {
		return v
	}
	return "unknown"
}

func errKind(err error) string {
	// real uses ro(err) lowercased — error code/name; approximate
	s := strings.ToLower(err.Error())
	for _, w := range []string{"timeout", "refused", "reset", "unreachable",
		"tls", "certificate", "dns", "eof"} {
		if strings.Contains(s, w) {
			return w
		}
	}
	return "unknown"
}

func (s *Stream) writeText(t string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.conn == nil {
		return fmt.Errorf("not connected")
	}
	return s.conn.WriteMessage(websocket.TextMessage, []byte(t))
}

func (s *Stream) writeBin(b []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.conn == nil {
		return fmt.Errorf("not connected")
	}
	return s.conn.WriteMessage(websocket.BinaryMessage, b)
}

func (s *Stream) keepalive() {
	t := time.NewTicker(keepAliveInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			s.mu.Lock()
			open := s.opened && !s.closed
			s.mu.Unlock()
			if open {
				s.writeText(keepAliveFrame)
			}
		}
	}
}

// Send pushes a PCM chunk as one binary frame; dropped silently after
// CloseStream, exactly like V.send.
func (s *Stream) Send(chunk []byte) {
	s.mu.Lock()
	if s.conn == nil || s.closed || s.closeSent {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.writeBin(chunk)
}

// Finalize mirrors V.finalize(): guards double-finalize, arms the safety
// (5s) and no-data (1500ms) timers, sends CloseStream, and resolves with
// the reason string. On resolve it promotes any unreported interim to a
// final transcript first — matching the `_?.(o)` resolve path.
func (s *Stream) Finalize() string {
	s.mu.Lock()
	if s.finalized || s.closeSent {
		s.mu.Unlock()
		return "ws_already_closed"
	}
	s.finalized = true
	s.mu.Unlock()

	resCh := make(chan string, 1)
	resolve := func(reason string) {
		s.finMu.Lock()
		if s.finResolve == nil {
			s.finMu.Unlock()
			return
		}
		s.finResolve = nil
		s.clearNo = nil
		st, nt := s.safetyT, s.noDataT
		s.finMu.Unlock()
		if st != nil {
			st.Stop()
		}
		if nt != nil {
			nt.Stop()
		}
		// Promote unreported interim to final before resolving (lwr `_`).
		s.promote("before " + reason + " resolve")
		resCh <- reason
	}
	s.finMu.Lock()
	s.finResolve = resolve
	s.clearNo = func() {
		if s.noDataT != nil {
			s.noDataT.Stop()
		}
	}
	// armed under finMu so a firing timer can't see unassigned fields
	s.safetyT = time.AfterFunc(safetyTimeout, func() { resolve("safety_timeout") })
	s.noDataT = time.AfterFunc(noDataTimeout, func() { resolve("no_data_timeout") })
	s.finMu.Unlock()

	s.mu.Lock()
	dead := s.conn == nil || s.closed
	s.mu.Unlock()
	if dead {
		resolve("ws_already_closed")
	} else {
		s.mu.Lock()
		s.closeSent = true
		s.mu.Unlock()
		go s.writeText(closeStreamFrame)
	}
	return <-resCh
}

// Close mirrors V.close(): marks CloseStream sent, stops keepalive, and
// closes the socket.
func (s *Stream) Close() {
	s.mu.Lock()
	s.closeSent = true
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	c := s.conn
	s.mu.Unlock()
	if c != nil {
		s.writeMu.Lock()
		// best-effort close frame then drop
		_ = c.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		s.writeMu.Unlock()
		c.Close()
	}
}

// IsConnected mirrors V.isConnected.
func (s *Stream) IsConnected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opened && !s.closed && s.conn != nil
}

func (s *Stream) resolveFinalize(reason string) {
	s.finMu.Lock()
	r := s.finResolve
	s.finMu.Unlock()
	if r != nil {
		r(reason)
	}
}

func (s *Stream) promote(why string) {
	s.mu.Lock()
	l := s.latest
	s.latest = ""
	s.mu.Unlock()
	if l != "" && s.cb.OnTranscript != nil {
		s.cb.OnTranscript(l, true)
	}
}

// wsEvent is the server frame schema (mod_1710 message handler).
type wsEvent struct {
	Type        string `json:"type"`
	Data        string `json:"data"`
	Description string `json:"description"`
	ErrorCode   string `json:"error_code"`
	Message     string `json:"message"`
}

func (s *Stream) pump() {
	var closeCode int
	var closeText string
	defer func() {
		s.mu.Lock()
		s.opened = false
		s.mu.Unlock()
		// on("close"): promote pending interim, resolve finalize, report
		// abnormal close, then onClose.
		s.promote("ws close")
		s.resolveFinalize("ws_close")
		s.mu.Lock()
		fin := s.finalized
		rej := s.rejected
		s.mu.Unlock()
		if !fin && !rej && closeCode != 1000 && closeCode != 1005 {
			msg := fmt.Sprintf("Connection closed: code %d", closeCode)
			if closeText != "" {
				msg += " — " + closeText
			}
			s.cb.OnError(msg, &ErrorInfo{
				ConnectFailureCode: fmt.Sprintf("ws_closed_%v", intRange(closeCode, 1000, 4999)),
			})
		}
		if s.cb.OnClose != nil {
			s.cb.OnClose()
		}
		s.doneMu.Do(func() { close(s.done) })
	}()
	closeCode = 1005 // no-status default until a close frame says otherwise
	for {
		_, msg, err := s.conn.ReadMessage()
		if err != nil {
			if ce, ok := err.(*websocket.CloseError); ok {
				closeCode = ce.Code
				closeText = ce.Text
			} else {
				closeCode = 1006 // abnormal — real treats !1000/!1005 as error
			}
			return
		}
		var ev wsEvent
		if json.Unmarshal(msg, &ev) != nil {
			continue
		}
		s.mu.Lock()
		closeSent := s.closeSent
		s.mu.Unlock()
		switch ev.Type {
		case "TranscriptInterim", "TranscriptText":
			if ev.Data == "" {
				continue
			}
			if closeSent {
				s.finMu.Lock()
				if s.clearNo != nil {
					s.clearNo()
				}
				s.finMu.Unlock()
			}
			s.mu.Lock()
			s.latest = ev.Data
			s.mu.Unlock()
			if s.cb.OnTranscript != nil {
				s.cb.OnTranscript(ev.Data, false)
			}
		case "TranscriptEndpoint":
			s.mu.Lock()
			l := s.latest
			s.latest = ""
			s.mu.Unlock()
			if l != "" && s.cb.OnTranscript != nil {
				s.cb.OnTranscript(l, true)
			}
			if closeSent {
				s.resolveFinalize("post_closestream_endpoint")
			}
		case "TranscriptError":
			desc := ev.Description
			if desc == "" {
				desc = ev.ErrorCode
			}
			if desc == "" {
				desc = "unknown transcription error"
			}
			s.promote("TranscriptError")
			s.mu.Lock()
			fin := s.finalized
			s.mu.Unlock()
			if !fin && s.cb.OnError != nil {
				s.cb.OnError(desc, nil)
			}
		case "error":
			desc := ev.Message
			if desc == "" {
				desc = "unstructured error frame"
			}
			s.promote("server error")
			s.mu.Lock()
			fin := s.finalized
			s.mu.Unlock()
			if !fin && s.cb.OnError != nil {
				s.cb.OnError(desc, nil)
			}
		}
	}
}
