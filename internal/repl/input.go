package repl

import (
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// keySource owns stdin for the life of the REPL and emits decoded keys.
type keySource struct {
	ch    chan key
	old   *term.State
	kitty bool // kitty keyboard protocol was enabled
}

// modifier bits carried on a key (kitty/CSI-u encoding: param-1).
const (
	modShift = 1
	modAlt   = 2
	modCtrl  = 4
	modCmd   = 8 // super/cmd
)

type key struct {
	b     byte   // literal byte (>=32 printable, or control)
	seq   string // escape sequence like "[A"
	r     rune   // decoded printable rune (0 if none)
	paste string // bracketed-paste content
	mod   uint8  // modifier mask (shift=1 alt=2 ctrl=4 cmd=8) from CSI-u/CSI mods
}

func startKeys() (*keySource, error) {
	st, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return nil, err
	}
	ks := &keySource{ch: make(chan key, 256), old: st}
	os.Stdout.WriteString("\x1b[?2004h") // bracketed paste on
	if kittyCapable() {
		// kitty keyboard protocol: push flags (1 = disambiguate escape codes).
		// Enables CSI-u reporting for ctrl+/alt+/shift+key combos.
		ks.kitty = true
		os.Stdout.WriteString("\x1b[>1u")
	}
	go ks.read()
	return ks, nil
}

func kittyCapable() bool {
	t := strings.ToLower(os.Getenv("TERM_PROGRAM") + " " + os.Getenv("TERM"))
	for _, s := range []string{"kitty", "ghostty", "wezterm", "iterm"} {
		if strings.Contains(t, s) {
			return true
		}
	}
	return false
}

func (ks *keySource) stop() {
	if ks.kitty {
		os.Stdout.WriteString("\x1b[<u") // pop kitty flags
	}
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
				// CSI: scan parameter bytes (0x30-0x3f) then a final byte (0x40-0x7e)
				j := i + 2
				for j < n && buf[j] >= 0x30 && buf[j] <= 0x3f {
					j++
				}
				if j < n && buf[j] >= 0x40 && buf[j] <= 0x7e {
					ks.ch <- decodeCSI(string(buf[i+1 : j+1]))
					i = j
					continue
				}
				ks.ch <- key{b: 0x1b, seq: string(buf[i+1 : i+3])}
				i += 2
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

// decodeCSI turns a CSI sequence (e.g. "[A", "[3~", "[1;5A", "[97;5u")
// into a key. s includes the leading '[' and the final byte.
func decodeCSI(s string) key {
	final := s[len(s)-1]
	params := s[1 : len(s)-1]
	nums := strings.Split(params, ";")
	pnum := func(idx int) int {
		if idx >= len(nums) {
			return 0
		}
		v := 0
		for _, ch := range nums[idx] {
			if ch < '0' || ch > '9' {
				break // ':' sub-parameters: keep the first number
			}
			v = v*10 + int(ch-'0')
		}
		return v
	}
	// modifier param is 1 + bitmask (shift=1 alt=2 ctrl=4 super=8)
	mod := pnum(1)
	if mod > 0 {
		mod--
	}
	if final == 'u' { // CSI-u / kitty key event: <code>;<mods>u
		k := key{mod: uint8(mod)}
		code := pnum(0)
		switch {
		case code == 13 && mod&modAlt != 0:
			k.b, k.r = '\n', '\n' // alt+enter -> newline (matches option+enter)
		case code == 13:
			k.b = '\r'
		case code == 10:
			k.b = '\n'
		case code == 9:
			k.b = '\t'
		case code == 8 || code == 127:
			k.b = 0x7f
		case code < 32:
			k.b = byte(code)
		case code >= 32 && code < 0xE000: // skip PUA functional key codes
			k.r = rune(code)
			if code < 128 {
				k.b = byte(code)
			}
		}
		return k
	}
	k := key{b: 0x1b, mod: uint8(mod)}
	if final == '~' {
		if n := pnum(0); n > 0 {
			k.seq = "[" + strconv.Itoa(n) + "~" // e.g. "[3~" (also "[3;5~")
		} else {
			k.seq = s
		}
		return k
	}
	// letter final: arrows/home/end/shift-tab ("[A", "[Z", "[1;5A" -> "[A"+mod)
	k.seq = "[" + string(final)
	return k
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
