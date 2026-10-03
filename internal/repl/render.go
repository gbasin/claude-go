package repl

import (
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
}

func newMDRenderer() *mdRenderer {
	return &mdRenderer{lineStart: true}
}

func (r *mdRenderer) Write(s string) {
	r.pending.WriteString(s)
	r.flush(false)
}

// Close flushes any held formatting chars.
func (r *mdRenderer) Close() { r.flush(true) }

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
			os.Stdout.WriteString(l + "\n")
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
	os.Stdout.WriteString(border("┌", "┬", "┐") + "\n")
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
		os.Stdout.WriteString(b.String() + "\n")
		if ri == 0 {
			os.Stdout.WriteString(border("├", "┼", "┤") + "\n")
		}
	}
	os.Stdout.WriteString(border("└", "┴", "┘") + "\n")
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
			os.Stdout.WriteString(reset + "\n")
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
					os.Stdout.WriteString(dim)
				} else {
					r.lang = ""
					os.Stdout.WriteString(reset)
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
						os.Stdout.WriteString(bold)
						i = j + 1
						r.lineStart = false
						continue
					}
				}
				if strings.HasPrefix(rest, "- ") || strings.HasPrefix(rest, "* ") {
					os.Stdout.WriteString("  " + cyan + "•" + reset + " ")
					i += 2
					r.lineStart = false
					continue
				}
				if strings.HasPrefix(rest, "> ") {
					os.Stdout.WriteString(dim + "▎" + reset)
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
				os.Stdout.WriteString(hl)
			} else {
				os.Stdout.WriteString(s[i:j])
			}
			i = j
			continue
		}
		// inline formatting
		if strings.HasPrefix(s[i:], "**") {
			r.bold = !r.bold
			if r.bold {
				os.Stdout.WriteString(bold)
			} else {
				os.Stdout.WriteString(reset)
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
					os.Stdout.WriteString("\x1b]8;;" + url + "\x07" + "\x1b[4m" + cyan + text + reset + "\x1b]8;;\x07")
					i += close + 2 + end + 1
					continue
				}
			}
		}
		if s[i] == '`' {
			r.inlineCode = !r.inlineCode
			if r.inlineCode {
				os.Stdout.WriteString(cyan)
			} else {
				os.Stdout.WriteString(reset)
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
				os.Stdout.WriteString("\x1b[3m")
			} else {
				os.Stdout.WriteString(reset)
			}
			i++
			continue
		}
		os.Stdout.WriteString(string(c))
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
