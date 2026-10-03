package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// The CCR v2 data plane (mod_1214.js) is NOT a websocket — it's an SSE read
// stream plus worker REST writes, all under a worker_jwt minted by
// POST /v1/code/sessions/{id}/bridge:
//
//   read : GET  {api_base_url}/v1/code/sessions/{cse}/worker/events/stream
//          (SSE; closes carry supersession codes — see CloseReason)
//   write: POST {sessionBase}/worker/events           (durable frames)
//          POST {sessionBase}/worker/events/delivery  (acks)
//          POST {sessionBase}/worker/internal-events  (control rows)
//          PUT  {sessionBase}/worker                  (worker_status etc.)
//          POST {sessionBase}/worker/heartbeat        (session keepalive)
//          GET  {sessionBase}/worker?include_hearth=true (state read-back)
//
// sessionBase = the session URL (SessionURL(apiBase, id)) minus nothing —
// the worker endpoints hang off the session path directly.
//
// What is intentionally NOT implemented: the frame protocol on the stream
// (envelope schema, upload batching, sequence fencing). The handshake below
// is faithful — registerWorker, PUT /worker worker_status, heartbeat shape
// — but frames on the wire are backend-gated and reported as unhandled.

// CloseReason codes real's transport treats as terminal stand-downs
// (sNn in mod_1214.js).
var CloseReason = map[string]int{
	"epoch_conflict":       4090,
	"superseded_by_worker": 4090,
	"session_not_active":   4090,
	"epoch_stale":          4090,
	"session_not_found":    4090,
	"token_expired":        4094,
	"auth_exhausted":       4094,
}

// CloseReasonText mirrors sbr()/the close-code message table.
func CloseReasonText(code int, reason string) string {
	switch code {
	case 4090:
		switch reason {
		case "superseded_by_worker":
			return "another connection took over this session (usually another device or Claude Code session) — this device is standing down (code 4090)"
		case "session_not_active":
			return "this session was ended or archived from another device or app (code 4090)"
		case "session_not_found":
			return "the server no longer reports this session — it may have been deleted from another device or app (code 4090)"
		case "epoch_stale":
			return "the session worker registration went stale — no active worker holds it (code 4090)"
		default:
			return "this connection is no longer the active worker for the session (code 4090)"
		}
	case 4094:
		return "worker credential expired or rejected (code 4094)"
	case 1002:
		return "server rejected the connection handshake (code 1002)"
	case 4001:
		return "session expired or not found on server (code 4001)"
	case 4003:
		return "server rejected credentials (code 4003)"
	case 0:
		return "no close code received"
	default:
		return fmt.Sprintf("code %d", code)
	}
}

// WorkerTransport is the honest handshake of the v2 data plane. Dial()
// performs registerWorker + opens /worker/events/stream and reports the
// stream as established; frame decode is stubbed — events surface as raw
// SSE payloads via OnFrame.
type WorkerTransport struct {
	Client     *http.Client
	BaseURL    string // session base, e.g. SessionURL(apiBase, id)
	IngressJWT string // worker_jwt (or session_ingress_token on the env lane)
	Epoch      int64
	SessionID  string

	// OnFrame gets raw (eventType, data) SSE tuples; the framed protocol is
	// intentionally not decoded — see package doc.
	OnFrame func(eventType string, data []byte)
	OnClose func(code int, reason string)
	OnDebug func(string)
}

func (t *WorkerTransport) debugf(f string, a ...any) {
	if t.OnDebug != nil {
		t.OnDebug(fmt.Sprintf(f, a...))
	}
}

func (t *WorkerTransport) authHeader() http.Header {
	return http.Header{
		"Authorization":     {"Bearer " + t.IngressJWT},
		"Content-Type":      {"application/json"},
		"anthropic-version": {anthropicVersion},
		"Accept":            {"text/event-stream"},
	}
}

// workerHeartbeat posts the mod_1214.js heartbeat shape:
// {session_id, worker_epoch, supports_heartbeat_probe?,
//
//	current_interval_seconds?, idle_seconds?}.
func (t *WorkerTransport) workerHeartbeat(ctx context.Context, intervalMs int, probe bool, idleSec *int) error {
	body := map[string]any{
		"session_id":   t.SessionID,
		"worker_epoch": t.Epoch,
	}
	if probe {
		body["supports_heartbeat_probe"] = true
		body["current_interval_seconds"] = intervalMs / 1000
		if idleSec != nil {
			body["idle_seconds"] = *idleSec
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.BaseURL+"/worker/heartbeat", strings.NewReader(mustJSON(body)))
	if err != nil {
		return err
	}
	for k, vs := range t.authHeader() {
		req.Header[k] = vs
	}
	resp, err := t.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return &FatalError{Status: resp.StatusCode, ErrorType: "token_expired",
			Message: CloseReasonText(4094, "")}
	}
	if resp.StatusCode == 409 {
		return &FatalError{Status: 409, ErrorType: "epoch_conflict",
			Message: CloseReasonText(4090, "epoch_conflict")}
	}
	if resp.StatusCode >= 400 {
		return &statusError{Status: resp.StatusCode,
			Message: fmt.Sprintf("Heartbeat: Failed with status %d", resp.StatusCode)}
	}
	return nil
}

func (t *WorkerTransport) httpClient() *http.Client {
	if t.Client != nil {
		return t.Client
	}
	return http.DefaultClient
}

// OpenStream dials GET {base}/worker/events/stream and reads SSE frames
// until ctx ends or the server closes. Handshake faithful (mod_1214.js
// ibr()/gQt): Bearer ingress token, Accept: text/event-stream.
// Frame payloads are passed through raw — decode is intentionally stubbed.
func (t *WorkerTransport) OpenStream(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		t.BaseURL+"/worker/events/stream", nil)
	if err != nil {
		return err
	}
	for k, vs := range t.authHeader() {
		req.Header[k] = vs
	}
	resp, err := t.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return &FatalError{Status: resp.StatusCode, ErrorType: "token_expired",
				Message: CloseReasonText(4094, "")}
		}
		return &statusError{Status: resp.StatusCode,
			Message: fmt.Sprintf("cli_sse_connect_http_error http_%d", resp.StatusCode)}
	}
	t.debugf("[bridge:repl] v2 transport ready for writes (epoch=%d); opening the read stream", t.Epoch)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var event string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimSpace(line[5:])
			if t.OnFrame != nil {
				t.OnFrame(event, []byte(data))
			}
			event = ""
		case line == "":
			event = ""
		}
	}
	return sc.Err()
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
