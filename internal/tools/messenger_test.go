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
		SetPeerSessionBusy(false)
		heldPeerMsgs.mu.Lock()
		heldPeerMsgs.q = nil
		heldPeerMsgs.mu.Unlock()
		outboundIdleSubs.mu.Lock()
		outboundIdleSubs.subs = nil
		outboundIdleSubs.mu.Unlock()
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

// ---------- remainder: creds, held→delivered, dedup, notice correlation, dir vet ----------

// peerInbox is a fake peer-session inbox that captures frames written to it
// (status replies real sends back to our `from` address).
func peerInbox(t *testing.T, name string) (string, chan peerFrame, func()) {
	t.Helper()
	sock := filepath.Join(socketDir(), name)
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	frames := make(chan peerFrame, 16)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 64<<10)
				var pending []byte
				for {
					n, err := c.Read(buf)
					pending = append(pending, buf[:n]...)
					for {
						i := bytes.IndexByte(pending, '\n')
						if i < 0 {
							break
						}
						var f peerFrame
						if json.Unmarshal(pending[:i], &f) == nil {
							frames <- f
						}
						pending = pending[i+1:]
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return sock, frames, func() { ln.Close(); os.Remove(sock) }
}

// waitStatus returns the first peer_message_status frame matching want on
// the fake inbox, skipping any other frames.
func waitStatus(t *testing.T, frames chan peerFrame, want, origID string) peerFrame {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case f := <-frames:
			if f.Action == "peer_message_status" && f.Status == want &&
				(origID == "" || f.OrigMsgID == origID) {
				return f
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no %q receipt for %s", want, origID)
		}
	}
	t.Fatalf("no %q receipt for %s", want, origID)
	return peerFrame{}
}

// Kernel peer creds: pid and uid must resolve for a same-proc peer on
// creds platforms (darwin LOCAL_PEERPID/PEERCRED, linux SO_PEERCRED).
func TestPeerCredsNonNegative(t *testing.T) {
	if !peerCredOSSupport {
		t.Skip("no kernel peer credentials on this platform")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "x.sock")
	ln, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	c, err := net.Dial("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ac := <-accepted
	defer ac.Close()

	pid, uid, gid, ok := peerCredsOf(c)
	if !ok {
		t.Fatal("peerCredsOf unsupported on a creds platform")
	}
	if pid <= 0 {
		t.Fatalf("peer pid = %d, want >0", pid)
	}
	if uid != os.Getuid() {
		t.Fatalf("peer uid = %d, want %d (non-negative path)", uid, os.Getuid())
	}
	_ = gid // darwin fills it via xucred only; don't assert

	// and the wrappers see the same values
	if peerPIDOf(c) != pid {
		t.Fatalf("peerPIDOf = %d, want %d", peerPIDOf(c), pid)
	}
	if peerUIDOf(c) != uid {
		t.Fatalf("peerUIDOf = %d, want %d", peerUIDOf(c), uid)
	}
}

// Held flow: while the session is mid-turn an inbound user frame is held;
// the sender gets a "held" receipt at its `from` address, nothing queues,
// and FlushHeldPeerMessages releases it with a "delivered" receipt.
func TestPeerHeldDelivered(t *testing.T) {
	_, done := setupMessenger(t)
	defer done()

	sock, frames, inboxDone := peerInbox(t, "99998.sock")
	defer inboxDone()

	SetPeerSessionBusy(true)
	defer SetPeerSessionBusy(false)

	msgID := newUUID()
	c, err := net.Dial("unix", socketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	writeLine(t, c, peerFrame{
		Type: "user", MsgID: msgID, From: udsAddr(sock),
		Message: mustJSON(map[string]any{"role": "user", "content": "hold me"}),
	})

	f := waitStatus(t, frames, "held", msgID)
	if f.From == "" {
		t.Fatal("held receipt carries no from address")
	}
	// nothing reached the prompt queue yet
	if drainUntil(t, "hold me", 200*time.Millisecond) {
		t.Fatal("held message queued before release")
	}

	// prompt boundary: flush → queued + "delivered" receipt
	FlushHeldPeerMessages()
	if !drainUntil(t, "hold me", 2*time.Second) {
		t.Fatal("held message never released into the queue")
	}
	waitStatus(t, frames, "delivered", msgID)
}

// Dedup: a re-delivery of the same msg_id inside the window is dropped and
// the sender gets a dropped/duplicate receipt.
func TestPeerDedup(t *testing.T) {
	_, done := setupMessenger(t)
	defer done()

	sock, frames, inboxDone := peerInbox(t, "99997.sock")
	defer inboxDone()

	msgID := newUUID()
	c, err := net.Dial("unix", socketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	writeLine(t, c, peerFrame{
		Type: "user", MsgID: msgID, From: udsAddr(sock),
		Message: mustJSON(map[string]any{"role": "user", "content": "first copy"}),
	})
	if !drainUntil(t, "first copy", 2*time.Second) {
		t.Fatal("first delivery never queued")
	}
	writeLine(t, c, peerFrame{
		Type: "user", MsgID: msgID, From: udsAddr(sock),
		Message: mustJSON(map[string]any{"role": "user", "content": "second copy"}),
	})
	f := waitStatus(t, frames, "dropped", msgID)
	if f.DropReason != "duplicate" {
		t.Fatalf("drop_reason = %q, want duplicate", f.DropReason)
	}
	if drainUntil(t, "second copy", 300*time.Millisecond) {
		t.Fatal("re-delivered msg_id was queued")
	}
}

// Uncorrelated peer_idle_notice is dropped (real's gRt); a notice
// correlated to an outbound notify_when_idle delivers once, then the
// subscription is consumed.
func TestUDSIdleNoticeCorrelation(t *testing.T) {
	_, done := setupMessenger(t)
	defer done()

	c, err := net.Dial("unix", socketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// uncorrelated: orig_msg_id names no subscription we sent → dropped
	writeLine(t, c, peerFrame{
		Type: "control", Action: "peer_idle_notice",
		OrigMsgID: newUUID(), State: "idle", From: "uds:/tmp/x.sock",
	})
	if drainUntil(t, "idle notice", 300*time.Millisecond) {
		t.Fatal("uncorrelated peer_idle_notice was delivered")
	}

	// correlated: record the outbound sub we would have sent, then the
	// notice for its msg_id is admitted
	mid := newUUID()
	if !recordOutboundIdleSub(mid, "/tmp/x.sock") {
		t.Fatal("outbound sub not recorded")
	}
	writeLine(t, c, peerFrame{
		Type: "control", Action: "peer_idle_notice",
		OrigMsgID: mid, State: "idle", From: "uds:/tmp/x.sock",
	})
	if !drainUntil(t, "idle notice", 2*time.Second) {
		t.Fatal("correlated peer_idle_notice was dropped")
	}
	if outboundIdleOutstanding(mid) {
		t.Fatal("subscription was not consumed by its notice")
	}

	// a second notice for the consumed sub is dropped again
	writeLine(t, c, peerFrame{
		Type: "control", Action: "peer_idle_notice",
		OrigMsgID: mid, State: "idle", From: "uds:/tmp/x.sock",
	})
	if drainUntil(t, "idle notice", 300*time.Millisecond) {
		t.Fatal("replayed peer_idle_notice was delivered")
	}
}

// Corrupted sockets dir (a regular file in the leaf position) is refused
// and we fall back to the per-uid dir (real's Re → Lfo path).
func TestSocketDirCorruptFallback(t *testing.T) {
	t.Setenv("CLAUDE_GO_MESSAGING_SOCKET_PATH", "")
	xdg := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", xdg)
	t.Setenv("CLAUDE_CODE_TMPDIR", "")
	if err := os.WriteFile(filepath.Join(xdg, "cc-socks"), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := socketDir()
	want := peerFallbackSocketDir()
	if got != want {
		t.Fatalf("socketDir = %q, want fallback %q", got, want)
	}
	fi, err := os.Stat(got)
	if err != nil || !fi.IsDir() {
		t.Fatalf("fallback dir not created: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("fallback dir mode = %04o, want 0700", fi.Mode().Perm())
	}

	// a group-writable non-sticky ANCESTOR (real's directory_rule) is also
	// refused — the leaf itself would just be chmod'd to 0700
	xdg2 := t.TempDir()
	shared := filepath.Join(xdg2, "shared")
	if err := os.Mkdir(shared, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o770); err != nil { // beat umask
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", shared)
	if got := socketDir(); got != want {
		t.Fatalf("socketDir through a group-writable ancestor = %q, want %q", got, want)
	}
	// and nothing was created inside the refused dir
	if _, err := os.Lstat(filepath.Join(shared, "cc-socks")); !os.IsNotExist(err) {
		t.Fatal("cc-socks was created inside a refused directory")
	}
}

// Explicit --messaging-socket-path vetting (real's Bns).
func TestExplicitSocketPathVet(t *testing.T) {
	if err := vetExplicitSocketPath("relative/sock.sock"); err == nil {
		t.Fatal("relative path accepted")
	}
	if err := vetExplicitSocketPath("/tmp/../etc/x.sock"); err == nil {
		t.Fatal("'..' segment accepted")
	}
	if err := vetExplicitSocketPath("/" + strings.Repeat("a", peerSockPathMax) + ".sock"); err == nil {
		t.Fatal("overlong path accepted")
	}
	// good: a missing dir under our temp root is created 0700
	good := filepath.Join(t.TempDir(), "priv", "x.sock")
	if err := vetExplicitSocketPath(good); err != nil {
		t.Fatalf("valid explicit path refused: %v", err)
	}
	fi, _ := os.Stat(filepath.Dir(good))
	if fi == nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("explicit dir not created 0700")
	}
	// a group/other-readable dir is refused
	lax := t.TempDir()
	os.Chmod(lax, 0o755)
	if err := vetExplicitSocketPath(filepath.Join(lax, "x.sock")); err == nil {
		t.Fatal("non-private explicit dir accepted")
	}
}
