package repl

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/term"
)

// keySource owns stdin for the life of the REPL and emits decoded keys.
type keySource struct {
	replay chan key // keys drained post-turn, replayed to the editor first
	ch     chan key
	old    *term.State
	kitty  bool // kitty keyboard protocol was enabled
	mok    bool // xterm modifyOtherKeys pushed (non-kitty path)
	// probe answers settle here — the async replies land on stdin between
	// keystrokes and get folded into terminalCaps (real's mod_0501 record)
	caps terminalCaps
	// dialog, when non-nil, gets every key forwarded — the permission dialog
	// owns stdin while open so the turn's busy-queue goroutine can't eat
	// the user's answer.
	dialog atomic.Pointer[chan key]
}

// probeCaps is the session-level copy of the last keySource's probe answers
// — redraw paths that don't hold the keySource read it (real's mod_0501
// record is likewise a host-level singleton).
var probeCaps struct {
	mu   sync.Mutex
	caps terminalCaps
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
	ks := &keySource{ch: make(chan key, 256), old: st, replay: make(chan key, 256)}
	os.Stdout.WriteString("\x1b[?2004h") // bracketed paste on
	// capability probe batch (real's DECRPM/XTVersion queries —
	// mod_0501 records extendedKeys/synchronizedOutput/kittyKeyboard/
	// kittyGraphics/mousePixels/programStatus from the replies)
	if os.Getenv("CLAUDE_GO_NO_TERMPROBE") == "" {
		os.Stdout.WriteString("\x1b[?2026$p") // synchronized output DECRPM
		os.Stdout.WriteString("\x1b[?u")      // kitty keyboard flags query
		os.Stdout.WriteString("\x1b[?1016$p") // SGR pixel-mouse DECRPM
		os.Stdout.WriteString("\x1b[c")       // primary DA (programStatus)
	}
	if kittyCapable() {
		// kitty keyboard protocol: push flags 5 = disambiguate (1) +
		// report alternate keys (4) — real's `>5u`. Without bit 4 a
		// shift+enter arrives as plain CR (or gets dropped), which ate
		// the keypress entirely.
		ks.kitty = true
		os.Stdout.WriteString("\x1b[>5u")
	} else {
		// xterm modifyOtherKeys level 2 — the non-kitty path real uses
		// (`>4;2m`) so ctrl+enter/shift+enter reach us as CSI 27;m;k~.
		ks.mok = true
		os.Stdout.WriteString("\x1b[>4;2m")
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
	ks.release()
}

// terminalCaps holds the DECRPM/DA probe answers (real's mod_0501 record).
type terminalCaps struct {
	syncOutput    bool // ?2026 DECRPM — bsu/esu batched repaint support
	kittyFlags    int  // ?u reply — kitty keyboard flags actually active
	sgrMousePixel bool // ?1016 DECRPM — pixel-position mouse reporting
	daAnswered    bool // CSI ?...c answered — terminal is a live terminal
}

// recordProbeReply recognizes capability-probe replies on the input stream
// (DECRPM \x1b[?N;M$y, kitty-flags \x1b[?Nu, DA \x1b[?…c). Returns true
// when the sequence was consumed as a probe answer (not a key).
func (ks *keySource) recordProbeReply(seq string) bool {
	if len(seq) < 3 || seq[0] != '[' || seq[1] != '?' {
		return false
	}
	body := seq[2:]
	switch {
	case strings.HasSuffix(body, "$y"): // DECRPM \x1b[?Ps;Pv$y
		var ps, pv int
		fmt.Sscanf(strings.TrimSuffix(body, "$y"), "%d;%d", &ps, &pv)
		on := pv == 1 || pv == 3 // 1=set, 3=permanently set (DECTEM states)
		switch ps {
		case 2026:
			ks.caps.syncOutput = on
		case 1016:
			ks.caps.sgrMousePixel = on
		}
		return true
	case strings.HasSuffix(body, "u"): // kitty keyboard query \x1b[?flagsu
		fmt.Sscanf(strings.TrimSuffix(body, "u"), "%d", &ks.caps.kittyFlags)
		return true
	case strings.HasSuffix(body, "c"): // primary DA \x1b[?…c
		ks.caps.daAnswered = true
		return true
	}
	return false
}

// syncOutput reports whether the terminal answered the ?2026 DECRPM —
// redraws wrap in bsu/esu markers then (real's synchronizedOutput gate).
func syncOutputSupported() bool {
	probeCaps.mu.Lock()
	defer probeCaps.mu.Unlock()
	return probeCaps.caps.syncOutput
}

// release hands the tty back to cooked mode — kitty flags and bracketed
// paste popped, termios restored — so an external actor ($EDITOR, or the
// shell while the process is SIGTSTP'd) owns a sane terminal. grab() undoes
// it. The read goroutine is untouched: it stays blocked in Read while the
// process is stopped and resumes with it.
func (ks *keySource) release() {
	if ks.kitty {
		os.Stdout.WriteString("\x1b[<u") // pop kitty flags
	} else if ks.mok {
		os.Stdout.WriteString("\x1b[>4m") // reset modifyOtherKeys
	}
	os.Stdout.WriteString("\x1b[?2004l")
	term.Restore(int(os.Stdin.Fd()), ks.old)
}

func (ks *keySource) grab() {
	if st, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
		ks.old = st
	}
	os.Stdout.WriteString("\x1b[?2004h")
	if ks.kitty {
		os.Stdout.WriteString("\x1b[>5u")
	} else if ks.mok {
		os.Stdout.WriteString("\x1b[>4;2m")
	}
}

const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// lastCSIStart returns the index where an unterminated CSI begins, or -1.
// The CSI probe replies (DECRPM/kitty/DA) can split across reads — hold
// them for the next buffer instead of mis-dispatching.
func lastCSIStart(b []byte) int {
	for i := len(b) - 2; i >= 0; i-- {
		if b[i] == 0x1b && b[i+1] == '[' {
			// does a final byte terminate this CSI before buffer end?
			for j := i + 2; j < len(b); j++ {
				c := b[j]
				if c >= 0x40 && c <= 0x7e && j > i+2 {
					return -1 // terminated
				}
				if c >= 0x40 && c <= 0x7e && j == i+2 {
					return -1 // \x1b[[A-style (F1) — also terminated
				}
				if c < 0x20 {
					return -1
				}
				if c > 0x3f && c < 0x60 || c >= 0x7b {
					return -1 // outside param/intermediate range → malformed
				}
			}
			return i // ran out of buffer mid-CSI
		}
	}
	return -1
}

func (ks *keySource) read() {
	var buf [512]byte
	var pasting []byte
	var pending []byte // trailing incomplete CSI held for the next read
	for {
		n, err := os.Stdin.Read(buf[len(pending):])
		if n <= 0 && err == nil {
			continue
		}
		if err != nil {
			close(ks.ch)
			return
		}
		n += len(pending)
		copy(buf[:len(pending)], pending)
		pending = nil
		// hold a truncated CSI tail for the next read
		if last := lastCSIStart(buf[:n]); last >= 0 && n-last < 32 {
			pending = append(pending, buf[last:n]...)
			n = last
		}
		for i := 0; i < n; i++ {
			c := buf[i]
			if pasting != nil {
				// look for paste end marker
				if c == 0x1b && i+5 < n && string(buf[i:i+6]) == pasteEnd {
					ks.push(key{paste: string(pasting)})
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
				ks.push(key{b: '\n', r: '\n'}) // option+enter -> newline
				i++
				continue
			}
			if c == 0x1b && i+2 < n && buf[i+1] == '[' {
				// CSI: parameter bytes (0x30-0x3f), intermediate bytes
				// (0x20-0x2f — DECRPM's $), then a final byte (0x40-0x7e)
				j := i + 2
				for j < n && ((buf[j] >= 0x30 && buf[j] <= 0x3f) || (buf[j] >= 0x20 && buf[j] <= 0x2f)) {
					j++
				}
				if j < n && buf[j] >= 0x40 && buf[j] <= 0x7e {
					if handled := ks.recordProbeReply(string(buf[i+1 : j+1])); !handled {
						ks.push(decodeCSI(string(buf[i+1 : j+1])))
					}
					// publish caps so redraws see the just-settled probe
					probeCaps.mu.Lock()
					probeCaps.caps = ks.caps
					probeCaps.mu.Unlock()
					i = j
					continue
				}
				ks.push(key{b: 0x1b, seq: string(buf[i+1 : i+3])})
				i += 2
				continue
			}
			if c == 0x1b && i+1 < n && (buf[i+1] == 0x7f || buf[i+1] == 8) {
				// ESC + backspace = meta+backspace -> backward-kill-word
				ks.push(key{b: 0x7f, mod: modAlt})
				i++
				continue
			}
			if c == 0x1b && i+1 < n && buf[i+1] >= 32 && buf[i+1] < 127 {
				// ESC + printable = alt/meta+key (real treats them identically)
				ks.push(key{b: buf[i+1], r: rune(buf[i+1]), mod: modAlt})
				i++
				continue
			}
			if c >= 0x80 {
				size := utf8Len(c)
				if i+size <= n {
					r := []rune(string(buf[i : i+size]))
					if len(r) == 1 {
						ks.push(key{b: c, r: r[0]})
					}
					i += size - 1
					continue
				}
			}
			ks.push(key{b: c})
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
		case code == 13 && mod&modCtrl == 0 && mod&(modAlt|modShift) != 0:
			// shift/alt+enter -> newline insert (real's jt: meta||shift
			// -> insert("\n")). ctrl+enter stays CR so its binding
			// (chat:sendNow) still resolves.
			k.b, k.r = '\n', '\n'
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
	if final == '~' && pnum(0) == 27 && len(nums) >= 3 {
		// xterm modifyOtherKeys: CSI 27;<mods>;<code>~ — decode like CSI-u.
		m2 := pnum(1)
		if m2 > 0 {
			m2--
		}
		k := decodeCSI("[" + strconv.Itoa(pnum(2)) + ";" + strconv.Itoa(m2+1) + "u")
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

// forward returns the channel keys should go to while a modal dialog is open.
// The send is non-blocking — a full dialog channel would otherwise wedge
// the stdin reader forever (dead prompt).
func (ks *keySource) forward(k key) bool {
	if ch := ks.dialog.Load(); ch != nil {
		select {
		case *ch <- k:
			return true
		default:
			return true // dialog owns but is full — drop rather than misroute
		}
	}
	return false
}

// push routes a key to the modal dialog when one is registered — the
// dialog owns input wherever it's open (mid-turn or idle), matching
// real's modal semantics. Falls back to ks.ch.
func (ks *keySource) push(k key) {
	if !ks.forward(k) {
		ks.ch <- k
	}
}
