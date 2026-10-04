package repl

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// mdRenderer incrementally renders streamed markdown to ANSI.
// Inline: `code` -> cyan, **bold** -> bold, *italic* -> italic,
// [text](url) -> OSC8 hyperlink. Blocks: # headers -> bold, ``` fence ->
// syntax-highlighted, "- " -> bullet, | tables -> box-drawing.
type mdRenderer struct {
	lineStart  bool
	fence      bool
	inlineCode bool
	bold       bool
	italic     bool
	lang       string
	table      []string // buffered |...| lines pending box render
	pending    strings.Builder
	out        stringWriter // emit sink — compInline-aware under the zone
}

func newMDRenderer() *mdRenderer {
	return &mdRenderer{lineStart: true, out: stdoutWriter{}}
}

// stdoutWriter is the non-compositor sink (plain scroll output).
type stdoutWriter struct{}

func (stdoutWriter) WriteString(s string) { os.Stdout.WriteString(s) }

func (r *mdRenderer) Write(s string) {
	r.pending.WriteString(s)
	r.flush(false)
}

// Close flushes any held formatting chars.
func (r *mdRenderer) Close() {
	r.flush(true)
	if tw, ok := r.out.(*turnWriter); ok {
		tw.finish()
	}
}

// flushTable renders buffered table lines as a box — or verbatim if it isn't
// actually a table (no |---| separator row).
func (r *mdRenderer) flushTable() {
	if len(r.table) == 0 {
		return
	}
	lines := r.table
	r.table = nil
	// need a separator row like |---|---| in position 1
	isTable := len(lines) >= 2 && isTableSep(lines[1])
	if !isTable {
		for _, l := range lines {
			r.out.WriteString(l + "\n")
		}
		return
	}
	rows := make([][]string, 0, len(lines)-2)
	rows = append(rows, splitRow(lines[0]))
	for _, l := range lines[2:] {
		rows = append(rows, splitRow(l))
	}
	cols := 0
	for _, row := range rows {
		if len(row) > cols {
			cols = len(row)
		}
	}
	widths := make([]int, cols)
	for _, row := range rows {
		for i, c := range row {
			if w := displayWidth(c); w > widths[i] {
				widths[i] = w
			}
		}
	}
	border := func(l, m, rt string) string {
		var b strings.Builder
		b.WriteString(dim + l)
		for i, w := range widths {
			b.WriteString(strings.Repeat("─", w+2))
			if i < len(widths)-1 {
				b.WriteString(m)
			}
		}
		b.WriteString(rt + reset)
		return b.String()
	}
	r.out.WriteString(border("┌", "┬", "┐") + "\n")
	for ri, row := range rows {
		var b strings.Builder
		b.WriteString(dim + "│" + reset)
		for i := 0; i < cols; i++ {
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			pad := widths[i] - displayWidth(cell)
			style := ""
			if ri == 0 {
				style = bold // header row
			}
			b.WriteString(" " + style + cell + reset + strings.Repeat(" ", pad) + " " + dim + "│" + reset)
		}
		r.out.WriteString(b.String() + "\n")
		if ri == 0 {
			r.out.WriteString(border("├", "┼", "┤") + "\n")
		}
	}
	r.out.WriteString(border("└", "┴", "┘") + "\n")
}

func isTableSep(l string) bool {
	l = strings.TrimSpace(l)
	if !strings.HasPrefix(l, "|") {
		return false
	}
	for _, c := range l {
		if c != '|' && c != '-' && c != ':' && c != ' ' {
			return false
		}
	}
	return strings.Contains(l, "-")
}

func splitRow(l string) []string {
	l = strings.TrimSpace(l)
	l = strings.TrimPrefix(l, "|")
	l = strings.TrimSuffix(l, "|")
	parts := strings.Split(l, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// displayWidth counts runes (approximation of cell width; CJK counted as 2).
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w++
		if r > 0x2E7F { // crude wide-char check
			w++
		}
	}
	return w
}

func (r *mdRenderer) flush(eof bool) {
	s := r.pending.String()
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '\n' {
			r.out.WriteString(reset + "\n")
			r.lineStart = true
			r.inlineCode, r.bold, r.italic = false, false, false
			i++
			continue
		}
		if r.lineStart {
			rest := s[i:]
			// table row start (never inside a fence)
			if !r.fence && strings.HasPrefix(rest, "|") {
				// buffer the whole line
				j := strings.IndexByte(rest, '\n')
				if j < 0 {
					if !eof {
						// hold incomplete table line
						r.pending.Reset()
						r.pending.WriteString(s[i:])
						return
					}
					r.table = append(r.table, rest)
					i = len(s)
					r.flushTable()
					break
				}
				r.table = append(r.table, rest[:j])
				i += j + 1
				r.lineStart = true
				continue
			}
			// non-table line → flush any pending table
			r.flushTable()
			if strings.HasPrefix(rest, "```") {
				r.fence = !r.fence
				j := i + 3
				if r.fence {
					langEnd := j
					for langEnd < len(s) && s[langEnd] != '\n' {
						langEnd++
					}
					r.lang = strings.TrimSpace(s[j:langEnd])
					r.out.WriteString(dim)
				} else {
					r.lang = ""
					r.out.WriteString(reset)
				}
				i = j
				for j < len(s) && s[j] != '\n' {
					j++
				}
				i = j
				continue
			}
			if !r.fence {
				if strings.HasPrefix(rest, "#") {
					j := i
					for j < len(s) && s[j] == '#' {
						j++
					}
					if j < len(s) && s[j] == ' ' {
						r.out.WriteString(bold)
						i = j + 1
						r.lineStart = false
						continue
					}
				}
				if strings.HasPrefix(rest, "- ") || strings.HasPrefix(rest, "* ") {
					r.out.WriteString("  " + cyan + "•" + reset + " ")
					i += 2
					r.lineStart = false
					continue
				}
				if strings.HasPrefix(rest, "> ") {
					r.out.WriteString(dim + "▎" + reset)
					i += 2
					r.lineStart = false
					continue
				}
			}
			r.lineStart = false
		}
		if r.fence {
			j := i
			for j < len(s) && s[j] != '\n' {
				j++
			}
			if hl := highlightLine(s[i:j], r.lang); hl != "" {
				r.out.WriteString(hl)
			} else {
				r.out.WriteString(s[i:j])
			}
			i = j
			continue
		}
		// inline formatting
		if strings.HasPrefix(s[i:], "**") {
			r.bold = !r.bold
			if r.bold {
				r.out.WriteString(bold)
			} else {
				r.out.WriteString(reset)
			}
			i += 2
			continue
		}
		// [text](url) → OSC8 hyperlink
		if s[i] == '[' {
			if close := strings.IndexByte(s[i:], ']'); close > 0 &&
				close+1 < len(s)-i && s[i+close+1] == '(' {
				end := strings.IndexByte(s[i+close+2:], ')')
				if end >= 0 {
					text := s[i+1 : i+close]
					url := s[i+close+2 : i+close+2+end]
					r.out.WriteString("\x1b]8;;" + url + "\x07" + "\x1b[4m" + cyan + text + reset + "\x1b]8;;\x07")
					i += close + 2 + end + 1
					continue
				}
			}
		}
		if s[i] == '`' {
			r.inlineCode = !r.inlineCode
			if r.inlineCode {
				r.out.WriteString(cyan)
			} else {
				r.out.WriteString(reset)
			}
			i++
			continue
		}
		if s[i] == '*' && !strings.HasPrefix(s[i:], "**") {
			if i == len(s)-1 && !eof {
				r.pending.Reset()
				r.pending.WriteString(s[i:])
				return
			}
			r.italic = !r.italic
			if r.italic {
				r.out.WriteString("\x1b[3m")
			} else {
				r.out.WriteString(reset)
			}
			i++
			continue
		}
		r.out.WriteString(string(c))
		i++
	}
	r.pending.Reset()
	if eof {
		r.flushTable()
	}
}

// table-line bookkeeping
func (r *mdRenderer) inTableLine() bool  { return false } // rows buffered at lineStart
func (r *mdRenderer) lineFrom() int      { return 0 }
func (r *mdRenderer) holdFrom(i int) int { return i }

var _ = utf8.RuneCountInString // kept for future grapheme-width work

type stringWriter interface{ WriteString(string) }

// turnWriter routes streamed turn output through compInline so text lands
// in the scroll region above the pinned zone (cursor rests in the input
// box during a turn; raw writes were painted over).
// turnWriter emits streamed turn output at the scroll-region bottom.
// During a turn the zone rows are repaint-only (cursor save/restore), so
// plain writes scroll correctly above the pinned zone — the earlier
// compInline-per-write approach repositioned to row-start and wrapped
// remnants duplicated lines.
type turnWriter struct{ line strings.Builder }

func (w *turnWriter) WriteString(s string) {
	if f := os.Getenv("CG_TRACEF"); f != "" {
		if fh, _ := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); fh != nil {
			fmt.Fprintf(fh, "tw %q streamOn=%v row=%d col=%d\n", s, compStreamOn, compStreamRow, compStreamCol)
			fh.Close()
		}
	}
	// raw mode is live during a turn — bare \n moves down without
	// returning carriage, so translate to CRLF for terminal semantics
	if strings.Contains(s, "\n") {
		s = strings.ReplaceAll(s, "\r\n", "\n") // normalize then expand
		s = strings.ReplaceAll(s, "\n", "\r\n")
	}
	compStreamWrite(s)
}

// finish resets per-turn state (no flush needed — writes are immediate).
func (w *turnWriter) finish() { w.line.Reset() }
