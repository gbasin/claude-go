package browsermcp

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"
)

func TestDbg(t *testing.T) {
	outR, outW := io.Pipe()
	h := &nativeHost{mcpClients: map[int]net.Conn{}, out: outW,
		computer: func() string { return "h" }}
	go h.handleMessage(`{"type":"ping"}`)
	var hdr [4]byte
	if _, err := io.ReadFull(outR, hdr[:]); err != nil {
		t.Fatal(err)
	}
	l := binary.LittleEndian.Uint32(hdr[:])
	t.Logf("len=%d hdr=%v", l, hdr)
	buf := make([]byte, l)
	io.ReadFull(outR, buf)
	t.Logf("buf=%q", string(buf))
	var m map[string]any
	t.Logf("unmarshal err=%v", json.Unmarshal(buf, &m))
	t.Logf("m=%v", m)
	time.Sleep(50 * time.Millisecond)
}
