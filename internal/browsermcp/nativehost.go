package browsermcp

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// nativehost.go — runChromeNativeHost (mod_0788.js, chunk-1exv0s4x): the
// `--chrome-native-host` mode. Chrome spawns this as a native messaging
// host; it owns this process's stdio with uint32-LE length-prefixed JSON
// (Chrome native messaging protocol), and listens on the per-pid unix
// socket so the `--claude-in-chrome-mcp` servers can reach the extension.
//
// Chrome → host messages: ping | get_status | tool_response |
// notification | unknown → error.
// host → Chrome messages: pong | status_response | tool_request |
// mcp_connected | mcp_disconnected | error.
// Socket clients speak the same uint32-LE framing; inbound frames are
// {method, params} forwarded to Chrome as {type:"tool_request", ...}.

const nativeHostVersion = "1.0.0" // U in mod_0788.js
const nativeHostMaxMsg = 1048576  // w — 1 MiB
const computerNameMax = 64        // T
const computerNameTimeout = 2000  // x — ms, scutil timeout

// chromeMsg is one decoded Chrome-side message.
type chromeMsg struct {
	Type string `json:"type"`
	Raw  map[string]any
}

// nativeHost mirrors class R: socket listener + MCP client registry.
type nativeHost struct {
	mu         sync.Mutex
	mcpClients map[int]net.Conn
	nextID     int
	listener   net.Listener
	socketPath string
	running    bool
	stdoutMu   sync.Mutex
	out        io.Writer
	computer   func() string // computer-name lookup (injectable)
}

// writeChrome mirrors c(e): uint32-LE + JSON to the extension over stdout.
func (h *nativeHost) writeChrome(v any) {
	b, _ := json.Marshal(v)
	frame := make([]byte, 4+len(b))
	binary.LittleEndian.PutUint32(frame, uint32(len(b)))
	copy(frame[4:], b)
	h.stdoutMu.Lock()
	defer h.stdoutMu.Unlock()
	h.out.Write(frame)
}

// start mirrors R.start(): mkdir 0700, reap stale *.sock for dead pids,
// listen, chmod 0600.
func (h *nativeHost) start() error {
	if h.running {
		return nil
	}
	h.socketPath = SocketPath()
	if runtime.GOOS != "windows" {
		dir := socketDir()
		os.MkdirAll(dir, 0o700)
		os.Chmod(dir, 0o700)
		if ents, err := os.ReadDir(dir); err == nil {
			for _, e := range ents {
				name := e.Name()
				if !strings.HasSuffix(name, ".sock") {
					continue
				}
				pid, err := strconv.Atoi(strings.TrimSuffix(name, ".sock"))
				if err != nil {
					continue
				}
				if !pidAlive(pid) {
					os.Remove(filepath.Join(dir, name))
					logHost("Removed stale socket for PID %d", pid)
				}
			}
		}
	}
	l, err := net.Listen("unix", h.socketPath)
	if err != nil {
		return fmt.Errorf("socket listen: %w", err)
	}
	h.listener = l
	h.running = true
	logHost("Socket server listening for connections")
	if runtime.GOOS != "windows" {
		if err := os.Chmod(h.socketPath, 0o600); err != nil {
			logHost("Failed to set socket permissions: %v", err)
		}
	}
	go h.acceptLoop()
	return nil
}

func (h *nativeHost) acceptLoop() {
	for {
		conn, err := h.listener.Accept()
		if err != nil {
			return
		}
		go h.handleMCPClient(conn)
	}
}

// stop mirrors R.stop(): destroy clients, close listener, remove socket
// file + the dir when empty.
func (h *nativeHost) stop() {
	if !h.running {
		return
	}
	h.mu.Lock()
	for id, c := range h.mcpClients {
		c.Close()
		delete(h.mcpClients, id)
	}
	h.mu.Unlock()
	if h.listener != nil {
		h.listener.Close()
		h.listener = nil
	}
	if runtime.GOOS != "windows" && h.socketPath != "" {
		os.Remove(h.socketPath)
		if ents, err := os.ReadDir(socketDir()); err == nil && len(ents) == 0 {
			os.Remove(socketDir())
			logHost("Removed empty socket directory")
		}
	}
	h.running = false
}

// broadcast forwards a Chrome message (minus "type") to every MCP client
// as a uint32-LE frame — mirrors the tool_response / notification cases.
func (h *nativeHost) broadcast(m map[string]any, what string) {
	delete(m, "type")
	b, _ := json.Marshal(m)
	frame := make([]byte, 4+len(b))
	binary.LittleEndian.PutUint32(frame, uint32(len(b)))
	copy(frame[4:], b)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.mcpClients) == 0 {
		return
	}
	logHost("Forwarding %s to %d MCP clients", what, len(h.mcpClients))
	for id, c := range h.mcpClients {
		if _, err := c.Write(frame); err != nil {
			logHost("Failed to send to MCP client %d: %v", id, err)
		}
	}
}

// handleMessage mirrors R.handleMessage — the Chrome-side dispatch.
func (h *nativeHost) handleMessage(raw string) {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		logHost("Invalid JSON from Chrome: %v", err)
		h.writeChrome(map[string]any{"type": "error", "error": "Invalid message format"})
		return
	}
	typ, _ := m["type"].(string)
	if typ == "" {
		h.writeChrome(map[string]any{"type": "error", "error": "Invalid message format"})
		return
	}
	logHost("Handling Chrome message type: %s", typ)
	switch typ {
	case "ping":
		h.writeChrome(map[string]any{"type": "pong", "timestamp": time.Now().UnixMilli()})
	case "get_status":
		name := h.computer()
		h.writeChrome(map[string]any{"type": "status_response",
			"native_host_version": nativeHostVersion, "computer_name": name})
	case "tool_response":
		h.broadcast(m, "tool response")
	case "notification":
		h.broadcast(m, "notification")
	default:
		logHost("Unknown message type: %s", typ)
		h.writeChrome(map[string]any{"type": "error", "error": "Unknown message type: " + typ})
	}
}

// handleMCPClient mirrors R.handleMcpClient: register, frame-parse inbound
// {method,params} → {type:"tool_request",method,params} to Chrome.
func (h *nativeHost) handleMCPClient(conn net.Conn) {
	h.mu.Lock()
	h.nextID++
	id := h.nextID
	h.mcpClients[id] = conn
	n := len(h.mcpClients)
	h.mu.Unlock()
	logHost("MCP client %d connected. Total clients: %d", id, n)
	h.writeChrome(map[string]any{"type": "mcp_connected"})
	defer func() {
		conn.Close()
		h.mu.Lock()
		delete(h.mcpClients, id)
		n := len(h.mcpClients)
		h.mu.Unlock()
		logHost("MCP client %d disconnected. Remaining clients: %d", id, n)
		h.writeChrome(map[string]any{"type": "mcp_disconnected"})
	}()
	var buf []byte
	tmp := make([]byte, 64<<10)
	for {
		nr, err := conn.Read(tmp)
		if nr > 0 {
			buf = append(buf, tmp[:nr]...)
			for len(buf) >= 4 {
				l := int(binary.LittleEndian.Uint32(buf))
				if l == 0 || l > nativeHostMaxMsg {
					logHost("Invalid message length from MCP client %d: %d", id, l)
					return
				}
				if len(buf) < 4+l {
					break
				}
				frame := buf[4 : 4+l]
				buf = buf[4+l:]
				var req map[string]any
				if err := json.Unmarshal(frame, &req); err != nil {
					logHost("Failed to parse tool request from MCP client %d: %v", id, err)
					continue
				}
				logHost("Forwarding tool request from MCP client %d: %v", id, req["method"])
				h.writeChrome(map[string]any{
					"type": "tool_request", "method": req["method"], "params": req["params"],
				})
			}
		}
		if err != nil {
			return
		}
	}
}

// computerName mirrors L()/z(): scutil ComputerName on darwin,
// COMPUTERNAME on win32, hostname otherwise — control chars stripped,
// ".local" suffix dropped, capped at 64.
func computerName() string {
	var raw string
	switch runtime.GOOS {
	case "darwin":
		ctx, cancel := context.WithTimeout(context.Background(),
			time.Duration(computerNameTimeout)*time.Millisecond)
		out, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--get", "ComputerName").Output()
		cancel()
		if err == nil {
			raw = string(out)
		}
	case "windows":
		raw = os.Getenv("COMPUTERNAME")
	default:
		h, err := os.Hostname()
		if err == nil {
			raw = h
		}
	}
	cleaned := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, raw)
	cleaned = strings.TrimSpace(cleaned)
	lower := strings.ToLower(cleaned)
	if strings.HasSuffix(lower, ".local") {
		cleaned = cleaned[:len(cleaned)-len(".local")]
	}
	cleaned = strings.TrimSpace(cleaned)
	if len(cleaned) > computerNameMax {
		cleaned = cleaned[:computerNameMax]
	}
	return cleaned
}

// RunNativeHost mirrors te(): start the socket listener, then read
// uint32-LE messages from stdin until EOF.
func RunNativeHost(in io.Reader, out io.Writer) int {
	logHost("Initializing...")
	h := &nativeHost{
		mcpClients: map[int]net.Conn{},
		out:        out,
		computer:   computerName,
	}
	if err := h.start(); err != nil {
		logHost("socket listen failed: %v", err)
		return 1
	}
	reader := bufio.NewReader(in)
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(reader, hdr[:]); err != nil {
			break
		}
		l := binary.LittleEndian.Uint32(hdr[:])
		if l == 0 || l > nativeHostMaxMsg {
			logHost("Invalid message length: %d", l)
			break
		}
		buf := make([]byte, l)
		if _, err := io.ReadFull(reader, buf); err != nil {
			break
		}
		h.handleMessage(string(buf))
	}
	h.stop()
	return 0
}

// logHost mirrors s() — stderr, prefixed.
func logHost(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "[Claude Chrome Native Host] "+f+"\n", a...)
}

// --- small helpers ---------------------------------------------------------

func pidAlive(pid int) bool {
	// mirrors process.kill(pid, 0) — existence probe
	err := syscallKill0(pid)
	return err == nil
}
