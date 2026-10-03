package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// Event is one parsed SSE frame.
type Event struct {
	Name string
	Data []byte
}

// SSEReader decodes text/event-stream.
type SSEReader struct {
	r *bufio.Reader
}

func NewSSEReader(r io.Reader) *SSEReader {
	return &SSEReader{r: bufio.NewReaderSize(r, 1<<16)}
}

func (s *SSEReader) Next() (*Event, error) {
	var name string
	var data bytes.Buffer
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			if err == io.EOF && data.Len() > 0 {
				return &Event{Name: name, Data: data.Bytes()}, io.EOF
			}
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if data.Len() > 0 || name != "" {
				return &Event{Name: name, Data: data.Bytes()}, nil
			}
			continue
		}
		if line[0] == ':' {
			continue // comment
		}
		field, val, _ := bytes.Cut([]byte(line), []byte(":"))
		v := string(val)
		if len(v) > 0 && v[0] == ' ' {
			v = v[1:]
		}
		switch string(field) {
		case "event":
			name = v
		case "data":
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(v)
		}
	}
}

// StreamEvents yields decoded StreamEvent values until message_stop/error/EOF.
func StreamEvents(body io.Reader, yield func(*StreamEvent) error) error {
	sr := NewSSEReader(body)
	for {
		ev, err := sr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if ev.Name == "ping" || len(ev.Data) == 0 {
			continue
		}
		var se StreamEvent
		if err := json.Unmarshal(ev.Data, &se); err != nil {
			continue // tolerate unknown event shapes
		}
		if se.Type == "error" {
			if se.Error != nil {
				return &ErrAPI{E: se.Error}
			}
			return &ErrAPI{E: &APIError{Type: "error", Message: string(ev.Data)}}
		}
		if err := yield(&se); err != nil {
			return err
		}
		if se.Type == "message_stop" {
			return nil
		}
	}
}
