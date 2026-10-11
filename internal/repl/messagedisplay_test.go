package repl

import (
	"strings"
	"testing"
	"time"

	"github.com/gbasin/claude-go/internal/hooks"
)

func TestMsgDisplayBatchesLines(t *testing.T) {
	var fired []map[string]any
	var rendered strings.Builder
	m := newMsgDisplay(
		func(p map[string]any) hooks.Result {
			fired = append(fired, p)
			return hooks.Result{}
		},
		func(s string) { rendered.WriteString(s) },
	)
	m.lastAt = time.Time{} // skip throttle for the test
	m.Delta("line one\nline two\npartial")
	if len(fired) != 1 || fired[0]["delta"] != "line one\nline two\n" {
		t.Fatalf("batch: %v", fired)
	}
	m.Delta(" done")
	m.Finalize()
	if len(fired) != 2 || fired[1]["delta"] != "partial done" || fired[1]["final"] != true {
		t.Fatalf("final batch: %v", fired)
	}
	if rendered.String() != "line one\nline two\npartial done" {
		t.Fatalf("rendered: %q", rendered.String())
	}
	if fired[0]["index"] != 0 || fired[1]["index"] != 1 {
		t.Fatalf("indices: %v", fired)
	}
	if fired[0]["turn_id"] == "" || fired[0]["message_id"] == "" {
		t.Fatal("ids missing")
	}
}

func TestMsgDisplayContentReplacesRenderOnly(t *testing.T) {
	var rendered strings.Builder
	m := newMsgDisplay(
		func(p map[string]any) hooks.Result {
			return hooks.Result{DisplayContent: "SHOWN " + p["delta"].(string)}
		},
		func(s string) { rendered.WriteString(s) },
	)
	m.lastAt = time.Time{}
	m.Delta("secret\n")
	if rendered.String() != "SHOWN secret\n" {
		t.Fatalf("display rewrite: %q", rendered.String())
	}
}
