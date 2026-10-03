// Package telemetry honors Claude Code's telemetry configuration surface
// without ever sending data anywhere. When CLAUDE_CODE_ENABLE_TELEMETRY=1,
// tengu_* events are appended to a local JSONL file for inspection — matching
// the event names real CC emits — instead of OTLP. This preserves config
// compatibility while keeping the replica network-quiet by design.
package telemetry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// real gate: telemetry requires the enable flag (any set value) AND
// OTEL_LOGS_EXPORTER != "none"; the traffic-mode envs can force it off.
var enabled = func() bool {
	if os.Getenv("DISABLE_TELEMETRY") != "" || truthy(os.Getenv("DO_NOT_TRACK")) ||
		os.Getenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC") != "" {
		return false
	}
	if os.Getenv("CLAUDE_CODE_ENABLE_TELEMETRY") == "" {
		return false
	}
	if os.Getenv("OTEL_LOGS_EXPORTER") == "none" ||
		os.Getenv("ANT_OTEL_LOGS_EXPORTER") == "none" {
		return false
	}
	return true
}()

func truthy(v string) bool {
	return v != "" && v != "0" && v != "false" && v != "no"
}

func path() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude-go", "telemetry.jsonl")
}

// Event appends a tengu_* event record locally (never sent).
func Event(name string, attrs map[string]any) {
	if !enabled {
		return
	}
	rec := map[string]any{"event": name, "ts": time.Now().UTC().Format(time.RFC3339Nano)}
	for k, v := range attrs {
		rec[k] = v
	}
	b, _ := json.Marshal(rec)
	os.MkdirAll(filepath.Dir(path()), 0o755)
	f, err := os.OpenFile(path(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, string(b))
}
