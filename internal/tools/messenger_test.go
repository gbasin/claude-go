package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// peerKeyFile locates our published sessions/<pid>.<sha256(sock)>.key.
func peerKeyFile(t *testing.T, home string) string {
	t.Helper()
	kf, _ := filepath.Glob(filepath.Join(home, "sessions", "*.key"))
	if len(kf) != 1 {
		t.Fatalf("key file not published in sessions dir: %v", kf)
	}
	return kf[0]
}

func readPeerToken(t *testing.T, keyPath string) string {
	t.Helper()
	var k struct {
		PeerToken string `json:"peerToken"`
	}
	b, _ := os.ReadFile(keyPath)
	if err := json.Unmarshal(b, &k); err != nil || k.PeerToken == "" {
		t.Fatalf("no peerToken in key file %s (err=%v)", keyPath, err)
	}
	return k.PeerToken
}

func writeLine(t *testing.T, c net.Conn, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

func drainUntil(t *testing.T, want string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case p := <-SessionQueue:
			if strings.Contains(p, want) {
				return true
			}
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	return false
}

// setupMessenger starts the messenger in a sandboxed HOME/config dir and
// returns sessionID + a cleanup that also restores mutated globals.
func setupMessenger(t *testing.T) (string, func()) {
	sessionID, _, done := setupMessengerHome(t)
	return sessionID, done
}

func setupMessengerHome(t *testing.T) (string, string, func()) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	sessionID := "test-sess-1"
	drain := StartMessenger(sessionID)
	oldDeadline, oldAuth := firstLineDeadlineMs, messengerAuthRequired
	return sessionID, home, func() {
		drain()
		firstLineDeadlineMs, messengerAuthRequired = oldDeadline, oldAuth
		SetSessionRenamer(nil)
	}
}

func TestPeerWire(t *testing.T) {
	sessionID, home, done := setupMessengerHome(t)
	defer done()

	// peerToken key file exists in the sessions dir, named <pid>.<64hex>.key
	kf := peerKeyFile(t, home)
	if !strings.HasSuffix(kf, ".key") {
		t.Fatalf("unexpected key file name: %s", kf)
	}
	base := filepath.Base(kf)
	// real's IAn regex: /^(\d+)\.[0-9a-f]{64}\.key$/
	hexPart := strings.TrimSuffix(strings.TrimPrefix(base, fmt.Sprint(os.Getpid())+"."), ".key")
	if len(hexPart) != 64 {
		t.Fatalf("key file name is not <pid>.<sha256>.key: %s", base)
	}
	tok := readPeerToken(t, kf)
	if len(tok) != 32 {
		t.Fatalf("peerToken is not 32-hex: %q", tok)
	}

	// inbound: real-protocol auth + user frame
	c, err := net.Dial("unix", socketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	writeLine(t, c, peerFrame{Type: "auth", Token: tok})
	writeLine(t, c, peerFrame{
		Type: "user", SessionID: sessionID, From: "uds:/tmp/peer.sock",
		Message: mustJSON(map[string]any{"role": "user", "content": "hello peer"}),
	})
	if !drainUntil(t, "hello peer", 2*time.Second) {
		t.Fatal("peer message never reached the queue")
	}

	// session_id mismatch is dropped
	c2, err := net.Dial("unix", socketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	writeLine(t, c2, peerFrame{
		Type: "user", SessionID: "other-session",
		Message: mustJSON(map[string]any{"role": "user", "content": "wrong target"}),
	})
	time.Sleep(100 * time.Millisecond)
	select {
	case p := <-SessionQueue:
		if strings.Contains(p, "wrong target") {
			t.Fatal("mismatched session_id delivered")
		}
	case <-time.After(100 * time.Millisecond):
	}
}

// wrong token: auth-optional platform → conn stays open, frames still
// delivered; auth-required → conn is dropped.
func TestMessengerWrongToken(t *testing.T) {
	sessionID, done := setupMessenger(t)
	defer done()

	c, err := net.Dial("unix", socketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	writeLine(t, c, peerFrame{Type: "auth", Token: "deadbeefdeadbeefdeadbeefdeadbeef"})
	writeLine(t, c, peerFrame{
		Type: "user", SessionID: sessionID,
		Message: mustJSON(map[string]any{"role": "user", "content": "unauthed ok"}),
	})
	if !drainUntil(t, "unauthed ok", 2*time.Second) {
		t.Fatal("auth-optional platform dropped a bad-token connection")
	}

	// with auth required (windows behavior) a bad token must drop the conn
	messengerAuthRequired = true
	c2, err := net.Dial("unix", socketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	writeLine(t, c2, peerFrame{Type: "auth", Token: "deadbeefdeadbeefdeadbeefdeadbeef"})
	writeLine(t, c2, peerFrame{
		Type: "user", SessionID: sessionID,
		Message: mustJSON(map[string]any{"role": "user", "content": "should not land"}),
	})
	c2.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := c2.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected dropped conn under required auth")
	}
	if drainUntil(t, "should not land", 200*time.Millisecond) {
		t.Fatal("unauthed frame delivered under required auth")
	}
}

func TestUDSControlRename(t *testing.T) {
	sessionID, done := setupMessenger(t)
	defer done()
	_ = sessionID

	renamed := make(chan string, 1)
	SetSessionRenamer(func(n string) { renamed <- n })

	c, err := net.Dial("unix", socketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	writeLine(t, c, peerFrame{Type: "control", Action: "rename", Name: "peer-chosen-name"})
	select {
	case n := <-renamed:
		if n != "peer-chosen-name" {
			t.Fatalf("rename got %q", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rename control frame never fired the callback")
	}
}

func TestUDSFirstLineDeadline(t *testing.T) {
	_, done := setupMessenger(t)
	defer done()
	firstLineDeadlineMs = 150 * time.Millisecond

	c, err := net.Dial("unix", socketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// send nothing — the conn must be closed by the deadline
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = c.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("silent connection was not closed by the first-line deadline")
	}
	// deadline cancels once the first complete line arrives
	firstLineDeadlineMs = 150 * time.Millisecond
	c2, err := net.Dial("unix", socketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	writeLine(t, c2, peerFrame{Type: "control", Action: "rename", Name: "x"})
	time.Sleep(300 * time.Millisecond) // past the deadline — conn should stay open
	writeLine(t, c2, peerFrame{
		Type:    "user",
		Message: mustJSON(map[string]any{"role": "user", "content": "post-deadline"}),
	})
	if !drainUntil(t, "post-deadline", 2*time.Second) {
		t.Fatal("conn was closed despite sending a first line in time")
	}
}

func TestUDSOversizeLineDrop(t *testing.T) {
	_, done := setupMessenger(t)
	defer done()

	c, err := net.Dial("unix", socketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// fUt = 1048576 chars in the pending buffer drops the conn
	big := append([]byte(`{"type":"user","message":{"role":"user","content":"`),
		bytes.Repeat([]byte("a"), peerLineCap)...)
	if _, err := c.Write(big); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("oversize line did not drop the connection")
	}
	// messenger still serves new connections
	c2, err := net.Dial("unix", socketPath())
	if err != nil {
		t.Fatalf("messenger died after oversize drop: %v", err)
	}
	defer c2.Close()
	writeLine(t, c2, peerFrame{
		Type:    "user",
		Message: mustJSON(map[string]any{"role": "user", "content": "still alive"}),
	})
	if !drainUntil(t, "still alive", 2*time.Second) {
		t.Fatal("messenger stopped serving after oversize drop")
	}
}

func TestPeerStatusFrame(t *testing.T) {
	_, done := setupMessenger(t)
	defer done()

	msgID := newUUID()
	waiter := trackSend(msgID)
	defer untrackSend(msgID)

	c, err := net.Dial("unix", socketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	writeLine(t, c, peerFrame{
		Type: "control", Action: "peer_message_status",
		Status: "held", OrigMsgID: msgID, Reason: "awaiting approval",
	})
	select {
	case r := <-waiter:
		if r.status != "held" {
			t.Fatalf("status = %q, want held", r.status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("peer_message_status never resolved the tracked send")
	}

	// expired + status_detail "refused" normalizes to refused (real's rule)
	w2 := trackSend(msgID)
	defer untrackSend(msgID)
	writeLine(t, c, peerFrame{
		Type: "control", Action: "peer_message_status",
		Status: "expired", StatusDetail: "refused", OrigMsgID: msgID,
	})
	select {
	case r := <-w2:
		if r.status != "refused" {
			t.Fatalf("expired/refused normalized to %q", r.status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second status frame unresolved")
	}

	// dropped resolves ids named in dropped_msg_ids too
	other := newUUID()
	w3 := trackSend(other)
	defer untrackSend(other)
	writeLine(t, c, peerFrame{
		Type: "control", Action: "peer_message_status",
		Status: "dropped", DropReason: "rate-limited",
		OrigMsgID: newUUID(), DroppedMsgIDs: []string{other},
	})
	select {
	case r := <-w3:
		if r.status != "dropped" || r.dropReason != "rate-limited" {
			t.Fatalf("dropped receipt = %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dropped_msg_ids receipt unresolved")
	}
}

// socketDeliver end-to-end: hashed key-file lookup, auth line, user frame.
func TestPeerSocketDeliver(t *testing.T) {
	sessionID, done := setupMessenger(t)
	defer done()

	self := sessionEntry{
		SessionID: sessionID, PID: os.Getpid(),
		MessagingSocketPath: socketPath(),
	}
	ok, note := socketDeliver(self, mailMsg{
		From: "test", To: sessionID, Kind: "message", Message: "deliver-me",
	})
	if !ok {
		t.Fatalf("socketDeliver failed: %s", note)
	}
	if !drainUntil(t, "deliver-me", 2*time.Second) {
		t.Fatal("socketDeliver frame never reached the queue")
	}
}

func TestUDSNotifyWhenIdle(t *testing.T) {
	_, done := setupMessenger(t)
	defer done()

	// a well-formed subscription is recorded
	c, err := net.Dial("unix", socketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	peerSock := filepath.Join(socketDir(), "99999.sock")
	writeLine(t, c, peerFrame{
		Type: "control", Action: "notify_when_idle",
		From: udsAddr(peerSock), MsgID: newUUID(),
	})
	time.Sleep(100 * time.Millisecond)
	peerIdleSubs.mu.Lock()
	n := len(peerIdleSubs.subs)
	peerIdleSubs.subs = nil // don't leak across tests
	peerIdleSubs.mu.Unlock()
	if n != 1 {
		t.Fatalf("idle subscription not recorded (subs=%d)", n)
	}

	// a self-target or non-uds from is dropped
	writeLine(t, c, peerFrame{
		Type: "control", Action: "notify_when_idle",
		From: udsAddr(socketPath()), MsgID: newUUID(),
	})
	writeLine(t, c, peerFrame{
		Type: "control", Action: "notify_when_idle",
		From: "/not/a/uds/addr.sock", MsgID: newUUID(),
	})
	writeLine(t, c, peerFrame{
		Type: "control", Action: "notify_when_idle",
		From: udsAddr(peerSock), MsgID: "not-a-uuid",
	})
	time.Sleep(100 * time.Millisecond)
	peerIdleSubs.mu.Lock()
	n = len(peerIdleSubs.subs)
	peerIdleSubs.subs = nil
	peerIdleSubs.mu.Unlock()
	if n != 0 {
		t.Fatalf("malformed notify_when_idle recorded (subs=%d)", n)
	}
}
