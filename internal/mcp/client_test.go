package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// rpcHandler answers initialize/tools/list/prompts over a JSON-RPC body.
func rpcHandler(body []byte) []byte {
	var req struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
	}
	json.Unmarshal(body, &req)
	var result any
	switch req.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}, "prompts": map[string]any{}},
			"serverInfo":      map[string]string{"name": "test", "version": "1.0"},
			"instructions":    "test instructions",
		}
	case "tools/list":
		result = map[string]any{"tools": []map[string]any{
			{"name": "t1", "description": "d", "inputSchema": map[string]any{"type": "object"}},
		}}
	case "prompts/list":
		result = map[string]any{"prompts": []map[string]any{
			{"name": "p1", "description": "pd"},
		}}
	case "tools/call":
		result = map[string]any{"content": []map[string]any{
			{"type": "text", "text": "tool-output"},
		}}
	case "prompts/get":
		result = map[string]any{"messages": []map[string]any{
			{"role": "user", "content": map[string]string{"type": "text", "text": "hi"}},
		}}
	case "resources/templates/list":
		result = map[string]any{"resourceTemplates": []map[string]any{
			{"uriTemplate": "file:///{p}", "name": "f"},
		}}
	default:
		out, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"error": map[string]any{"code": -32601, "message": "nf"},
		})
		return out
	}
	out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	return out
}

func TestHTTPTransportHandshake(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			ID int64 `json:"id"`
		}
		json.Unmarshal(body, &probe)
		if probe.ID == 0 { // notification
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(rpcHandler(body))
	}))
	defer srv.Close()

	c, err := Connect("t", ServerConfig{Type: "http", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if len(c.Tools) != 1 || c.Tools[0].Name != "t1" {
		t.Fatalf("tools: %+v", c.Tools)
	}
	if c.Instructions != "test instructions" {
		t.Fatalf("instructions: %q", c.Instructions)
	}
	if Instructions["t"] != "test instructions" {
		t.Fatalf("package Instructions: %v", Instructions)
	}
	prompts, _, err := c.ListPrompts(context.Background(), "")
	if err != nil || len(prompts) != 1 || prompts[0].Name != "p1" {
		t.Fatalf("prompts: %v %v", prompts, err)
	}
	pr, err := c.GetPrompt(context.Background(), "p1", nil)
	if err != nil || len(pr.Messages) != 1 {
		t.Fatalf("get prompt: %v %v", pr, err)
	}
	tmpl, _, err := c.ListResourceTemplates(context.Background(), "")
	if err != nil || len(tmpl) != 1 {
		t.Fatalf("templates: %v %v", tmpl, err)
	}
	if st, _ := Status("t"); st.Status != "connected" {
		t.Fatalf("status: %v", st)
	}
}

func TestSSETransportHandshake(t *testing.T) {
	var postCh chan []byte = make(chan []byte, 16)
	var streams atomic.Int32
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "event: endpoint\ndata: /msg\n\n")
		fl.Flush()
		streams.Add(1)
		for msg := range postCh {
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
			fl.Flush()
		}
	})
	mux.HandleFunc("/msg", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			ID int64 `json:"id"`
		}
		json.Unmarshal(body, &probe)
		if probe.ID == 0 { // notification
			w.WriteHeader(http.StatusAccepted)
			return
		}
		postCh <- rpcHandler(body)
		w.WriteHeader(http.StatusAccepted)
	})
	defer close(postCh)

	c, err := Connect("s", ServerConfig{Type: "sse", URL: srv.URL + "/sse"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if len(c.Tools) != 1 || c.Tools[0].Name != "t1" {
		t.Fatalf("tools: %+v", c.Tools)
	}
	out, isErr, err := c.CallTool(context.Background(), "t1", nil)
	_ = isErr
	if err != nil {
		t.Fatalf("calltool over sse: %v", err)
	}
	_ = out
}

func TestHTTPTransportSSEBody(t *testing.T) {
	// server returns SSE-framed responses to POSTs (streamable-http mode)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			ID int64 `json:"id"`
		}
		json.Unmarshal(body, &probe)
		if probe.ID == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", rpcHandler(body))
	}))
	defer srv.Close()

	c, err := Connect("b", ServerConfig{Type: "http", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if len(c.Tools) != 1 {
		t.Fatalf("tools: %+v", c.Tools)
	}
}

func TestAuthHeaderAndEnvInterp(t *testing.T) {
	t.Setenv("MCP_TEST_TOK", "sekret")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		body, _ := io.ReadAll(r.Body)
		w.Write(rpcHandler(body))
	}))
	defer srv.Close()
	c, err := Connect("a", ServerConfig{
		Type: "http", URL: srv.URL,
		Authorization: "Bearer ${MCP_TEST_TOK}",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if gotAuth != "Bearer sekret" {
		t.Fatalf("authorization: %q", gotAuth)
	}
}

func TestNeedsAuthStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := Connect("na", ServerConfig{Type: "http", URL: srv.URL})
	if err == nil {
		t.Fatal("expected auth error")
	}
	var ae *AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("expected AuthError, got %T: %v", err, err)
	}
	st, ok := Status("na")
	if !ok || st.Status != "needs-auth" {
		t.Fatalf("status: %+v ok=%v", st, ok)
	}
}

func TestNotificationDispatch(t *testing.T) {
	notify := make(chan string, 4)
	var postCh chan []byte = make(chan []byte, 16)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "event: endpoint\ndata: /msg\n\n")
		fl.Flush()
		for msg := range postCh {
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
			fl.Flush()
		}
	})
	mux.HandleFunc("/msg", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		json.Unmarshal(body, &probe)
		if probe.ID != 0 {
			if probe.Method == "tools/call" {
				// interleave a server notification with the response
				postCh <- []byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":0.5}}`)
			}
			postCh <- rpcHandler(body)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	defer close(postCh)

	c, err := Connect("n", ServerConfig{Type: "sse", URL: srv.URL + "/sse"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetNotificationHandler(func(method string, _ json.RawMessage) { notify <- method })
	// trigger the notification via a call
	go c.CallTool(context.Background(), "t1", nil)
	select {
	case m := <-notify:
		if m != "notifications/progress" {
			t.Fatalf("method: %s", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notification received")
	}
}

// tools/list_changed: the client refetches tools/list and fires
// OnToolsChanged so the registry re-registers mcp__server__tool rows.
func TestToolsChangedRefresh(t *testing.T) {
	changed := make(chan struct{}, 2)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	postCh := make(chan []byte, 16)
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "event: endpoint\ndata: /msg\n\n")
		fl.Flush()
		for msg := range postCh {
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
			fl.Flush()
		}
	})
	mux.HandleFunc("/msg", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		json.Unmarshal(body, &probe)
		if probe.Method == "tools/list" {
			// second tools/list returns a DIFFERENT set — the registry
			// must see the updated list via OnToolsChanged
			out, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": probe.ID,
				"result": map[string]any{"tools": []map[string]any{
					{"name": "t2", "description": "d2", "inputSchema": map[string]any{"type": "object"}},
				}},
			})
			postCh <- out
		} else if probe.ID != 0 {
			postCh <- rpcHandler(body)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	defer close(postCh)

	c, err := Connect("n", ServerConfig{Type: "sse", URL: srv.URL + "/sse"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.OnToolsChanged = func() { changed <- struct{}{} }
	// simulate the server sending the notification via the stream
	go func() {
		time.Sleep(100 * time.Millisecond)
		postCh <- []byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{}}`)
	}()
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("OnToolsChanged never fired after tools/list_changed")
	}
	c.mu.Lock()
	got := c.Tools
	c.mu.Unlock()
	if len(got) != 1 || got[0].Name != "t2" {
		t.Fatalf("tools not refreshed: %+v", got)
	}
}
