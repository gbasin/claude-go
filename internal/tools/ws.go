package tools

// minimal WebSocket client (RFC6455, no masking verification needed — client masks)
import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net"
	"net/url"
	"strings"
)

func wsDial(rawurl string) (net.Conn, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	conn, err := net.Dial("tcp", host)
	if err != nil {
		return nil, err
	}
	keyBytes := make([]byte, 16)
	rand.Read(keyBytes)
	key := base64.StdEncoding.EncodeToString(keyBytes)
	path := u.Path
	if path == "" {
		path = "/"
	}
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		path, u.Host, key)
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		conn.Close()
		return nil, fmt.Errorf("ws handshake failed: %s", strings.TrimSpace(status))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil || line == "\r\n" {
			break
		}
	}
	// wrap: conn is a raw net.Conn; keep the bufio reader via a shim conn
	return &wsConn{conn, br}, nil
}

type wsConn struct {
	net.Conn
	br *bufio.Reader
}

func (w *wsConn) Read(p []byte) (int, error) { return w.br.Read(p) }

// wsRead returns (opcode, payload, err) — handles text(1)/binary(2)/close(8)/ping(9)/pong(10)
func wsRead(c net.Conn) (byte, []byte, error) {
	var hdr [2]byte
	if _, err := readFull(c, hdr[:]); err != nil {
		return 0, nil, err
	}
	op := hdr[0] & 0x0f
	masked := hdr[1]&0x80 != 0
	l := uint64(hdr[1] & 0x7f)
	if l == 126 {
		var ext [2]byte
		if _, err := readFull(c, ext[:]); err != nil {
			return 0, nil, err
		}
		l = uint64(binary.BigEndian.Uint16(ext[:]))
	} else if l == 127 {
		var ext [8]byte
		if _, err := readFull(c, ext[:]); err != nil {
			return 0, nil, err
		}
		l = binary.BigEndian.Uint64(ext[:])
	}
	if l > 4<<20 {
		return 0, nil, fmt.Errorf("frame too large: %d", l)
	}
	var mask [4]byte
	if masked {
		if _, err := readFull(c, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, l)
	if _, err := readFull(c, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	if op == 9 { // ping → pong
		wsWrite(c, 10, payload)
		return wsRead(c)
	}
	if op == 8 {
		return 0, nil, fmt.Errorf("ws closed by server")
	}
	return op, payload, nil
}

func wsWrite(c net.Conn, op byte, payload []byte) error {
	var hdr []byte
	hdr = append(hdr, 0x80|op)
	maskKey := make([]byte, 4)
	rand.Read(maskKey)
	l := len(payload)
	switch {
	case l < 126:
		hdr = append(hdr, 0x80|byte(l))
	case l < 65536:
		hdr = append(hdr, 0x80|126, byte(l>>8), byte(l))
	default:
		hdr = append(hdr, 0x80|127)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(l))
		hdr = append(hdr, b[:]...)
	}
	hdr = append(hdr, maskKey...)
	for i := range payload {
		payload[i] ^= maskKey[i%4]
	}
	if _, err := c.Write(append(hdr, payload...)); err != nil {
		return err
	}
	return nil
}

func readFull(c net.Conn, p []byte) (int, error) {
	total := 0
	for total < len(p) {
		n, err := c.Read(p[total:])
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

var _ = sha1.New
