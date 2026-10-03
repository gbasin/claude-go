package repl

import (
	"os"

	"golang.org/x/term"
)

// keySource owns stdin for the life of the REPL and emits decoded keys.
type keySource struct {
	ch  chan key
	old *term.State
}

type key struct {
	b     byte   // literal byte (>=32 printable, or control)
	seq   string // escape sequence like "[A"
	r     rune   // decoded printable rune (0 if none)
	paste string // bracketed-paste content
}

func startKeys() (*keySource, error) {
	st, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return nil, err
	}
	ks := &keySource{ch: make(chan key, 256), old: st}
	os.Stdout.WriteString("\x1b[?2004h") // bracketed paste on
	go ks.read()
	return ks, nil
}

func (ks *keySource) stop() {
	os.Stdout.WriteString("\x1b[?2004l")
	term.Restore(int(os.Stdin.Fd()), ks.old)
}

const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

func (ks *keySource) read() {
	var buf [512]byte
	var pasting []byte
	for {
		n, err := os.Stdin.Read(buf[:])
		if err != nil {
			close(ks.ch)
			return
		}
		for i := 0; i < n; i++ {
			c := buf[i]
			if pasting != nil {
				// look for paste end marker
				if c == 0x1b && i+5 < n && string(buf[i:i+6]) == pasteEnd {
					ks.ch <- key{paste: string(pasting)}
					pasting = nil
					i += 5
					continue
				}
				pasting = append(pasting, c)
				continue
			}
			if c == 0x1b && i+5 < n && string(buf[i:i+6]) == pasteStart {
				pasting = []byte{}
				i += 5
				continue
			}
			if c == 0x1b && i+1 < n && buf[i+1] == '\r' {
				ks.ch <- key{b: '\n', r: '\n'} // option+enter -> newline
				i++
				continue
			}
			if c == 0x1b && i+2 < n && buf[i+1] == '[' {
				ks.ch <- key{b: 0x1b, seq: string(buf[i+1 : i+3])}
				i += 2
				continue
			}
			if c == 0x1b && i+3 < n && buf[i+1] == '[' && buf[i+3] == '~' {
				ks.ch <- key{b: 0x1b, seq: string(buf[i+1 : i+4])}
				i += 3
				continue
			}
			if c >= 0x80 {
				size := utf8Len(c)
				if i+size <= n {
					r := []rune(string(buf[i : i+size]))
					if len(r) == 1 {
						ks.ch <- key{b: c, r: r[0]}
					}
					i += size - 1
					continue
				}
			}
			ks.ch <- key{b: c}
		}
	}
}

func utf8Len(b byte) int {
	switch {
	case b&0xF0 == 0xF0:
		return 4
	case b&0xE0 == 0xE0:
		return 3
	case b&0xC0 == 0xC0:
		return 2
	}
	return 1
}
