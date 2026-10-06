package bg

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"time"

	"golang.org/x/term"
)

// Attach connects the local terminal to a running bg session — real's
// `claude attach <id>`: raw passthrough both ways, resize forwarding, and
// Ctrl+Z detaches (inside an attach, ^Z means "leave", matching real's
// "Ctrl+Z to detach").
func Attach(prefix string, stdin, stdout *os.File) error {
	// a fresh spawn can lag the registry write — real's attach retries the
	// rendezvous too
	var j *Job
	var err error
	for i := 0; i < 40; i++ {
		j, err = Lookup(prefix)
		if errors.Is(err, errAmbiguous) {
			return fmt.Errorf("session %q is ambiguous — use more of the id", prefix)
		}
		if j != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if j == nil {
		return fmt.Errorf("no running background session %q — `claude agents` lists live ones", prefix)
	}
	// the registry write lands before the rendezvous socket binds — retry
	// the dial too, same window as the lookup
	var conn net.Conn
	for i := 0; i < 40; i++ {
		conn, err = net.Dial("unix", j.SocketPath())
		if err == nil {
			break
		}
		if !j.Alive() {
			return fmt.Errorf("session %s is not running anymore", j.Short)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("attach %s: %w", j.Short, err)
	}
	defer conn.Close()

	cols, rows, _ := term.GetSize(int(stdout.Fd()))
	if cols == 0 {
		cols, rows = 120, 40
	}
	hello, _ := json.Marshal(map[string]int{"cols": cols, "rows": rows})
	if err := writeFrame(conn, opHello, hello); err != nil {
		return err
	}
	op, _, err := readFrame(conn)
	if err != nil {
		return fmt.Errorf("attach %s: %w", j.Short, err)
	}
	if op != opReady {
		return fmt.Errorf("attach %s: unexpected handshake op %d", j.Short, op)
	}

	isTTY := term.IsTerminal(int(stdin.Fd()))
	var old *term.State
	if isTTY {
		if st, err := term.MakeRaw(int(stdin.Fd())); err == nil {
			old = st
		}
	}
	restore := func() {
		if old != nil {
			_ = term.Restore(int(stdin.Fd()), old)
		}
	}
	defer restore()

	// SIGWINCH → resize frame
	winch := make(chan os.Signal, 2)
	signal.Notify(winch, sigWinch)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			c, r, _ := term.GetSize(int(stdout.Fd()))
			if c > 0 {
				b, _ := json.Marshal(map[string]int{"cols": c, "rows": r})
				_ = writeFrame(conn, opResize, b)
			}
		}
	}()

	// socket → stdout
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			op, payload, err := readFrame(conn)
			if err != nil {
				return
			}
			switch op {
			case opData:
				stdout.Write(payload)
			case opExit:
				return
			}
		}
	}()

	// stdin → socket; \x1a (ctrl+z) detaches — real's "Ctrl+Z to detach"
	buf := make([]byte, 32*1024)
	detached := false
	for {
		n, rerr := stdin.Read(buf)
		cut := n
		for i, b := range buf[:n] {
			if b == 0x1a { // ctrl+z detaches — bytes before it still go through
				cut, detached = i, true
				break
			}
		}
		if cut > 0 {
			if werr := writeFrame(conn, opData, buf[:cut]); werr != nil {
				break
			}
		}
		if detached || rerr != nil {
			break
		}
	}
	if detached {
		_ = writeFrame(conn, opDetach, nil)
	} else {
		<-done // host ended the session — wait for the opExit flush
	}
	restore()
	if detached {
		fmt.Printf("\r\ndetached — session %s keeps running; `claude attach %s` reattaches, `claude agents` lists\r\n",
			j.Short, j.Short)
	} else {
		fmt.Printf("\r\nsession %s ended\r\n", j.Short)
	}
	return nil
}
