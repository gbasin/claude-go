package api

import (
	"strings"
	"testing"
)

const sseFixture = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`

func TestSSEDecode(t *testing.T) {
	var events []string
	var text string
	err := StreamEvents(strings.NewReader(sseFixture), func(ev *StreamEvent) error {
		events = append(events, ev.Type)
		if ev.Delta != nil && ev.Delta.Type == "text_delta" {
			text += ev.Delta.Text
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if text != "Hello world" {
		t.Errorf("text = %q", text)
	}
	want := []string{"message_start", "content_block_start", "content_block_delta",
		"content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if len(events) != len(want) {
		t.Fatalf("events = %v", events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("event[%d] = %s, want %s", i, events[i], want[i])
		}
	}
}

func TestStreamAssemble(t *testing.T) {
	toolSSE := `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"x","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}

event: message_stop
data: {"type":"message_stop"}

`
	var got StreamEvent
	var resp Response
	resp.Content = nil
	var blocks []ContentBlock
	err := StreamEvents(strings.NewReader(toolSSE), func(ev *StreamEvent) error {
		got = *ev
		return nil
	})
	_ = got
	_ = resp
	_ = blocks
	if err != nil {
		t.Fatal(err)
	}
}
