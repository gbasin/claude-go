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
	b   byte   // literal byte (>=32 printable, or control)
	seq string // escape sequence like "[A"
	r   rune   // decoded printable rune (0 if none)
}

func startKeys() (*keySource, error) {
	st, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return nil, err
	}
	ks := &keySource{ch: make(chan key, 64), old: st}
	go ks.read()
	return ks, nil
}

func (ks *keySource) stop() {
	term.Restore(int(os.Stdin.Fd()), ks.old)
}

func (ks *keySource) read() {
	var buf [128]byte
	for {
		n, err := os.Stdin.Read(buf[:])
		if err != nil {
			close(ks.ch)
			return
		}
		for i := 0; i < n; i++ {
			c := buf[i]
			if c == 0x1b && i+2 < n && buf[i+1] == '[' {
				ks.ch <- key{b: 0x1b, seq: string(buf[i+1 : i+3])}
				i += 2
				continue
			}
			if c == 0x1b && i+3 < n && buf[i+1] == '[' && buf[i+2] == '3' && buf[i+3] == '~' {
				ks.ch <- key{b: 0x1b, seq: "[3~"}
				i += 3
				continue
			}
			if c >= 0x80 {
				// decode utf8 sequence
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
