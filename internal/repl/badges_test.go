package repl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenBG(t *testing.T) {
	// palette stores bg colors as fg escapes; the bg form must flip the
	// SGR class (38→48 for rgb/256, 3x→4x / 9x→10x for ansi)
	got := tokenBG("memoryBackgroundColor")
	if !strings.HasPrefix(got, "\x1b[4") && !strings.HasPrefix(got, "\x1b[10") {
		t.Fatalf("tokenBG(memoryBackgroundColor) = %q, not a bg SGR", got)
	}
}

func TestModeBadgeRender(t *testing.T) {
	got := modeBadgeRender("# buy milk")
	// '#' wrapped in remember fg + memoryBackgroundColor bg — under any
	// palette the '#' must be SGR-wrapped and the text untouched
	if !strings.HasPrefix(got, "\x1b[") || !strings.Contains(got, "#\x1b[0m") || !strings.HasSuffix(got, " buy milk") {
		t.Fatalf("hash badge wrong: %q", got)
	}
	got = modeBadgeRender("!ls")
	// activeTokens may be remapped by sibling tests — just require a
	// colored '!' followed by the untouched command text
	if !strings.HasPrefix(got, "\x1b[") || !strings.Contains(got, "!") || !strings.HasSuffix(got, "ls") {
		t.Fatalf("bang badge wrong: %q", got)
	}
	if got := modeBadgeRender("plain"); got != "plain" {
		t.Fatalf("plain buffer changed: %q", got)
	}
	if got := modeBadgeRender(""); got != "" {
		t.Fatalf("empty buffer changed: %q", got)
	}
}

func TestEchoRows(t *testing.T) {
	m := memoryInputEcho("buy milk")
	if !strings.Contains(m, " buy milk ") || !strings.Contains(m, "#") {
		t.Fatalf("memory echo wrong: %q", m)
	}
	ack := false
	for _, a := range memoryAcks {
		if strings.Contains(m, a) {
			ack = true
		}
	}
	if !ack {
		t.Fatalf("memory echo missing ack: %q", m)
	}
	b := bashInputEcho("ls -la")
	if !strings.Contains(b, "! ") || !strings.Contains(b, "ls -la") {
		t.Fatalf("bash echo wrong: %q", b)
	}
}

func TestOSC52(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TERM", "xterm-256color")
	if got := osc52Sequence("hi"); got != "\x1b]52;c;aGk=\x07" {
		t.Fatalf("bare osc52 wrong: %q", got)
	}
	t.Setenv("TMUX", "/tmp/tmux-1000/default,1,0")
	got := osc52Sequence("hi")
	if !strings.HasPrefix(got, "\x1bPtmux;") || !strings.HasSuffix(got, "\x1b\\") || !strings.Contains(got, "\x1b\x1b]52;") {
		t.Fatalf("tmux wrap wrong: %q", got)
	}
	t.Setenv("TMUX", "")
	t.Setenv("TERM", "screen-256color")
	if got := osc52Sequence("hi"); !strings.HasPrefix(got, "\x1bP\x1b]52;") || !strings.HasSuffix(got, "\x1b\\") {
		t.Fatalf("screen wrap wrong: %q", got)
	}
}

func TestImageMagic(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "a.png")
	if err := os.WriteFile(png, append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, make([]byte, 32)...), 0o644); err != nil {
		t.Fatal(err)
	}
	if !imageMagicOK(png) {
		t.Fatal("png magic not detected")
	}
	txt := filepath.Join(dir, "b.png")
	if err := os.WriteFile(txt, []byte("definitely not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if imageMagicOK(txt) {
		t.Fatal("text file sniffed as image")
	}
	if imageMagicOK(filepath.Join(dir, "missing.png")) {
		t.Fatal("missing file sniffed as image")
	}
}
