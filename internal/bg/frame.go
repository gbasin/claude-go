package bg

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Rendezvous socket framing — real runs a framed protocol over the session
// UDS; ours is [1B op][4B BE len][payload]:
//
//	opHello  client → host: attach request (payload: JSON {cols,rows})
//	opReady  host → client: attach accepted (payload: JSON {cols,rows,session})
//	opData   both ways: raw pty bytes
//	opResize client → host: terminal resize (payload: JSON {cols,rows})
//	opDetach client → host: clean detach (host keeps running)
//	opExit   host → client: session process exited (payload: exit note)
const (
	opHello  byte = 1
	opReady  byte = 2
	opData   byte = 3
	opResize byte = 4
	opDetach byte = 5
	opExit   byte = 6
)

func writeFrame(w io.Writer, op byte, payload []byte) error {
	var hdr [5]byte
	hdr[0] = op
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := w.Write(payload)
		return err
	}
	return nil
}

// readFrame returns (op, payload); io.EOF when the peer closes cleanly.
func readFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > 1<<20 {
		return 0, nil, fmt.Errorf("frame op %d oversized (%d bytes)", hdr[0], n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}
