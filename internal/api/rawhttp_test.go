package api

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHopHeader(t *testing.T) {
	hops := []string{"Connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade",
		"connection", "te"}
	for _, h := range hops {
		if !hopHeader(h) {
			t.Fatalf("hopHeader(%q) = false", h)
		}
	}
	for _, h := range []string{"Authorization", "Content-Type", "X-Custom", "anthropic-beta"} {
		if hopHeader(h) {
			t.Fatalf("hopHeader(%q) = true", h)
		}
	}
}

func TestChunkReader(t *testing.T) {
	in := "4\r\nWiki\r\n5\r\npedia\r\nE\r\n in\r\n\r\nchunks.\r\n0\r\nX-Trailer: ignored\r\n\r\n"
	c := &chunkReader{br: bufio.NewReader(strings.NewReader(in))}
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "Wikipedia in\r\n\r\nchunks." {
		t.Fatalf("decoded %q", got)
	}
	// after the terminal chunk we're at EOF permanently
	if n, err := c.Read(make([]byte, 8)); n != 0 || err != io.EOF {
		t.Fatalf("post-EOF read = %d, %v", n, err)
	}
}

func TestChunkReaderExtensions(t *testing.T) {
	// chunk extensions after the size are ignored
	in := "3;ext=1\r\nabc\r\n0\r\n\r\n"
	c := &chunkReader{br: bufio.NewReader(strings.NewReader(in))}
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "abc" {
		t.Fatalf("ext: %q %v", got, err)
	}
}

func TestWriteRawRequest(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	rc := &rawConn{conn: client, br: bufio.NewReader(client)}

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages",
		strings.NewReader(`{"x":1}`))
	req.Header.Set("User-Agent", "github.com/gbasin/claude-go/test")
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("X-Custom", "z")
	req.Header.Set("Connection", "close") // hop header: must not be written

	done := make(chan error, 1)
	go func() { done <- writeRawRequest(rc, req) }()

	server.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := io.ReadAll(bufio.NewReader(server))
	if err != nil && !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("read: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("write: %v", err)
	}
	wire := string(got)
	if !strings.HasPrefix(wire, "POST /v1/messages HTTP/1.1\r\n") {
		t.Fatalf("request line: %q", wire[:min(60, len(wire))])
	}
	// ordered headers come in rawHeaderOrder position
	ai := strings.Index(wire, "Authorization:")
	ui := strings.Index(wire, "User-Agent:")
	if ai < 0 || ui < 0 || ai > ui {
		t.Fatalf("header order wrong: %q", wire)
	}
	// defaults filled for Accept + Content-Type on a POST
	if !strings.Contains(wire, "Accept: application/json") ||
		!strings.Contains(wire, "Content-Type: application/json") {
		t.Fatalf("defaults missing: %q", wire)
	}
	// extra headers sorted after the ordered set; caller's Connection
	// value is dropped in favor of the writer's own keep-alive trailer
	if !strings.Contains(wire, "X-Custom: z") || strings.Contains(wire, "Connection: close") ||
		!strings.Contains(wire, "Connection: keep-alive") {
		t.Fatalf("extra/hop headers: %q", wire)
	}
}

func TestWriteRawRequestNoBodyDefaults(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	rc := &rawConn{conn: client, br: bufio.NewReader(client)}
	req, _ := http.NewRequest("GET", "https://x/y", nil)

	done := make(chan error, 1)
	go func() { done <- writeRawRequest(rc, req) }()
	server.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, _ := io.ReadAll(bufio.NewReader(server))
	if err := <-done; err != nil {
		t.Fatalf("write: %v", err)
	}
	wire := string(got)
	// bodyless GET: Accept default yes, Content-Type default no
	if !strings.Contains(wire, "Accept: application/json") {
		t.Fatalf("GET missing Accept default: %q", wire)
	}
	if strings.Contains(wire, "Content-Type:") {
		t.Fatalf("GET should not default Content-Type: %q", wire)
	}
}

func TestIsTimeoutErr(t *testing.T) {
	if isTimeoutErr(errors.New("nope")) {
		t.Fatal("plain error is not a timeout")
	}
	if !isTimeoutErr(timeoutErr{}) {
		t.Fatal("net.Error timeout not detected")
	}
	// strict by design: a non-net.Error with "timeout" in the message
	// doesn't count
	if isTimeoutErr(errors.New("i/o timeout")) {
		t.Fatal("non-net.Error must not match")
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "deadline exceeded" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}
