package extract

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDirEnvOverride(t *testing.T) {
	d := t.TempDir()
	t.Setenv("CLAUDE_GO_EXTRACTED_DIR", d)
	if Dir() != d {
		t.Fatalf("Dir() = %q, want %q", Dir(), d)
	}
}

func TestAvailableAndMeta(t *testing.T) {
	d := t.TempDir()
	t.Setenv("CLAUDE_GO_EXTRACTED_DIR", d)
	if Available() {
		t.Fatal("Available before meta.json exists")
	}
	meta := `{"claude_version":"2.1.296","harvested_at":"2026-10-10T00:00:00Z","models":["fable"]}`
	os.WriteFile(filepath.Join(d, "meta.json"), []byte(meta), 0o644)
	if !Available() {
		t.Fatal("Available false after meta.json")
	}
	m, err := ReadMeta()
	if err != nil || m.ClaudeVersion != "2.1.296" || m.Models[0] != "fable" {
		t.Fatalf("meta: %v %+v", err, m)
	}
}

func TestTextAndJSON(t *testing.T) {
	d := t.TempDir()
	t.Setenv("CLAUDE_GO_EXTRACTED_DIR", d)
	if _, ok := Text("missing.txt"); ok {
		t.Fatal("missing file returned ok")
	}
	if _, ok := JSON("missing.json"); ok {
		t.Fatal("missing json returned ok")
	}
	os.WriteFile(filepath.Join(d, "p.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(d, "t.json"), []byte(`[{"name":"X"}]`), 0o644)
	if s, ok := Text("p.txt"); !ok || s != "hello" {
		t.Fatalf("Text: %q %v", s, ok)
	}
	if b, ok := JSON("t.json"); !ok || string(b) != `[{"name":"X"}]` {
		t.Fatalf("JSON: %s %v", b, ok)
	}
}

func TestRenderRoundTrip(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	home, _ = filepath.EvalSymlinks(home) // match capture()'s resolution
	cwd, _ = filepath.EvalSymlinks(cwd)
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("USER", "testuser")

	slug := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, filepath.Clean(cwd))
	memDir := filepath.Join(home, ".claude", "projects", slug, "memory")

	in := "work " + cwd + " home " + home + " mem " + memDir + "/ user testuser date " +
		time.Now().Format("2006-01-02")
	tok := tokenize(in, cwd, home)
	for _, want := range []string{"{{CWD}}", "{{HOME}}", "{{MEMDIR}}", "{{USER}}", "{{DATE}}"} {
		if !strings.Contains(tok, want) {
			t.Fatalf("tokenize missing %s: %s", want, tok)
		}
	}
	if strings.Contains(tok, cwd) || strings.Contains(tok, home) {
		t.Fatalf("tokenize left raw paths: %s", tok)
	}
	back := Render(tok, cwd)
	if back != in {
		t.Fatalf("round trip:\n got: %s\nwant: %s", back, in)
	}
}

func TestMainSystemText(t *testing.T) {
	mk := func(s string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"type": "text", "text": s})
		return b
	}
	sys := []json.RawMessage{mk("billing"), mk("identity"), mk("main body")}
	if got := mainSystemText(sys); got != "main body" {
		t.Fatalf("mainSystemText = %q", got)
	}
	if got := mainSystemText(nil); got != "" {
		t.Fatalf("empty = %q", got)
	}
	// non-text / malformed blocks skipped
	sys = []json.RawMessage{mk("a"), json.RawMessage(`{"type":"image"}`), mk("b")}
	if got := mainSystemText(sys); got != "b" {
		t.Fatalf("last-text = %q", got)
	}
}

func TestWriteJSONLines(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "tools.json")
	items := []json.RawMessage{
		json.RawMessage(`{"name":"A"}`), json.RawMessage(`{"name":"B"}`)}
	if err := writeJSONLines(p, items); err != nil {
		t.Fatal(err)
	}
	var got []map[string]string
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &got); err != nil || len(got) != 2 || got[0]["name"] != "A" {
		t.Fatalf("round trip: %v %v", err, got)
	}
}

func TestHarvestNoBin(t *testing.T) {
	if _, err := Harvest(context.Background(), "", nil); err == nil {
		t.Fatal("expected error for empty binary path")
	}
}

func TestWriteWireCaptureShape(t *testing.T) {
	d := t.TempDir()
	raw := []byte(`{"model":"claude-fable-5-1","max_tokens":128000,"stream":true}`)
	hdrs := make(map[string][]string)
	hdrs["User-Agent"] = []string{"claude-cli/2.1.296"}
	writeWireCapture(d, "print_fable", raw, hdrs)
	b, err := os.ReadFile(filepath.Join(d, "realwire_print_fable.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wc struct {
		RequestLine string            `json:"request_line"`
		Headers     map[string]string `json:"headers"`
		Body        struct {
			Model string `json:"model"`
		} `json:"body"`
	}
	if err := json.Unmarshal(b, &wc); err != nil {
		t.Fatalf("capture not fixture-shaped: %v", err)
	}
	if wc.RequestLine == "" || wc.Headers["user-agent"] == "" || wc.Body.Model != "claude-fable-5-1" {
		t.Fatalf("capture fields: %+v", wc)
	}
}
