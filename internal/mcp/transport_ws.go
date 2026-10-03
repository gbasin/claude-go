package mcp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// WebSocket transport — used by the IDE bridge (extension writes a
// ~/.claude/ide/<port>.lock with transport:"ws"; the MCP endpoint speaks
// JSON-RPC over RFC6455). Minimal client: handshake + masked text frames.

type wsConn struct {
	conn   net.Conn
	reader *bufio.Reader
	wmu    sync.Mutex
}

func wsDial(addr, path, authToken string) (*wsConn, error) {
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 16)
	rand.Read(key)
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n",
		path, addr, base64.StdEncoding.EncodeToString(key))
	if authToken != "" {
		req += "x-claude-code-ide-authorization: " + authToken + "\r\n"
	}
	req += "\r\n"
	if _, err := c.Write([]byte(req)); err != nil {
		c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		c.Close()
		return nil, err
	}
	if resp.StatusCode != 101 {
		resp.Body.Close()
		c.Close()
		return nil, fmt.Errorf("ws handshake: %s", resp.Status)
	}
	return &wsConn{conn: c, reader: br}, nil
}

// sendFrame writes one masked text frame.
func (w *wsConn) sendFrame(payload []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	var hdr []byte
	hdr = append(hdr, 0x81) // FIN + text
	l := len(payload)
	switch {
	case l < 126:
		hdr = append(hdr, 0x80|byte(l)) // mask bit + len
	case l < 65536:
		hdr = append(hdr, 0x80|126)
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(l))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(l))
	}
	mask := make([]byte, 4)
	rand.Read(mask)
	hdr = append(hdr, mask...)
	masked := make([]byte, l)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := w.conn.Write(hdr); err != nil {
		return err
	}
	_, err := w.conn.Write(masked)
	return err
}

// readFrame returns the next full message payload (fragments reassembled,
// server frames are unmasked, ping→pong, close→io.EOF).
func (w *wsConn) readFrame() ([]byte, error) {
	var msg []byte
	for {
		h := make([]byte, 2)
		if _, err := io.ReadFull(w.reader, h); err != nil {
			return nil, err
		}
		fin := h[0]&0x80 != 0
		op := h[0] & 0x0f
		masked := h[1]&0x80 != 0
		l := int64(h[1] & 0x7f)
		switch l {
		case 126:
			var b [2]byte
			io.ReadFull(w.reader, b[:])
			l = int64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			io.ReadFull(w.reader, b[:])
			l = int64(binary.BigEndian.Uint64(b[:]))
		}
		if l > 8<<20 {
			return nil, fmt.Errorf("ws frame too large")
		}
		var mask [4]byte
		if masked {
			io.ReadFull(w.reader, mask[:])
		}
		payload := make([]byte, l)
		if _, err := io.ReadFull(w.reader, payload); err != nil {
			return nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		switch op {
		case 0x8: // close
			return nil, io.EOF
		case 0x9: // ping → pong
			w.sendControl(0xA, payload)
			continue
		case 0xA: // pong
			continue
		case 0x0, 0x1, 0x2: // continuation / text / binary
			msg = append(msg, payload...)
			if fin {
				return msg, nil
			}
		}
	}
}

func (w *wsConn) sendControl(op byte, payload []byte) {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	mask := make([]byte, 4)
	rand.Read(mask)
	f := []byte{0x80 | op, 0x80 | byte(len(payload))}
	f = append(f, mask...)
	for i := range payload {
		f = append(f, payload[i]^mask[i%4])
	}
	w.conn.Write(f)
}

// wsTransport adapts wsConn to the MCP transport interface.
type wsTransport struct {
	ws *wsConn
}

func (t *wsTransport) send(_ context.Context, msg []byte) error {
	return t.ws.sendFrame(msg)
}

func (t *wsTransport) close() error {
	t.ws.sendControl(0x8, nil)
	return t.ws.conn.Close()
}

// ConnectWS connects to a JSON-RPC-over-WebSocket MCP endpoint (IDE bridge).
func ConnectWS(name, addr, path, authToken string) (*Client, error) {
	ws, err := wsDial(addr, path, authToken)
	if err != nil {
		return nil, err
	}
	c := &Client{
		Name:    name,
		tr:      &wsTransport{ws: ws},
		pending: map[string]chan rpcResp{},
	}
	go c.wsReadLoop(ws)
	if err := c.handshake(); err != nil {
		c.Close()
		return nil, err
	}
	recordStatus(name, "connected", "", "")
	return c, nil
}

func (c *Client) wsReadLoop(ws *wsConn) {
	for {
		msg, err := ws.readFrame()
		if err != nil {
			return
		}
		if len(msg) == 0 || strings.TrimSpace(string(msg)) == "" {
			continue
		}
		cp := make([]byte, len(msg))
		copy(cp, msg)
		c.dispatch(cp)
	}
}
