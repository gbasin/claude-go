package tools

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPeerWire(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	sessionID := "test-sess-1"
	drain := StartMessenger(sessionID)
	defer drain()

	// peerToken key file exists beside the socket
	kf, _ := filepath.Glob(filepath.Join(socketDir(), "*.key"))
	if len(kf) != 1 {
		t.Fatalf("key file not published: %v", kf)
	}
	var k struct {
		PeerToken string `json:"peerToken"`
	}
	b, _ := os.ReadFile(kf[0])
	json.Unmarshal(b, &k)
	if k.PeerToken == "" {
		t.Fatal("no peerToken in key file")
	}

	// inbound: real-protocol auth + user frame
	c, err := net.Dial("unix", socketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	auth := peerFrame{Type: "auth", Token: k.PeerToken}
	ab, _ := json.Marshal(auth)
	c.Write(append(ab, '\n'))
	user := peerFrame{
		Type: "user", SessionID: sessionID, From: "/tmp/peer.sock",
		Message: mustJSON(map[string]any{"role": "user", "content": "hello peer"}),
	}
	ub, _ := json.Marshal(user)
	c.Write(append(ub, '\n'))
	time.Sleep(50 * time.Millisecond)

	// the message should land in the session queue
	got := false
	for i := 0; i < 20; i++ {
		select {
		case p := <-SessionQueue:
			if strings.Contains(p, "hello peer") {
				got = true
			}
		default:
			time.Sleep(20 * time.Millisecond)
		}
		if got {
			break
		}
	}
	if !got {
		t.Fatal("peer message never reached the queue")
	}

	// session_id mismatch is dropped
	c2, _ := net.Dial("unix", socketPath())
	defer c2.Close()
	bad := peerFrame{
		Type: "user", SessionID: "other-session",
		Message: mustJSON(map[string]any{"role": "user", "content": "wrong target"}),
	}
	bb, _ := json.Marshal(bad)
	c2.Write(append(bb, '\n'))
	time.Sleep(80 * time.Millisecond)
	select {
	case p := <-SessionQueue:
		if strings.Contains(p, "wrong target") {
			t.Fatal("mismatched session_id delivered")
		}
	case <-time.After(80 * time.Millisecond):
	}
}
