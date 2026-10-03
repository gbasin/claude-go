package repl

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestToolCardEditDiff(t *testing.T) {
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	printToolCard("Edit", json.RawMessage(`{"file_path":"/x.go","old_string":"a","new_string":"b"}`), "")
	os.Stdout = old
	w.Close()
	buf := make([]byte, 1<<16)
	n, _ := r.Read(buf)
	out := string(buf[:n])
	if !strings.Contains(out, "file_path") || !strings.Contains(out, "\x1b[31m-\x1b") ||
		!strings.Contains(out, "\x1b[32m+\x1b") {
		t.Fatalf("edit card wrong: %q", out)
	}
}

func TestToolCardScalars(t *testing.T) {
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	printToolCard("Bash", json.RawMessage(`{"command":"ls","timeout":30}`), "")
	os.Stdout = old
	w.Close()
	buf := make([]byte, 1<<16)
	n, _ := r.Read(buf)
	out := string(buf[:n])
	if !strings.Contains(out, "command") || !strings.Contains(out, "ls") {
		t.Fatalf("scalar card wrong: %q", out)
	}
}
