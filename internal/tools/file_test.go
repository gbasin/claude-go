package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ctx(t *testing.T) *Context {
	return &Context{Ctx: context.Background(), CWD: t.TempDir()}
}

func TestReadWriteEdit(t *testing.T) {
	c := ctx(t)
	p := filepath.Join(c.CWD, "a.txt")

	// Write new file (no read needed)
	w, _ := json.Marshal(map[string]string{"file_path": p, "content": "hello\nworld\n"})
	if r := execWrite(c, w); r.IsError {
		t.Fatalf("write: %s", r.Content)
	}

	// Read it back, check cat -n format
	rd, _ := json.Marshal(map[string]string{"file_path": p})
	r := execRead(c, rd)
	if r.IsError {
		t.Fatalf("read: %s", r.Content)
	}
	if !strings.Contains(r.Content, "     1\thello") {
		t.Errorf("expected line numbers, got:\n%s", r.Content)
	}

	// Edit requires prior read — should pass now
	e, _ := json.Marshal(map[string]any{
		"file_path": p, "old_string": "world", "new_string": "go",
	})
	if r := execEdit(c, e); r.IsError {
		t.Fatalf("edit: %s", r.Content)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "hello\ngo\n" {
		t.Errorf("content = %q", b)
	}

	// Edit without prior read fails
	p2 := filepath.Join(c.CWD, "b.txt")
	os.WriteFile(p2, []byte("x"), 0o644)
	e2, _ := json.Marshal(map[string]any{"file_path": p2, "old_string": "x", "new_string": "y"})
	if r := execEdit(c, e2); !r.IsError {
		t.Error("expected edit without read to fail")
	}
}

func TestEditUniqueness(t *testing.T) {
	c := ctx(t)
	p := filepath.Join(c.CWD, "dup.txt")
	os.WriteFile(p, []byte("aaa\naaa\n"), 0o644)
	rd, _ := json.Marshal(map[string]string{"file_path": p})
	execRead(c, rd)
	e, _ := json.Marshal(map[string]any{"file_path": p, "old_string": "aaa", "new_string": "b"})
	if r := execEdit(c, e); !r.IsError || !strings.Contains(r.Content, "2 times") {
		t.Errorf("expected uniqueness error, got: %s", r.Content)
	}
	e2, _ := json.Marshal(map[string]any{"file_path": p, "old_string": "aaa", "new_string": "b", "replace_all": true})
	if r := execEdit(c, e2); r.IsError {
		t.Fatalf("replace_all: %s", r.Content)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "b\nb\n" {
		t.Errorf("content = %q", b)
	}
}

func TestReadOffsetLimit(t *testing.T) {
	c := ctx(t)
	p := filepath.Join(c.CWD, "big.txt")
	var lines []string
	for i := 1; i <= 50; i++ {
		lines = append(lines, "line")
	}
	os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o644)
	rd, _ := json.Marshal(map[string]any{"file_path": p, "offset": 10, "limit": 5})
	r := execRead(c, rd)
	got := strings.Count(r.Content, "\n") + 1
	if got != 5 {
		t.Errorf("expected 5 lines, got %d:\n%s", got, r.Content)
	}
	if !strings.Contains(r.Content, "    11\t") {
		t.Errorf("expected to start at line 11:\n%s", r.Content)
	}
}
