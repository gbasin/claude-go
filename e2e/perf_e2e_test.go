// Perf regression guards — the replica's whole point is lighter/faster.
// Generous thresholds (loaded-machine safe); failures mean real
// regressions, not jitter.
package e2e

import (
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// TestPrintModeLatency: --mock -p must finish well under the real
// binary's turn time — real claude -p typically takes 3-8s before the
// first byte; we should be far under that end-to-end.
func TestPrintModeLatency(t *testing.T) {
	s := newSession(t)
	start := time.Now()
	r := s.run(t, "--mock", "-p", "say hi")
	el := time.Since(start)
	if r.code != 0 {
		t.Fatalf("print mode failed: %v", r.stderr)
	}
	if el > 10*time.Second {
		t.Fatalf("-p took %v — should be far under real's turn latency", el)
	}
	t.Logf("-p latency: %v", el)
}

// TestIdleRSS: idle REPL memory floor — real claude idles ~360MB; we
// must stay under 150MB even on loaded machines (measured ~29MB).
func TestIdleRSS(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("rss check needs ps")
	}
	s := newSession(t)
	p := startREPL(t, s, "--permission-mode", "bypassPermissions")
	defer p.exit()
	time.Sleep(500 * time.Millisecond)
	out, err := exec.Command("ps", "-o", "rss=", "-p",
		itoa(p.cmd.Process.Pid)).Output()
	if err != nil {
		t.Skipf("ps: %v", err)
	}
	var kb int
	for _, ch := range string(out) {
		if ch >= '0' && ch <= '9' {
			kb = kb*10 + int(ch-'0')
		}
	}
	if mb := kb / 1024; mb > 150 {
		t.Fatalf("idle RSS %dMB — replica should be far under real's ~360MB", mb)
	} else {
		t.Logf("idle RSS: %dMB", mb)
	}
}

// TestBinarySize: the whole point of Go was a small binary — guard
// against dependency bloat (real is ~225MB bundled).
func TestBinarySize(t *testing.T) {
	if binPath == "" {
		t.Skip("no binary")
	}
	fi, err := os.Stat(binPath)
	if err != nil {
		t.Skip(err)
	}
	if fi.Size() > 60<<20 {
		t.Fatalf("binary is %dMB — should stay far under real's ~225MB", fi.Size()>>20)
	}
	t.Logf("binary size: %dMB", fi.Size()>>20)
}

// TestStartupCPU: user+sys time for --version — catches init bloat.
func TestStartupCPU(t *testing.T) {
	if binPath == "" {
		t.Skip("no binary")
	}
	cmd := exec.Command(binPath, "--version")
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Skip(err)
	}
	st, _ := cmd.ProcessState.SysUsage().(*syscall.Rusage)
	cpu := time.Duration(st.Utime.Usec+st.Stime.Usec) * time.Microsecond
	if cpu > 2*time.Second {
		t.Fatalf("--version used %v CPU (elapsed %v)", cpu, time.Since(start))
	}
	t.Logf("--version: %v cpu, %v elapsed", cpu, time.Since(start))
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
