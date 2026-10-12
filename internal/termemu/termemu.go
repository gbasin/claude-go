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
	cx, cy     int // cursor
	sx, sy     int // saved cursor
	top, bot   int // scroll region [top,bot) rows, 0-based
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

// Feed consumes a byte stream, applying it to the grid.
func (s *Screen) Feed(b []byte) {
	i := 0
	for i < len(b) {
		c := b[i]
		switch {
		case c == 0x1b && i+1 < len(b) && b[i+1] == '[':
			i = s.csi(b, i)
		case c == 0x1b && i+1 < len(b) && b[i+1] == ']':
			// OSC — skip to BEL or ST
			i += 2
			for i < len(b) && b[i] != 0x07 && !(b[i] == 0x1b && i+1 < len(b) && b[i+1] == '\\') {
				i++
			}
			if i < len(b) && b[i] == 0x07 {
				i++
			} else if i+1 < len(b) {
				i += 2
			}
		case c == 0x1b && i+1 < len(b):
			// two-char escapes: 7/8 save/restore, others ignored
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
			r, w := utf8Rune(b[i:])
			s.put(r)
			i += w
		}
	}
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
	s.grid[s.cy][s.cx] = r
	s.cx++
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
func (s *Screen) scrollUp(top, bot int) {
	row := s.grid[top]
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

// Lines renders the grid top-down, right-trimmed; trailing blank lines
// dropped.
func (s *Screen) Lines() []string {
	out := make([]string, 0, s.Rows)
	for r := 0; r < s.Rows; r++ {
		out = append(out, strings.TrimRight(string(s.grid[r]), " "))
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
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
