package voice

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestStreamURL(t *testing.T) {
	t.Setenv("VOICE_STREAM_BASE_URL", "")
	u := StreamURL("https://api.anthropic.com", &Options{Language: "en"})
	want := "wss://api.anthropic.com/api/ws/speech_to_text/voice_stream" +
		"?encoding=linear16&sample_rate=16000&channels=1&endpointing_ms=300" +
		"&utterance_end_ms=1000&language=en&use_conversation_engine=true"
	if u != want {
		t.Fatalf("url:\n got %s\nwant %s", u, want)
	}
	u = StreamURL("https://api.anthropic.com", &Options{Language: "ja", ForwardInterims: true})
	if !strings.Contains(u, "language=ja") || !strings.HasSuffix(u, "&forward_interims=typed") {
		t.Fatalf("url missing params: %s", u)
	}
	// VOICE_STREAM_BASE_URL override wins verbatim
	t.Setenv("VOICE_STREAM_BASE_URL", "ws://localhost:9999")
	u = StreamURL("https://api.anthropic.com", nil)
	if !strings.HasPrefix(u, "ws://localhost:9999/api/ws/speech_to_text/voice_stream?") {
		t.Fatalf("override not applied: %s", u)
	}
	if !strings.Contains(u, "language=en") {
		t.Fatalf("default language missing: %s", u)
	}
}

// wsServer records the request and runs a scripted exchange.
type wsServer struct {
	srv *httptest.Server

	mu       sync.Mutex
	path     string
	rawQuery string
	auth     string
	xapp     string
	platform string
	keyterms string

	frames   chan wsFrame // frames received from the client
	conn     *websocket.Conn
	upgraded chan struct{}
}

type wsFrame struct {
	mt  int
	msg []byte
}

func newWSServer(t *testing.T, script func(s *wsServer)) *wsServer {
	t.Helper()
	w := &wsServer{frames: make(chan wsFrame, 64), upgraded: make(chan struct{})}
	up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		w.path = r.URL.Path
		w.rawQuery = r.URL.RawQuery
		w.auth = r.Header.Get("Authorization")
		w.xapp = r.Header.Get("x-app")
		w.platform = r.Header.Get("anthropic-client-platform")
		w.keyterms = r.Header.Get("x-config-keyterms")
		w.mu.Unlock()
		c, err := up.Upgrade(rw, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		w.conn = c
		close(w.upgraded)
		go func() {
			for {
				mt, msg, err := c.ReadMessage()
				if err != nil {
					return
				}
				w.frames <- wsFrame{mt, msg}
			}
		}()
		if script != nil {
			go script(w)
		}
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *wsServer) wsURL() string {
	return "ws" + strings.TrimPrefix(w.srv.URL, "http")
}

func (w *wsServer) sendJSON(t *testing.T, s string) {
	t.Helper()
	if err := w.conn.WriteMessage(websocket.TextMessage, []byte(s)); err != nil {
		t.Errorf("server write: %v", err)
	}
}

func (w *wsServer) nextFrame(t *testing.T, d time.Duration) wsFrame {
	t.Helper()
	select {
	case f := <-w.frames:
		return f
	case <-time.After(d):
		t.Fatal("timed out waiting for client frame")
		return wsFrame{}
	}
}

func TestConnectHandshakeAndTranscript(t *testing.T) {
	var transcripts [][2]interface{} // [text, final]
	var tm sync.Mutex
	cb := &Callbacks{
		OnTranscript: func(text string, final bool) {
			tm.Lock()
			transcripts = append(transcripts, [2]interface{}{text, final})
			tm.Unlock()
		},
	}
	ready := make(chan *Stream, 1)
	cb.OnReady = func(s *Stream) { ready <- s }
	var errs []string
	var em sync.Mutex
	cb.OnError = func(msg string, _ *ErrorInfo) {
		em.Lock()
		errs = append(errs, msg)
		em.Unlock()
	}

	ws := newWSServer(t, func(w *wsServer) {
		// initial KeepAlive must be the first text frame
		f := w.nextFrame(t, 3*time.Second)
		if f.mt != websocket.TextMessage || string(f.msg) != `{"type":"KeepAlive"}` {
			t.Errorf("first frame = %d %q, want KeepAlive", f.mt, f.msg)
		}
		// wait for one binary audio frame then send interim+endpoint
		f = w.nextFrame(t, 3*time.Second)
		if f.mt != websocket.BinaryMessage {
			t.Errorf("audio frame mt=%d, want binary", f.mt)
		}
		if string(f.msg) != "pcm-chunk-1" {
			t.Errorf("audio payload = %q", f.msg)
		}
		w.sendJSON(t, `{"type":"TranscriptInterim","data":"hel"}`)
		w.sendJSON(t, `{"type":"TranscriptText","data":"hello"}`)
		w.sendJSON(t, `{"type":"TranscriptEndpoint"}`)
		// wait for CloseStream, then answer an endpoint to release finalize
		f = w.nextFrame(t, 3*time.Second)
		if string(f.msg) != `{"type":"CloseStream"}` {
			t.Errorf("expected CloseStream, got %d %q", f.mt, f.msg)
		}
		w.sendJSON(t, `{"type":"TranscriptEndpoint"}`)
	})
	t.Setenv("VOICE_STREAM_BASE_URL", ws.wsURL())

	s := Connect(cb, &Options{Language: "en", Keyterms: []string{"foo", "bar"}},
		"test-token", "https://api.anthropic.com")
	if s == nil {
		t.Fatal("Connect returned nil")
	}
	defer s.Close()
	st := <-ready
	st.Send([]byte("pcm-chunk-1"))

	reason := st.Finalize()
	if reason != "post_closestream_endpoint" {
		t.Fatalf("Finalize reason = %q", reason)
	}

	ws.mu.Lock()
	if ws.path != "/api/ws/speech_to_text/voice_stream" {
		t.Errorf("path = %s", ws.path)
	}
	for _, p := range []string{
		"encoding=linear16", "sample_rate=16000", "channels=1",
		"endpointing_ms=300", "utterance_end_ms=1000", "language=en",
		"use_conversation_engine=true",
	} {
		if !strings.Contains(ws.rawQuery, p) {
			t.Errorf("query missing %s: %s", p, ws.rawQuery)
		}
	}
	if ws.auth != "Bearer test-token" {
		t.Errorf("Authorization = %q", ws.auth)
	}
	if ws.xapp != "cli" || ws.platform != "claude_code_cli" {
		t.Errorf("headers x-app=%q platform=%q", ws.xapp, ws.platform)
	}
	if ws.keyterms != "foo,bar" {
		t.Errorf("x-config-keyterms = %q", ws.keyterms)
	}
	ws.mu.Unlock()

	tm.Lock()
	defer tm.Unlock()
	// interim "hel" → interim "hello" → endpoint promotes "hello" to final
	want := [][2]interface{}{
		{"hel", false}, {"hello", false}, {"hello", true},
	}
	if len(transcripts) != len(want) {
		t.Fatalf("transcripts = %v", transcripts)
	}
	for i := range want {
		if transcripts[i] != want[i] {
			t.Fatalf("transcripts[%d] = %v, want %v", i, transcripts[i], want[i])
		}
	}
	em.Lock()
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	em.Unlock()
}

func TestConnectNoToken(t *testing.T) {
	s := Connect(&Callbacks{}, &Options{}, "", "https://api.anthropic.com")
	if s != nil {
		t.Fatal("expected nil stream without token")
	}
}

func TestUpgradeRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("cf-mitigated", "challenge")
		w.WriteHeader(403)
	}))
	defer srv.Close()
	t.Setenv("VOICE_STREAM_BASE_URL", strings.TrimPrefix(srv.URL, "http"))
	// force ws scheme — http→ws replace happens only when using baseURL
	t.Setenv("VOICE_STREAM_BASE_URL", "ws"+strings.TrimPrefix(srv.URL, "http"))

	var got string
	var info *ErrorInfo
	done := make(chan struct{})
	cb := &Callbacks{
		OnError: func(msg string, i *ErrorInfo) {
			got, info = msg, i
			close(done)
		},
	}
	Connect(cb, &Options{}, "tok", "https://api.anthropic.com")
	<-done
	if got != "WebSocket upgrade rejected with HTTP 403" {
		t.Fatalf("msg = %q", got)
	}
	if info == nil || !info.Fatal {
		t.Fatalf("expected fatal for 4xx: %+v", info)
	}
	if info.ConnectFailureCode != "cf_mitigated_403" {
		t.Fatalf("code = %q", info.ConnectFailureCode)
	}
}

func TestFinalizeNoDataTimeout(t *testing.T) {
	old := noDataTimeout
	noDataTimeout = 50 * time.Millisecond
	defer func() { noDataTimeout = old }()

	ws := newWSServer(t, func(w *wsServer) {
		w.nextFrame(t, 3*time.Second) // KeepAlive
		// server never answers — finalize must resolve via no_data_timeout
	})
	t.Setenv("VOICE_STREAM_BASE_URL", ws.wsURL())
	ready := make(chan *Stream, 1)
	s := Connect(&Callbacks{OnReady: func(s *Stream) { ready <- s }},
		&Options{}, "tok", "https://api.anthropic.com")
	defer s.Close()
	st := <-ready
	reason := st.Finalize()
	if reason != "no_data_timeout" {
		t.Fatalf("reason = %q", reason)
	}
	// CloseStream was still sent
	f := ws.nextFrame(t, 3*time.Second)
	if string(f.msg) != `{"type":"CloseStream"}` {
		t.Fatalf("frame = %q", f.msg)
	}
}

func TestFinalizePromotesInterim(t *testing.T) {
	var finals []string
	var mu sync.Mutex
	ws := newWSServer(t, func(w *wsServer) {
		w.nextFrame(t, 3*time.Second) // KeepAlive
		w.sendJSON(t, `{"type":"TranscriptInterim","data":"almost done"}`)
		// never endpoint — safety timeout resolves, promoting the interim
	})
	t.Setenv("VOICE_STREAM_BASE_URL", ws.wsURL())
	old := noDataTimeout
	noDataTimeout = 50 * time.Millisecond
	defer func() { noDataTimeout = old }()
	ready := make(chan *Stream, 1)
	cb := &Callbacks{
		OnReady: func(s *Stream) { ready <- s },
		OnTranscript: func(text string, final bool) {
			if final {
				mu.Lock()
				finals = append(finals, text)
				mu.Unlock()
			}
		},
	}
	s := Connect(cb, &Options{}, "tok", "https://api.anthropic.com")
	defer s.Close()
	st := <-ready
	time.Sleep(100 * time.Millisecond) // let the interim arrive
	if r := st.Finalize(); r != "no_data_timeout" {
		t.Fatalf("reason = %q", r)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(finals) != 1 || finals[0] != "almost done" {
		t.Fatalf("promoted finals = %v", finals)
	}
}
