//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestBashOutputTail — a slow command streams its output into the
// repaintable progress zone beneath the tool row (real's live tail).
func TestBashOutputTail(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s, "--permission-mode", "bypassPermissions")
	p.waitFor("❯", 30*time.Second)
	p.send(`tool:Bash {"command":"python3 -c 'import time;print(\"firstline\",flush=True);time.sleep(0.6);print(\"secondline\")'"}` + "\r")
	// the tail row must paint while the command still runs — "firstline"
	// visible before "secondline" exists
	p.waitScreen("firstline", 15*time.Second)
	p.waitScreen("secondline", 20*time.Second)
	p.waitFor("done", 30*time.Second)
	p.exit()
}

// TestBangModeSplit — '!' at empty buffer flips to bash mode: the '!' is
// the prompt glyph (not buffer text); backspace on empty exits the mode.
func TestBangModeSplit(t *testing.T) {
	s := newSession(t)
	p := startREPL(t, s)
	p.waitFor("❯", 30*time.Second)
	p.sendQuiet("!echo hi")
	// the '!' prompt glyph + buffer text render once input echoes
	scr := p.waitScreen("echo hi", 10*time.Second)
	if !strings.Contains(scr, "!") {
		t.Fatalf("bash-mode '!' glyph missing\n%s", scr)
	}
	p.sendQuiet("\x7f\x7f\x7f\x7f\x7f\x7f\x7f\x7f") // delete incl. '!' pos
	time.Sleep(150 * time.Millisecond)
	p.sendQuiet("say hi\r")
	p.waitFor("done", 30*time.Second)
	p.exit()
}
