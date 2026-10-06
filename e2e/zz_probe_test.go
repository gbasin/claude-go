//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

func TestProbeKeysMidTurn(t *testing.T) {
	s := newSession(t)
	p := s.runPTY(t, []string{"CG_DBGKEY=1"}, "--mock", "--permission-mode", "bypassPermissions")
	p.waitFor("❯", 30*time.Second)
	p.send(`tool:Bash {"command":"python3 -c \"import time;time.sleep(5)\""}` + "\r")
	time.Sleep(1500 * time.Second / 1000)
	p.send("x\r\x1b")
	time.Sleep(2 * time.Second)
	out := p.clean()
	t.Logf("dbgkey=%v queued=%v", strings.Contains(out, "dbgkey"), strings.Contains(out, "queued"))
	t.Logf("tail: %s", out[max(0, len(out)-700):])
	p.exit()
}
