package repl

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// capture stdout while rendering
func renderToString(t *testing.T, md string) string {
	t.Helper()
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	rd := newMDRenderer()
	rd.Write(md)
	rd.Close()
	os.Stdout = old
	w.Close()
	b := make([]byte, 65536)
	n, _ := r.Read(b)
	out := string(b[:n])
	var rest strings.Builder
	for {
		n, err := r.Read(b)
		rest.Write(b[:n])
		if err != nil {
			break
		}
	}
	return out + rest.String()
}

func TestTableRenders(t *testing.T) {
	out := renderToString(t, "| Name | Age |\n|---|---|\n| a | 3 |\n| bb | 10 |\n")
	if !strings.Contains(out, "┌") || !strings.Contains(out, "┐") ||
		!strings.Contains(out, "├") || !strings.Contains(out, "└") {
		t.Fatalf("table didn't box-render: %q", out)
	}
	if !strings.Contains(out, "Name") || !strings.Contains(out, "bb") {
		t.Fatalf("cells missing: %q", out)
	}
}

func TestNonTablePipeLine(t *testing.T) {
	out := renderToString(t, "| not a table\nnext line\n")
	if strings.Contains(out, "┌") {
		t.Fatalf("non-table got boxed: %q", out)
	}
}

func TestFenceHighlight(t *testing.T) {
	out := renderToString(t, "```go\nfunc main() { return 1 } // hi\n```\n")
	if !strings.Contains(out, "\x1b[38;2;249;38;114mfunc") { // Monokai keyword pink
		t.Fatalf("keyword not highlighted: %q", out)
	}
	if !strings.Contains(out, "\x1b[38;2;117;113;94m// hi") {
		t.Fatalf("comment not dimmed: %q", out)
	}
}

func TestOSC8Link(t *testing.T) {
	out := renderToString(t, "see [docs](https://ex.com/x) ok")
	if !strings.Contains(out, "\x1b]8;;https://ex.com/x\x07") ||
		!strings.Contains(out, "docs") {
		t.Fatalf("OSC8 link missing: %q", out)
	}
}

func TestStreamedTable(t *testing.T) {
	// chunked delivery — table must still render
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	rd := newMDRenderer()
	for _, chunk := range []string{"| A", " |", "\n|---|", "\n| x |", "\ndone\n"} {
		rd.Write(chunk)
	}
	rd.Close()
	os.Stdout = old
	w.Close()
	b, _ := exec.Command("cat").Output()
	_ = b
	buf := make([]byte, 65536)
	n, _ := r.Read(buf)
	out := string(buf[:n])
	if !strings.Contains(out, "┌") {
		t.Fatalf("chunked table lost: %q", out)
	}
}
