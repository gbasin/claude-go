package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"claude-go/internal/api"
	"claude-go/internal/auth"
	"claude-go/internal/mock"
)

func TestMockToolInput(t *testing.T) {
	s, err := mock.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := &api.Client{BaseURL: s.Addr, HTTP: http.DefaultClient,
		Creds: &auth.Credentials{IsAPIKey: true, AccessToken: "x"}}
	resp, err := c.StreamMessage(context.Background(), &api.Request{
		Model: "m", Stream: true,
		Messages: []api.Message{api.TextMessage("user", `tool:Bash {"command":"echo hi"}`)},
	}, func(*api.StreamEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range resp.Content {
		t.Logf("type=%s input=%s", b.Type, string(b.Input))
		var m map[string]any
		t.Logf("unmarshal: %v %v", json.Unmarshal(b.Input, &m), m)
	}
}
