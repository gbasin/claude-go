//go:build e2e

// pty_test.go — PTY harness for interactive (raw-mode) REPL tests.
//
// The REPL requires a real terminal: term.IsTerminal(stdin) gate plus
// term.MakeRaw. github.com/creack/pty is not in the module cache, so this
// implements openpty directly: /dev/ptmx + TIOCPTYGNAME ioctl (Darwin
// auto-grants/unlocks the slave on master open). Tests t.Skip on platforms
// where the pty can't be allocated.
package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openPTY allocates a master/slave pair (120x40).
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skipf("pty harness implemented for darwin only (got %s)", runtime.GOOS)
	}
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("open /dev/ptmx: %v", err)
	}
	var buf [128]byte
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, master.Fd(),
		uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		master.Close()
		t.Skipf("ioctl TIOCPTYGNAME: %v", errno)
	}
	name := string(buf[:bytes.IndexByte(buf[:], 0)])
	slave, err = os.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		master.Close()
		t.Skipf("open pty slave %s: %v", name, err)
	}
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ,
		&unix.Winsize{Row: 40, Col: 120}); err != nil {
		t.Logf("set winsize: %v (continuing)", err)
	}
	return master, slave
}

// ptyProc is a claude-go process running with stdin/stdout/stderr on a pty.
type ptyProc struct {
	t      *testing.T
	cmd    *exec.Cmd
	master *os.File
	done   chan error

	mu  sync.Mutex
	buf bytes.Buffer
}

// runPTY spawns the binary attached to a fresh pty.
func (s *sess) runPTY(t *testing.T, extraEnv []string, args ...string) *ptyProc {
	t.Helper()
	master, slave := openPTY(t)
	cmd := exec.Command(binPath, args...)
	cmd.Dir = s.cwd
	cmd.Env = append(append([]string{}, s.env...), extraEnv...)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	if err := cmd.Start(); err != nil {
		master.Close()
		slave.Close()
		t.Fatalf("start on pty: %v", err)
	}
	slave.Close() // child owns it now
	p := &ptyProc{t: t, cmd: cmd, master: master, done: make(chan error, 1)}
	go func() {
		var tmp [4096]byte
		for {
			n, err := master.Read(tmp[:])
			if n > 0 {
				p.mu.Lock()
				p.buf.Write(tmp[:n])
				p.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	go func() { p.done <- cmd.Wait() }()
	t.Cleanup(p.close)
	return p
}

func (p *ptyProc) close() {
	p.master.Close()
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		p.cmd.Process.Kill()
		<-p.done
	}
}

// send writes keystrokes to the pty (e.g. "hi\r").
func (p *ptyProc) send(s string) {
	p.t.Helper()
	if _, err := p.master.WriteString(s); err != nil {
		p.t.Fatalf("pty write %q: %v", s, err)
	}
}

// output returns everything captured so far (raw, ANSI escapes included).
func (p *ptyProc) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.String()
}

// stripANSI removes CSI/OSC escape sequences for substring matching.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c == 0x1b {
			if i+1 < len(s) && s[i+1] == '[' {
				// CSI: skip until final byte 0x40-0x7e
				j := i + 2
				for j < len(s) && !(s[j] >= 0x40 && s[j] <= 0x7e) {
					j++
				}
				if j < len(s) {
					i = j + 1
					continue
				}
				i = j
				continue
			}
			if i+1 < len(s) && s[i+1] == ']' {
				// OSC: skip until BEL or ST
				j := i + 2
				for j < len(s) && s[j] != 0x07 && !(s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\') {
					j++
				}
				if j < len(s) && s[j] == 0x1b {
					j += 2
				} else if j < len(s) {
					j++
				}
				i = j
				continue
			}
			if i+2 < len(s) && s[i+1] == '>' { // kitty protocol \x1b[>1u handled above; \x1b>... rare
				i += 2
				continue
			}
			i++ // lone escape
			continue
		}
		if c == '\r' || c == 0x07 {
			i++
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

// clean returns the captured output with ANSI escapes stripped.
func (p *ptyProc) clean() string {
	return stripANSI(p.output())
}

// waitFor polls until substr appears in the cleaned output or timeout hits.
func (p *ptyProc) waitFor(substr string, timeout time.Duration) string {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(p.clean(), substr) {
			return p.clean()
		}
		select {
		case err := <-p.done:
			p.done <- err
			p.t.Fatalf("process exited while waiting for %q: %v\noutput:\n%s",
				substr, err, p.clean())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.t.Fatalf("timed out (%s) waiting for %q\noutput:\n%s",
		timeout, substr, truncStr(p.clean(), 4000))
	return ""
}

// waitCount waits for the n-th occurrence of substr (prompts repeat).
func (p *ptyProc) waitCount(substr string, n int, timeout time.Duration) {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Count(p.clean(), substr) >= n {
			return
		}
		select {
		case err := <-p.done:
			p.done <- err
			p.t.Fatalf("process exited while waiting for %d x %q: %v\noutput:\n%s",
				n, substr, err, p.clean())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.t.Fatalf("timed out waiting for %d x %q\noutput:\n%s", n, substr, truncStr(p.clean(), 4000))
}

// waitExit waits for the child to exit, returning the error (nil = clean).
func (p *ptyProc) waitExit(timeout time.Duration) error {
	p.t.Helper()
	select {
	case err := <-p.done:
		p.done <- err
		return err
	case <-time.After(timeout):
		p.t.Fatalf("process did not exit within %s\noutput:\n%s", timeout, truncStr(p.clean(), 4000))
		return nil
	}
}

// utf8OK reports the buffer is valid utf8 (box-drawing sanity check).
func utf8OK(s string) bool { return utf8.ValidString(s) }
