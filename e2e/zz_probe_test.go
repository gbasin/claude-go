//go:build e2e

package e2e

import (
	"testing"
	"time"
	"unicode/utf8"
)

func TestProbeUTF8Tool(t *testing.T) {
	s := newSession(t)
	p := s.runPTYSize(t, nil, 50, 24, "--mock")
	p.waitFor("❯", 30*time.Second)
	p.sendUntilEchoed(t, `tool:Bash {"command":"echo narrow-ok"}`+"\r", "❯ tool:Bash", 10)
	p.waitFor("─ permission", 30*time.Second)
	p.sendUntil("1", "Tool results received", 15)
	time.Sleep(500 * time.Millisecond)
	out := p.clean()
	for i := 0; i < len(out); {
		r, n := utf8.DecodeRuneInString(out[i:])
		if r == utf8.RuneError && n <= 1 {
			start := i - 40
			if start < 0 {
				start = 0
			}
			end := i + 40
			if end > len(out) {
				end = len(out)
			}
			t.Logf("bad at %d: ctx %q", i, out[start:end])
			i++
			continue
		}
		i += n
	}
	p.exit()
}
