package mcp

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// IDE bridge — real CC reads ~/.claude/ide/*.lock (filename = port; body has
// workspaceFolders, pid, ideName, transport:"ws", authToken), probes the
// port, and connects the extension's MCP-over-WS endpoint. The extension then
// exposes mcp__ide__* tools (diagnostics, selection, openDiff...).

type ideLock struct {
	WorkspaceFolders []string `json:"workspaceFolders"`
	PID              int      `json:"pid"`
	IDEName          string   `json:"ideName"`
	Transport        string   `json:"transport"`
	RunningInWindows bool     `json:"runningInWindows"`
	AuthToken        string   `json:"authToken"`
}

// ConnectIDE discovers a live IDE lockfile whose workspace contains cwd and
// dials its websocket MCP endpoint. Returns nil,nil when no IDE is present.
func ConnectIDE(cwd, configDir string) (*Client, error) {
	ideDir := filepath.Join(configDir, "ide")
	ents, err := os.ReadDir(ideDir)
	if err != nil {
		return nil, nil
	}
	type cand struct {
		port int
		path string
		mt   time.Time
		lk   *ideLock
	}
	var cands []cand
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		p := filepath.Join(ideDir, e.Name())
		info, _ := e.Info()
		var port int
		fmt.Sscanf(strings.TrimSuffix(e.Name(), ".lock"), "%d", &port)
		if port == 0 {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var lk ideLock
		if json.Unmarshal(b, &lk) != nil {
			continue
		}
		// workspace match (or at least the lock is for our project tree)
		matched := false
		for _, wf := range lk.WorkspaceFolders {
			if wf == cwd || strings.HasPrefix(cwd, wf+string(filepath.Separator)) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		cands = append(cands, cand{port: port, path: p, mt: info.ModTime(), lk: &lk})
	}
	if len(cands) == 0 {
		return nil, nil
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mt.After(cands[j].mt) })
	for _, cn := range cands {
		if cn.lk.Transport != "ws" {
			continue
		}
		// probe the port first (real does a TCP connect check)
		addr := fmt.Sprintf("127.0.0.1:%d", cn.port)
		pc, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err != nil {
			continue // stale lock
		}
		pc.Close()
		name := "ide"
		if cn.lk.IDEName != "" {
			name = strings.ToLower(strings.Fields(cn.lk.IDEName)[0])
		}
		c, err := ConnectWS(name, addr, "/mcp", cn.lk.AuthToken)
		if err != nil {
			continue
		}
		return c, nil
	}
	return nil, nil
}
