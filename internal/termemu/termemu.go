// Package termemu is a minimal VT102/xterm screen emulator for tests.
// It interprets the ANSI subset our compositor/editor emit — CSI cursor
// moves, erases, scrolls, DECSTBM regions, DECSC/DECRC — into a cell grid
// so tests assert on the rendered screen, not the byte stream.
package termemu

import (
	"strings"
	"unicode/utf8"
)

// Screen is the emulated terminal grid.
type Screen struct {
	Cols, Rows int
	grid       [][]rune
	sb         [][]rune // scrollback — rows that scrolled off the region top
	pending    []byte   // split escape/rune tail held for the next Feed
	cx, cy     int      // cursor
	sx, sy     int      // saved cursor
	top, bot   int      // scroll region [top,bot) rows, 0-based
	wrap       bool
}

func New(cols, rows int) *Screen {
	g := make([][]rune, rows)
	for i := range g {
		g[i] = make([]rune, cols)
	}
	for i := range g {
		for j := range g[i] {
			g[i][j] = ' '
		}
	}
	return &Screen{Cols: cols, Rows: rows, grid: g, bot: rows}
}

// Feed consumes a byte stream, applying it to the grid. A sequence split
// across calls (partial escape or truncated UTF-8 tail — the pty reads in
// 4KB chunks) is held in pending and prepended to the next call.
func (s *Screen) Feed(b []byte) {
	if len(s.pending) > 0 {
		b = append(append([]byte{}, s.pending...), b...)
		s.pending = nil
	}
	i := 0
	for i < len(b) {
		c := b[i]
		switch {
		case c == 0x1b && i+1 < len(b) && b[i+1] == '[':
			// hold a CSI that ran out of bytes mid-sequence
			j := i + 2
			for j < len(b) && !(b[j] >= 0x40 && b[j] <= 0x7e) {
				j++
			}
			if j >= len(b) {
				s.pending = append(s.pending, b[i:]...)
				return
			}
			i = s.csi(b, i)
		case c == 0x1b && i+1 < len(b) && b[i+1] == ']':
			// OSC — skip to BEL or ST; an unterminated OSC is held
			i += 2
			for i < len(b) && b[i] != 0x07 && !(b[i] == 0x1b && i+1 < len(b) && b[i+1] == '\\') {
				i++
			}
			if i >= len(b) {
				s.pending = append(s.pending, b[max(0, len(b)-200):]...)
				return
			}
			if i < len(b) && b[i] == 0x07 {
				i++
			} else if i+1 < len(b) {
				i += 2
			}
		case c == 0x1b:
			// two-char escapes: 7/8 save/restore, others ignored — a lone
			// ESC at buffer end is held for the next call
			if i+1 >= len(b) {
				s.pending = append(s.pending, b[i:]...)
				return
			}
			switch b[i+1] {
			case '7':
				s.sx, s.sy = s.cx, s.cy
			case '8':
				s.cx, s.cy = s.sx, s.sy
			}
			i += 2
		case c == '\r':
			s.cx = 0
			i++
		case c == '\n':
			s.linefeed()
			i++
		case c == 0x07, c == 0x08:
			if c == 0x08 && s.cx > 0 {
				s.cx--
			}
			i++
		case c < 0x20:
			i++
		default:
			// incomplete UTF-8 tail — hold for the next call rather
			// than decoding junk (a split '─' rendered as 'b' via the
			// ASCII-mask fallback)
			if b[i] >= 0xC0 && !utf8.FullRune(b[i:]) {
				s.pending = append(s.pending, b[i:]...)
				return
			}
			r, w := utf8Rune(b[i:])
			s.put(r)
			i += w
		}
	}
}

// cellCont marks the second cell of a double-width glyph.
const cellCont = '\x00'

// cellWidth: 2 for East Asian Wide/Fullwidth + common emoji (TR11 ranges,
// mirroring repl's doubleWidth table — the source of truth); 0 for
// combining/zero-width marks (they merge into the previous cell).
func cellWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r >= 0x300 && r <= 0x36F, r == 0x200B, r >= 0xFE00 && r <= 0xFE0F,
		r >= 0xFE20 && r <= 0xFE2F, r >= 0x20D0 && r <= 0x20F0:
		return 0
	case r >= 0x1100 && r <= 0x115F, r == 0x231A || r == 0x231B,
		r == 0x2329 || r == 0x232A, r >= 0x23E9 && r <= 0x23EC,
		r == 0x23F0 || r == 0x23F3, r >= 0x25FD && r <= 0x25FE,
		r >= 0x2614 && r <= 0x2615, r >= 0x2648 && r <= 0x2653,
		r == 0x267F, r == 0x2693, r == 0x26A1, r == 0x26AA || r == 0x26AB,
		r >= 0x26BD && r <= 0x26BE, r == 0x26C4 || r == 0x26C5,
		r == 0x26CE, r == 0x26D4, r == 0x26EA, r >= 0x26F2 && r <= 0x26F3,
		r == 0x26F5, r == 0x26FA, r == 0x26FD || r == 0x26FE,
		r == 0x2705, r >= 0x270A && r <= 0x270B, r == 0x2728, r == 0x274C,
		r == 0x274E, r >= 0x2753 && r <= 0x2755, r == 0x2757,
		r >= 0x2795 && r <= 0x2797, r == 0x27B0, r == 0x27BF,
		r == 0x2B1B || r == 0x2B1C, r == 0x2B50, r == 0x2B55,
		r >= 0x2E80 && r <= 0x303E, r >= 0x3041 && r <= 0x33FF,
		r >= 0x3400 && r <= 0x4DBF, r >= 0x4E00 && r <= 0x9FFF,
		r >= 0xA000 && r <= 0xA4CF, r >= 0xA960 && r <= 0xA97F,
		r >= 0xAC00 && r <= 0xD7A3, r >= 0xF900 && r <= 0xFAFF,
		r >= 0xFE10 && r <= 0xFE19, r >= 0xFE30 && r <= 0xFE52,
		r >= 0xFE54 && r <= 0xFE66, r >= 0xFE68 && r <= 0xFE6B,
		r >= 0xFF00 && r <= 0xFF60, r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x16FE0 && r <= 0x16FE4, r >= 0x17000 && r <= 0x18AFF,
		r >= 0x1B000 && r <= 0x1B152, r >= 0x1B164 && r <= 0x1B167,
		r >= 0x1B170 && r <= 0x1B2FB, r == 0x1F004, r == 0x1F0CF,
		r == 0x1F18E, r >= 0x1F191 && r <= 0x1F19A, r >= 0x1F200 && r <= 0x1F202,
		r >= 0x1F210 && r <= 0x1F23B, r >= 0x1F240 && r <= 0x1F248,
		r >= 0x1F250 && r <= 0x1F251, r >= 0x1F300 && r <= 0x1F64F,
		r >= 0x1F900 && r <= 0x1F9FF, r >= 0x1FA70 && r <= 0x1FAFF,
		r >= 0x20000 && r <= 0x2FFFD, r >= 0x30000 && r <= 0x3FFFD:
		return 2
	}
	return 1
}

func (s *Screen) put(r rune) {
	if s.wrap {
		s.cx = 0
		s.linefeed()
		s.wrap = false
	}
	if s.cy < 0 || s.cy >= s.Rows || s.cx < 0 || s.cx >= s.Cols {
		return
	}
	w := cellWidth(r)
	switch {
	case w == 0:
		// combining/zero-width — merge into the cell at cx-1 (keep the
		// base glyph; the emulator doesn't need cluster fidelity)
		return
	case w == 2:
		if s.cx == s.Cols-1 {
			// wide glyph can't start in the last cell — wrap first
			s.linefeed()
			if s.cy < 0 || s.cy >= s.Rows {
				return
			}
		}
		// overwrite of a continuation clears the pair
		s.grid[s.cy][s.cx] = r
		s.grid[s.cy][s.cx+1] = cellCont
		s.cx += 2
	default:
		// a plain write over a wide glyph's tail clears the head too
		if s.grid[s.cy][s.cx] == cellCont && s.cx > 0 {
			s.grid[s.cy][s.cx-1] = ' '
		}
		s.grid[s.cy][s.cx] = r
		s.cx++
	}
	if s.cx >= s.Cols {
		s.wrap = true
		s.cx = s.Cols - 1
	}
}

func (s *Screen) linefeed() {
	if s.cy == s.bot-1 {
		s.scrollUp(s.top, s.bot)
		return
	}
	if s.cy < s.Rows-1 {
		s.cy++
	}
}

func (s *Screen) csi(b []byte, i int) int {
	i += 2
	// params
	pStart := i
	for i < len(b) && !(b[i] >= 0x40 && b[i] <= 0x7e) {
		i++
	}
	if i >= len(b) {
		return i
	}
	params := string(b[pStart:i])
	final := b[i]
	i++
	priv := strings.HasPrefix(params, "?") || strings.HasPrefix(params, ">")
	params = strings.TrimLeft(params, "?><!")
	args := []int{0}
	if params != "" {
		args = nil
		for _, p := range strings.Split(params, ";") {
			n := 0
			for _, d := range p {
				if d >= '0' && d <= '9' {
					n = n*10 + int(d-'0')
				}
			}
			args = append(args, n)
		}
	}
	arg := func(d int) int {
		if len(args) == 0 || args[0] == 0 {
			return d
		}
		return args[0]
	}
	if priv {
		return i // modes — not tracked
	}
	switch final {
	case 'H', 'f':
		r, c := 1, 1
		if len(args) > 0 && args[0] > 0 {
			r = args[0]
		}
		if len(args) > 1 && args[1] > 0 {
			c = args[1]
		}
		s.cy, s.cx = clamp(r-1, s.Rows), clamp(c-1, s.Cols)
		s.wrap = false
	case 'A':
		s.cy = clamp(s.cy-arg(1), s.Rows)
		s.wrap = false
	case 'B', 'e':
		s.cy = clamp(s.cy+arg(1), s.Rows)
		s.wrap = false
	case 'C':
		s.cx = clamp(s.cx+arg(1), s.Cols)
	case 'D':
		s.cx = clamp(s.cx-arg(1), s.Cols)
	case 'E':
		s.cy = clamp(s.cy+arg(1), s.Rows)
		s.cx = 0
		s.wrap = false
	case 'F':
		s.cy = clamp(s.cy-arg(1), s.Rows)
		s.cx = 0
		s.wrap = false
	case 'G', '`':
		s.cx = clamp(arg(1)-1, s.Cols)
	case 'd':
		s.cy = clamp(arg(1)-1, s.Rows)
		s.wrap = false
	case 'J':
		switch arg(0) {
		case 0:
			s.eraseToEnd()
		case 2:
			for r := range s.grid {
				for j := range s.grid[r] {
					s.grid[r][j] = ' '
				}
			}
		}
	case 'K':
		switch arg(0) {
		case 0:
			for j := s.cx; j < s.Cols; j++ {
				s.grid[s.cy][j] = ' '
			}
		case 2:
			for j := range s.grid[s.cy] {
				s.grid[s.cy][j] = ' '
			}
		}
	case 'r':
		s.top = 0
		s.bot = s.Rows
		if len(args) > 0 && args[0] > 0 {
			s.top = args[0] - 1
		}
		if len(args) > 1 && args[1] > 0 {
			s.bot = args[1]
		}
	case 'S': // scroll region up n lines
		for n := 0; n < arg(1); n++ {
			s.scrollUp(s.top, s.bot)
		}
	case 'T': // scroll region down n
		for n := 0; n < arg(1); n++ {
			s.scrollDown(s.top, s.bot)
		}
	case 's':
		s.sx, s.sy = s.cx, s.cy
	case 'u':
		s.cx, s.cy = s.sx, s.sy
		s.wrap = false
	case 'L': // insert lines at cursor row
		for n := 0; n < arg(1); n++ {
			s.scrollDown(s.cy, s.bot)
		}
	case 'M': // delete lines
		for n := 0; n < arg(1); n++ {
			s.scrollUp(s.cy, s.bot)
		}
	}
	return i
}

// scrollUp/Down rotate row storage — copy on [][]rune aliases backing
// arrays, so the scrolled-out row is recycled instead of zeroed in place.
// Rows leaving the region top are pushed to scrollback.
func (s *Screen) scrollUp(top, bot int) {
	row := s.grid[top]
	// a copy goes to scrollback (row itself is recycled to the bottom)
	rec := make([]rune, s.Cols)
	copy(rec, row)
	s.sb = append(s.sb, rec)
	copy(s.grid[top:bot-1], s.grid[top+1:bot])
	s.grid[bot-1] = row
	for j := range row {
		row[j] = ' '
	}
}

func (s *Screen) scrollDown(top, bot int) {
	row := s.grid[bot-1]
	copy(s.grid[top+1:bot], s.grid[top:bot-1])
	s.grid[top] = row
	for j := range row {
		row[j] = ' '
	}
}

func (s *Screen) eraseToEnd() {
	for j := s.cx; j < s.Cols; j++ {
		s.grid[s.cy][j] = ' '
	}
	for r := s.cy + 1; r < s.Rows; r++ {
		for j := range s.grid[r] {
			s.grid[r][j] = ' '
		}
	}
}

func clamp(v, hi int) int {
	if v < 0 {
		return 0
	}
	if v >= hi {
		return hi - 1
	}
	return v
}

func rowText(row []rune) string {
	// continuation cells of wide glyphs render nothing
	var b strings.Builder
	for _, r := range row {
		if r == cellCont || r == 0 {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimRight(b.String(), " ")
}

// Lines renders the grid top-down, right-trimmed; trailing blank lines
// dropped.
func (s *Screen) Lines() []string {
	out := make([]string, 0, s.Rows)
	for r := 0; r < s.Rows; r++ {
		out = append(out, rowText(s.grid[r]))
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// Scrollback renders the rows that scrolled off the region top —
// xterm-style history. Newest last.
func (s *Screen) Scrollback() []string {
	out := make([]string, 0, len(s.sb))
	for _, r := range s.sb {
		out = append(out, rowText(r))
	}
	return out
}

// History returns scrollback + current grid — the full visible record.
func (s *Screen) History() []string {
	return append(s.Scrollback(), s.Lines()...)
}

// String renders the grid as text.
func (s *Screen) String() string {
	return strings.Join(s.Lines(), "\n")
}

// Cursor returns the emulated cursor (row, col).
func (s *Screen) Cursor() (int, int) { return s.cy, s.cx }

// utf8Rune decodes one rune from b.
func utf8Rune(b []byte) (rune, int) {
	r, w := utf8.DecodeRune(b)
	if r == utf8.RuneError && w <= 1 {
		return rune(b[0] & 0x7f), 1
	}
	return r, w
}
